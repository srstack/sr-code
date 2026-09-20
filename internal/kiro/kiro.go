// Package kiro adapts the Kiro CLI to usher's backend contract.
//
// Sessions are driven over Agent Client Protocol stdio: one long-lived
// `kiro-cli acp` child per active session (v3 engine for sess_* ids, the
// default v2 engine for legacy flat-store uuids — which must not get an
// explicit engine flag, or resume fails with "ACP load_session failed").
// ACP gives live streaming updates and a permission callback channel, unlike
// the earlier headless `chat --no-interactive` mode, which auto-approved
// everything and could never surface questions.
//
// The transcript of record stays kiro's native messages.jsonl (v3) /
// cli/<id>.jsonl (legacy), parsed by Transcript / LegacyTranscript; ACP
// updates only feed the live view.
package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/acp"
	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
)

// Runtime drives kiro over ACP, one worker process per active session.
type Runtime struct {
	cmd     string
	root    string // sessions root (~/.kiro/sessions), used to locate transcripts
	logger  *slog.Logger
	models  *ModelCatalog
	hooks   *hook.Manager
	maxLive int

	mu      sync.Mutex
	workers map[string]*worker
}

func NewRuntime(cmd, root string, maxLive int, hooks *hook.Manager, logger *slog.Logger) *Runtime {
	if logger == nil {
		logger = slog.Default()
	}
	if maxLive <= 0 {
		maxLive = 8
	}
	return &Runtime{
		cmd:     cmd,
		root:    root,
		logger:  logger,
		models:  &ModelCatalog{Cmd: cmd},
		hooks:   hooks,
		maxLive: maxLive,
		workers: map[string]*worker{},
	}
}

// Models exposes the model catalog so main can register it on the backend.
func (r *Runtime) Models() *ModelCatalog { return r.models }

// worker is one ACP child bound to one session.
type worker struct {
	conn     *acp.Conn
	id       string
	cwd      string
	busy     bool
	lastUsed time.Time
}

// isV3 reports whether the session id belongs to the v3 engine store.
func isV3(id string) bool { return id == "" || strings.HasPrefix(id, "sess_") }

func (r *Runtime) acpArgs(id string) []string {
	if isV3(id) {
		return []string{"acp", "--agent-engine", "v3", "--auth-method", "cli"}
	}
	// Legacy (v2 engine): --auth-method is a v3-only flag; the v2 ACP process
	// misbehaves with it (loads fine, then prompts return instantly without
	// touching the transcript).
	return []string{"acp"}
}

// spawnWorker starts a kiro ACP child for cwd.
func (r *Runtime) spawnWorker(ctx context.Context, id, cwd string) (*worker, error) {
	w := &worker{id: id, cwd: cwd, lastUsed: time.Now()}
	conn, err := acp.Start(ctx, r.cmd, r.acpArgs(id), cwd, nil, func(reqID int64, m acp.ServerRequest) {
		r.handleServerRequest(w, reqID, m)
	})
	if err != nil {
		return nil, err
	}
	w.conn = conn
	if err := conn.Initialize(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("kiro acp initialize: %w", err)
	}
	return w, nil
}

// workerFor returns the live worker for id, spawning and attaching
// (session/load) a cold one when needed.
func (r *Runtime) workerFor(ctx context.Context, id, cwd string) (*worker, error) {
	r.mu.Lock()
	w := r.workers[id]
	r.mu.Unlock()
	if w != nil {
		select {
		case <-w.conn.Done():
			r.mu.Lock()
			delete(r.workers, id)
			r.mu.Unlock()
		default:
			return w, nil
		}
	}
	if cwd == "" {
		cwd = r.cwdFor(id)
	}
	w, err := r.spawnWorker(ctx, id, cwd)
	if err != nil {
		return nil, err
	}
	if err := w.conn.SessionLoad(ctx, id, cwd); err != nil {
		w.conn.Close()
		return nil, fmt.Errorf("kiro resume %s: %w", id, err)
	}
	r.mu.Lock()
	r.workers[id] = w
	r.evictIdleLocked()
	r.mu.Unlock()
	return w, nil
}

// evictIdleLocked closes the least-recently-used idle workers beyond maxLive.
// Caller holds r.mu.
func (r *Runtime) evictIdleLocked() {
	for len(r.workers) > r.maxLive {
		var oldestID string
		var oldest time.Time
		for id, w := range r.workers {
			if w.busy {
				continue
			}
			if oldestID == "" || w.lastUsed.Before(oldest) {
				oldestID, oldest = id, w.lastUsed
			}
		}
		if oldestID == "" {
			return // all busy
		}
		r.workers[oldestID].conn.Close()
		delete(r.workers, oldestID)
	}
}

// Start creates a new v3 session and runs the first turn.
func (r *Runtime) Start(ctx context.Context, req backend.StartRequest) (string, <-chan backend.Event, error) {
	if strings.TrimSpace(req.Cwd) == "" {
		var err error
		req.Cwd, err = os.Getwd()
		if err != nil {
			return "", nil, err
		}
	}
	w, err := r.spawnWorker(ctx, "", req.Cwd)
	if err != nil {
		return "", nil, err
	}
	id, err := w.conn.SessionNew(ctx, req.Cwd)
	if err != nil || id == "" {
		w.conn.Close()
		if err == nil {
			err = errors.New("kiro did not return a session id")
		}
		return "", nil, err
	}
	w.id = id
	r.mu.Lock()
	r.workers[id] = w
	r.evictIdleLocked()
	r.mu.Unlock()
	return id, r.runTurn(ctx, w, req.Prompt, req.Model, true), nil
}

func (r *Runtime) Send(ctx context.Context, id, prompt, cwd string) (<-chan backend.Event, error) {
	return r.SendWithModel(ctx, id, prompt, cwd, "")
}

func (r *Runtime) SendWithModel(ctx context.Context, id, prompt, cwd, model string) (<-chan backend.Event, error) {
	w, err := r.workerFor(ctx, id, cwd)
	if err != nil {
		return nil, err
	}
	return r.runTurn(ctx, w, prompt, model, false), nil
}

// runTurn executes one prompt and streams display events until it ends.
func (r *Runtime) runTurn(ctx context.Context, w *worker, prompt, model string, fresh bool) <-chan backend.Event {
	out := make(chan backend.Event, 64)
	r.mu.Lock()
	w.busy = true
	r.mu.Unlock()

	// Per-turn update stream bound to this turn's channel.
	updates := make(chan acp.Update, 256)
	w.conn.SetUpdateHandler(func(u acp.Update) {
		if u.SessionID == w.id {
			select {
			case updates <- u:
			default:
			}
		}
	})

	go func() {
		defer close(out)
		defer func() {
			r.mu.Lock()
			w.busy = false
			w.lastUsed = time.Now()
			r.mu.Unlock()
		}()
		defer w.conn.SetUpdateHandler(nil)

		emit := func(ev backend.Event) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		emitErr := func(msg string) {
			r.logger.Warn("kiro turn error", "session", w.id, "err", msg)
			raw, _ := json.Marshal(backend.ErrorPayload{Message: msg})
			select {
			case out <- backend.Event{Type: backend.EventError, Raw: raw}:
			default:
			}
		}

		started, _ := json.Marshal(backend.ProcessStartedPayload{Cwd: w.cwd, Fresh: fresh})
		if !emit(backend.Event{Type: backend.EventProcessStarted, Raw: started}) {
			return
		}
		// Echo the user prompt as a transcript-shaped line so the assembler
		// produces the user turn (the native file gets its own copy from kiro).
		userLine := kiroUserLine(w.id, prompt)
		if !isV3(w.id) {
			userLine = legacyUserLine(prompt)
		}
		if !emit(backend.Event{Type: "kiro", Raw: userLine}) {
			return
		}

		var usagePct float64
		promptDone := make(chan error, 1)
		go func() {
			_, err := w.conn.Prompt(ctx, w.id, prompt)
			promptDone <- err
		}()
		for {
			select {
			case u := <-updates:
				if pct := u.ContextUsagePct(); pct > 0 {
					usagePct = pct
				}
				if ev := translateUpdate(w.id, u); ev != nil {
					if !emit(*ev) {
						return
					}
				}
			case err := <-promptDone:
				if err != nil && ctx.Err() == nil {
					if tail := strings.TrimSpace(w.conn.StderrTail()); tail != "" {
						r.logger.Warn("kiro acp stderr", "session", w.id, "tail", tail)
					}
					emitErr("kiro turn failed: " + err.Error())
				}
				r.emitRuntime(out, ctx, w.id, usagePct)
				exited, _ := json.Marshal(map[string]any{"code": 0})
				emit(backend.Event{Type: backend.EventProcessExit, Raw: exited})
				return
			case <-ctx.Done():
				return
			case <-w.conn.Done():
				emitErr("kiro acp process exited")
				return
			}
		}
	}()
	return out
}

// emitRuntime reports context occupancy for the turn.
func (r *Runtime) emitRuntime(out chan<- backend.Event, ctx context.Context, id string, usagePct float64) {
	if usagePct <= 0 {
		return
	}
	model := r.modelOf(id)
	rt := core.SessionRuntime{Model: model, ContextWindow: r.models.ContextWindow(ctx, model)}
	rt.ContextTokens = int64(usagePct / 100 * float64(rt.ContextWindow))
	raw, _ := json.Marshal(rt)
	select {
	case out <- backend.Event{Type: backend.EventRuntime, Raw: raw}:
	case <-ctx.Done():
	}
}

// translateUpdate maps one ACP session/update to a display event (nil =
// nothing to show). Text streams as deltas; tool calls are synthesized as
// transcript lines in the session's own format (v3 or legacy) so the matching
// assembler renders the cards.
func translateUpdate(sessionID string, u acp.Update) *backend.Event {
	legacy := !isV3(sessionID)
	switch u.SessionUpdate {
	case "agent_message_chunk", "agent_thought_chunk":
		if u.Text() == "" {
			return nil
		}
		raw, _ := json.Marshal(backend.PartDeltaPayload{Delta: u.Text()})
		return &backend.Event{Type: backend.EventPartDelta, Raw: raw}
	case "tool_call":
		var raw json.RawMessage
		if legacy {
			raw = legacyToolCallLine(u)
		} else {
			raw = kiroToolCallLine(sessionID, u)
		}
		if raw == nil {
			return nil
		}
		return &backend.Event{Type: "kiro", Raw: raw}
	case "tool_call_update":
		if u.Status != "completed" && u.Status != "failed" {
			return nil
		}
		var raw json.RawMessage
		if legacy {
			raw = legacyToolResultLine(u)
		} else {
			raw = kiroToolResultLine(sessionID, u)
		}
		if raw == nil {
			return nil
		}
		return &backend.Event{Type: "kiro", Raw: raw}
	}
	return nil
}

// handleServerRequest relays session/request_permission to usher's
// interaction UI and answers with the chosen option.
func (r *Runtime) handleServerRequest(w *worker, reqID int64, m acp.ServerRequest) {
	if m.Method != "session/request_permission" {
		_ = w.conn.RespondError(reqID, -32601, "unsupported request: "+m.Method)
		return
	}
	var p acp.PermissionParams
	if json.Unmarshal(m.Params, &p) != nil {
		_ = w.conn.RespondError(reqID, -32602, "invalid permission params")
		return
	}
	allowAlways := false
	for _, o := range p.Options {
		if o.Kind == "allow_always" {
			allowAlways = true
		}
	}
	input := p.ToolCall.RawInput
	if len(input) == 0 {
		input, _ = json.Marshal(map[string]any{"title": p.ToolCall.Title, "kind": p.ToolCall.Kind})
	}
	resp, err := r.hooks.Submit(context.Background(), hook.Event{
		SessionID:   w.id,
		ToolUseID:   p.ToolCall.ToolCallID,
		Event:       "PermissionRequest",
		ToolName:    p.ToolCall.Title,
		ToolInput:   input,
		Cwd:         w.cwd,
		AllowAlways: allowAlways,
	})
	if err != nil {
		_ = w.conn.RespondError(reqID, -32000, err.Error())
		return
	}
	kind := "reject_once"
	if resp.Behavior == "allow" {
		if resp.Scope == "session" {
			kind = "allow_always"
		} else {
			kind = "allow_once"
		}
	}
	optionID := ""
	for _, o := range p.Options {
		if o.Kind == kind {
			optionID = o.OptionID
			break
		}
	}
	if optionID == "" && len(p.Options) > 0 {
		optionID = p.Options[0].OptionID
	}
	_ = w.conn.Respond(reqID, map[string]any{
		"outcome": map[string]any{"outcome": "selected", "optionId": optionID},
	})
}

// modelOf reads the session's model from session.json (v3); legacy sessions
// carry it in the sidecar's rts_model_state.
func (r *Runtime) modelOf(id string) string {
	p := r.locate(id)
	if p == "" {
		return ""
	}
	if !isV3(id) {
		raw, err := os.ReadFile(strings.TrimSuffix(p, ".jsonl") + ".json")
		if err != nil {
			return ""
		}
		var sc legacySidecar
		if json.Unmarshal(raw, &sc) == nil {
			return sc.SessionState.RtsModelState.ModelInfo.ModelID
		}
		return ""
	}
	s, err := readSessionJSON(filepath.Dir(p))
	if err != nil {
		return ""
	}
	return s.ModelID
}

// cwdFor returns the cwd recorded for a session ("" when unknown; the worker
// then inherits usher's cwd, which kiro scopes its session store to).
func (r *Runtime) cwdFor(id string) string {
	p := r.locate(id)
	if p == "" {
		return ""
	}
	if !isV3(id) {
		raw, err := os.ReadFile(strings.TrimSuffix(p, ".jsonl") + ".json")
		if err != nil {
			return ""
		}
		var sc legacySidecar
		if json.Unmarshal(raw, &sc) == nil {
			return sc.Cwd
		}
		return ""
	}
	if s, err := readSessionJSON(filepath.Dir(p)); err == nil && len(s.WorkspacePaths) > 0 {
		return s.WorkspacePaths[0]
	}
	return ""
}

// locate finds the transcript for id under the sessions root.
func (r *Runtime) locate(id string) string { return Locate(r.root, id) }

func (r *Runtime) Has(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[sessionID]
	return ok && w.busy
}

func (r *Runtime) LiveSessions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.workers))
	for id := range r.workers {
		out = append(out, id)
	}
	return out
}

// Interrupt cancels the in-flight turn via session/cancel.
func (r *Runtime) Interrupt(sessionID string) error {
	r.mu.Lock()
	w := r.workers[sessionID]
	r.mu.Unlock()
	if w == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.conn.Cancel(ctx, sessionID)
}

// Kill closes the session's worker (the turn dies with the process).
func (r *Runtime) Kill(sessionID string) error {
	r.mu.Lock()
	w := r.workers[sessionID]
	delete(r.workers, sessionID)
	r.mu.Unlock()
	if w != nil {
		w.conn.Close()
	}
	return nil
}

func (r *Runtime) Shutdown() {
	r.mu.Lock()
	workers := make([]*worker, 0, len(r.workers))
	for _, w := range r.workers {
		workers = append(workers, w)
	}
	r.workers = map[string]*worker{}
	r.mu.Unlock()
	for _, w := range workers {
		w.conn.Close()
	}
}

// DeleteNative sweeps kiro's own records after usher removed the transcript
// file: v3 keeps session.json (and snapshots) beside messages.jsonl inside
// <root>/<hash>/sess_<id>/; legacy keeps <id>.json/.history/.lock sidecars in
// cli/. Without this, kiro's own session list keeps offering a husk.
func (r *Runtime) DeleteNative(id string) error {
	if isV3(id) {
		var dir string
		_ = filepath.Walk(r.root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || !info.IsDir() {
				return nil
			}
			if info.Name() == "cli" {
				return filepath.SkipDir
			}
			if info.Name() == id {
				dir = path
				return filepath.SkipAll
			}
			return nil
		})
		if dir != "" {
			return os.RemoveAll(dir)
		}
		return nil
	}
	for _, ext := range []string{".json", ".history", ".lock"} {
		_ = os.Remove(filepath.Join(r.root, "cli", id+ext))
	}
	return nil
}

// Locate finds the transcript for id under root, "" when absent. Legacy
// (v1/v2) sessions are flat files at cli/<uuid>.jsonl; v3 sessions are
// <project-hash>/sess_<uuid>/messages.jsonl.
func Locate(root, id string) string {
	if root == "" || id == "" {
		return ""
	}
	if !isV3(id) {
		p := filepath.Join(root, "cli", id+".jsonl")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		return ""
	}
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == "cli" { // legacy flat store, not v3 sessions
				return filepath.SkipDir
			}
			return nil
		}
		if info.Name() == "messages.jsonl" && filepath.Base(filepath.Dir(path)) == id {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
