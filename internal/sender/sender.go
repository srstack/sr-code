// Package sender drives one long-lived interactive `claude` process per
// session — spawned in a window of a dedicated tmux server socket — and
// reports each turn's output by tailing the session's jsonl file.
//
// This replaces the original headless `claude -p --resume` design: `-p` is
// moving to metered billing, whereas interactive claude runs under the user's
// Claude subscription. Discovery and transcript reads stay jsonl-based; only
// the send path changed. See the pool (process lifecycle) and tailTurn (turn
// streaming + end-of-turn detection) for the two halves.
//
// Concurrency note: usher keeps ONE process per session and serializes sends
// at the router. If the user also has the same session open elsewhere (their
// IDE), interactive resume forks the jsonl into a tree — a known corner case
// usher does not handle in v0.x (linear tail only).
package sender

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/appserver"
	"github.com/nexustar/usher/internal/hook"
)

// StreamEvent is one event for a turn. Type is the jsonl line's "type"
// (e.g. "user", "assistant", "system"), or one of the synthesized values
// "subprocess.started" / "subprocess.exit" / "error". The names are kept from
// the headless era so the broker/web layer needs minimal change; the payloads
// are now whole jsonl lines (message granularity), not stream-json token
// deltas.
type StreamEvent struct {
	Type string
	Raw  json.RawMessage
}

var ErrHeadless = errors.New("terminal mirror is unavailable for headless sessions")

// timing groups the tunable delays for driving the TUI. Defaults are set in
// New; tests override them for speed.
type timing struct {
	spawnSettle   time.Duration // after spawning a fresh window, before first keystroke (claude boot)
	trustToInject time.Duration // after the trust-accept Enter, before pasting the prompt
	warmSettle    time.Duration // before pasting into an already-running window
	resumeReady   time.Duration // max wait for a fresh resume's input box (incl. answering the resume chooser)
	confirm       time.Duration // how long to wait for a brand-new session's jsonl to appear
	poll          time.Duration // pane/file poll interval
}

type Sender struct {
	app         *appserver.Client // non-nil for the headless Codex backend
	pool        *pool
	backend     backend
	projectsDir string
	logger      *slog.Logger
	t           timing
	tail        tailConfig
	leftovers   *idlePaneStore // frames left by turnless '/'-command sends (see tailCfg)

	// busy holds session ids with an in-flight turn. The pool consults it
	// (via isBusy) so LRU eviction never kills a session mid-turn. Marked
	// synchronously in run() before the turn goroutine starts and cleared when
	// that goroutine exits (any path), so the flag can't leak.
	busyMu sync.Mutex
	busy   map[string]struct{}

	// cwdLocks serialize new Codex-session creation per cwd so two concurrent
	// same-cwd creates can't mis-identify each other's rollout in discoverNewID.
	cwdLocksMu sync.Mutex
	cwdLocks   map[string]*sync.Mutex
}

// lockCwd acquires the per-cwd creation lock and returns its unlock func.
func (s *Sender) lockCwd(cwd string) func() {
	s.cwdLocksMu.Lock()
	if s.cwdLocks == nil {
		s.cwdLocks = map[string]*sync.Mutex{}
	}
	m := s.cwdLocks[cwd]
	if m == nil {
		m = &sync.Mutex{}
		s.cwdLocks[cwd] = m
	}
	s.cwdLocksMu.Unlock()
	m.Lock()
	return m.Unlock
}

// claudeMCPConfigArgs writes an MCP config registering `usher mcp-stdio` next to
// the hook socket and returns the `--mcp-config` flags to load it. Returns nil
// (disabling the feature, not erroring) if the executable can't be resolved, so
// a write hiccup never blocks spawns.
func claudeMCPConfigArgs(hookSock string, logger *slog.Logger) []string {
	if hookSock == "" {
		return nil
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.Abs(exe)
	}
	if err != nil {
		logger.Warn("mcp config: cannot resolve usher executable; show_image disabled", "err", err)
		return nil
	}
	// alwaysLoad exempts the server from Claude Code's Tool Search deferral so the
	// tool is always loaded, not hidden behind a ToolSearch step.
	cfg := map[string]any{"mcpServers": map[string]any{
		"usher": map[string]any{"command": exe, "args": []string{"mcp-stdio"}, "alwaysLoad": true},
	}}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil
	}
	path := filepath.Join(filepath.Dir(hookSock), "mcp.json")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		logger.Warn("mcp config: write failed; show_image disabled", "path", path, "err", err)
		return nil
	}
	return []string{"--mcp-config", path}
}

// New builds a Sender. claudeCmd is the claude binary; permissionMode (if
// non-empty) is passed through as --permission-mode; projectsDir is Claude
// Code's projects root (used to locate session jsonl files by their globally
// unique id); socket is the dedicated tmux server socket name; hookSock, if
// non-empty, is set as USHER_HOOK_SOCK on spawned claude processes so their
// permission hooks route back to this instance; maxLive caps concurrent live
// processes (LRU-evicted beyond it).
func New(claudeCmd, permissionMode, projectsDir, socket, hookSock string, maxLive int, injectMCPTools bool, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	if socket == "" {
		socket = tmuxSessionName
	}
	var extra []string
	if permissionMode != "" {
		extra = []string{"--permission-mode", permissionMode}
	}
	// Suppress claude's post-turn feedback survey: left on the pane it eats the
	// next send's keystrokes. Disabling at spawn beats per-send detection.
	env := []string{"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1"}
	if hookSock != "" {
		env = append(env, "USHER_HOOK_SOCK="+hookSock)
	}
	// Register the show_image MCP server (unless --disable-usher-tools). Additive
	// — no --strict-mcp-config — so the user's own MCP servers are untouched.
	if injectMCPTools {
		extra = append(extra, claudeMCPConfigArgs(hookSock, logger)...)
	}
	runner := execRunner{bin: "tmux", socket: socket}
	t := timing{
		spawnSettle:   5 * time.Second,
		trustToInject: 1500 * time.Millisecond,
		warmSettle:    400 * time.Millisecond,
		resumeReady:   8 * time.Second,
		confirm:       8 * time.Second,
		poll:          150 * time.Millisecond,
	}
	p := newPool(runner, claudeCmd, extra, env, maxLive, logger)
	lp := &idlePaneStore{}
	b := claudeBackend{p: p, t: t, projectsDir: projectsDir, claudeCmd: claudeCmd, extraArgs: extra, leftovers: lp}
	s := &Sender{
		pool:        p,
		backend:     b,
		projectsDir: projectsDir,
		logger:      logger,
		busy:        make(map[string]struct{}),
		t:           t,
		tail:        tailConfig{poll: 150 * time.Millisecond, appearWait: 20 * time.Second, turnComplete: b.turnComplete, turnActivity: b.turnActivity, turnAborted: b.turnAborted},
		leftovers:   lp,
	}
	s.pool.isBusy = s.isBusy
	return s
}

// codexMCPConfigArgs returns the `-c` override VALUES registering the show_image
// MCP server on a codex spawn — command, args, and env_vars (which both delivers
// and gates USHER_HOOK_SOCK, since codex doesn't forward env to MCP children).
// Values are TOML (exe as a basic string, arrays as TOML arrays); each is later
// emitted as `-c <shellQuote(value)>`. Returns nil if the exe can't be resolved.
func codexMCPConfig(logger *slog.Logger) map[string]any {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.Abs(exe)
	}
	if err != nil {
		logger.Warn("codex mcp: cannot resolve usher executable; show_image disabled", "err", err)
		return nil
	}
	return map[string]any{
		"mcp_servers.usher.command":             exe,
		"mcp_servers.usher.args":                []string{"mcp-stdio"},
		"mcp_servers.usher.env_vars":            []string{"USHER_HOOK_SOCK"},
		"code_mode.direct_only_tool_namespaces": []string{"usher"},
	}
}

// codexMCPConfigArgs derives the legacy TUI -c overrides from the same native
// values used by app-server.
func codexMCPConfigArgs(logger *slog.Logger) []string {
	cfg := codexMCPConfig(logger)
	if cfg == nil {
		return nil
	}
	exe := cfg["mcp_servers.usher.command"].(string)
	tomlExe := strings.ReplaceAll(exe, `\`, `\\`)
	tomlExe = strings.ReplaceAll(tomlExe, `"`, `\"`)
	return []string{
		`mcp_servers.usher.command="` + tomlExe + `"`,
		`mcp_servers.usher.args=["mcp-stdio"]`,
		`mcp_servers.usher.env_vars=["USHER_HOOK_SOCK"]`,
		`code_mode.direct_only_tool_namespaces=["usher"]`,
	}
}

// NewCodex builds a Sender that drives interactive `codex` instead of `claude`.
// codexCmd is the codex binary; sessionsDir is ~/.codex/sessions (the rollout
// root, used to locate logs); sandboxArgs are extra codex flags (e.g.
// --sandbox workspace-write); hookSock, if set, routes the codex permission hook
// back to this instance; maxLive caps live processes.
//
// Resume goes straight in (`codex resume <id>`, no chooser); a brand-new session
// (Codex assigns its own id) is created via StartCodexSession.
func NewCodex(codexCmd, sessionsDir, socket, hookSock string, sandboxArgs []string, maxLive int, injectMCPTools bool, hooks *hook.Manager, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	if socket == "" {
		socket = tmuxSessionName
	}
	var env []string
	if hookSock != "" {
		env = append(env, "USHER_HOOK_SOCK="+hookSock)
	}
	runner := execRunner{bin: "tmux", socket: socket}
	t := timing{
		spawnSettle:   5 * time.Second,
		trustToInject: 1500 * time.Millisecond,
		warmSettle:    400 * time.Millisecond,
		resumeReady:   8 * time.Second,
		confirm:       8 * time.Second,
		poll:          150 * time.Millisecond,
	}
	var mcpConf []string
	if injectMCPTools && hookSock != "" {
		mcpConf = codexMCPConfigArgs(logger)
	}
	p := newPool(runner, codexCmd, nil, env, maxLive, logger)
	b := codexBackend{p: p, t: t, codexCmd: codexCmd, sessionsDir: sessionsDir, extraArgs: sandboxArgs, mcpConfArgs: mcpConf}
	// Codex's command differs from the Claude default; route spawn through it.
	p.spawnOverride = b.spawnCommand
	appConfig := map[string]any{}
	if injectMCPTools {
		appConfig = codexMCPConfig(logger)
	}
	sandbox, config := codexHeadlessParams(sandboxArgs, logger)
	for k, v := range appConfig {
		config[k] = v
	}
	s := &Sender{
		app:         appserver.New(codexCmd, hooks, sandbox, config, env, logger),
		pool:        p,
		backend:     b,
		projectsDir: sessionsDir,
		logger:      logger,
		busy:        make(map[string]struct{}),
		t:           t,
		tail:        tailConfig{poll: 150 * time.Millisecond, appearWait: 20 * time.Second, turnComplete: b.turnComplete, turnActivity: b.turnActivity, turnAborted: b.turnAborted},
	}
	s.pool.isBusy = s.isBusy
	return s
}

func codexHeadlessParams(args []string, logger *slog.Logger) (map[string]any, map[string]any) {
	p, cfg := map[string]any{}, map[string]any{}
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--sandbox" || args[i] == "-s") && i+1 < len(args):
			p["sandbox"] = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--sandbox="):
			p["sandbox"] = strings.TrimPrefix(args[i], "--sandbox=")
		case args[i] == "-c" && i+1 < len(args):
			kv := strings.SplitN(args[i+1], "=", 2)
			if len(kv) == 2 {
				cfg[kv[0]] = codexConfigValue(kv[1])
			} else {
				logger.Warn("headless codex: invalid -c override", "value", args[i+1])
			}
			i++
		default:
			logger.Warn("headless codex: unsupported --codex-args option", "option", args[i])
		}
	}
	return p, cfg
}

// Codex's common TOML literals (strings, booleans, numbers and arrays) are
// also valid JSON. Preserve bare TOML words as strings.
func codexConfigValue(raw string) any {
	var v any
	if json.Unmarshal([]byte(raw), &v) == nil {
		return v
	}
	return raw
}

// isBusy reports whether sessionID has an in-flight turn (the pool's eviction
// guard). markBusy/clearBusy bracket a tracked turn in run().
func (s *Sender) isBusy(sessionID string) bool {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	_, ok := s.busy[sessionID]
	return ok
}

func (s *Sender) markBusy(sessionID string) {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if s.busy == nil {
		s.busy = make(map[string]struct{})
	}
	s.busy[sessionID] = struct{}{}
}

func (s *Sender) clearBusy(sessionID string) {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	delete(s.busy, sessionID)
}

// Send injects prompt into the session's live interactive claude (resuming /
// spawning it as needed) and streams the resulting turn's events. The channel
// closes when the turn ends or ctx is cancelled.
func (s *Sender) Send(ctx context.Context, sessionID, prompt, cwd string) (<-chan StreamEvent, error) {
	if s.app != nil {
		return s.appTurn(ctx, sessionID, prompt, cwd, false)
	}
	// Resumes keep their original model, so no model is threaded here.
	return s.run(ctx, sessionID, prompt, cwd, "", true)
}

// SendNew is like Send but starts a brand-new session with the given id
// (`--session-id`). The jsonl is created lazily once claude writes the first
// turn; the tailer waits for it to appear.
func (s *Sender) SendNew(ctx context.Context, sessionID, prompt, cwd, model string) (<-chan StreamEvent, error) {
	return s.run(ctx, sessionID, prompt, cwd, model, false)
}

// PreAssignsID reports whether usher picks a new session's id up front (Claude,
// via --session-id) or the backend assigns its own to be discovered after spawn
// (Codex). The router uses it to choose the new-session path.
func (s *Sender) PreAssignsID() bool { return s.backend.preAssignsID() }

// StartCodexSession spawns a brand-new session whose id the backend assigns
// itself (Codex has no --session-id flag). It spawns under the temporary window
// handle tempID, gets the TUI ready, injects prompt, discovers the id the
// backend just wrote, renames the window to it, and returns that real id with
// the turn's event stream. It blocks only until the id is known (the session log
// is flushed at start, so this is quick); the turn then streams in the returned
// channel. Callers gate on PreAssignsID()==false.
func (s *Sender) StartCodexSession(ctx context.Context, tempID, prompt, cwd, model string, discoverTimeout time.Duration) (string, <-chan StreamEvent, error) {
	if s.app != nil {
		id, err := s.app.StartThread(ctx, cwd, model)
		if err != nil {
			return "", nil, err
		}
		ch, err := s.appTurn(ctx, id, prompt, cwd, true)
		return id, ch, err
	}
	// Serialize same-cwd creates across snapshot→spawn→discover (released on
	// return, before the tail goroutine, which runs unlocked).
	defer s.lockCwd(cwd)()

	known := s.backend.knownSessionIDs()
	fresh, err := s.pool.ensure(tempID, cwd, model, false)
	if err != nil {
		return "", nil, err
	}
	s.markBusy(tempID)

	if err := s.backend.waitReady(ctx, tempID, cwd, fresh, false); err != nil {
		s.clearBusy(tempID)
		return "", nil, err
	}
	if err := s.pool.inject(tempID, prompt); err != nil {
		s.clearBusy(tempID)
		return "", nil, err
	}

	realID := s.discoverWait(ctx, cwd, known, discoverTimeout)
	if realID == "" {
		s.clearBusy(tempID)
		return "", nil, errors.New("codex did not create a session log after the prompt")
	}
	if err := s.pool.rename(tempID, realID); err != nil {
		s.logger.Warn("rename codex window", "from", tempID, "to", realID, "err", err)
	}
	// The window is now named realID; move the eviction guard with it.
	s.clearBusy(tempID)
	s.markBusy(realID)

	path := s.backend.locate(realID)
	out := make(chan StreamEvent, 64)
	go func() {
		defer close(out)
		defer s.clearBusy(realID)
		defer func() {
			if ctx.Err() != nil {
				emitTerminalExit(out)
			}
		}()
		started, _ := json.Marshal(struct {
			Cwd   string `json:"cwd"`
			Fresh bool   `json:"fresh"`
		}{cwd, true})
		if !sendEvent(ctx, out, StreamEvent{Type: "subprocess.started", Raw: started}) {
			return
		}
		if path == "" {
			emitError(ctx, out, "codex session log not found: "+realID)
			return
		}
		// Offset 0: a brand-new rollout — tail the whole first turn from the top.
		// No idle-frame recording: only the claude backend consumes it.
		for ev := range tailTurn(ctx, path, 0, s.logger, s.tailCfg(realID, false)) {
			if !sendEvent(ctx, out, ev) {
				return
			}
		}
	}()
	return realID, out, nil
}

// discoverWait polls the backend for the id of the session just spawned in cwd
// (the newest log not in the pre-spawn snapshot), until it appears or the
// deadline/ctx fires.
func (s *Sender) discoverWait(ctx context.Context, cwd string, known map[string]bool, timeout time.Duration) string {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(s.t.poll)
	defer ticker.Stop()
	for {
		if id := s.backend.discoverNewID(cwd, known); id != "" {
			return id
		}
		select {
		case <-ctx.Done():
			return ""
		case <-deadline.C:
			return ""
		case <-ticker.C:
		}
	}
}

// Inject readies the session's window (resuming if cold) and pastes prompt
// without tailing a turn — for input that drives the TUI directly instead of
// starting a model turn (a leading '!', claude's own bash mode), which logs no
// turn_duration and would otherwise wedge the tailer. Never starts a session.
func (s *Sender) Inject(ctx context.Context, sessionID, prompt, cwd string) error {
	if s.app != nil {
		_, err := s.app.StartTurn(ctx, sessionID, prompt, cwd)
		return err
	}
	fresh, err := s.pool.ensure(sessionID, cwd, "", true)
	if err != nil {
		return err
	}
	if err := s.backend.waitReady(ctx, sessionID, cwd, fresh, true); err != nil {
		return err
	}
	return s.pool.inject(sessionID, prompt)
}

// Has reports whether usher currently holds a live interactive process for
// sessionID.
func (s *Sender) Has(sessionID string) bool {
	if s.app != nil {
		return s.app.Has(sessionID)
	}
	return s.pool.has(sessionID)
}

// LiveSessions returns the ids of all sessions usher currently holds a live
// interactive process for. One tmux query; use it to decorate session lists.
func (s *Sender) LiveSessions() []string {
	if s.app != nil {
		return s.app.LiveSessions()
	}
	return s.pool.liveSessions()
}

// Interrupt stops the in-flight turn for sessionID without killing the
// process (Ctrl-C into the pane).
func (s *Sender) Interrupt(sessionID string) error {
	if s.app != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.app.Interrupt(ctx, sessionID)
	}
	return s.pool.interrupt(sessionID)
}

// Kill tears down usher's live window for sessionID, if any (its claude
// exits). Used when deleting a session; a no-op when nothing is live.
func (s *Sender) Kill(sessionID string) error {
	if s.app != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.app.Kill(ctx, sessionID)
	}
	s.leftovers.clear(sessionID)
	return s.pool.kill(sessionID)
}

// CapturePane returns the current rendered contents (with colour escapes) of
// the session's interactive pane, for the read-only terminal mirror. Errors
// if usher holds no live window for sessionID.
func (s *Sender) CapturePane(sessionID string) (string, error) {
	if s.app != nil {
		return "", ErrHeadless
	}
	return s.pool.capturePane(sessionID)
}

// SendKeys forwards tmux key names to the session's pane, powering the
// terminal mirror's soft keys.
func (s *Sender) SendKeys(sessionID string, keys ...string) error {
	if s.app != nil {
		return ErrHeadless
	}
	return s.pool.sendKeys(sessionID, keys...)
}

// ResizeCanvas sets the session pane to cols×rows for the terminal mirror (also
// repairs any manual-attach drift). Called when the mirror opens.
func (s *Sender) ResizeCanvas(sessionID string, cols, rows int) error {
	if s.app != nil {
		return ErrHeadless
	}
	return s.pool.resizeCanvas(sessionID, cols, rows)
}

// Shutdown tears down usher's tmux server (all live windows). Call on exit if
// you do NOT want processes to survive for the next usher run.
func (s *Sender) Shutdown() {
	if s.app != nil {
		s.app.Shutdown()
		return
	}
	s.pool.shutdown()
}

// appTurn keeps rollout jsonl as the content plane while app-server supplies
// the driving and terminal lifecycle signal.
func (s *Sender) appTurn(ctx context.Context, id, prompt, cwd string, fresh bool) (<-chan StreamEvent, error) {
	path := s.locate(id)
	var offset int64
	if path != "" {
		if fi, e := os.Stat(path); e == nil {
			offset = fi.Size()
		}
	}
	done, err := s.app.StartTurn(ctx, id, prompt, cwd)
	if err != nil {
		return nil, err
	}
	out := make(chan StreamEvent, 64)
	go func() {
		defer close(out)
		started, _ := json.Marshal(map[string]any{"cwd": cwd, "fresh": fresh})
		if !sendEvent(ctx, out, StreamEvent{Type: "subprocess.started", Raw: started}) {
			return
		}
		if path == "" {
			path = s.locateWait(ctx, id, s.t.confirm)
		}
		if path == "" {
			emitError(ctx, out, "codex session log did not appear after prompt")
			return
		}
		tailCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		events := tailTurn(tailCtx, path, offset, s.logger, s.tailCfg(id, false))
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					// File completion is only a straggler backstop. Give the
					// protocol event a grace period before finalizing without it.
					select {
					case result := <-done:
						if result.Status == "failed" {
							emitError(ctx, out, "codex turn failed")
						}
					case <-time.After(5 * time.Second):
						s.logger.Warn("codex turn finalized from rollout backstop", "thread_id", id)
					case <-ctx.Done():
					}
					return
				}
				if !sendEvent(ctx, out, ev) {
					return
				}
			case result := <-done:
				if result.Status == "failed" {
					emitError(ctx, out, "codex turn failed")
				}
				cancel()
				for ev := range events {
					if !sendEvent(ctx, out, ev) {
						return
					}
				}
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (s *Sender) run(ctx context.Context, sessionID, prompt, cwd, model string, resume bool) (<-chan StreamEvent, error) {
	fresh, err := s.pool.ensure(sessionID, cwd, model, resume)
	if err != nil {
		return nil, err
	}

	// Protect this session from LRU eviction for the duration of the turn.
	// Marked here (not inside the goroutine) so a concurrent ensure for another
	// session can't pick it as a victim in the gap before the goroutine runs.
	s.markBusy(sessionID)

	out := make(chan StreamEvent, 64)
	go func() {
		defer close(out)
		defer s.clearBusy(sessionID)
		// On cancel, guarantee a terminal event even if we returned before the
		// tailer ran (ESC during waitReady) or its exit was dropped in transit.
		defer func() {
			if ctx.Err() != nil {
				emitTerminalExit(out)
			}
		}()

		started, _ := json.Marshal(struct {
			Cwd   string `json:"cwd"`
			Fresh bool   `json:"fresh"`
		}{cwd, fresh})
		if !sendEvent(ctx, out, StreamEvent{Type: "subprocess.started", Raw: started}) {
			return
		}

		// Get the TUI ready to receive the prompt. A timeout here means the box
		// never became injectable: surface it (unless the caller cancelled)
		// instead of blind-pasting into an unknown screen.
		if err := s.backend.waitReady(ctx, sessionID, cwd, fresh, resume); err != nil {
			if ctx.Err() == nil {
				emitError(ctx, out, "session did not become ready: "+err.Error())
			}
			return
		}

		// Resolve the jsonl path and capture the pre-inject size so the tailer
		// reports only this turn. For a brand-new session the file may not
		// exist yet (created on first write); offset stays 0 and we resolve
		// the path after injecting.
		path := s.locate(sessionID)
		var offset int64
		if path != "" {
			if fi, statErr := os.Stat(path); statErr == nil {
				offset = fi.Size()
			}
		}

		// Inject once, then tail — no re-inject-on-miss: the old "did a user line
		// appear in time" oracle false-negatived on a slow flush and
		// double-submitted. A missed paste now shows in the live mirror instead.
		if err := s.pool.inject(sessionID, prompt); err != nil {
			emitError(ctx, out, "inject prompt: "+err.Error())
			return
		}
		if path == "" {
			// Brand-new session: jsonl is created on first write; resolve its
			// path now so the tailer has one (discovery, not a retry).
			if path = s.locateWait(ctx, sessionID, s.t.confirm); path == "" {
				emitError(ctx, out, "session jsonl did not appear after prompt")
				return
			}
		}

		for ev := range tailTurn(ctx, path, offset, s.logger, s.tailCfg(sessionID, strings.HasPrefix(prompt, "/"))) {
			if !sendEvent(ctx, out, ev) {
				return
			}
		}
	}()

	return out, nil
}

// Markers for matching TUI states in a plain pane capture: the resume chooser's
// two option lines and the chooser's arrow. Input-box readiness is NOT a string
// match — it is detected structurally by composerReady, since the footer below
// the box ("? for shortcuts") sits on a line the user can replace with a custom
// statusLine and so can't be relied on.
const (
	resumeChooserMarker = "Resume full session as-is" // the option usher wants
	resumeSummaryMarker = "Resume from summary"       // the highlighted default
	chooserArrow        = "❯"
)

// busyMarker is shown by both TUIs while a turn runs (claude's footer, codex's
// "Working (1s • esc to interrupt)"), matched case-insensitively. It is only a
// supplementary busy vote — the primary signal is pane motion (see tailCfg) —
// so a reworded string degrades margin, not correctness.
const busyMarker = "esc to interrupt"

// paneShowsBusy reports whether a pane capture carries the running-turn hint.
// Only the bottom rows are scanned (both TUIs render it there): a copy of the
// marker in the scrolled transcript must not hold the idle fallback forever.
func paneShowsBusy(text string) bool {
	lines := strings.Split(text, "\n")
	if len(lines) > composerScanLines {
		lines = lines[len(lines)-composerScanLines:]
	}
	return strings.Contains(strings.ToLower(strings.Join(lines, "\n")), busyMarker)
}

// idlePaneStore remembers, per session, the pane frame the idle fallback saw
// when it finalized a turnless '/'-command send — the dialog that command left
// behind. dismissLeftoverDialog may clear the pane only while it still shows
// exactly this frame, so a dialog usher didn't cause (a mirror-started turn's
// permission or question prompt) is never dismissed. A nil store disables both
// recording and dismissal.
type idlePaneStore struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *idlePaneStore) set(id, text string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[id] = text
}

// take returns and removes the recorded frame for id. One-shot: the record
// describes the pane right after one turnless send, so it is valid for exactly
// one check — kept longer, it would match a user manually reopening the same
// (byte-identical) dialog much later.
func (s *idlePaneStore) take(id string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	text, ok := s.m[id]
	delete(s.m, id)
	return text, ok
}

func (s *idlePaneStore) clear(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// chooserArrowOn reports whether the chooser's selection arrow sits on `option`
// — arrow and option text on the SAME line. A loose Contains(option) also
// matches that string replayed in a transcript, which fired keys into the
// prompt box during the boot frames before the input footer rendered.
func chooserArrowOn(text, option string) bool {
	for _, ln := range strings.Split(text, "\n") {
		if strings.Contains(ln, chooserArrow) && strings.Contains(ln, option) {
			return true
		}
	}
	return false
}

const (
	composerScanLines = 10 // search only the bottom N pane lines for the box
	composerBorderRun = 5  // min "─" chars for a line to count as a box border
)

// composerReady reports whether claude's idle input composer is mounted: two
// "─" rules sandwiching the empty "❯" prompt. Preferred over the "? for
// shortcuts" footer, which a custom statusLine can replace.
//
// Trailing blank rows are dropped first — tmux pads empty rows below the
// content, so the composer sits at the bottom of the non-blank region, not the
// grid — then the scan is confined to the last composerScanLines lines so the
// same shape replayed in the transcript can't trip it.
func composerReady(text string) bool {
	lines := strings.Split(text, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > composerScanLines {
		lines = lines[len(lines)-composerScanLines:]
	}
	for i := 0; i+2 < len(lines); i++ {
		if isBoxBorder(lines[i]) && isPromptLine(lines[i+1]) && isBoxBorder(lines[i+2]) {
			return true
		}
	}
	return false
}

// isBoxBorder reports whether a line is a composer rule — at least
// composerBorderRun horizontal box-drawing characters ("─").
func isBoxBorder(line string) bool {
	return strings.Count(line, "─") >= composerBorderRun
}

// isPromptLine reports whether a line is the composer's empty prompt row: just
// "❯" (or ">") once padding and rule/box glyphs are stripped. Strict (no
// trailing text) so a transcript command like "❯ /exit" isn't mistaken for it.
func isPromptLine(line string) bool {
	// Strip padding, the non-breaking space after the prompt (\u00a0, which a
	// plain space misses), and box-side glyphs — but NOT "─", so a transcript
	// line like "❯ ────" stays distinct from the empty prompt.
	s := strings.Trim(line, " \t\u00a0│|╭╮╰╯")
	return s == "❯" || s == ">"
}

// tailCfg returns the per-send tail config: the shared defaults plus a paneBusy
// probe bound to this session's window. Busy = the capture changed since the
// previous probe (a running TUI animates every second, an idle one is static —
// version-independent, unlike marker text) OR it carries busyMarker. The first
// probe only seeds the comparison and reports busy. State lives in the closure:
// one tailCfg per send, called only from that turn's tail goroutine.
//
// recordIdle (set for '/'-prefixed prompts, the only ones that can leave a
// command dialog behind) stores the finalize-time frame in s.leftovers for
// dismissLeftoverDialog to match against on the next send.
func (s *Sender) tailCfg(sessionID string, recordIdle bool) tailConfig {
	cfg := s.tail
	if cfg.maxIdleWait == 0 {
		cfg.maxIdleWait = 30 * time.Second
	}
	start := time.Now()
	var prev string
	var seeded bool
	cfg.paneBusy = func() bool {
		text, err := s.pool.paneText(sessionID)
		if err != nil {
			// Can't see the pane — err toward busy: the worst case is the old
			// wait-forever behavior, never a premature finalize.
			return true
		}
		moved := !seeded || text != prev
		prev, seeded = text, true
		if time.Since(start) >= cfg.maxIdleWait {
			// Motion this long with no log line is a clock/status animation, not
			// a running turn; a really-running TUI still shows busyMarker.
			return paneShowsBusy(text)
		}
		return moved || paneShowsBusy(text)
	}
	if recordIdle {
		cfg.idleReason = "local_command"
		cfg.onIdleExit = func() { s.leftovers.set(sessionID, prev) }
	} else {
		cfg.idleReason = "submission_unconfirmed"
	}
	return cfg
}

// locate finds the session log for sessionID via the active backend.
func (s *Sender) locate(sessionID string) string {
	return s.backend.locate(sessionID)
}

// locateWait polls locate until the file appears or timeout/ctx fires.
func (s *Sender) locateWait(ctx context.Context, sessionID string, timeout time.Duration) string {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(s.t.poll)
	defer ticker.Stop()
	for {
		if p := s.locate(sessionID); p != "" {
			return p
		}
		select {
		case <-ctx.Done():
			return ""
		case <-deadline.C:
			return ""
		case <-ticker.C:
		}
	}
}

// sendEvent delivers ev unless ctx is cancelled. Returns true if delivered.
func sendEvent(ctx context.Context, ch chan<- StreamEvent, ev StreamEvent) bool {
	select {
	case ch <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// emitTerminalExit pushes a synthetic subprocess.exit so a client finalizes the
// turn. It's the cancel-path backstop: a cancelled turn (cancel button / ESC)
// can bail out before the tailer emits its own exit (e.g. during waitReady), or
// drop it while forwarding through a now-cancelled ctx. Uses Background so the
// cancelled turn ctx can't swallow this one too. Safe to call redundantly — the
// web client handles subprocess.exit idempotently.
func emitTerminalExit(out chan<- StreamEvent) {
	sendEvent(context.Background(), out, StreamEvent{Type: "subprocess.exit", Raw: json.RawMessage("{}")})
}

func emitError(ctx context.Context, out chan<- StreamEvent, msg string) {
	raw, _ := json.Marshal(map[string]string{"message": msg})
	sendEvent(ctx, out, StreamEvent{Type: "error", Raw: raw})
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
