package bridge

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var remoteID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,511}$`)

const keepaliveEvery = 10 * time.Second

// Handler returns the gateway HTTP handler.
func (b *Bridge) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/capabilities", b.handleCapabilities)
	mux.HandleFunc("POST /v1/runs", b.handleCreateRun)
	mux.HandleFunc("GET /v1/runs/{id}/events", b.handleEvents)
	mux.HandleFunc("POST /v1/runs/{id}/approval", b.handleApproval)
	mux.HandleFunc("POST /v1/runs/{id}/stop", b.handleStop)
	mux.HandleFunc("GET /api/sessions/{id}/messages", b.handleMessages)
	return b.auth(mux)
}

func (b *Bridge) auth(next http.Handler) http.Handler {
	want := []byte(b.cfg.APIKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(want) == 0 || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (b *Bridge) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	model := b.cfg.ModelLabel
	if model == "" {
		model = "acp-agent"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "hermes.api_server.capabilities",
		"model":  model,
		"features": map[string]bool{
			"run_submission":        true,
			"run_events_sse":        true,
			"run_stop":              true,
			"run_approval_response": true,
			"session_resources":     true,
		},
	})
}

type createRunRequest struct {
	Input               string    `json:"input"`
	Instructions        string    `json:"instructions"`
	SessionID           string    `json:"session_id"`
	ConversationHistory []Message `json:"conversation_history"`
}

func (b *Bridge) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var req createRunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "input_required"})
		return
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = r.Header.Get("X-Hermes-Session-Id")
	}
	if sessionID != "" && !remoteID.MatchString(sessionID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_session_id"})
		return
	}
	run, err := b.startRun(sessionID, req.Input, req.Instructions, req.ConversationHistory)
	if errors.Is(err, errBusy) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "session_busy"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run_id": run.ID, "session_id": run.SessionID, "status": "started",
	})
}

func (b *Bridge) handleEvents(w http.ResponseWriter, r *http.Request) {
	run := b.getRun(r.PathValue("id"))
	if run == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "run_not_found"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "no_streaming"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(keepaliveEvery)
	defer ticker.Stop()
	for idx := 0; ; {
		ev, st := run.next(r.Context(), idx, ticker.C)
		switch st {
		case nextDone:
			if r.Context().Err() == nil {
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
			}
			return
		case nextTick:
			fmt.Fprint(w, ": keepalive\n\n")
		case nextEvent:
			payload, _ := json.Marshal(ev.Data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, payload)
			idx++
		}
		flusher.Flush()
	}
}

func (b *Bridge) handleApproval(w http.ResponseWriter, r *http.Request) {
	run := b.getRun(r.PathValue("id"))
	if run == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "run_not_found"})
		return
	}
	var body struct {
		Choice string `json:"choice"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil ||
		(body.Choice != choiceOnce && body.Choice != choiceDeny) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_choice"})
		return
	}
	if !run.answerApproval(body.Choice) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "no_pending_approval"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "choice": body.Choice})
}

func (b *Bridge) handleStop(w http.ResponseWriter, r *http.Request) {
	run := b.getRun(r.PathValue("id"))
	if run == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "run_not_found"})
		return
	}
	b.stopRun(run)
	writeJSON(w, http.StatusOK, map[string]any{"status": "stopping"})
}

func (b *Bridge) handleMessages(w http.ResponseWriter, r *http.Request) {
	sess := b.getSession(r.PathValue("id"))
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session_not_found"})
		return
	}
	sess.mu.Lock()
	rows := append([]Message{}, sess.history...)
	sess.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": rows})
}
