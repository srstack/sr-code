package sender

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
)

// pool manages one long-lived interactive `claude` process per session, each
// in its own window of a single tmux session living on a dedicated tmux
// server socket (`tmux -L <socket>`). The dedicated socket isolates usher's
// windows from the user's own tmux and lets them survive usher restarts
// (re-adopted via adopt()). Windows are named by Claude Code session id, so
// the mapping window<->session needs no extra bookkeeping.
//
// Capacity is bounded: at most max live windows, evicted least-recently-used.
// An evicted window's claude exits; the next send to that session re-spawns
// and resumes it (context is reloaded from jsonl on resume).
const tmuxSessionName = "usher"

type tmuxRunner interface {
	// run executes `tmux -L <socket> <args...>` and returns combined output.
	run(args ...string) (string, error)
	// runStdin is run with in fed to the command's stdin. Used by load-buffer
	// to ship a large paste without putting it on the command line, where tmux
	// rejects it as "command too long".
	runStdin(in string, args ...string) (string, error)
}

type execRunner struct {
	bin    string
	socket string
}

func (e execRunner) run(args ...string) (string, error) {
	return e.runStdin("", args...)
}

func (e execRunner) runStdin(in string, args ...string) (string, error) {
	full := append([]string{"-L", e.socket}, args...)
	cmd := exec.Command(e.bin, full...)
	if in != "" {
		cmd.Stdin = strings.NewReader(in)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

type pool struct {
	runner    tmuxRunner
	claudeCmd string
	extraArgs []string // extra claude flags, e.g. ["--permission-mode","default"]
	env       []string // KEY=VAL pairs set on the spawned window's process
	max       int
	logger    *slog.Logger

	mu  sync.Mutex
	lru []string // session ids, least-recently-used first
}

func newPool(runner tmuxRunner, claudeCmd string, extraArgs, env []string, max int, logger *slog.Logger) *pool {
	if max <= 0 {
		max = 8
	}
	if logger == nil {
		logger = slog.Default()
	}
	p := &pool{runner: runner, claudeCmd: claudeCmd, extraArgs: extraArgs, env: env, max: max, logger: logger}
	p.adopt()
	return p
}

// adopt seeds the LRU from windows already alive on the socket (usher
// restarted while its tmux server kept running). Order among adopted windows
// is arbitrary; they are all treated as equally old.
func (p *pool) adopt() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range p.liveWindows() {
		if !contains(p.lru, id) {
			p.lru = append(p.lru, id)
		}
	}
	if len(p.lru) > 0 {
		p.logger.Info("adopted live tmux windows", "count", len(p.lru))
	}
}

// has reports whether a live window exists for sessionID.
func (p *pool) has(sessionID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return contains(p.liveWindows(), sessionID)
}

// liveSessions returns the ids of all sessions with a live window, via a
// single tmux query — cheaper than calling has() per session when decorating
// a whole list.
func (p *pool) liveSessions() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.liveWindows()
}

// ensure guarantees a live interactive claude window for sessionID, spawning
// (and LRU-evicting if at capacity) if necessary, and marks it most-recently
// used. resume selects `--resume <id>` (existing) vs `--session-id <id>`
// (brand new). fresh reports whether a new window was spawned this call (vs a
// warm window reused) — callers use it to decide whether to dismiss the
// first-launch trust prompt and how long to let the TUI settle. Safe to call
// before every send.
func (p *pool) ensure(sessionID, cwd string, resume bool) (fresh bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if contains(p.liveWindows(), sessionID) {
		p.touch(sessionID)
		return false, nil
	}

	// Evict until there is room (recompute live set each round in case an
	// adopted id in the LRU no longer has a window).
	for {
		live := p.liveWindows()
		if len(live) < p.max {
			break
		}
		victim := p.oldestLive(live)
		if victim == "" {
			break // LRU desynced from reality; allow the spawn anyway
		}
		p.logger.Info("evicting LRU session window", "session", victim)
		if _, err := p.runner.run("kill-window", "-t", target(victim)); err != nil {
			p.logger.Warn("kill-window", "session", victim, "err", err)
		}
		p.remove(victim)
	}

	if err := p.spawn(sessionID, cwd, resume); err != nil {
		return false, err
	}
	p.touch(sessionID)
	return true, nil
}

// spawn creates the window running interactive claude and dismisses the
// first-launch "trust this folder" prompt (CR on the default = trust). The
// trust prompt only appears for not-yet-trusted cwds; the CR is a harmless
// no-op on an already-trusted resume.
func (p *pool) spawn(sessionID, cwd string, resume bool) error {
	idFlag := "--session-id"
	if resume {
		idFlag = "--resume"
	}
	parts := []string{shellQuote(p.claudeCmd), idFlag, shellQuote(sessionID)}
	for _, a := range p.extraArgs {
		parts = append(parts, shellQuote(a))
	}
	cmd := strings.Join(parts, " ")

	// -e propagates env (notably USHER_HOOK_SOCK) into the spawned claude, so
	// its permission hooks route back to THIS usher instance rather than
	// whatever owns the default-data-dir socket. The dedicated tmux server
	// freezes its env at creation, so inheritance alone is not reliable.
	envFlags := make([]string, 0, len(p.env)*2)
	for _, kv := range p.env {
		envFlags = append(envFlags, "-e", kv)
	}

	var err error
	if !p.sessionExists() {
		args := append([]string{"new-session", "-d", "-s", tmuxSessionName,
			"-n", sessionID, "-c", cwd, "-x", "200", "-y", "50"}, envFlags...)
		_, err = p.runner.run(append(args, cmd)...)
	} else {
		args := append([]string{"new-window", "-t", tmuxSessionName,
			"-n", sessionID, "-c", cwd}, envFlags...)
		_, err = p.runner.run(append(args, cmd)...)
	}
	if err != nil {
		return fmt.Errorf("spawn window for %s: %w", sessionID, err)
	}
	// Keep claude's window name stable (don't let the TUI rename it).
	_, _ = p.runner.run("set-window-option", "-t", target(sessionID), "automatic-rename", "off")
	return nil
}

// inject pastes prompt into the session's window using bracketed-paste mode
// (paste-buffer -p), then submits with Enter. Bracketed paste keeps embedded
// newlines literal and stops a leading '/' or '@' from being read as a slash
// command or mention.
//
// The buffer is loaded via stdin (load-buffer -), not set-buffer -- <prompt>:
// a long paste passed as a command argument overflows tmux's command parser
// ("command too long"). Stdin has no such limit.
func (p *pool) inject(sessionID, prompt string) error {
	buf := "usher-" + sessionID
	if _, err := p.runner.runStdin(prompt, "load-buffer", "-b", buf, "-"); err != nil {
		return fmt.Errorf("load-buffer: %w", err)
	}
	if _, err := p.runner.run("paste-buffer", "-p", "-d", "-b", buf, "-t", target(sessionID)); err != nil {
		return fmt.Errorf("paste-buffer: %w", err)
	}
	if _, err := p.runner.run("send-keys", "-t", target(sessionID), "Enter"); err != nil {
		return fmt.Errorf("send-keys Enter: %w", err)
	}
	return nil
}

// acceptTrust sends a single Enter to accept a possible "trust this folder"
// prompt right after spawn. Separated from inject so callers can space it out
// from the prompt paste (the TUI needs a beat to render the dialog).
func (p *pool) acceptTrust(sessionID string) error {
	_, err := p.runner.run("send-keys", "-t", target(sessionID), "Enter")
	return err
}

// interrupt sends Ctrl-C to stop the current turn WITHOUT killing the
// process (the window/claude stays alive for the next send).
func (p *pool) interrupt(sessionID string) error {
	_, err := p.runner.run("send-keys", "-t", target(sessionID), "C-c")
	return err
}

func (p *pool) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessionExists() {
		_, _ = p.runner.run("kill-session", "-t", tmuxSessionName)
	}
	p.lru = nil
}

// --- internals (callers hold p.mu) ---------------------------------------

func (p *pool) sessionExists() bool {
	_, err := p.runner.run("has-session", "-t", tmuxSessionName)
	return err == nil
}

// liveWindows returns the session ids of currently-alive windows.
func (p *pool) liveWindows() []string {
	out, err := p.runner.run("list-windows", "-t", tmuxSessionName, "-F", "#{window_name}")
	if err != nil {
		return nil
	}
	var names []string
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			names = append(names, ln)
		}
	}
	return names
}

// oldestLive returns the least-recently-used id that still has a live window.
func (p *pool) oldestLive(live []string) string {
	for _, id := range p.lru {
		if contains(live, id) {
			return id
		}
	}
	if len(live) > 0 {
		return live[0]
	}
	return ""
}

func (p *pool) touch(sessionID string) {
	p.remove(sessionID)
	p.lru = append(p.lru, sessionID)
}

func (p *pool) remove(sessionID string) {
	for i, id := range p.lru {
		if id == sessionID {
			p.lru = append(p.lru[:i], p.lru[i+1:]...)
			return
		}
	}
}

func target(sessionID string) string { return tmuxSessionName + ":" + sessionID }

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// shellQuote wraps s in single quotes for safe embedding in the command
// string passed to tmux new-window/new-session.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
