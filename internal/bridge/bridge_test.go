package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testKey = "test-key"

var fakeAgentBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakeagent")
	if err != nil {
		panic(err)
	}
	fakeAgentBin = filepath.Join(dir, "fakeagent")
	if out, err := exec.Command("go", "build", "-o", fakeAgentBin, "../../cmd/fakeagent").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func newServer(t *testing.T) (*httptest.Server, *Bridge) {
	t.Helper()
	return newServerWith(t, Config{})
}

func newServerWith(t *testing.T, cfg Config) (*httptest.Server, *Bridge) {
	t.Helper()
	cfg.AgentCmd = []string{fakeAgentBin}
	cfg.APIKey = testKey
	b := New(cfg)
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(func() { srv.Close(); b.Close() })
	return srv, b
}

func do(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func startRun(t *testing.T, srv *httptest.Server, input, session string) (runID, sessionID string) {
	t.Helper()
	body := map[string]any{"input": input, "instructions": "be brief"}
	if session != "" {
		body["session_id"] = session
	}
	status, out := do(t, srv, "POST", "/v1/runs", body)
	if status != http.StatusAccepted {
		t.Fatalf("POST /v1/runs = %d %v", status, out)
	}
	return out["run_id"].(string), out["session_id"].(string)
}

// readEvents follows the SSE stream, calling onEvent for each event, until the
// run's terminal event and [DONE] arrive.
func readEvents(t *testing.T, srv *httptest.Server, runID string, onEvent func(map[string]any)) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/runs/"+runID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []map[string]any
	sawDone := false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == "data: [DONE]" {
			sawDone = true
			break
		}
		if raw, ok := strings.CutPrefix(line, "data: "); ok {
			var ev map[string]any
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				t.Fatalf("bad event json %q: %v", raw, err)
			}
			events = append(events, ev)
			if onEvent != nil {
				onEvent(ev)
			}
		}
	}
	if !sawDone {
		t.Fatalf("stream ended without [DONE]; events=%v err=%v", events, sc.Err())
	}
	return events
}

func names(evs []map[string]any) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i], _ = e["event"].(string)
	}
	return out
}

func TestAuthRequired(t *testing.T) {
	srv, _ := newServer(t)
	resp, err := http.Get(srv.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestCapabilities(t *testing.T) {
	srv, _ := newServer(t)
	status, out := do(t, srv, "GET", "/v1/capabilities", nil)
	if status != 200 || out["object"] != "hermes.api_server.capabilities" {
		t.Fatalf("got %d %v", status, out)
	}
	feats := out["features"].(map[string]any)
	for _, k := range []string{"run_submission", "run_events_sse", "run_stop", "run_approval_response", "session_resources"} {
		if feats[k] != true {
			t.Errorf("feature %s = %v", k, feats[k])
		}
	}
}

func TestEchoRunAndHistory(t *testing.T) {
	srv, _ := newServer(t)
	runID, sid := startRun(t, srv, "hello", "")
	evs := readEvents(t, srv, runID, nil)
	last := evs[len(evs)-1]
	// The first prompt of a new ACP session carries the instructions.
	const firstOutput = "echo: be brief\n\nhello"
	if last["event"] != "run.completed" || last["output"] != firstOutput {
		t.Fatalf("unexpected terminal event: %v (all %v)", last, names(evs))
	}

	status, out := do(t, srv, "GET", "/api/sessions/"+sid+"/messages", nil)
	if status != 200 || out["object"] != "list" {
		t.Fatalf("messages = %d %v", status, out)
	}
	data := out["data"].([]any)
	if len(data) != 2 || data[0].(map[string]any)["role"] != "user" ||
		data[1].(map[string]any)["content"] != firstOutput {
		t.Fatalf("history = %v", data)
	}

	// Second turn on the same session reuses it.
	runID2, sid2 := startRun(t, srv, "again", sid)
	if sid2 != sid {
		t.Fatalf("session changed: %s -> %s", sid, sid2)
	}
	evs = readEvents(t, srv, runID2, nil)
	if got := evs[len(evs)-1]["output"]; got != "echo: again" {
		t.Fatalf("second output = %v", got)
	}
}

func TestUnknownSessionMessages404(t *testing.T) {
	srv, _ := newServer(t)
	if status, _ := do(t, srv, "GET", "/api/sessions/nope/messages", nil); status != 404 {
		t.Fatalf("status = %d", status)
	}
}

func TestToolAndReasoningEvents(t *testing.T) {
	srv, _ := newServer(t)
	runID, _ := startRun(t, srv, "use a tool", "")
	got := strings.Join(names(readEvents(t, srv, runID, nil)), ",")
	want := "reasoning.available,tool.started,tool.completed,message.delta,run.completed"
	if got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}

func approvalFlow(t *testing.T, choice, wantVerdict string) {
	srv, _ := newServer(t)
	runID, _ := startRun(t, srv, "please approve", "")
	evs := readEvents(t, srv, runID, func(ev map[string]any) {
		if ev["event"] == "approval.request" {
			status, out := do(t, srv, "POST", "/v1/runs/"+runID+"/approval", map[string]any{"choice": choice})
			if status != 200 {
				t.Errorf("approval = %d %v", status, out)
			}
		}
	})
	last := evs[len(evs)-1]
	if last["event"] != "run.completed" || last["output"] != "permission "+wantVerdict {
		t.Fatalf("terminal = %v (all %v)", last, names(evs))
	}
}

func TestApprovalOnce(t *testing.T) { approvalFlow(t, "once", "allowed") }
func TestApprovalDeny(t *testing.T) { approvalFlow(t, "deny", "denied") }

func TestApprovalWithoutPending(t *testing.T) {
	srv, _ := newServer(t)
	runID, _ := startRun(t, srv, "hi", "")
	readEvents(t, srv, runID, nil)
	status, _ := do(t, srv, "POST", "/v1/runs/"+runID+"/approval", map[string]any{"choice": "once"})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
}

func TestStop(t *testing.T) {
	srv, _ := newServer(t)
	runID, sid := startRun(t, srv, "slow one", "")
	go func() {
		time.Sleep(300 * time.Millisecond)
		do(t, srv, "POST", "/v1/runs/"+runID+"/stop", map[string]any{})
	}()
	evs := readEvents(t, srv, runID, nil)
	if last := evs[len(evs)-1]["event"]; last != "run.cancelled" {
		t.Fatalf("terminal = %v", last)
	}
	// Session is usable again after a stop.
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, _ := do(t, srv, "POST", "/v1/runs", map[string]any{"input": "x", "session_id": sid})
		if status == http.StatusAccepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session stayed busy after stop (last status %d)", status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestBusySession(t *testing.T) {
	srv, _ := newServer(t)
	runID, sid := startRun(t, srv, "slow", "")
	status, _ := do(t, srv, "POST", "/v1/runs", map[string]any{"input": "x", "session_id": sid})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	do(t, srv, "POST", "/v1/runs/"+runID+"/stop", map[string]any{})
	readEvents(t, srv, runID, nil)
}

func TestInputRequired(t *testing.T) {
	srv, _ := newServer(t)
	if status, _ := do(t, srv, "POST", "/v1/runs", map[string]any{"input": "  "}); status != 400 {
		t.Fatalf("status = %d", status)
	}
}

func TestAgentRestartReplaysHistory(t *testing.T) {
	srv, b := newServer(t)
	runID, sid := startRun(t, srv, "first", "")
	readEvents(t, srv, runID, nil)

	b.mu.Lock()
	p := b.proc
	b.mu.Unlock()
	_ = p.cmd.Process.Kill()
	<-p.conn.Done()

	runID, _ = startRun(t, srv, "second", sid)
	evs := readEvents(t, srv, runID, nil)
	out, _ := evs[len(evs)-1]["output"].(string)
	// The fresh ACP session has no memory, so the bridge must replay history.
	if !strings.Contains(out, "Previous conversation:") || !strings.Contains(out, "user: first") {
		t.Fatalf("history not replayed after restart: %q", out)
	}
}

func TestAuthenticateBeforeSession(t *testing.T) {
	t.Setenv("FAKE_AUTH_METHOD", "fake-key")
	srv, _ := newServerWith(t, Config{AuthMethod: "fake-key"})
	runID, _ := startRun(t, srv, "hello", "")
	evs := readEvents(t, srv, runID, nil)
	if last := evs[len(evs)-1]; last["event"] != "run.completed" {
		t.Fatalf("terminal = %v", last)
	}
}

func TestMissingAuthMethodFailsRun(t *testing.T) {
	t.Setenv("FAKE_AUTH_METHOD", "fake-key")
	srv, _ := newServer(t)
	runID, _ := startRun(t, srv, "hello", "")
	evs := readEvents(t, srv, runID, nil)
	last := evs[len(evs)-1]
	if last["event"] != "run.failed" || !strings.Contains(last["error"].(string), "authentication required") {
		t.Fatalf("terminal = %v", last)
	}
}

func TestWrongAuthMethodFailsRun(t *testing.T) {
	t.Setenv("FAKE_AUTH_METHOD", "fake-key")
	srv, _ := newServerWith(t, Config{AuthMethod: "other"})
	runID, _ := startRun(t, srv, "hello", "")
	evs := readEvents(t, srv, runID, nil)
	last := evs[len(evs)-1]
	if last["event"] != "run.failed" || !strings.Contains(last["error"].(string), "authenticate (other)") {
		t.Fatalf("terminal = %v", last)
	}
}
