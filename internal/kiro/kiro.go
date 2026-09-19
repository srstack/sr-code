// Package kiro adapts the Kiro CLI (v3 agent engine) to usher's backend
// contract.
//
// Sessions are driven through per-turn headless chat processes:
//
//	kiro-cli --v3 chat --no-interactive --trust-all-tools [--model M] <prompt>
//	kiro-cli --v3 chat --no-interactive --trust-all-tools --resume-id <id> <prompt>
//
// kiro writes its own native transcript (messages.jsonl) under
// ~/.kiro/sessions/<project-hash>/sess_<uuid>/, which is both the discovery
// source (KiroSource) and the live event source: the runtime tails the file
// while the child runs and emits each new record.
//
// Headless chat auto-approves tools (--trust-all-tools); kiro's interactive
// question flows are not reachable in this mode, so nothing is surfaced to
// usher's permission UI from this backend.
package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
)

// Runtime drives kiro through `kiro-cli --v3 chat --no-interactive` — one
// child process per turn, resumed across turns with --resume-id.
type Runtime struct {
	cmd    string
	root   string // sessions root (~/.kiro/sessions), used to locate transcripts
	logger *slog.Logger
	models *ModelCatalog

	mu      sync.Mutex
	running map[string]context.CancelFunc
	paths   map[string]string // session id -> messages.jsonl path (learned at Start)
}

func NewRuntime(cmd, root string, logger *slog.Logger) *Runtime {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runtime{
		cmd:     cmd,
		root:    root,
		logger:  logger,
		models:  &ModelCatalog{Cmd: cmd},
		running: map[string]context.CancelFunc{},
		paths:   map[string]string{},
	}
}

// Models exposes the model catalog so main can register it on the backend.
func (r *Runtime) Models() *ModelCatalog { return r.models }

// Start begins a brand-new session. kiro assigns the id itself (sess_<uuid>),
// so Start spawns the first turn, then polls `chat --list-sessions` for the
// cwd until a previously-unseen session appears.
func (r *Runtime) Start(ctx context.Context, req backend.StartRequest) (string, <-chan backend.Event, error) {
	if strings.TrimSpace(req.Cwd) == "" {
		var err error
		req.Cwd, err = os.Getwd()
		if err != nil {
			return "", nil, err
		}
	}
	before := r.sessionIDs(ctx, req.Cwd)
	t, err := r.spawn(ctx, "", req.Prompt, req.Cwd, req.Model)
	if err != nil {
		return "", nil, err
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		id, path := r.findNewSession(ctx, req.Cwd, before)
		if id != "" {
			t.attach(id, path)
			return id, t.events(), nil
		}
		select {
		case err := <-t.failed:
			return "", nil, err
		case <-time.After(500 * time.Millisecond):
			if time.Now().After(deadline) {
				t.cancel()
				return "", nil, errors.New("kiro did not register a session within 60s")
			}
		case <-ctx.Done():
			t.cancel()
			return "", nil, ctx.Err()
		}
	}
}

// Send resumes an existing session via --resume-id.
func (r *Runtime) Send(ctx context.Context, id, prompt, cwd string) (<-chan backend.Event, error) {
	return r.SendWithModel(ctx, id, prompt, cwd, "")
}

func (r *Runtime) SendWithModel(ctx context.Context, id, prompt, cwd, model string) (<-chan backend.Event, error) {
	if strings.TrimSpace(cwd) == "" {
		cwd = r.cwdFor(id)
	}
	t, err := r.spawn(ctx, id, prompt, cwd, model)
	if err != nil {
		return nil, err
	}
	t.attach(id, r.locate(id))
	return t.events(), nil
}

// cwdFor returns the cwd recorded for a live session ("" when unknown; the
// child then inherits usher's cwd, which kiro scopes its session store to).
func (r *Runtime) cwdFor(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.paths[id]; p != "" {
		if s, err := readSessionJSON(filepath.Dir(p)); err == nil && len(s.WorkspacePaths) > 0 {
			return s.WorkspacePaths[0]
		}
	}
	return ""
}

// locate finds the messages.jsonl for id under the sessions root.
func (r *Runtime) locate(id string) string {
	r.mu.Lock()
	if p := r.paths[id]; p != "" {
		r.mu.Unlock()
		return p
	}
	r.mu.Unlock()
	found := Locate(r.root, id)
	if found != "" {
		r.mu.Lock()
		r.paths[id] = found
		r.mu.Unlock()
	}
	return found
}

// sessionIDs lists kiro's known session ids for cwd (empty on any failure —
// Start then treats every session as new, which is correct on first use).
func (r *Runtime) sessionIDs(ctx context.Context, cwd string) map[string]bool {
	out := map[string]bool{}
	for _, s := range listSessions(ctx, r.cmd, cwd) {
		out[s] = true
	}
	return out
}

// findNewSession returns the first session id for cwd not in before, plus its
// transcript path. Falls back to an fs scan when list-sessions lags the write.
func (r *Runtime) findNewSession(ctx context.Context, cwd string, before map[string]bool) (string, string) {
	for _, id := range listSessions(ctx, r.cmd, cwd) {
		if !before[id] {
			if p := Locate(r.root, id); p != "" {
				return id, p
			}
			return id, ""
		}
	}
	return "", ""
}

// turn owns one spawned headless chat process and its transcript tailer.
type turn struct {
	rt     *Runtime
	cmd    *exec.Cmd
	cancel context.CancelFunc

	mu       sync.Mutex
	id       string
	path     string // messages.jsonl
	ready    chan struct{}
	failed   chan error
	waitDone chan struct{} // closed when the child exits
	waitErr  error

	out chan backend.Event
}

func (r *Runtime) spawn(ctx context.Context, id, prompt, cwd, model string) (*turn, error) {
	childCtx, cancel := context.WithCancel(ctx)
	args := []string{"--v3", "chat", "--no-interactive", "--trust-all-tools"}
	if id != "" {
		args = append(args, "--resume-id", id)
	}
	if model != "" && model != "default" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)
	cmd := exec.CommandContext(childCtx, r.cmd, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	// kiro's final answer text goes to stdout, diagnostics to stderr; the
	// transcript file is the display source, so both are just drained.
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
		rt:       r,
		cmd:      cmd,
		cancel:   cancel,
		id:       id,
		ready:    make(chan struct{}),
		failed:   make(chan error, 1),
		waitDone: make(chan struct{}),
		out:      make(chan backend.Event, 64),
	}
	go t.run(stdout, stderr, childCtx, cwd)
	return t, nil
}

// attach binds the turn to its transcript once the session id is known.
func (t *turn) attach(id, path string) {
	t.mu.Lock()
	t.id, t.path = id, path
	t.mu.Unlock()
	t.rt.mu.Lock()
	t.rt.paths[id] = path
	t.rt.mu.Unlock()
	close(t.ready)
}

func (t *turn) events() <-chan backend.Event { return t.out }

func (t *turn) run(stdout, stderr io.Reader, ctx context.Context, cwd string) {
	defer close(t.out)
	defer t.cancel()

	var errBuf bytes.Buffer
	errDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&errBuf, stderr)
		close(errDone)
	}()
	// stdout carries the final answer text only; discard it (transcript wins).
	discardDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stdout)
		close(discardDone)
	}()

	// Wait in a goroutine so the tailer can observe process exit (ProcessState
	// is only set once Wait returns).
	go func() {
		err := t.cmd.Wait()
		t.mu.Lock()
		t.waitErr = err
		t.mu.Unlock()
		close(t.waitDone)
	}()

	// Wait for attach (Start resolves the id asynchronously; Send pre-attaches).
	select {
	case <-t.ready:
	case <-t.waitDone:
		// Process exited before a session id was resolved (bad prompt, auth,
		// model error): report it instead of letting Start poll to timeout.
		<-errDone // process is gone, so stderr hits EOF promptly
		t.mu.Lock()
		werr := t.waitErr
		t.mu.Unlock()
		msg := stderrTail(&errBuf)
		if msg == "(no stderr)" && werr != nil {
			msg = werr.Error()
		}
		if msg == "(no stderr)" {
			msg = "kiro exited without creating a session"
		}
		select {
		case t.failed <- errors.New(msg):
		default:
		}
		<-discardDone
		return
	case <-ctx.Done():
		<-t.waitDone
		<-errDone
		<-discardDone
		return
	}

	t.mu.Lock()
	id, path := t.id, t.path
	t.mu.Unlock()

	t.rt.mu.Lock()
	if prev := t.rt.running[id]; prev != nil {
		prev() // a duplicate send kills the older process
	}
	t.rt.running[id] = t.cancel
	t.rt.mu.Unlock()
	defer func() {
		t.rt.mu.Lock()
		delete(t.rt.running, id)
		t.rt.mu.Unlock()
	}()

	started, _ := json.Marshal(backend.ProcessStartedPayload{Cwd: cwd, Fresh: true})
	if !t.emit(ctx, backend.Event{Type: backend.EventProcessStarted, Raw: started}) {
		t.killAndWait()
		<-errDone
		<-discardDone
		return
	}

	// Tail the transcript, emitting each appended record until the child exits.
	usagePct := t.tail(ctx, path)

	<-t.waitDone
	t.mu.Lock()
	waitErr := t.waitErr
	t.mu.Unlock()
	<-errDone
	<-discardDone
	if waitErr != nil && ctx.Err() == nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		t.emitErr("kiro turn failed: " + msg)
	}

	if usagePct > 0 {
		rt := core.SessionRuntime{
			Model:         t.modelOf(id),
			ContextWindow: t.rt.models.ContextWindow(ctx, t.modelOf(id)),
		}
		rt.ContextTokens = int64(usagePct / 100 * float64(rt.ContextWindow))
		raw, _ := json.Marshal(rt)
		if !t.emit(ctx, backend.Event{Type: backend.EventRuntime, Raw: raw}) {
			return
		}
	}
	exited, _ := json.Marshal(map[string]any{"code": exitCode(waitErr)})
	t.emit(ctx, backend.Event{Type: backend.EventProcessExit, Raw: exited})
}

// modelOf reads the session's model from session.json (kiro picks the default
// when none was passed on the command line).
func (t *turn) modelOf(id string) string {
	p := t.rt.locate(id)
	if p == "" {
		return ""
	}
	s, err := readSessionJSON(filepath.Dir(p))
	if err != nil {
		return ""
	}
	return s.ModelID
}

// tail polls messages.jsonl for growth and emits each complete new line.
// Returns the last contextUsage percentage seen (0 when none).
func (t *turn) tail(ctx context.Context, path string) float64 {
	var offset int64
	var usagePct float64
	var pending []byte
	for {
		select {
		case <-ctx.Done():
			return usagePct
		default:
		}
		if path == "" {
			// Transcript not written yet (still booting); keep waiting.
			t.mu.Lock()
			path = t.path
			t.mu.Unlock()
			if path == "" {
				if t.exited() {
					return usagePct
				}
				if !sleepOrDone(ctx, 200*time.Millisecond) {
					return usagePct
				}
				continue
			}
		}
		f, err := os.Open(path)
		if err != nil {
			if t.exited() {
				return usagePct
			}
			if !sleepOrDone(ctx, 200*time.Millisecond) {
				return usagePct
			}
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err == nil {
			buf, _ := io.ReadAll(f)
			pending = append(pending, buf...)
			offset += int64(len(buf))
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := bytes.TrimSpace(pending[:i])
				pending = pending[i+1:]
				if len(line) == 0 {
					continue
				}
				if pct := contextUsagePct(line); pct > 0 {
					usagePct = pct
				}
				if !t.emit(ctx, backend.Event{Type: "kiro", Raw: append([]byte(nil), line...)}) {
					f.Close()
					return usagePct
				}
			}
		}
		f.Close()
		if t.exited() {
			// One final drain pass after process exit picks up the last flush.
			if drained := t.drainOnce(ctx, path, &offset, &pending, &usagePct); drained {
				return usagePct
			}
		}
		if !sleepOrDone(ctx, 150*time.Millisecond) {
			return usagePct
		}
	}
}

// drainOnce reads any bytes appended since the last poll; reports false while
// it consumed something (caller should poll again to confirm quiescence).
func (t *turn) drainOnce(ctx context.Context, path string, offset *int64, pending *[]byte, usagePct *float64) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	if _, err := f.Seek(*offset, io.SeekStart); err != nil {
		return true
	}
	buf, _ := io.ReadAll(f)
	if len(buf) == 0 {
		return true
	}
	*offset += int64(len(buf))
	*pending = append(*pending, buf...)
	for {
		i := bytes.IndexByte(*pending, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace((*pending)[:i])
		*pending = (*pending)[i+1:]
		if len(line) == 0 {
			continue
		}
		if pct := contextUsagePct(line); pct > 0 {
			*usagePct = pct
		}
		if !t.emit(ctx, backend.Event{Type: "kiro", Raw: append([]byte(nil), line...)}) {
			return true
		}
	}
	return false
}

// exited reports whether the child process has finished (Wait returned).
func (t *turn) exited() bool {
	select {
	case <-t.waitDone:
		return true
	default:
		return false
	}
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

// killAndWait kills the child and blocks until the run's Wait goroutine
// observes the exit (calling cmd.Wait twice is an error, so wait on waitDone).
func (t *turn) killAndWait() {
	_ = t.cmd.Process.Kill()
	<-t.waitDone
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

// Interrupt cancels the in-flight turn; headless chat has no softer interrupt.
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

// Locate finds the messages.jsonl for id under root, "" when absent.
func Locate(root, id string) string {
	if root == "" || id == "" {
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

// contextUsagePct extracts session_metadata contextUsage (a percentage).
func contextUsagePct(line []byte) float64 {
	var e struct {
		Payload struct {
			Type  string `json:"type"`
			Key   string `json:"key"`
			Value struct {
				UsagePercentage float64 `json:"usagePercentage"`
			} `json:"value"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &e) != nil {
		return 0
	}
	if e.Payload.Type == "session_metadata" && e.Payload.Key == "contextUsage" {
		return e.Payload.Value.UsagePercentage
	}
	return 0
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
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
