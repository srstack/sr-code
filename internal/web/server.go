// Package web serves the usher HTTP API and embedded static UI.
//
// Two listeners run side by side:
//
//   - the **web** listener (TCP at s.addr) serves the SPA, JSON API, SSE
//     stream, and /login. Every non-exempt request is gated by auth
//     middleware when a password is configured.
//   - the **hook** listener (Unix socket at s.hookSockPath, mode 0600)
//     serves only /hook/{event}. Hook traffic from the local `usher hook`
//     subprocess never touches the public TCP port, so it doesn't need
//     auth and a leaky-netns container can't reach it (fs-isolated).
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"usher/internal/agent/usheragent"
	"usher/internal/auth"
	"usher/internal/core"
	"usher/internal/hook"
	"usher/internal/jsonl"
	"usher/internal/mainchat"
	"usher/internal/router"
)

//go:embed static
var staticFS embed.FS

//go:embed login.tmpl.html
var loginTemplateRaw string

var loginTmpl = template.Must(template.New("login").Parse(loginTemplateRaw))

func init() {
	// Go's default MIME table has no .webmanifest entry; without this the
	// FileServer would serve the PWA manifest as application/octet-stream and
	// browsers would reject it.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// Verify Router satisfies AgentAPI at compile time.
var _ usheragent.AgentAPI = (*router.Router)(nil)

type Server struct {
	addr         string
	hookSockPath string
	auth         *auth.Store
	router       *router.Router
	main         *mainchat.Store
	agent        usheragent.Agent
	logger       *slog.Logger
}

func NewServer(
	addr string,
	hookSockPath string,
	authStore *auth.Store,
	r *router.Router,
	main *mainchat.Store,
	agent usheragent.Agent,
	logger *slog.Logger,
) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		addr:         addr,
		hookSockPath: hookSockPath,
		auth:         authStore,
		router:       r,
		main:         main,
		agent:        agent,
		logger:       logger,
	}
}

func (s *Server) Run(ctx context.Context) error {
	webMux := http.NewServeMux()

	webMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	webMux.HandleFunc("GET /login", s.handleLogin)
	webMux.HandleFunc("POST /login", s.handleLogin)
	webMux.HandleFunc("GET /logout", s.handleLogout)
	webMux.HandleFunc("POST /logout", s.handleLogout)

	webMux.HandleFunc("GET /api/sessions", s.handleListSessions)
	webMux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	webMux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	webMux.HandleFunc("GET /api/sessions/{id}/transcript", s.handleTranscript)
	webMux.HandleFunc("POST /api/sessions/{id}/send", s.handleSend)
	webMux.HandleFunc("DELETE /api/sessions/{id}/send", s.handleCancelSend)
	webMux.HandleFunc("GET /api/sessions/{id}/events", s.handleEvents)
	webMux.HandleFunc("POST /api/sessions/{id}/auto-approve", s.handleAutoApprove)
	webMux.HandleFunc("POST /api/sessions/{id}/archive", s.handleArchive)
	webMux.HandleFunc("DELETE /api/sessions/{id}/archive", s.handleUnarchive)

	webMux.HandleFunc("GET /api/mainchats", s.handleListMainChats)
	webMux.HandleFunc("GET /api/mainchats/{id}", s.handleGetMainChat)
	webMux.HandleFunc("GET /api/mainchats/{id}/messages", s.handleListMainChatMessages)
	webMux.HandleFunc("POST /api/mainchats/{id}/send", s.handleMainChatSend)

	webMux.HandleFunc("GET /api/interactions", s.handleListInteractions)
	webMux.HandleFunc("POST /api/interactions/{id}/respond", s.handleRespondInteraction)

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}
	webMux.Handle("GET /", http.FileServer(http.FS(sub)))

	hookMux := http.NewServeMux()
	hookMux.HandleFunc("POST /hook/{event}", s.handleHook)

	webSrv := &http.Server{
		Addr:              s.addr,
		Handler:           s.authMiddleware(webMux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	hookSrv := &http.Server{
		Handler:           hookMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	sockListener, err := listenUnixSocket(s.hookSockPath)
	if err != nil {
		return fmt.Errorf("hook socket: %w", err)
	}

	errCh := make(chan error, 2)
	go func() {
		s.logger.Info("usher web listening", "addr", s.addr)
		errCh <- webSrv.ListenAndServe()
	}()
	go func() {
		s.logger.Info("usher hook listening", "socket", s.hookSockPath)
		errCh <- hookSrv.Serve(sockListener)
	}()

	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = webSrv.Shutdown(shutdownCtx)
		_ = hookSrv.Shutdown(shutdownCtx)
		_ = os.Remove(s.hookSockPath)
	}

	select {
	case <-ctx.Done():
		shutdown()
		return nil
	case err := <-errCh:
		shutdown()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// listenUnixSocket binds a Unix domain socket at path with mode 0600. A
// stale socket file from a previous unclean shutdown is removed first.
func listenUnixSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Remove stale socket from a prior unclean shutdown. We intentionally
	// don't check whether an instance is currently bound — net.Listen will
	// fail loudly below if another process holds the address.
	if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return ln, nil
}

// --- auth middleware + handlers -----------------------------------------

// authMiddleware gates every web route on a valid cookie when a password
// is configured. When auth is not configured (loopback-only test mode) it
// is a no-op pass-through. /healthz, /login, and /logout are always
// exempt.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil || !s.auth.IsConfigured() {
			next.ServeHTTP(w, r)
			return
		}
		if isAuthExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(auth.CookieName)
		if err == nil && s.auth.VerifyCookie(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		// API/XHR clients get 401; full-page navigations get a redirect to
		// /login so the user lands on the form instead of seeing JSON.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		target := "/login"
		if r.URL.Path != "/" && r.URL.Path != "" {
			target = "/login?next=" + safeNextEscape(r.URL.RequestURI())
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	})
}

func isAuthExempt(p string) bool {
	switch p {
	case "/healthz", "/login", "/logout",
		// PWA install assets must be reachable without auth. Browsers fetch
		// the manifest, service worker, and icons to evaluate installability
		// (sometimes before or independent of the page's auth state); a 303
		// to /login here gets followed and cached by the SW as if it were
		// the manifest, permanently breaking install. None of these files
		// contain user data.
		"/manifest.webmanifest", "/sw.js":
		return true
	}
	return strings.HasPrefix(p, "/icons/")
}

// safeNextEscape URL-encodes a path for use in ?next=. We don't accept
// scheme/authority; callers should already have a relative path.
func safeNextEscape(p string) string {
	// http.Redirect and the browser will handle the rest; just keep the
	// payload safe to embed in a query string.
	r := strings.NewReplacer("&", "%26", "?", "%3F", "#", "%23", " ", "%20")
	return r.Replace(p)
}

// validateNext rejects open redirects: anything other than a same-origin
// absolute path collapses to "/".
func validateNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") {
		return "/"
	}
	if strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || !s.auth.IsConfigured() {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		s.renderLogin(w, r, "", http.StatusOK, r.URL.Query().Get("next"))
		return
	}
	// POST
	ip := auth.ClientIP(r)
	if delay, ok := s.auth.Limiter.Acquire(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(delay.Seconds())+1))
		s.renderLogin(w, r,
			fmt.Sprintf("too many attempts; try again in %ds", int(delay.Seconds())+1),
			http.StatusTooManyRequests,
			r.FormValue("next"))
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, "invalid form submission", http.StatusBadRequest, "")
		return
	}
	pw := r.PostForm.Get("password")
	next := r.PostForm.Get("next")
	if !s.auth.Verify(pw) {
		s.auth.Limiter.OnFailure(ip)
		s.logger.Info("login failed", "ip", ip)
		s.renderLogin(w, r, "invalid password", http.StatusUnauthorized, next)
		return
	}
	s.auth.Limiter.OnSuccess(ip)
	cookieVal, err := s.auth.IssueCookie()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.SetCookie(w, s.auth.NewSessionCookie(cookieVal))
	http.Redirect(w, r, validateNext(next), http.StatusSeeOther)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string, status int, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = loginTmpl.Execute(w, struct {
		Error string
		Next  string
	}{
		Error: errMsg,
		Next:  validateNext(next),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		http.SetCookie(w, s.auth.ClearCookie())
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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

// sessionDTO wraps core.Session with web-only fields (auto-approve flag
// and archive visibility). We don't put these on core.Session because
// the state is process-local and shouldn't leak into the discovery model.
type sessionDTO struct {
	core.Session
	AutoApprove bool `json:"auto_approve"`
	Archived    bool `json:"archived"`
}

// handleListSessions returns sessions visible by default, or the full
// set when ?include_archived=1 is passed (used by the sidebar's per-cwd
// "show archived" disclosure).
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	includeArchived := r.URL.Query().Get("include_archived") == "1"
	sessions := s.router.ListSessions()
	out := make([]sessionDTO, 0, len(sessions))
	for _, sess := range sessions {
		archived := s.router.IsArchived(sess.ID)
		if archived && !includeArchived {
			continue
		}
		out = append(out, sessionDTO{
			Session:     sess,
			AutoApprove: s.router.IsAutoApprove(sess.ID),
			Archived:    archived,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type createSessionRequest struct {
	Cwd            string `json:"cwd"`
	InitialMessage string `json:"initial_message"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	id, err := s.router.StartSession(req.Cwd, req.InitialMessage)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.router.GetSession(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, sessionDTO{
		Session:     sess,
		AutoApprove: s.router.IsAutoApprove(id),
		Archived:    s.router.IsArchived(id),
	})
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.router.GetSession(id); !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	s.router.Archive(id)
	writeJSON(w, http.StatusOK, map[string]bool{"archived": true})
}

func (s *Server) handleUnarchive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.router.GetSession(id); !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	s.router.Unarchive(id)
	writeJSON(w, http.StatusOK, map[string]bool{"archived": false})
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
