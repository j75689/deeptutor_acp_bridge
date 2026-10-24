// Package bridge exposes the Hermes Agent gateway HTTP/SSE contract that
// DeepTutor's "hermes_remote" backend speaks, and drives an ACP agent behind it.
package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// Config configures a Bridge.
type Config struct {
	// AgentCmd is the ACP agent command line (stdio JSON-RPC).
	AgentCmd []string
	// Cwd is the working directory handed to ACP sessions.
	Cwd string
	// APIKey is the bearer token DeepTutor must present.
	APIKey string
	// ModelLabel overrides the name reported by /v1/capabilities.
	ModelLabel string
}

// Message is one resumable history row served by /api/sessions/{id}/messages.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Session is a DeepTutor-visible conversation. It owns one ACP session, which
// is recreated (with history replayed as context) if the agent process restarts.
type Session struct {
	ID string

	mu      sync.Mutex
	history []Message
	acpID   acp.SessionId
	gen     int
	busy    bool
}

const maxHistory = 40

// Bridge holds sessions, runs and the shared ACP agent process.
type Bridge struct {
	cfg Config

	mu       sync.Mutex
	proc     *agentProc
	gen      int
	sessions map[string]*Session
	runs     map[string]*Run
	byACP    map[acp.SessionId]*Run
}

// New builds a Bridge. The agent process starts lazily on first use.
func New(cfg Config) *Bridge {
	if cfg.Cwd == "" {
		cfg.Cwd = "."
	}
	return &Bridge{
		cfg:      cfg,
		sessions: map[string]*Session{},
		runs:     map[string]*Run{},
		byACP:    map[acp.SessionId]*Run{},
	}
}

func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func (b *Bridge) runForACP(id acp.SessionId) *Run {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.byACP[id]
}

func (b *Bridge) getRun(id string) *Run {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runs[id]
}

func (b *Bridge) getSession(id string) *Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[id]
}

var errBusy = errors.New("session already has an active run")

// startRun registers a run and executes it in the background.
func (b *Bridge) startRun(sessionID, input, instructions string, history []Message) (*Run, error) {
	b.mu.Lock()
	sess := b.sessions[sessionID]
	if sess == nil {
		if sessionID == "" {
			sessionID = newID("s_")
		}
		sess = &Session{ID: sessionID}
		// DeepTutor re-sends history when it resumes a session this bridge no
		// longer holds; keep it so it can be replayed to a fresh ACP session.
		sess.history = trimHistory(history)
		b.sessions[sessionID] = sess
	}
	b.mu.Unlock()

	sess.mu.Lock()
	if sess.busy {
		sess.mu.Unlock()
		return nil, errBusy
	}
	sess.busy = true
	sess.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	run := newRun(newID("r_"), sess.ID, cancel)
	b.mu.Lock()
	b.runs[run.ID] = run
	b.mu.Unlock()

	go b.execute(ctx, run, sess, input, instructions)
	return run, nil
}

func trimHistory(h []Message) []Message {
	if len(h) > maxHistory {
		h = h[len(h)-maxHistory:]
	}
	return append([]Message(nil), h...)
}

func (b *Bridge) execute(ctx context.Context, run *Run, sess *Session, input, instructions string) {
	defer run.cancel()
	defer func() {
		sess.mu.Lock()
		sess.busy = false
		sess.mu.Unlock()
		// Keep finished runs briefly so a late SSE reader can still replay them.
		time.AfterFunc(10*time.Minute, func() {
			b.mu.Lock()
			delete(b.runs, run.ID)
			b.mu.Unlock()
		})
	}()

	fail := func(err error) {
		slog.Error("run failed", "run", run.ID, "err", err)
		run.emit("run.failed", map[string]any{"error": err.Error()})
	}

	proc, err := b.ensureAgent(ctx)
	if err != nil {
		fail(err)
		return
	}

	sess.mu.Lock()
	needNew := sess.acpID == "" || sess.gen != proc.gen
	replay := append([]Message(nil), sess.history...)
	sess.mu.Unlock()

	if needNew {
		resp, err := proc.conn.NewSession(ctx, acp.NewSessionRequest{
			Cwd:        b.cfg.Cwd,
			McpServers: []acp.McpServer{},
		})
		if err != nil {
			fail(fmt.Errorf("new session: %w", err))
			return
		}
		sess.mu.Lock()
		sess.acpID, sess.gen = resp.SessionId, proc.gen
		sess.mu.Unlock()
	}
	sess.mu.Lock()
	acpID := sess.acpID
	sess.mu.Unlock()

	b.mu.Lock()
	b.byACP[acpID] = run
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.byACP[acpID] == run {
			delete(b.byACP, acpID)
		}
		b.mu.Unlock()
	}()

	text := input
	if needNew {
		// ACP has no system-prompt field and a new ACP session has no memory,
		// so fold instructions and prior turns into the first prompt.
		text = joinNonEmpty("\n\n", instructions, transcript(replay), input)
	}
	resp, err := proc.conn.Prompt(ctx, acp.PromptRequest{
		SessionId: acpID,
		Prompt:    []acp.ContentBlock{acp.TextBlock(text)},
	})
	switch {
	case run.wasStopped() || (err == nil && resp.StopReason == acp.StopReasonCancelled):
		run.emit("run.cancelled", nil)
	case err != nil:
		fail(err)
	default:
		out := run.text()
		sess.mu.Lock()
		sess.history = trimHistory(append(sess.history,
			Message{Role: "user", Content: input},
			Message{Role: "assistant", Content: out}))
		sess.mu.Unlock()
		run.emit("run.completed", map[string]any{"output": out, "stop_reason": string(resp.StopReason)})
	}
}

func transcript(h []Message) string {
	if len(h) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Previous conversation:\n")
	for _, m := range h {
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
	}
	return sb.String()
}

// stopRun cancels the run's current ACP prompt turn.
func (b *Bridge) stopRun(run *Run) {
	run.markStopped()
	sess := b.getSession(run.SessionID)
	if sess == nil {
		return
	}
	sess.mu.Lock()
	acpID := sess.acpID
	sess.mu.Unlock()
	b.mu.Lock()
	p := b.proc
	b.mu.Unlock()
	if p != nil && acpID != "" && !run.isFinished() {
		_ = p.conn.Cancel(context.Background(), acp.CancelNotification{SessionId: acpID})
	}
}
