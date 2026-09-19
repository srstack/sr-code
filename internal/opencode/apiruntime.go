package opencode

// APIRuntime drives OpenCode 2 through the background service's HTTP API
// instead of `opencode2 run`. The headless run CLI auto-dismisses question
// forms and permission requests (no client attached to answer them); driving
// sessions as an API client keeps them pending so usher's interaction UI can
// answer (form reply / permission reply endpoints).
//
// Live display streams from the service's global SSE feed (/api/event),
// translated into the same Claude-shaped shadow lines as the run-based v1
// path; at turn end v2FetchSession rewrites the shadow authoritatively, which
// also heals any events lost to an SSE reconnect.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
)

type APIRuntime struct {
	cmd    string
	root   string
	logger *slog.Logger
	hooks  *hook.Manager
	// legacy is the run-based v2 Runtime, retained for its store helpers
	// (v2FetchSession's modelWindow) and the background SyncLoop. APIRuntime
	// never spawns `run` children itself.
	legacy *Runtime

	mu       sync.Mutex
	inflight map[string]*apiTurn

	hubMu sync.Mutex
	hub   *sseHub

	cwMu  sync.Mutex
	cwMap map[string]int64
	cwAt  time.Time
}

func NewAPIRuntime(cmd, root string, legacy *Runtime, hooks *hook.Manager, logger *slog.Logger) *APIRuntime {
	if logger == nil {
		logger = slog.Default()
	}
	return &APIRuntime{
		cmd:      cmd,
		root:     root,
		logger:   logger,
		hooks:    hooks,
		legacy:   legacy,
		inflight: map[string]*apiTurn{},
	}
}

// --- service requests (POST/PUT bodies; GET/DELETE live in api.go) ---------

// v2APIBody performs one service request with a JSON body.
func v2APIBody(ctx context.Context, cmd, method, path string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(string(raw))
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		svc, err := v2ReadService()
		if err == nil {
			req, rerr := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(svc.URL, "/")+path, body)
			if rerr == nil {
				req.SetBasicAuth("opencode", svc.Password)
				if payload != nil {
					req.Header.Set("Content-Type", "application/json")
				}
				if raw, herr := v2Do(req); herr == nil {
					return raw, nil
				} else {
					last = herr
				}
			} else {
				last = rerr
			}
		} else {
			last = err
		}
		// Service missing or unhealthy: (re)start it, then re-read the
		// registration (the port can change across restarts).
		startCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_ = exec.CommandContext(startCtx, cmd, "service", "start").Run()
		cancel()
	}
	return nil, last
}

// --- SSE hub ----------------------------------------------------------------

// sseEvent is one line of the global /api/event feed.
type sseEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// sseHub owns one /api/event connection and fans events out to per-session
// subscribers. Reconnects forever with backoff; the registration is re-read
// on every attempt because a service restart changes the port.
type sseHub struct {
	cmd    string
	logger *slog.Logger

	mu   sync.Mutex
	subs map[string][]chan sseEvent // sessionID -> subscribers
}

func newSSEHub(cmd string, logger *slog.Logger) *sseHub {
	h := &sseHub{cmd: cmd, logger: logger, subs: map[string][]chan sseEvent{}}
	go h.loop()
	return h
}

func (h *sseHub) subscribe(sessionID string) (<-chan sseEvent, func()) {
	ch := make(chan sseEvent, 256)
	h.mu.Lock()
	h.subs[sessionID] = append(h.subs[sessionID], ch)
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		subs := h.subs[sessionID]
		for i, c := range subs {
			if c == ch {
				h.subs[sessionID] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
	}
	return ch, cancel
}

func (h *sseHub) dispatch(ev sseEvent) {
	var d struct {
		SessionID string `json:"sessionID"`
	}
	if json.Unmarshal(ev.Data, &d) != nil || d.SessionID == "" {
		return
	}
	h.mu.Lock()
	subs := append([]chan sseEvent(nil), h.subs[d.SessionID]...)
	h.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // drop-on-full: a stalled turn never blocks the hub
		}
	}
}

func (h *sseHub) loop() {
	for {
		err := h.stream()
		if err != nil {
			h.logger.Debug("opencode2 event stream ended", "err", err)
		}
		time.Sleep(time.Second)
	}
}

func (h *sseHub) stream() error {
	svc, err := v2ReadService()
	if err != nil {
		startCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		_ = exec.CommandContext(startCtx, h.cmd, "service", "start").Run()
		cancel()
		return err
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(svc.URL, "/")+"/api/event", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth("opencode", svc.Password)
	resp, err := v2HTTPClient.Do(req) // no timeout: long-lived stream
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opencode2 event stream: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.HasPrefix(string(line), "data: ") {
			continue
		}
		var ev sseEvent
		if json.Unmarshal(line[len("data: "):], &ev) != nil {
			continue
		}
		h.dispatch(ev)
	}
	return sc.Err()
}

// ensureHub starts the shared SSE hub on first use.
func (r *APIRuntime) ensureHub() *sseHub {
	r.hubMu.Lock()
	defer r.hubMu.Unlock()
	if r.hub == nil {
		r.hub = newSSEHub(r.cmd, r.logger)
	}
	return r.hub
}

// --- turns ------------------------------------------------------------------

// apiTurn is one in-flight prompt execution driven through the API.
type apiTurn struct {
	rt         *APIRuntime
	id         string // session id
	cwd        string
	path       string // shadow transcript
	out        chan backend.Event
	cancel     context.CancelFunc
	startedMs  int64                      // prompt post time; the completion backstop compares time.idle
	toolIDs    map[string]string          // tool call id -> tool name (from tool.input.started)
	toolInputs map[string]json.RawMessage // tool call id -> input (from tool.called)
	usage      *tokenTotals
}

func (r *APIRuntime) Start(ctx context.Context, req backend.StartRequest) (string, <-chan backend.Event, error) {
	if strings.TrimSpace(req.Cwd) == "" {
		var err error
		req.Cwd, err = os.Getwd()
		if err != nil {
			return "", nil, err
		}
	}
	payload := map[string]any{
		"title":    truncateForTitle(req.Prompt),
		"location": map[string]any{"directory": req.Cwd},
	}
	if req.Model != "" && req.Model != "default" {
		payload["model"] = v2ModelPayload(req.Model)
	}
	raw, err := v2APIBody(ctx, r.cmd, http.MethodPost, "/api/session", payload)
	if err != nil {
		return "", nil, fmt.Errorf("opencode2 create session: %w", err)
	}
	var resp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil || resp.Data.ID == "" {
		return "", nil, fmt.Errorf("opencode2 create session: unexpected response: %.200s", raw)
	}
	id := resp.Data.ID
	ch, err := r.prompt(ctx, id, req.Prompt, req.Cwd, true)
	if err != nil {
		return "", nil, err
	}
	return id, ch, nil
}

func (r *APIRuntime) Send(ctx context.Context, id, prompt, cwd string) (<-chan backend.Event, error) {
	return r.SendWithModel(ctx, id, prompt, cwd, "")
}

func (r *APIRuntime) SendWithModel(ctx context.Context, id, prompt, cwd, model string) (<-chan backend.Event, error) {
	if model != "" && model != "default" {
		if cur := v2SessionModel(ctx, r.cmd, id); cur != model {
			if _, err := v2APIBody(ctx, r.cmd, http.MethodPost, "/api/session/"+id+"/model", map[string]any{"model": v2ModelPayload(model)}); err != nil {
				r.logger.Warn("opencode2 switch model failed; sending with current", "session", id, "err", err)
			}
		}
	}
	return r.prompt(ctx, id, prompt, cwd, false)
}

// prompt posts one user message and streams the turn's events. The returned
// channel closes when the execution finishes (or ctx is cancelled).
func (r *APIRuntime) prompt(ctx context.Context, id, prompt, cwd string, fresh bool) (<-chan backend.Event, error) {
	if cwd == "" {
		cwd = r.cwdFor(ctx, id)
	}
	childCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	if prev := r.inflight[id]; prev != nil {
		prev.cancel() // a duplicate send kills the older turn
	}
	t := &apiTurn{
		rt:         r,
		id:         id,
		cwd:        cwd,
		path:       logPath(r.root, cwd, id),
		out:        make(chan backend.Event, 64),
		cancel:     cancel,
		startedMs:  time.Now().UnixMilli(),
		toolIDs:    map[string]string{},
		toolInputs: map[string]json.RawMessage{},
	}
	r.inflight[id] = t
	r.mu.Unlock()

	events, unsubscribe := r.ensureHub().subscribe(id)
	if _, err := v2APIBody(childCtx, r.cmd, http.MethodPost, "/api/session/"+id+"/prompt", map[string]any{"text": prompt}); err != nil {
		unsubscribe()
		cancel()
		r.mu.Lock()
		delete(r.inflight, id)
		r.mu.Unlock()
		return nil, fmt.Errorf("opencode2 prompt: %w", err)
	}
	go t.run(childCtx, events, unsubscribe, prompt, fresh)
	return t.out, nil
}

func (r *APIRuntime) cwdFor(ctx context.Context, id string) string {
	var resp struct {
		Data struct {
			Location struct {
				Directory string `json:"directory"`
			} `json:"location"`
		} `json:"data"`
	}
	if apiGetJSON(ctx, r.cmd, "/api/session/"+id, &resp) == nil {
		return resp.Data.Location.Directory
	}
	return ""
}

// run consumes the turn's events until the execution settles, then refreshes
// the shadow from the API (authoritative; heals SSE gaps) and closes out.
func (t *apiTurn) run(ctx context.Context, events <-chan sseEvent, unsubscribe func(), prompt string, fresh bool) {
	defer close(t.out)
	defer unsubscribe()
	defer func() {
		t.rt.mu.Lock()
		delete(t.rt.inflight, t.id)
		t.rt.mu.Unlock()
	}()

	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		t.emitErr("opencode2 shadow log setup failed: " + err.Error())
		return
	}
	started, _ := json.Marshal(backend.ProcessStartedPayload{Cwd: t.cwd, Fresh: fresh})
	if !t.emit(ctx, backend.Event{Type: backend.EventProcessStarted, Raw: started}) {
		return
	}
	userRaw := userLine(t.id, t.cwd, prompt, time.Now().UTC())
	if !t.appendAndEmit(ctx, "user", userRaw) {
		return
	}

	// Surface question forms and permission requests to usher's UI while the
	// turn runs; replies go back through the service API.
	bridgeDone := make(chan struct{})
	go t.bridgeFormsAndPermissions(ctx, bridgeDone)
	defer close(bridgeDone)

	// SSE is the live source; a slow poll of the session's execution state is
	// the completion backstop for events lost to a hub reconnect.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			if !t.handleEvent(ctx, ev) {
				return
			}
			if ev.Type == "session.execution.succeeded" || ev.Type == "session.execution.failed" {
				t.finish(ctx)
				return
			}
		case <-ticker.C:
			if t.settled(ctx) {
				t.finish(ctx)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// settled polls the session for completion (backstop when the terminal
// execution event was lost to an SSE reconnect). `outcome` is NOT usable: it
// keeps the previous turn's result while a new turn runs. `time.idle` is
// re-stamped when the session goes idle, so a stamp newer than our prompt
// means this turn has finished.
func (t *apiTurn) settled(ctx context.Context) bool {
	var resp struct {
		Data struct {
			Time struct {
				Idle int64 `json:"idle"`
			} `json:"time"`
		} `json:"data"`
	}
	if apiGetJSON(ctx, t.rt.cmd, "/api/session/"+t.id, &resp) != nil {
		return false
	}
	return resp.Data.Time.Idle >= t.startedMs
}

// handleEvent translates one SSE event into shadow lines + display events.
// Returns false when the consumer is gone.
func (t *apiTurn) handleEvent(ctx context.Context, ev sseEvent) bool {
	var d struct {
		SessionID          string          `json:"sessionID"`
		AssistantMessageID string          `json:"assistantMessageID"`
		ID                 string          `json:"id"`   // tool call id
		Name               string          `json:"name"` // tool name (input.started)
		Ordinal            int             `json:"ordinal"`
		Text               string          `json:"text"`  // text/reasoning ended
		Input              json.RawMessage `json:"input"` // tool called
		Content            []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			URI  string `json:"uri"`
		} `json:"content"` // tool success
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Tokens *tokenTotals `json:"tokens"`
	}
	if json.Unmarshal(ev.Data, &d) != nil || d.SessionID != t.id {
		return true
	}
	ts := time.Now().UTC()
	switch ev.Type {
	case "session.tool.input.started":
		if d.ID != "" && d.Name != "" {
			t.toolIDs[d.ID] = d.Name
		}
	case "session.tool.called":
		// Live placeholder: tool card shows immediately while it runs.
		if d.ID != "" && len(d.Input) > 0 {
			t.toolInputs[d.ID] = append([]byte(nil), d.Input...)
		}
		pp := partPayload{ID: d.ID, Tool: t.toolIDs[d.ID], CallID: d.ID, State: &toolState{Status: "running", Input: d.Input}}
		if raw := assistantLine(t.id, toolUseBlocks(pp), ts); raw != nil {
			return t.emit(ctx, backend.Event{Type: "assistant", Raw: raw})
		}
	case "session.tool.success", "session.tool.error":
		st := &toolState{Status: "completed", Input: t.toolInputs[d.ID]}
		if ev.Type == "session.tool.error" {
			st.Status = "error"
		}
		var outs []string
		for _, c := range d.Content {
			if c.Type == "text" && c.Text != "" {
				outs = append(outs, c.Text)
			} else if c.Type == "file" && c.URI != "" {
				outs = append(outs, c.URI)
			}
		}
		st.Output = strings.Join(outs, "\n")
		if d.Error != nil {
			st.Error = d.Error.Message
		}
		pp := partPayload{ID: d.ID, Tool: t.toolIDs[d.ID], CallID: d.ID, State: st}
		if !t.appendAndEmit(ctx, "assistant", assistantLine(t.id, toolUseBlocks(pp), ts)) {
			return false
		}
		return t.appendAndEmit(ctx, "user", toolResultLine(t.id, t.cwd, pp, ts))
	case "session.text.ended":
		if strings.TrimSpace(d.Text) == "" {
			return true
		}
		return t.appendAndEmit(ctx, "assistant", assistantLine(t.id, textBlocks(d.Text), ts))
	case "session.reasoning.ended":
		if strings.TrimSpace(d.Text) == "" {
			return true
		}
		return t.appendAndEmit(ctx, "assistant", assistantLine(t.id, thinkingBlocks(d.Text), ts))
	case "session.usage.updated":
		if d.Tokens != nil {
			t.usage = d.Tokens
		}
	}
	return true
}

// finish writes the authoritative shadow from the API and emits the closing
// runtime/exit events.
func (t *apiTurn) finish(ctx context.Context) {
	// The service flushes its own store before outcome is observable, but a
	// just-finished turn can lag by a beat; one short grace settles it.
	select {
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
		return
	}
	fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entry := sessionEntry{ID: t.id, Directory: t.cwd}
	if err := v2FetchSession(fctx, t.rt.legacy, entry, t.path); err != nil {
		t.rt.logger.Warn("opencode2 shadow refresh failed", "session", t.id, "err", err)
	}
	model := v2SessionModel(fctx, t.rt.cmd, t.id)
	rt := core.SessionRuntime{Model: model, ContextWindow: t.rt.contextWindow(fctx, model)}
	if t.usage != nil {
		rt.ContextTokens = t.usage.contextTokens()
	}
	raw, _ := json.Marshal(rt)
	if !t.emit(ctx, backend.Event{Type: backend.EventRuntime, Raw: raw}) {
		return
	}
	// turnCompleteLine closes the assistant turn for readers that don't
	// re-fetch (the live assembler).
	if !t.appendAndEmit(ctx, "system", turnCompleteLine(t.id, time.Now().UTC(), "")) {
		return
	}
	exited, _ := json.Marshal(map[string]any{"code": 0})
	t.emit(ctx, backend.Event{Type: backend.EventProcessExit, Raw: exited})
}

// --- forms & permissions bridge ---------------------------------------------

// v2Form is one pending form request (GET /api/session/{id}/form).
type v2Form struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Metadata struct {
		Kind string `json:"kind"`
	} `json:"metadata"`
	Fields []v2FormField `json:"fields"`
}

type v2FormField struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Type        string `json:"type"` // string | multiselect | …
	Custom      bool   `json:"custom"`
	Options     []struct {
		Value       string `json:"value"`
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

// v2Permission is one pending permission request.
type v2Permission struct {
	ID        string   `json:"id"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
	Message   string   `json:"message"`
}

// bridgeFormsAndPermissions polls the session's pending forms and permission
// requests and brokers them through usher's interaction UI (hook.Manager)
// until ctx ends. Each request is answered exactly once via the reply API.
func (t *apiTurn) bridgeFormsAndPermissions(ctx context.Context, done <-chan struct{}) {
	if t.rt.hooks == nil {
		return
	}
	seen := map[string]bool{}
	poll := func() {
		forms := t.listForms(ctx)
		for _, f := range forms {
			key := "form:" + f.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			go t.answerForm(ctx, f)
		}
		perms := t.listPermissions(ctx)
		for _, p := range perms {
			key := "perm:" + p.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			go t.answerPermission(ctx, p)
		}
	}
	poll()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			poll()
		case <-ctx.Done():
			return
		case <-done:
			return
		}
	}
}

func (t *apiTurn) listForms(ctx context.Context) []v2Form {
	var resp struct {
		Data []v2Form `json:"data"`
	}
	if err := apiGetJSON(ctx, t.rt.cmd, "/api/session/"+t.id+"/form", &resp); err != nil {
		return nil
	}
	return resp.Data
}

func (t *apiTurn) listPermissions(ctx context.Context) []v2Permission {
	var resp struct {
		Data []v2Permission `json:"data"`
	}
	if err := apiGetJSON(ctx, t.rt.cmd, "/api/session/"+t.id+"/permission", &resp); err != nil {
		return nil
	}
	return resp.Data
}

// answerForm surfaces a form as an AskUserQuestion-shaped pending interaction
// (the web UI's existing choice picker renders it unchanged) and posts the
// reply back to the service.
func (t *apiTurn) answerForm(ctx context.Context, f v2Form) {
	questions := make([]map[string]any, 0, len(f.Fields))
	for _, fld := range f.Fields {
		q := map[string]any{
			"question":    fld.Title,
			"header":      f.Title,
			"multiSelect": fld.Type == "multiselect",
		}
		if fld.Description != "" && fld.Description != fld.Title {
			q["question"] = fld.Title + " — " + fld.Description
		}
		var opts []map[string]any
		for _, o := range fld.Options {
			opts = append(opts, map[string]any{"label": o.Label, "description": o.Description})
		}
		q["options"] = opts
		questions = append(questions, q)
	}
	input, _ := json.Marshal(map[string]any{"questions": questions})
	resp, err := t.rt.hooks.Submit(ctx, hook.Event{
		SessionID: t.id,
		ToolUseID: "form:" + f.ID,
		Event:     "PermissionRequest",
		ToolName:  "AskUserQuestion",
		ToolInput: input,
		Cwd:       t.cwd,
	})
	if err != nil {
		return
	}
	if resp.Behavior != "allow" {
		// "ignore" in the UI maps to dismissing the form server-side.
		_, _ = v2APIBody(context.Background(), t.rt.cmd, http.MethodPost,
			"/api/session/"+t.id+"/form/"+f.ID+"/cancel", nil)
		return
	}
	answer := map[string]any{}
	for _, fld := range f.Fields {
		label := resp.Answers[fld.Title]
		if label == "" {
			label = resp.Answers[fld.Title+" — "+fld.Description]
		}
		answer[fld.Key] = fld.valueFor(label)
	}
	if _, err := v2APIBody(context.Background(), t.rt.cmd, http.MethodPost,
		"/api/session/"+t.id+"/form/"+f.ID+"/reply", map[string]any{"answer": answer}); err != nil {
		t.rt.logger.Warn("opencode2 form reply failed", "session", t.id, "form", f.ID, "err", err)
	}
}

// valueFor maps the UI's answer text back to a form value: option labels map
// to their option values; anything else is a custom free-text answer.
// Multiselect answers arrive as a ", "-joined label list (the native
// AskUserQuestion format) and become a string array.
func (f v2FormField) valueFor(label string) any {
	lookup := func(s string) string {
		for _, o := range f.Options {
			if o.Label == s {
				return o.Value
			}
		}
		return s
	}
	if f.Type == "multiselect" {
		var out []string
		for _, part := range strings.Split(label, ", ") {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, lookup(s))
			}
		}
		return out
	}
	return lookup(label)
}

// answerPermission surfaces a permission request as a generic allow/deny
// interaction; the reply maps onto opencode2's once/always/reject vocabulary.
func (t *apiTurn) answerPermission(ctx context.Context, p v2Permission) {
	input, _ := json.Marshal(map[string]any{
		"action":    p.Action,
		"resources": p.Resources,
		"message":   p.Message,
	})
	resp, err := t.rt.hooks.Submit(ctx, hook.Event{
		SessionID:   t.id,
		ToolUseID:   "perm:" + p.ID,
		Event:       "PermissionRequest",
		ToolName:    p.Action,
		ToolInput:   input,
		Cwd:         t.cwd,
		AllowAlways: true,
	})
	if err != nil {
		return
	}
	reply := "reject"
	if resp.Behavior == "allow" {
		if resp.Scope == "session" {
			reply = "always"
		} else {
			reply = "once"
		}
	}
	if _, err := v2APIBody(context.Background(), t.rt.cmd, http.MethodPost,
		"/api/session/"+t.id+"/permission/"+p.ID+"/reply",
		map[string]any{"reply": reply, "message": resp.Reason}); err != nil {
		t.rt.logger.Warn("opencode2 permission reply failed", "session", t.id, "request", p.ID, "err", err)
	}
}

// --- plumbing ----------------------------------------------------------------

func (t *apiTurn) appendAndEmit(ctx context.Context, typ string, raw json.RawMessage) bool {
	if raw == nil {
		return true
	}
	if err := appendLogLine(t.path, raw); err != nil {
		t.emitErr("opencode2 shadow log write failed: " + err.Error())
		return false
	}
	return t.emit(ctx, backend.Event{Type: typ, Raw: raw})
}

func (t *apiTurn) emit(ctx context.Context, ev backend.Event) bool {
	select {
	case t.out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

func (t *apiTurn) emitErr(msg string) {
	raw, _ := json.Marshal(backend.ErrorPayload{Message: msg})
	select {
	case t.out <- backend.Event{Type: backend.EventError, Raw: raw}:
	default:
	}
}

func (r *APIRuntime) Has(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.inflight[sessionID]
	return ok
}

func (r *APIRuntime) LiveSessions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.inflight))
	for id := range r.inflight {
		out = append(out, id)
	}
	return out
}

func (r *APIRuntime) Interrupt(sessionID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := v2APIBody(ctx, r.cmd, http.MethodPost, "/api/session/"+sessionID+"/interrupt", nil)
	return err
}

// Kill cancels the local turn; the remote execution is interrupted too.
func (r *APIRuntime) Kill(sessionID string) error {
	_ = r.Interrupt(sessionID)
	r.mu.Lock()
	t := r.inflight[sessionID]
	r.mu.Unlock()
	if t != nil {
		t.cancel()
	}
	return nil
}

func (r *APIRuntime) Shutdown() {
	r.mu.Lock()
	turns := make([]*apiTurn, 0, len(r.inflight))
	for _, t := range r.inflight {
		turns = append(turns, t)
	}
	r.mu.Unlock()
	for _, t := range turns {
		t.cancel()
	}
}

// DeleteNative removes the session from opencode2's own store, invoked when
// the user deletes the shadow session in usher — otherwise the next sync tick
// would export it right back. Delegates to the legacy runtime, which owns the
// tombstone set the sync loop checks.
func (r *APIRuntime) DeleteNative(id string) error {
	return r.legacy.DeleteNative(id)
}

// v2ModelPayload builds a Model.Ref from a "provider/model[#variant]" ref.
func v2ModelPayload(ref string) map[string]any {
	provider, rest, _ := strings.Cut(ref, "/")
	id, variant, _ := strings.Cut(rest, "#")
	m := map[string]any{"providerID": provider, "id": id}
	if variant != "" {
		m["variant"] = variant
	}
	return m
}

// contextWindow returns the model's context limit, cached for the process
// lifetime in practice (model limits change rarely). 0 when unknown.
func (r *APIRuntime) contextWindow(ctx context.Context, model string) int64 {
	if model == "" || model == "default" {
		return 0
	}
	model, _, _ = strings.Cut(model, "#") // strip any effort variant suffix
	r.cwMu.Lock()
	defer r.cwMu.Unlock()
	if r.cwMap != nil && time.Since(r.cwAt) < 24*time.Hour {
		return r.cwMap[model]
	}
	if m, err := v2ContextWindows(ctx, r.cmd); err == nil {
		r.cwMap = m
		r.cwAt = time.Now()
	}
	return r.cwMap[model]
}

// truncateForTitle caps a prompt for use as a session title.
func truncateForTitle(s string) string {
	rs := []rune(strings.TrimSpace(s))
	if len(rs) > 60 {
		return string(rs[:60])
	}
	return string(rs)
}
