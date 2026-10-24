package bridge

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Event is one Hermes-gateway SSE event. Name becomes the SSE "event:" line and
// is also mirrored into the JSON payload as "event".
type Event struct {
	Name string
	Data map[string]any
}

const (
	choiceOnce = "once"
	choiceDeny = "deny"
)

// Run is one prompt turn. It keeps an append-only event log so an SSE consumer
// that connects after POST /v1/runs still replays everything from the start.
type Run struct {
	ID        string
	SessionID string

	mu       sync.Mutex
	events   []Event
	finished bool
	wake     chan struct{} // closed and replaced on every append

	output  strings.Builder
	tools   map[string]string // toolCallId -> title
	cancel  context.CancelFunc
	stopped bool
	stopCh  chan struct{} // closed by the first markStopped

	// pendingApproval is non-nil while a permission request waits for the
	// DeepTutor side to answer through POST /v1/runs/{id}/approval.
	pendingApproval chan string
}

func newRun(id, sessionID string, cancel context.CancelFunc) *Run {
	return &Run{
		ID:        id,
		SessionID: sessionID,
		wake:      make(chan struct{}),
		tools:     map[string]string{},
		cancel:    cancel,
		stopCh:    make(chan struct{}),
	}
}

func (r *Run) emit(name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["event"] = name
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	r.events = append(r.events, Event{Name: name, Data: data})
	switch name {
	case "run.completed", "run.failed", "run.cancelled":
		r.finished = true
	}
	close(r.wake)
	r.wake = make(chan struct{})
}

type nextStatus int

const (
	nextEvent nextStatus = iota
	nextDone
	nextTick
)

// next returns the event at idx, blocking until one exists. It returns
// nextDone once the run is finished and idx is past the end (or ctx is done),
// and nextTick when tick fires first so the caller can send a keepalive.
func (r *Run) next(ctx context.Context, idx int, tick <-chan time.Time) (Event, nextStatus) {
	for {
		r.mu.Lock()
		if idx < len(r.events) {
			ev := r.events[idx]
			r.mu.Unlock()
			return ev, nextEvent
		}
		if r.finished {
			r.mu.Unlock()
			return Event{}, nextDone
		}
		wake := r.wake
		r.mu.Unlock()
		select {
		case <-wake:
		case <-tick:
			return Event{}, nextTick
		case <-ctx.Done():
			return Event{}, nextDone
		}
	}
}

func (r *Run) appendText(s string) {
	r.mu.Lock()
	r.output.WriteString(s)
	r.mu.Unlock()
}

func (r *Run) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(r.output.String())
}

func (r *Run) setTool(id, title string) {
	r.mu.Lock()
	r.tools[id] = title
	r.mu.Unlock()
}

func (r *Run) toolTitle(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.tools[id]; t != "" {
		return t
	}
	return "tool"
}

func (r *Run) markStopped() {
	r.mu.Lock()
	if !r.stopped {
		close(r.stopCh)
	}
	r.stopped = true
	ch := r.pendingApproval
	r.mu.Unlock()
	if ch != nil {
		select {
		case ch <- choiceDeny:
		default:
		}
	}
}

func (r *Run) wasStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func (r *Run) isFinished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finished
}

// beginApproval registers a pending approval and returns the channel the
// answer arrives on.
func (r *Run) beginApproval() chan string {
	ch := make(chan string, 1)
	r.mu.Lock()
	r.pendingApproval = ch
	stopped := r.stopped
	r.mu.Unlock()
	if stopped {
		ch <- choiceDeny
	}
	return ch
}

func (r *Run) endApproval() {
	r.mu.Lock()
	r.pendingApproval = nil
	r.mu.Unlock()
}

// answerApproval delivers choice to a waiting permission request. It reports
// false when nothing is waiting.
func (r *Run) answerApproval(choice string) bool {
	r.mu.Lock()
	ch := r.pendingApproval
	r.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- choice:
		return true
	default:
		return false
	}
}
