package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// agentProc is one running ACP agent child process and its JSON-RPC connection.
type agentProc struct {
	cmd  *exec.Cmd
	conn *acp.ClientSideConnection
	gen  int
	name string
}

func (p *agentProc) alive() bool {
	select {
	case <-p.conn.Done():
		return false
	default:
		return true
	}
}

// authTimeout bounds the authenticate call; browser-based OAuth waits for the
// user to finish signing in.
const authTimeout = 5 * time.Minute

// ensureAgent returns the live agent process, (re)starting it if needed. Starts
// are serialised by startMu so a slow authenticate never blocks b.mu, which the
// HTTP handlers and agent callbacks need.
func (b *Bridge) ensureAgent(ctx context.Context) (*agentProc, error) {
	b.startMu.Lock()
	defer b.startMu.Unlock()

	b.mu.Lock()
	p := b.proc
	b.mu.Unlock()
	if p != nil && p.alive() {
		return p, nil
	}
	if len(b.cfg.AgentCmd) == 0 {
		return nil, errors.New("no agent command configured")
	}
	cmd := exec.Command(b.cfg.AgentCmd[0], b.cfg.AgentCmd[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Dir = b.cfg.Cwd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}
	kill := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	conn := acp.NewClientSideConnection(&agentClient{b: b}, stdin, stdout)
	conn.SetLogger(slog.Default())

	// No fs/terminal capabilities: the agent uses its own tools, so the bridge
	// never has to touch the filesystem on its behalf.
	resp, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion:    acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{},
	})
	if err != nil {
		kill()
		return nil, fmt.Errorf("initialize agent: %w", err)
	}
	if b.cfg.AuthMethod != "" {
		authCtx, cancel := context.WithTimeout(ctx, authTimeout)
		_, err := conn.Authenticate(authCtx, acp.AuthenticateRequest{MethodId: b.cfg.AuthMethod})
		cancel()
		if err != nil {
			kill()
			return nil, fmt.Errorf("authenticate (%s): %w", b.cfg.AuthMethod, err)
		}
	}
	name := b.cfg.ModelLabel
	if name == "" && resp.AgentInfo != nil {
		name = resp.AgentInfo.Name
	}
	if name == "" {
		name = "acp-agent"
	}

	b.mu.Lock()
	b.gen++
	p = &agentProc{cmd: cmd, conn: conn, gen: b.gen, name: name}
	b.proc = p
	b.mu.Unlock()
	go func() {
		_ = p.cmd.Wait()
		slog.Warn("acp agent exited", "gen", p.gen)
	}()
	return p, nil
}

// Close stops the agent process.
func (b *Bridge) Close() {
	b.mu.Lock()
	p := b.proc
	b.mu.Unlock()
	if p != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// agentClient is the ACP client side: it routes agent callbacks to the run that
// owns the ACP session.
type agentClient struct{ b *Bridge }

var _ acp.Client = (*agentClient)(nil)

func (c *agentClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	run := c.b.runForACP(n.SessionId)
	if run == nil {
		return nil
	}
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		if t := u.AgentMessageChunk.Content.Text; t != nil && t.Text != "" {
			run.appendText(t.Text)
			run.emit("message.delta", map[string]any{"delta": t.Text})
		}
	case u.AgentThoughtChunk != nil:
		if t := u.AgentThoughtChunk.Content.Text; t != nil && t.Text != "" {
			run.emit("reasoning.available", map[string]any{"text": t.Text})
		}
	case u.ToolCall != nil:
		tc := u.ToolCall
		run.setTool(string(tc.ToolCallId), tc.Title)
		run.emit("tool.started", map[string]any{
			"tool":    tc.Title,
			"preview": preview(tc.RawInput),
		})
		emitToolDone(run, string(tc.ToolCallId), &tc.Status)
	case u.ToolCallUpdate != nil:
		tu := u.ToolCallUpdate
		if tu.Title != nil && *tu.Title != "" {
			run.setTool(string(tu.ToolCallId), *tu.Title)
		}
		emitToolDone(run, string(tu.ToolCallId), tu.Status)
	}
	return nil
}

func emitToolDone(run *Run, id string, status *acp.ToolCallStatus) {
	if status == nil {
		return
	}
	switch *status {
	case acp.ToolCallStatusCompleted:
		run.emit("tool.completed", map[string]any{"tool": run.toolTitle(id), "error": false})
	case acp.ToolCallStatusFailed:
		run.emit("tool.completed", map[string]any{"tool": run.toolTitle(id), "error": true})
	}
}

func preview(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// RequestPermission surfaces the agent's permission prompt as an
// approval.request event and blocks until DeepTutor answers (or the run stops).
func (c *agentClient) RequestPermission(ctx context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	cancelled := acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}
	run := c.b.runForACP(p.SessionId)
	if run == nil {
		return cancelled, nil
	}
	title := ""
	if p.ToolCall.Title != nil {
		title = *p.ToolCall.Title
	}
	opts := make([]map[string]string, 0, len(p.Options))
	for _, o := range p.Options {
		opts = append(opts, map[string]string{
			"id": string(o.OptionId), "name": o.Name, "kind": string(o.Kind),
		})
	}
	ch := run.beginApproval()
	defer run.endApproval()
	run.emit("approval.request", map[string]any{"tool": title, "options": opts})

	var choice string
	select {
	case choice = <-ch:
	case <-ctx.Done():
		return cancelled, nil
	}
	if id, ok := pickOption(p.Options, choice); ok {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(id)}, nil
	}
	return cancelled, nil
}

// pickOption maps DeepTutor's once/deny choice onto an ACP permission option.
func pickOption(opts []acp.PermissionOption, choice string) (acp.PermissionOptionId, bool) {
	want := []acp.PermissionOptionKind{acp.PermissionOptionKindRejectOnce, acp.PermissionOptionKindRejectAlways}
	if choice == choiceOnce {
		want = []acp.PermissionOptionKind{acp.PermissionOptionKindAllowOnce, acp.PermissionOptionKindAllowAlways}
	}
	for _, k := range want {
		for _, o := range opts {
			if o.Kind == k {
				return o.OptionId, true
			}
		}
	}
	return "", false
}

var errUnsupported = errors.New("not supported by deeptutor_acp_bridge")

func (c *agentClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errUnsupported
}

func (c *agentClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errUnsupported
}

func (c *agentClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errUnsupported
}

func (c *agentClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errUnsupported
}

func (c *agentClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errUnsupported
}

func (c *agentClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errUnsupported
}

func (c *agentClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errUnsupported
}

func joinNonEmpty(sep string, parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
