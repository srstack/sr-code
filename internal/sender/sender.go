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
	"log/slog"
	"os"
	"path/filepath"
	"time"
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
type timing struct {
	spawnSettle   time.Duration // after spawning a fresh window, before first keystroke (claude boot)
	trustToInject time.Duration // after the trust-accept Enter, before pasting the prompt
	warmSettle    time.Duration // before pasting into an already-running window
	confirm       time.Duration // how long to wait for the injected user turn to land
	poll          time.Duration // file poll interval for confirm + tail
	attempts      int           // inject attempts before giving up
}

type Sender struct {
	pool        *pool
	projectsDir string
	logger      *slog.Logger
	t           timing
	tail        tailConfig
}

// New builds a Sender. claudeCmd is the claude binary; permissionMode (if
// non-empty) is passed through as --permission-mode; projectsDir is Claude
// Code's projects root (used to locate session jsonl files by their globally
// unique id); socket is the dedicated tmux server socket name; maxLive caps
// concurrent live processes (LRU-evicted beyond it).
func New(claudeCmd, permissionMode, projectsDir, socket string, maxLive int, logger *slog.Logger) *Sender {
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
	runner := execRunner{bin: "tmux", socket: socket}
	return &Sender{
		pool:        newPool(runner, claudeCmd, extra, maxLive, logger),
		projectsDir: projectsDir,
		logger:      logger,
		t: timing{
			spawnSettle:   5 * time.Second,
			trustToInject: 1500 * time.Millisecond,
			warmSettle:    400 * time.Millisecond,
			confirm:       8 * time.Second,
			poll:          150 * time.Millisecond,
			attempts:      2,
		},
		tail: tailConfig{poll: 150 * time.Millisecond, appearWait: 20 * time.Second},
	}
}

// Send injects prompt into the session's live interactive claude (resuming /
// spawning it as needed) and streams the resulting turn's events. The channel
// closes when the turn ends or ctx is cancelled.
func (s *Sender) Send(ctx context.Context, sessionID, prompt, cwd string) (<-chan StreamEvent, error) {
	return s.run(ctx, sessionID, prompt, cwd, true)
}

// SendNew is like Send but starts a brand-new session with the given id
// (`--session-id`). The jsonl is created lazily once claude writes the first
// turn; the tailer waits for it to appear.
func (s *Sender) SendNew(ctx context.Context, sessionID, prompt, cwd string) (<-chan StreamEvent, error) {
	return s.run(ctx, sessionID, prompt, cwd, false)
}

// Has reports whether usher currently holds a live interactive process for
// sessionID. Used by the hook bridge to decide ownership.
func (s *Sender) Has(sessionID string) bool { return s.pool.has(sessionID) }

// Interrupt stops the in-flight turn for sessionID without killing the
// process (Ctrl-C into the pane).
func (s *Sender) Interrupt(sessionID string) error { return s.pool.interrupt(sessionID) }

// Shutdown tears down usher's tmux server (all live windows). Call on exit if
// you do NOT want processes to survive for the next usher run.
func (s *Sender) Shutdown() { s.pool.shutdown() }

func (s *Sender) run(ctx context.Context, sessionID, prompt, cwd string, resume bool) (<-chan StreamEvent, error) {
	fresh, err := s.pool.ensure(sessionID, cwd, resume)
	if err != nil {
		return nil, err
	}

	out := make(chan StreamEvent, 64)
	go func() {
		defer close(out)

		started, _ := json.Marshal(struct {
			Cwd   string `json:"cwd"`
			Fresh bool   `json:"fresh"`
		}{cwd, fresh})
		if !sendEvent(ctx, out, StreamEvent{Type: "subprocess.started", Raw: started}) {
			return
		}

		// Let a freshly-spawned window boot and dismiss the trust prompt; a
		// warm window only needs a brief beat.
		if fresh {
			if !sleepCtx(ctx, s.t.spawnSettle) {
				return
			}
			_ = s.pool.acceptTrust(sessionID)
			if !sleepCtx(ctx, s.t.trustToInject) {
				return
			}
		} else if !sleepCtx(ctx, s.t.warmSettle) {
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

		// Inject, confirming the prompt landed (re-inject on a missed paste).
		landed := false
		for attempt := 0; attempt < s.t.attempts; attempt++ {
			if err := s.pool.inject(sessionID, prompt); err != nil {
				emitError(ctx, out, "inject prompt: "+err.Error())
				return
			}
			if path == "" {
				path = s.locateWait(ctx, sessionID, s.t.confirm)
			}
			if path != "" && waitForUserTurn(ctx, path, offset, s.t.confirm, s.t.poll) {
				landed = true
				break
			}
			s.logger.Warn("injected prompt did not land; retrying", "session", sessionID, "attempt", attempt+1)
		}
		if !landed {
			emitError(ctx, out, "prompt did not register in session (TUI not ready?)")
			return
		}

		for ev := range tailTurn(ctx, path, offset, s.logger, s.tail) {
			if !sendEvent(ctx, out, ev) {
				return
			}
		}
	}()

	return out, nil
}

// locate finds the session jsonl by its globally unique id, sidestepping the
// ambiguous cwd<->dir mapping (a cwd may legitimately contain '-'). Returns ""
// if not found.
func (s *Sender) locate(sessionID string) string {
	matches, err := filepath.Glob(filepath.Join(s.projectsDir, "*", sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
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
