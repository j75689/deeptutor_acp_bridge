// Command fakeagent is a tiny scripted ACP agent used by the bridge tests.
//
// Prompt keywords: "slow" blocks until cancelled, "approve" asks for a tool
// permission, "tool" runs a tool call, anything else is echoed back.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

type agent struct {
	conn    *acp.AgentSideConnection
	mu      sync.Mutex
	n       int
	cancels map[string]context.CancelFunc
}

var _ acp.Agent = (*agent)(nil)

func (a *agent) SetAgentConnection(c *acp.AgentSideConnection) { a.conn = c }

func (a *agent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo:       &acp.Implementation{Name: "fakeagent", Version: "0"},
	}, nil
}

func (a *agent) NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	return acp.NewSessionResponse{SessionId: acp.SessionId(fmt.Sprintf("fake-%d", a.n))}, nil
}

func (a *agent) Cancel(_ context.Context, p acp.CancelNotification) error {
	a.mu.Lock()
	cancel := a.cancels[string(p.SessionId)]
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (a *agent) say(ctx context.Context, sid acp.SessionId, u acp.SessionUpdate) error {
	return a.conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: u})
}

func (a *agent) Prompt(_ context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.mu.Lock()
	if a.cancels == nil {
		a.cancels = map[string]context.CancelFunc{}
	}
	a.cancels[string(p.SessionId)] = cancel
	a.mu.Unlock()

	text := ""
	for _, b := range p.Prompt {
		if b.Text != nil {
			text += b.Text.Text
		}
	}
	switch {
	case strings.Contains(text, "slow"):
		<-ctx.Done()
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	case strings.Contains(text, "approve"):
		resp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			SessionId: p.SessionId,
			ToolCall:  acp.ToolCallUpdate{ToolCallId: "perm1", Title: acp.Ptr("rm -rf /tmp/x")},
			Options: []acp.PermissionOption{
				{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionId: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		verdict := "denied"
		if resp.Outcome.Selected != nil && resp.Outcome.Selected.OptionId == "allow" {
			verdict = "allowed"
		}
		_ = a.say(ctx, p.SessionId, acp.UpdateAgentMessageText("permission "+verdict))
	case strings.Contains(text, "tool"):
		_ = a.say(ctx, p.SessionId, acp.UpdateAgentThoughtText("thinking"))
		_ = a.say(ctx, p.SessionId, acp.StartToolCall("t1", "read file",
			acp.WithStartStatus(acp.ToolCallStatusPending),
			acp.WithStartRawInput(map[string]any{"path": "a.txt"})))
		_ = a.say(ctx, p.SessionId, acp.UpdateToolCall("t1", acp.WithUpdateStatus(acp.ToolCallStatusCompleted)))
		_ = a.say(ctx, p.SessionId, acp.UpdateAgentMessageText("done"))
	default:
		_ = a.say(ctx, p.SessionId, acp.UpdateAgentMessageText("echo: "))
		_ = a.say(ctx, p.SessionId, acp.UpdateAgentMessageText(text))
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *agent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (a *agent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}

func (a *agent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionClose)
}

func (a *agent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
}

func (a *agent) LoadSession(context.Context, acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	return acp.LoadSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionLoad)
}

func (a *agent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
}

func (a *agent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}

func (a *agent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

func main() {
	a := &agent{}
	conn := acp.NewAgentSideConnection(a, os.Stdout, os.Stdin)
	a.conn = conn
	<-conn.Done()
}
