// Package opencode adapts the opencode CLI to usher's backend contract.
//
// opencode stores its native session state in SQLite, so instead of binding
// discovery to that schema this package writes usher-owned shadow transcripts:
// Claude-shaped jsonl under <root>/<sanitized-cwd>/<id>.jsonl. Live turns are
// driven through `opencode run --format json` and translated event-by-event;
// a background sync additionally mirrors sessions created outside usher (TUI,
// other frontends) via `opencode session list` + `opencode export`.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
)

// Runtime drives opencode through `opencode run` — one child process per
// turn, resumed across turns with --session. Session ids are opencode's own
// (ses_…): a new session's id is learned from the first streamed event.
type Runtime struct {
	cmd    string
	root   string
	logger *slog.Logger

	mu      sync.Mutex
	running map[string]context.CancelFunc

	cwMu  sync.Mutex
	cwMap map[string]int64
	cwAt  time.Time
}

func NewRuntime(cmd, root string, logger *slog.Logger) *Runtime {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runtime{
		cmd:     cmd,
		root:    root,
		logger:  logger,
		running: map[string]context.CancelFunc{},
	}
}

// Root exposes the shadow directory for the sync loop.
func (r *Runtime) Root() string { return r.root }

// Cmd exposes the opencode binary path for the sync loop.
func (r *Runtime) Cmd() string { return r.cmd }

// Start begins a brand-new session. opencode assigns the id itself, so Start
// spawns the first turn without --session, blocks until the stream reveals
// the id, then returns it alongside the live event channel.
func (r *Runtime) Start(ctx context.Context, req backend.StartRequest) (string, <-chan backend.Event, error) {
	t, err := r.spawn(ctx, "", req.Prompt, req.Cwd, req.Model, true)
	if err != nil {
		return "", nil, err
	}
	select {
	case <-t.ready:
		return t.id, t.events(), nil
	case err := <-t.failed:
		return "", nil, err
	case <-time.After(60 * time.Second):
		t.cancel()
		return "", nil, errors.New("opencode did not emit a session id within 60s")
	}
}

// Send resumes an existing session; the id is known up front.
func (r *Runtime) Send(ctx context.Context, id, prompt, cwd string) (<-chan backend.Event, error) {
	return r.SendWithModel(ctx, id, prompt, cwd, "")
}

// SendWithModel resumes with a per-turn model override (`opencode run
// --session … --model provider/model`).
func (r *Runtime) SendWithModel(ctx context.Context, id, prompt, cwd, model string) (<-chan backend.Event, error) {
	t, err := r.spawn(ctx, id, prompt, cwd, model, false)
	if err != nil {
		return nil, err
	}
	return t.events(), nil
}

// turn owns one spawned `opencode run` process and its translation pipeline:
// NDJSON stdout → Claude-shaped shadow jsonl + backend events.
type turn struct {
	rt     *Runtime
	cmd    *exec.Cmd
	cancel context.CancelFunc
	model  string

	id        string
	ready     chan struct{}
	readyOnce sync.Once
	failed    chan error

	out chan backend.Event
}

// rawEvent is one line of `opencode run --format json` output.
type rawEvent struct {
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part"`
}

// partPayload is the union of streamed part shapes we translate.
type partPayload struct {
	ID     string     `json:"id"`
	Type   string     `json:"type"`
	Text   string     `json:"text"`
	Tool   string     `json:"tool"`
	CallID string     `json:"callID"`
	State  *toolState `json:"state"`
	Tokens *struct {
		Total  int64 `json:"total"`
		Input  int64 `json:"input"`
		Output int64 `json:"output"`
	} `json:"tokens"`
}

func (r *Runtime) spawn(ctx context.Context, id, prompt, cwd, model string, fresh bool) (*turn, error) {
	if strings.TrimSpace(cwd) == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	childCtx, cancel := context.WithCancel(ctx)
	args := []string{"run", "--format", "json", "--dir", cwd}
	if id != "" {
		args = append(args, "--session", id)
	}
	if model != "" && model != "default" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)
	cmd := exec.CommandContext(childCtx, r.cmd, args...)
	cmd.Dir = cwd
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	t := &turn{
		rt:     r,
		cmd:    cmd,
		cancel: cancel,
		id:     id,
		model:  model,
		ready:  make(chan struct{}),
		failed: make(chan error, 1),
		out:    make(chan backend.Event, 64),
	}
	if id != "" {
		t.readyOnce.Do(func() { close(t.ready) }) // Send path: id known from the start.
	}
	go t.pump(stdout, stderr, childCtx, cwd, prompt, fresh)
	return t, nil
}

func (t *turn) events() <-chan backend.Event { return t.out }

// pump is the single consumer of the NDJSON stream. It resolves the session
// id from the first event, sets up the shadow log, then translates every
// event in order.
func (t *turn) pump(stdout, stderr io.Reader, ctx context.Context, cwd, prompt string, fresh bool) {
	defer close(t.out)
	defer t.cancel()

	var errBuf bytes.Buffer
	errDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&errBuf, stderr)
		close(errDone)
	}()

	rawCh := make(chan rawEvent, 64)
	scanDone := make(chan struct{})
	go func() {
		defer close(rawCh)
		defer close(scanDone)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var ev rawEvent
			if json.Unmarshal(line, &ev) != nil {
				continue
			}
			rawCh <- ev
		}
	}()

	fail := func(err error) {
		select {
		case t.failed <- err:
		default:
		}
	}

	// Phase 1: resolve the session id (Send preset it; Start learns it here).
	var pending []rawEvent
	for t.id == "" {
		select {
		case ev, ok := <-rawCh:
			if !ok {
				fail(fmt.Errorf("opencode exited before emitting a session id: %s", stderrTail(&errBuf)))
				t.wait()
				<-errDone
				return
			}
			pending = append(pending, ev)
			if ev.SessionID != "" {
				t.id = ev.SessionID
			}
		case <-ctx.Done():
			t.wait()
			<-errDone
			return
		}
	}
	t.readyOnce.Do(func() { close(t.ready) })

	path := logPath(t.rt.root, cwd, t.id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.emitErr("opencode shadow log setup failed: " + err.Error())
		t.killAndWait()
		<-errDone
		return
	}

	t.rt.mu.Lock()
	if prev := t.rt.running[t.id]; prev != nil {
		prev() // a duplicate send kills the older process
	}
	t.rt.running[t.id] = t.cancel
	t.rt.mu.Unlock()
	defer func() {
		t.rt.mu.Lock()
		delete(t.rt.running, t.id)
		t.rt.mu.Unlock()
	}()

	started, _ := json.Marshal(backend.ProcessStartedPayload{Cwd: cwd, Fresh: fresh})
	if !t.emit(ctx, backend.Event{Type: backend.EventProcessStarted, Raw: started}) {
		t.killAndWait()
		<-errDone
		return
	}
	userRaw := userLine(t.id, cwd, prompt, time.Now().UTC())
	if !t.appendAndEmit(ctx, path, "user", userRaw) {
		t.killAndWait()
		<-errDone
		return
	}

	// Phase 2: translate events — the buffered pre-id ones first, then live.
	stream := func(ev rawEvent) bool {
		ts := eventTime(ev.Timestamp)
		switch ev.Type {
		case "step_finish":
			// Token accounting for the ctx-usage pie. opencode reports the
			// turn's cumulative totals; the context window comes from the
			// model's models.dev metadata (cached in the runtime).
			var p partPayload
			if json.Unmarshal(ev.Part, &p) != nil || p.Tokens == nil {
				return true
			}
			rt := core.SessionRuntime{
				Model:         t.model,
				ContextTokens: p.Tokens.Total,
				ContextWindow: t.rt.contextWindow(t.model),
			}
			raw, _ := json.Marshal(rt)
			return t.emit(ctx, backend.Event{Type: backend.EventRuntime, Raw: raw})
		case "text", "reasoning":
			var p partPayload
			if json.Unmarshal(ev.Part, &p) != nil {
				return true
			}
			var raw json.RawMessage
			if ev.Type == "reasoning" {
				raw = assistantLine(t.id, thinkingBlocks(p.Text), ts)
			} else {
				raw = assistantLine(t.id, textBlocks(p.Text), ts)
			}
			if raw == nil {
				return true // empty part (e.g. blank reasoning): skip
			}
			return t.appendAndEmit(ctx, path, "assistant", withModel(raw, t.model))
		case "tool_use":
			var p partPayload
			if json.Unmarshal(ev.Part, &p) != nil || p.State == nil {
				return true
			}
			if p.State.Status == "running" || p.State.Status == "pending" {
				// Live placeholder only: the card shows immediately while the
				// tool runs; the shadow gets the completed pair on finish.
				if raw := assistantLine(t.id, toolUseBlocks(p), ts); raw != nil {
					t.emit(ctx, backend.Event{Type: "assistant", Raw: withModel(raw, t.model)})
				}
				return true
			}
			if p.State.Status != "completed" && p.State.Status != "error" {
				return true
			}
			if !t.appendAndEmit(ctx, path, "assistant", withModel(assistantLine(t.id, toolUseBlocks(p), ts), t.model)) {
				return false
			}
			return t.appendAndEmit(ctx, path, "user", toolResultLine(t.id, cwd, p, ts))
		}
		return true
	}
	for _, ev := range pending {
		if !stream(ev) {
			t.killAndWait()
			<-errDone
			return
		}
	}
	pending = nil
	for ev := range rawCh {
		if !stream(ev) {
			t.killAndWait()
			<-errDone
			return
		}
	}
	<-scanDone

	waitErr := t.wait()
	<-errDone
	if waitErr != nil && ctx.Err() == nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		t.emitErr("opencode turn failed: " + msg)
	}
	systemRaw := turnCompleteLine(t.id, time.Now().UTC())
	if !t.appendAndEmit(ctx, path, "system", systemRaw) {
		return
	}
	exited, _ := json.Marshal(map[string]any{"code": exitCode(waitErr)})
	t.emit(ctx, backend.Event{Type: backend.EventProcessExit, Raw: exited})
}

func (t *turn) wait() error { return t.cmd.Wait() }

func (t *turn) killAndWait() {
	_ = t.cmd.Process.Kill()
	_, _ = t.cmd.Process.Wait()
}

func (t *turn) appendAndEmit(ctx context.Context, path, typ string, raw json.RawMessage) bool {
	if err := appendLogLine(path, raw); err != nil {
		t.emitErr("opencode shadow log write failed: " + err.Error())
		return false
	}
	return t.emit(ctx, backend.Event{Type: typ, Raw: raw})
}

func (t *turn) emit(ctx context.Context, ev backend.Event) bool {
	select {
	case t.out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

func (t *turn) emitErr(msg string) {
	raw, _ := json.Marshal(backend.ErrorPayload{Message: msg})
	select {
	case t.out <- backend.Event{Type: backend.EventError, Raw: raw}:
	default:
	}
}

func stderrTail(buf *bytes.Buffer) string {
	s := strings.TrimSpace(buf.String())
	if s == "" {
		return "(no stderr)"
	}
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return s
}

func (r *Runtime) Has(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.running[sessionID]
	return ok
}

func (r *Runtime) LiveSessions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.running))
	for id := range r.running {
		out = append(out, id)
	}
	return out
}

// Interrupt cancels the in-flight turn; opencode has no softer interrupt.
func (r *Runtime) Interrupt(sessionID string) error { return r.Kill(sessionID) }

func (r *Runtime) Kill(sessionID string) error {
	r.mu.Lock()
	cancel := r.running[sessionID]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (r *Runtime) Shutdown() {
	r.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(r.running))
	for _, cancel := range r.running {
		cancels = append(cancels, cancel)
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// contextWindow returns the model's context limit from `opencode models
// --verbose` (models.dev metadata), cached for five minutes. 0 when unknown —
// the usage pie just hides without a window.
func (r *Runtime) contextWindow(model string) int64 {
	if model == "" || model == "default" {
		return 0
	}
	r.cwMu.Lock()
	defer r.cwMu.Unlock()
	if r.cwMap != nil && time.Since(r.cwAt) < 5*time.Minute {
		return r.cwMap[model]
	}
	out, err := exec.Command(r.cmd, "models", "--verbose").Output()
	if err != nil {
		return r.cwMap[model]
	}
	m := map[string]int64{}
	// Format: "provider/model\n{…pretty JSON…}\n" blocks per model; the JSON
	// is multi-line, so accumulate a block and parse it whole.
	var block strings.Builder
	var curID string
	depth := 0
	flush := func() {
		if curID == "" || block.Len() == 0 {
			return
		}
		var doc struct {
			Limit struct {
				Context int64 `json:"context"`
			} `json:"limit"`
		}
		if json.Unmarshal([]byte(block.String()), &doc) == nil && doc.Limit.Context > 0 {
			m[curID] = doc.Limit.Context
		}
	}
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if depth == 0 && trimmed != "" && !strings.HasPrefix(trimmed, "{") {
			flush()
			curID = trimmed
			block.Reset()
			continue
		}
		if curID == "" {
			continue
		}
		block.WriteString(line)
		block.WriteByte('\n')
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth <= 0 && block.Len() > 0 {
			flush()
			curID = ""
			block.Reset()
			depth = 0
		}
	}
	flush()
	r.cwMap = m
	r.cwAt = time.Now()
	return m[model]
}

// Locate finds the shadow transcript for id under root, "" when absent.
func Locate(root, id string) string {	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) == id+".jsonl" {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func logPath(root, cwd, id string) string {
	return filepath.Join(root, projectKey(cwd), id+".jsonl")
}

func projectKey(cwd string) string {
	clean := filepath.Clean(cwd)
	if runtime.GOOS == "windows" {
		clean = strings.TrimSuffix(filepath.VolumeName(clean), ":") + strings.TrimPrefix(clean, filepath.VolumeName(clean))
	}
	clean = strings.Trim(clean, string(os.PathSeparator))
	if clean == "" || clean == "." {
		return "root"
	}
	replacer := strings.NewReplacer(
		string(os.PathSeparator), "-",
		":", "-",
		" ", "-",
	)
	return replacer.Replace(clean)
}

func appendLogLine(path string, raw json.RawMessage) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	_, err = f.Write([]byte("\n"))
	return err
}

func eventTime(ms int64) time.Time {
	if ms > 0 {
		return time.UnixMilli(ms).UTC()
	}
	return time.Now().UTC()
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}
