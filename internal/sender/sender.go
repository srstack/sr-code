// Package sender drives headless Claude and Codex sessions and reports each
// turn's output by tailing the backend's session log.
package sender

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/appserver"
	"github.com/nexustar/usher/internal/claudestream"
	"github.com/nexustar/usher/internal/codexrollout"
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

// timing groups the tunable delays for driving the TUI. Defaults are set in
// New; tests override them for speed.
type timing struct{ confirm, poll time.Duration }

type Sender struct {
	app          *appserver.Manager // non-nil for the headless Codex backend
	claude       *claudestream.Manager
	preAssignsID bool
	locateFn     func(string) string
	logger       *slog.Logger
	t            timing
	tail         tailConfig
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
// unique id); socket is retained for configuration compatibility; hookSock
// routes permission hooks back to this instance; maxLive caps Claude workers.
func New(claudeCmd, permissionMode, projectsDir, socket, hookSock string, maxLive int, injectMCPTools bool, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	_ = socket // retained for CLI/config compatibility
	var extra []string
	if permissionMode != "" {
		extra = []string{"--permission-mode", permissionMode}
	}
	// Register the show_image MCP server (unless --disable-usher-tools). Additive
	// — no --strict-mcp-config — so the user's own MCP servers are untouched.
	var mcpArgs []string
	if injectMCPTools {
		mcpArgs = claudeMCPConfigArgs(hookSock, logger)
		extra = append(extra, mcpArgs...)
	}
	t := timing{confirm: 8 * time.Second, poll: 150 * time.Millisecond}
	return &Sender{
		claude:       claudestream.New(claudeCmd, claudeHookSettings(hookSock, logger), hookSock, extra, maxLive, logger),
		preAssignsID: true,
		locateFn:     func(id string) string { return locateClaude(projectsDir, id) },
		logger:       logger,
		t:            t,
		tail:         tailConfig{poll: 150 * time.Millisecond, appearWait: 20 * time.Second, turnComplete: isTurnComplete},
	}
}

func claudeHookSettings(hookSock string, logger *slog.Logger) string {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.Abs(exe)
	}
	if err != nil {
		logger.Warn("claude hook: cannot resolve usher executable", "err", err)
		return ""
	}
	hookCommand := func(event string) string {
		cmd := exe + " hook " + event
		if hookSock != "" {
			cmd = "USHER_HOOK_SOCK=" + hookSock + " " + cmd
		}
		return cmd
	}
	handler := func(event string) []any {
		return []any{map[string]any{
			"type": "command", "command": hookCommand(event), "timeout": 604800,
		}}
	}
	settings := map[string]any{
		"hooks": map[string]any{
			// Let Claude's native permission policy settle safe operations and
			// route only prompts it would otherwise show to a terminal through
			// usher. AskUserQuestion still needs PreToolUse so the web UI can
			// collect an answer and return it as updatedInput under -p.
			"PermissionRequest": []any{map[string]any{"matcher": "*", "hooks": handler("PermissionRequest")}},
			"PreToolUse":        []any{map[string]any{"matcher": "AskUserQuestion", "hooks": handler("PreToolUse")}},
		},
	}
	b, _ := json.Marshal(settings)
	return string(b)
}

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
		"mcp_servers.usher.command":  exe,
		"mcp_servers.usher.args":     []string{"mcp-stdio"},
		"mcp_servers.usher.env_vars": []string{"USHER_HOOK_SOCK"},
		// Codex's default callable MCP namespace keeps the legacy mcp__ prefix;
		// the unprefixed form covers installations with that feature enabled.
		"code_mode.direct_only_tool_namespaces": []string{"mcp__usher", "usher"},
	}
}

// NewCodex builds a Sender that drives Codex through per-session app-server
// workers.
// codexCmd is the codex binary; sessionsDir is ~/.codex/sessions (the rollout
// root, used to locate logs); sandboxArgs are extra codex flags (e.g.
// --sandbox workspace-write); hookSock, if set, routes the codex permission hook
// back to this instance. maxLive caps Codex workers; idle workers are shut down
// and cold-resumed on the next send. Codex assigns ids for new threads.
func NewCodex(codexCmd, sessionsDir, socket, hookSock string, sandboxArgs []string, maxLive int, injectMCPTools bool, hooks *hook.Manager, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	_ = socket // retained for CLI/config compatibility
	var env []string
	if hookSock != "" {
		env = append(env, "USHER_HOOK_SOCK="+hookSock)
	}
	t := timing{confirm: 8 * time.Second, poll: 150 * time.Millisecond}
	appConfig := map[string]any{}
	if injectMCPTools {
		appConfig = codexMCPConfig(logger)
	}
	sandbox, config := codexHeadlessParams(sandboxArgs, logger)
	for k, v := range appConfig {
		config[k] = v
	}
	return &Sender{
		app:          appserver.NewManager(codexCmd, hooks, sandbox, config, env, maxLive, logger),
		preAssignsID: false,
		locateFn:     func(id string) string { return locateCodex(sessionsDir, id) },
		logger:       logger,
		t:            t,
		tail:         tailConfig{poll: 150 * time.Millisecond, appearWait: 20 * time.Second, turnComplete: codexrollout.IsTurnComplete, turnAborted: codexrollout.IsTurnAborted},
	}
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

// Send injects prompt into the session's live interactive claude (resuming /
// spawning it as needed) and streams the resulting turn's events. The channel
// closes when the turn ends or ctx is cancelled.
func (s *Sender) Send(ctx context.Context, sessionID, prompt, cwd string) (<-chan StreamEvent, error) {
	if s.app != nil {
		return s.appTurn(ctx, sessionID, prompt, cwd, false)
	}
	if s.claude != nil {
		return s.claudeTurn(ctx, sessionID, prompt, cwd, "", true)
	}
	return nil, errors.New("sender has no headless backend")
}

// SendNew is like Send but starts a brand-new session with the given id
// (`--session-id`). The jsonl is created lazily once claude writes the first
// turn; the tailer waits for it to appear.
func (s *Sender) SendNew(ctx context.Context, sessionID, prompt, cwd, model string) (<-chan StreamEvent, error) {
	if s.claude != nil {
		return s.claudeTurn(ctx, sessionID, prompt, cwd, model, false)
	}
	return nil, errors.New("new sessions are unsupported by this backend")
}

// PreAssignsID reports whether usher picks a new session's id up front (Claude,
// via --session-id) or the backend assigns its own to be discovered after spawn
// (Codex). The router uses it to choose the new-session path.
func (s *Sender) PreAssignsID() bool { return s.preAssignsID }

// StartCodexSession spawns a brand-new session whose id the backend assigns
// itself (Codex has no --session-id flag). It spawns under the temporary window
// handle tempID, gets the TUI ready, injects prompt, discovers the id the
// backend just wrote, renames the window to it, and returns that real id with
// the turn's event stream. It blocks only until the id is known (the session log
// is flushed at start, so this is quick); the turn then streams in the returned
// channel. Callers gate on PreAssignsID()==false.
func (s *Sender) StartCodexSession(ctx context.Context, tempID, prompt, cwd, model string, discoverTimeout time.Duration) (string, <-chan StreamEvent, error) {
	_ = tempID
	_ = discoverTimeout
	if s.app == nil {
		return "", nil, errors.New("Codex app-server is unavailable")
	}
	id, err := s.app.StartThread(ctx, cwd, model)
	if err != nil {
		return "", nil, err
	}
	ch, err := s.appTurn(ctx, id, prompt, cwd, true)
	return id, ch, err
}

// Has reports whether usher currently holds a live interactive process for
// sessionID.
func (s *Sender) Has(sessionID string) bool {
	if s.app != nil {
		return s.app.Has(sessionID)
	}
	if s.claude != nil {
		return s.claude.Has(sessionID)
	}
	return false
}

// LiveSessions returns the ids of sessions with a live backend worker.
func (s *Sender) LiveSessions() []string {
	if s.app != nil {
		return s.app.LiveSessions()
	}
	if s.claude != nil {
		return s.claude.LiveSessions()
	}
	return nil
}

// Interrupt stops the in-flight turn for sessionID without killing its worker.
func (s *Sender) Interrupt(sessionID string) error {
	if s.app != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.app.Interrupt(ctx, sessionID)
	}
	if s.claude != nil {
		return s.claude.Interrupt(sessionID)
	}
	return nil
}

// Kill tears down usher's live worker for sessionID, if any.
func (s *Sender) Kill(sessionID string) error {
	if s.app != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.app.Kill(ctx, sessionID)
	}
	if s.claude != nil {
		return s.claude.Kill(sessionID)
	}
	return nil
}

// Shutdown tears down all backend workers.
func (s *Sender) Shutdown() {
	if s.app != nil {
		s.app.Shutdown()
		return
	}
	if s.claude != nil {
		s.claude.Shutdown()
		return
	}
}

func (s *Sender) claudeTurn(ctx context.Context, id, prompt, cwd, model string, resume bool) (<-chan StreamEvent, error) {
	path := s.locate(id)
	var offset int64
	if path != "" {
		if fi, e := os.Stat(path); e == nil {
			offset = fi.Size()
		}
	}
	done, deltas, fresh, queuedAhead, err := s.claude.Send(ctx, id, prompt, cwd, model, resume)
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
			emitError(ctx, out, "claude session jsonl did not appear after prompt")
			return
		}
		tailCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		cfg := s.tail
		cfg.skipCompletions = queuedAhead
		events := tailTurn(tailCtx, path, offset, s.logger, cfg)
		for {
			select {
			case delta, ok := <-deltas:
				if !ok {
					deltas = nil
					continue
				}
				if !emitLiveDelta(ctx, out, "text", delta.Text) {
					return
				}
			case ev, ok := <-events:
				if !ok {
					select {
					case result := <-done:
						emitClaudeRuntime(ctx, out, result)
						if result.IsError && result.Subtype != "error_during_execution" {
							emitError(ctx, out, "claude turn failed: "+result.Subtype)
						}
					case <-time.After(3 * time.Second):
						s.logger.Warn("claude turn finalized from jsonl backstop", "session_id", id)
					case <-ctx.Done():
					}
					return
				}
				if !sendEvent(ctx, out, ev) {
					return
				}
			case result := <-done:
				if result.IsError && result.Subtype != "error_during_execution" {
					emitError(ctx, out, "claude turn failed: "+result.Subtype)
				}
				// The protocol result can beat the tailer's next file poll even
				// though the final assistant/tool lines are already on disk. Let
				// the log reach its completion marker before cancelling it.
				drainTail(ctx, out, events, cancel, 3*time.Second, s.logger,
					"claude rollout drain timed out", "session_id", id)
				emitClaudeRuntime(ctx, out, result)
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func emitClaudeRuntime(ctx context.Context, out chan<- StreamEvent, result claudestream.Result) bool {
	if result.ContextWindow <= 0 {
		return true
	}
	raw, _ := json.Marshal(map[string]any{
		"model":          result.Model,
		"context_window": result.ContextWindow,
	})
	return sendEvent(ctx, out, StreamEvent{Type: "session.runtime", Raw: raw})
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
	done, deltas, err := s.app.StartTurn(ctx, id, prompt, cwd)
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
		events := tailTurn(tailCtx, path, offset, s.logger, s.tail)
		lastKind := ""
		for {
			select {
			case delta, ok := <-deltas:
				if !ok {
					deltas = nil
					continue
				}
				if delta.Kind == "reasoning" && lastKind == "reasoning" {
					continue // emit "thinking" once per reasoning stretch
				}
				lastKind = delta.Kind
				if !emitLiveDelta(ctx, out, delta.Kind, delta.Text) {
					return
				}
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
				drainTail(ctx, out, events, cancel, 5*time.Second, s.logger,
					"codex rollout drain timed out", "thread_id", id)
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func emitLiveDelta(ctx context.Context, out chan<- StreamEvent, kind, delta string) bool {
	if delta == "" {
		return true
	}
	typ, payload := "part.delta", map[string]string{"delta": delta}
	if kind == "reasoning" {
		typ, payload = "turn.status", map[string]string{"status": "thinking"}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return true
	}
	return sendEvent(ctx, out, StreamEvent{Type: typ, Raw: raw})
}

// drainTail lets a completed backend protocol flush the final log records into
// the live event stream. Protocol completion and file visibility are separate
// clocks; cancelling the tail immediately can strand already-written parts
// until the browser next reloads the transcript.
func drainTail(ctx context.Context, out chan<- StreamEvent, events <-chan StreamEvent,
	cancel context.CancelFunc, timeout time.Duration, logger *slog.Logger, msg, key, id string) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			if !sendEvent(ctx, out, ev) {
				cancel()
				return
			}
		case <-timer.C:
			logger.Warn(msg, key, id)
			cancel()
			for ev := range events {
				if !sendEvent(ctx, out, ev) {
					return
				}
			}
			return
		case <-ctx.Done():
			cancel()
			return
		}
	}
}

func locateClaude(root, id string) string {
	matches, _ := filepath.Glob(filepath.Join(root, "*", id+".jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func locateCodex(root, id string) string {
	matches, _ := filepath.Glob(filepath.Join(root, "*", "*", "*", "rollout-*-"+id+".jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func (s *Sender) locate(sessionID string) string {
	if s.locateFn == nil {
		return ""
	}
	return s.locateFn(sessionID)
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

func emitError(ctx context.Context, out chan<- StreamEvent, msg string) {
	raw, _ := json.Marshal(map[string]string{"message": msg})
	sendEvent(ctx, out, StreamEvent{Type: "error", Raw: raw})
}
