package dsh

// ACP runtime for dsh: one `dsh --profile acp` child per active session.
// dsh's session store is single-writer — a session open in the embedded web
// UI holds a write handle, and ACP resume then fails with "already owned by
// an active write handle"; the error is surfaced verbatim so the user knows
// to close it there first. The transcript of record stays the on-disk event
// log (Transcript); ACP updates feed only the live view.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/acp"
	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/hook"
)

type Runtime struct {
	cmd    string
	logger *slog.Logger
	hooks  *hook.Manager

	mu      sync.Mutex
	workers map[string]*worker
}

type worker struct {
	conn     *acp.Conn
	id       string
	cwd      string
	busy     bool
	lastUsed time.Time
}

func NewRuntime(cmd string, hooks *hook.Manager, logger *slog.Logger) *Runtime {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runtime{cmd: cmd, logger: logger, hooks: hooks, workers: map[string]*worker{}}
}

func (r *Runtime) spawn(ctx context.Context, id, cwd string) (*worker, error) {
	w := &worker{id: id, cwd: cwd, lastUsed: time.Now()}
	conn, err := acp.Start(ctx, r.cmd, []string{"--profile", "acp"}, cwd, nil, func(reqID int64, m acp.ServerRequest) {
		r.handleServerRequest(w, reqID, m)
	})
	if err != nil {
		return nil, err
	}
	w.conn = conn
	if err := conn.Initialize(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("dsh acp initialize: %w", err)
	}
	return w, nil
}

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
	w, err := r.spawn(ctx, id, cwd)
	if err != nil {
		return nil, err
	}
	if err := w.conn.SessionResume(ctx, id, cwd); err != nil {
		w.conn.Close()
		return nil, fmt.Errorf("dsh resume %s: %w", id, err)
	}
	r.mu.Lock()
	r.workers[id] = w
	r.mu.Unlock()
	return w, nil
}

// Start creates a new dsh session and runs the first turn.
func (r *Runtime) Start(ctx context.Context, req backend.StartRequest) (string, <-chan backend.Event, error) {
	if req.Cwd == "" {
		var err error
		req.Cwd, err = os.Getwd()
		if err != nil {
			return "", nil, err
		}
	}
	w, err := r.spawn(ctx, "", req.Cwd)
	if err != nil {
		return "", nil, err
	}
	id, err := w.conn.SessionNew(ctx, req.Cwd)
	if err != nil || id == "" {
		w.conn.Close()
		if err == nil {
			err = errors.New("dsh did not return a session id")
		}
		return "", nil, err
	}
	w.id = id
	r.mu.Lock()
	r.workers[id] = w
	r.mu.Unlock()
	return id, r.runTurn(ctx, w, req.Prompt, true), nil
}

func (r *Runtime) Send(ctx context.Context, id, prompt, cwd string) (<-chan backend.Event, error) {
	w, err := r.workerFor(ctx, id, cwd)
	if err != nil {
		return nil, err
	}
	return r.runTurn(ctx, w, prompt, false), nil
}

// runTurn executes one prompt and streams display events until it ends. dsh
// writes its own event log; only deltas and tool placeholders are emitted
// live, and the disk transcript is the truth at turn end.
func (r *Runtime) runTurn(ctx context.Context, w *worker, prompt string, fresh bool) <-chan backend.Event {
	out := make(chan backend.Event, 64)
	r.mu.Lock()
	w.busy = true
	r.mu.Unlock()

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
		defer w.conn.SetUpdateHandler(nil)
		defer func() {
			r.mu.Lock()
			w.busy = false
			w.lastUsed = time.Now()
			r.mu.Unlock()
		}()

		emit := func(ev backend.Event) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		emitErr := func(msg string) {
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

		promptDone := make(chan error, 1)
		go func() {
			_, err := w.conn.Prompt(ctx, w.id, prompt)
			promptDone <- err
		}()
		for {
			select {
			case u := <-updates:
				if ev := translateUpdate(u); ev != nil {
					if !emit(*ev) {
						return
					}
				}
			case err := <-promptDone:
				if err != nil && ctx.Err() == nil {
					emitErr("dsh turn failed: " + err.Error())
				}
				exited, _ := json.Marshal(map[string]any{"code": 0})
				emit(backend.Event{Type: backend.EventProcessExit, Raw: exited})
				return
			case <-ctx.Done():
				return
			case <-w.conn.Done():
				emitErr("dsh acp process exited")
				return
			}
		}
	}()
	return out
}

// translateUpdate maps one ACP session/update to a display event. dsh's disk
// log owns history; live only needs streaming text.
func translateUpdate(u acp.Update) *backend.Event {
	switch u.SessionUpdate {
	case "agent_message_chunk", "agent_thought_chunk":
		if u.Text() == "" {
			return nil
		}
		raw, _ := json.Marshal(backend.PartDeltaPayload{Delta: u.Text()})
		return &backend.Event{Type: backend.EventPartDelta, Raw: raw}
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
