// Package web serves the usher HTTP API and embedded static UI.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"usher/internal/agent/usheragent"
	"usher/internal/core"
	"usher/internal/hook"
	"usher/internal/jsonl"
	"usher/internal/mainchat"
	"usher/internal/router"
)

//go:embed static
var staticFS embed.FS

// Verify Router satisfies AgentAPI at compile time.
var _ usheragent.AgentAPI = (*router.Router)(nil)

type Server struct {
	addr   string
	router *router.Router
	main   *mainchat.Store
	agent  usheragent.Agent
	logger *slog.Logger
}

func NewServer(
	addr string,
	r *router.Router,
	main *mainchat.Store,
	agent usheragent.Agent,
	logger *slog.Logger,
) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{addr: addr, router: r, main: main, agent: agent, logger: logger}
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("GET /api/sessions/{id}/transcript", s.handleTranscript)
	mux.HandleFunc("POST /api/sessions/{id}/send", s.handleSend)
	mux.HandleFunc("DELETE /api/sessions/{id}/send", s.handleCancelSend)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handleEvents)
	mux.HandleFunc("POST /api/sessions/{id}/auto-approve", s.handleAutoApprove)

	mux.HandleFunc("GET /api/mainchats", s.handleListMainChats)
	mux.HandleFunc("GET /api/mainchats/{id}", s.handleGetMainChat)
	mux.HandleFunc("GET /api/mainchats/{id}/messages", s.handleListMainChatMessages)
	mux.HandleFunc("POST /api/mainchats/{id}/send", s.handleMainChatSend)

	mux.HandleFunc("POST /hook/{event}", s.handleHook)
	mux.HandleFunc("GET /api/interactions", s.handleListInteractions)
	mux.HandleFunc("POST /api/interactions/{id}/respond", s.handleRespondInteraction)

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("usher listening", "addr", s.addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// --- sessions ------------------------------------------------------------

// sessionDTO wraps core.Session with web-only fields (currently just the
// auto-approve flag). We don't put auto_approve on core.Session because
// hook state is process-local and shouldn't leak into the discovery model.
type sessionDTO struct {
	core.Session
	AutoApprove bool `json:"auto_approve"`
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions := s.router.ListSessions()
	out := make([]sessionDTO, len(sessions))
	for i, sess := range sessions {
		out[i] = sessionDTO{Session: sess, AutoApprove: s.router.IsAutoApprove(sess.ID)}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.router.GetSession(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, sessionDTO{Session: sess, AutoApprove: s.router.IsAutoApprove(id)})
}

type autoApproveRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleAutoApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.router.GetSession(id); !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	var req autoApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	s.router.SetAutoApprove(id, req.Enabled)
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": req.Enabled})
}

type sendRequest struct {
	Text string `json:"text"`
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	if err := s.router.SendToSession(id, req.Text); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *Server) handleCancelSend(w http.ResponseWriter, r *http.Request) {
	if err := s.router.CancelSend(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path, ok := s.router.SessionPath(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	turns, err := jsonl.ReadTurns(path, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if turns == nil {
		turns = []jsonl.Turn{}
	}
	writeJSON(w, http.StatusOK, turns)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.router.GetSession(id); !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := s.router.SubscribeSession(id)
	defer cancel()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload := ev.Raw
			if len(payload) == 0 {
				payload = []byte("{}")
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// --- main chat -----------------------------------------------------------

func (s *Server) handleListMainChats(w http.ResponseWriter, r *http.Request) {
	chats, err := s.main.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if chats == nil {
		chats = []mainchat.Chat{}
	}
	writeJSON(w, http.StatusOK, chats)
}

func (s *Server) handleListMainChatMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	msgs, err := s.main.Read(id, limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if msgs == nil {
		msgs = []mainchat.Message{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

type mainChatSendResponse struct {
	Messages []mainchat.Message `json:"messages"`
	Focus    *focusDetail       `json:"focus,omitempty"`
}

type focusDetail struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd,omitempty"`
	Title     string `json:"title,omitempty"`
}

type mainChatInfo struct {
	ID    string       `json:"id"`
	Focus *focusDetail `json:"focus,omitempty"`
}

const (
	mainChatHistoryCap = 40 // 20 turns of user+agent
	stateBlockMaxRows  = 30 // session rows in the rendered <current_state> preamble
)

// renderStateBlock produces a compact ground-truth dump of the router's
// current view of sessions + the active focus. We append it to the user's
// message every turn so trivia questions ("how many sessions?", "what's
// the focused cwd?", "is X running?") can be answered straight from
// context instead of hallucinated. Patterned after Hermes-Agent's
// per-turn state injection — kept off the system prompt so cache hits
// still happen on the static prefix.
func (s *Server) renderStateBlock(focusID string) string {
	sessions := s.router.ListSessions()
	pending := s.router.ListPendingInteractions()

	var b strings.Builder
	b.WriteString("<current_state>\n")
	fmt.Fprintf(&b, "session_count: %d\n", len(sessions))
	fmt.Fprintf(&b, "pending_permission_requests: %d\n", len(pending))
	if focusID != "" {
		if sess, ok := s.router.GetSession(focusID); ok {
			fmt.Fprintf(&b, "focus: %s (cwd %s, title %q)\n",
				focusID, sess.Cwd, truncateRunes(sess.Title, 60))
		} else {
			fmt.Fprintf(&b, "focus: %s (no longer in discovery)\n", focusID)
		}
	} else {
		b.WriteString("focus: (none yet)\n")
	}
	b.WriteString("sessions:\n")
	rows := sessions
	if len(rows) > stateBlockMaxRows {
		rows = rows[:stateBlockMaxRows]
	}
	for _, sess := range rows {
		mark := ""
		if sess.ID == focusID {
			mark = "  [FOCUS]"
		}
		fmt.Fprintf(&b, "  %s  %-30s  %-7s  %s%s\n",
			sess.ID,
			truncateRunes(sess.Cwd, 30),
			string(sess.Status),
			truncateRunes(sess.Title, 50),
			mark)
	}
	if len(sessions) > stateBlockMaxRows {
		fmt.Fprintf(&b, "  … %d more (truncated)\n", len(sessions)-stateBlockMaxRows)
	}
	b.WriteString("</current_state>")
	return b.String()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (s *Server) handleGetMainChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgs, err := s.main.Read(id, 0)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	info := mainChatInfo{ID: id, Focus: s.lastFocus(msgs)}
	writeJSON(w, http.StatusOK, info)
}

// lastFocus walks msgs newest-first and returns the most recent non-empty
// FocusSession decorated with current session metadata (cwd, title) if
// the session is still discoverable.
func (s *Server) lastFocus(msgs []mainchat.Message) *focusDetail {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].FocusSession == "" {
			continue
		}
		fd := &focusDetail{SessionID: msgs[i].FocusSession}
		if sess, ok := s.router.GetSession(msgs[i].FocusSession); ok {
			fd.Cwd = sess.Cwd
			fd.Title = sess.Title
		}
		return fd
	}
	return nil
}

func (s *Server) handleMainChatSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}

	// Read history (and prior focus) BEFORE persisting the new user message.
	prior, err := s.main.Read(id, mainChatHistoryCap)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	history := make([]usheragent.HistoryMessage, 0, len(prior))
	for _, m := range prior {
		history = append(history, usheragent.HistoryMessage{Role: m.Role, Content: m.Content})
	}
	prevFocus := ""
	if fd := s.lastFocus(prior); fd != nil {
		prevFocus = fd.SessionID
	}

	userMsg := mainchat.Message{Role: "user", Content: req.Text, Time: time.Now().UTC()}
	if err := s.main.Append(id, userMsg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Append a compact ground-truth block to the user message. The agent
	// (especially small models like Haiku / Flash / mini) uses it to answer
	// metadata trivia without hallucinating, and to verify focus before
	// claiming a switch.
	enrichedUserMsg := req.Text + "\n\n" + s.renderStateBlock(prevFocus)

	res, err := s.agent.Handle(r.Context(), history, prevFocus, enrichedUserMsg)
	if err != nil {
		s.logger.Warn("agent handle", "err", err)
		res = usheragent.AgentResult{Reply: "agent error: " + err.Error()}
	}
	// Carry forward focus when this turn didn't touch any session.
	newFocus := res.FocusSession
	if newFocus == "" {
		newFocus = prevFocus
	}
	agentMsg := mainchat.Message{
		Role:         "agent",
		Content:      res.Reply,
		Time:         time.Now().UTC(),
		FocusSession: newFocus,
	}
	if err := s.main.Append(id, agentMsg); err != nil {
		s.logger.Warn("main chat append agent", "err", err)
	}

	resp := mainChatSendResponse{Messages: []mainchat.Message{userMsg, agentMsg}}
	if newFocus != "" {
		fd := &focusDetail{SessionID: newFocus}
		if sess, ok := s.router.GetSession(newFocus); ok {
			fd.Cwd = sess.Cwd
			fd.Title = sess.Title
		}
		resp.Focus = fd
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- hooks ---------------------------------------------------------------

type hookPayload struct {
	SessionID      string          `json:"session_id"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	Cwd            string          `json:"cwd"`
	TranscriptPath string          `json:"transcript_path"`
}

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	eventName := r.PathValue("event")

	body, _ := io.ReadAll(r.Body)
	var ev hookPayload
	if len(body) > 0 {
		if err := json.Unmarshal(body, &ev); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid hook payload: "+err.Error())
			return
		}
	}
	if ev.HookEventName == "" {
		ev.HookEventName = eventName
	}

	if eventName != "PreToolUse" {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	resp, err := s.router.HandleHook(r.Context(), hook.Event{
		SessionID: ev.SessionID,
		Event:     eventName,
		ToolName:  ev.ToolName,
		ToolInput: ev.ToolInput,
		Cwd:       ev.Cwd,
	})
	if err != nil {
		s.logger.Warn("hook submit cancelled", "session", ev.SessionID, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	decision := resp.Behavior
	if decision == "" {
		decision = "allow"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            eventName,
			"permissionDecision":       decision,
			"permissionDecisionReason": resp.Reason,
		},
	})
}

func (s *Server) handleListInteractions(w http.ResponseWriter, r *http.Request) {
	list := s.router.ListPendingInteractions()
	if list == nil {
		list = []hook.Pending{}
	}
	writeJSON(w, http.StatusOK, list)
}

type respondReq struct {
	Behavior string `json:"behavior"`
	Reason   string `json:"reason"`
	Scope    string `json:"scope"` // "" or "once" or "session"
}

func (s *Server) handleRespondInteraction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req respondReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Behavior != "allow" && req.Behavior != "deny" {
		writeErr(w, http.StatusBadRequest, "behavior must be allow|deny")
		return
	}
	if req.Scope != "" && req.Scope != "once" && req.Scope != "session" {
		writeErr(w, http.StatusBadRequest, "scope must be once|session")
		return
	}
	if err := s.router.RespondInteraction(id, hook.Response{
		Behavior: req.Behavior,
		Reason:   req.Reason,
		Scope:    req.Scope,
	}); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
