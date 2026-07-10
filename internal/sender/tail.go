package sender

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"time"
)

// tailConfig tunes the turn tailer. Zero values fall back to sane defaults
// in tailTurn so callers (and tests) only set what they care about.
type tailConfig struct {
	poll       time.Duration // how often to re-check the file for growth
	appearWait time.Duration // how long to wait for a not-yet-created file

	// turnComplete reports whether a session-log line is the backend's
	// end-of-turn marker. Backend-specific: Claude Code uses system/turn_duration
	// (the default below); Codex uses event_msg/task_complete. nil → Claude.
	turnComplete func(line []byte) bool

	// Idle fallback for prompts that never start a model turn (TUI-local
	// commands like /model log no end-of-turn marker, often nothing at all).
	// The turn is finalized early only when ALL hold:
	//   - turnActivity has matched no line yet (a latch: once a model turn is
	//     proven in flight, the fallback is permanently disarmed),
	//   - the log has been silent for `quiet`,
	//   - idleProbeStrikes consecutive paneBusy probes, probeEvery apart,
	//     reported idle (guards the submit→first-model-line window, which
	//     routinely runs tens of seconds).
	// nil paneBusy disables the fallback.
	turnActivity func(line []byte) bool
	paneBusy     func() bool
	quiet        time.Duration
	probeEvery   time.Duration

	// onIdleExit, if set, is called once when the idle fallback finalizes the
	// turn (not on any other exit path).
	onIdleExit func()
}

// idleProbeStrikes is how many consecutive idle probes the fallback needs — a
// single capture can catch the TUI mid-repaint.
const idleProbeStrikes = 2

func (c tailConfig) withDefaults() tailConfig {
	if c.poll <= 0 {
		c.poll = 100 * time.Millisecond
	}
	if c.appearWait <= 0 {
		c.appearWait = 15 * time.Second
	}
	if c.turnComplete == nil {
		c.turnComplete = isTurnComplete
	}
	if c.turnActivity == nil {
		c.turnActivity = isTurnActivity
	}
	if c.quiet <= 0 {
		c.quiet = 5 * time.Second
	}
	if c.probeEvery <= 0 {
		c.probeEvery = max(c.poll, c.quiet/5)
	}
	return c
}

// tailTurn follows the session jsonl at path starting from byteOffset and
// streams the events appended during the current turn. byteOffset is the file
// size captured just before the prompt was injected, so the tailer reports
// only this turn's new lines.
//
// Output: each complete jsonl line becomes a StreamEvent{Type, Raw} (Type is
// the line's "type" field). When Claude Code logs its end-of-turn marker (a
// "system/turn_duration" event — see isTurnComplete), the tailer emits a
// synthesized "subprocess.exit" and closes the channel.
//
// The channel also closes on ctx cancellation, or if the file never appears
// within cfg.appearWait (brand-new sessions create their jsonl lazily). Unlike
// the old subprocess sender, nothing here owns a process: the interactive
// claude lives in the tmux pool and outlives the turn.
func tailTurn(ctx context.Context, path string, byteOffset int64, logger *slog.Logger, cfg tailConfig) <-chan StreamEvent {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	out := make(chan StreamEvent, 64)

	go func() {
		defer close(out)
		// A turn ends one of two ways: Claude Code logs its turn_duration marker,
		// or the send is cancelled — the cancel button, or an esc the user pressed
		// in the mirror, which interrupts claude but (being an interrupt) never
		// logs turn_duration. Emit subprocess.exit for both so the web UI finalizes
		// the turn instead of waiting forever on a marker that isn't coming.
		emitExit := func() {
			sendEvent(context.Background(), out, StreamEvent{Type: "subprocess.exit", Raw: json.RawMessage(`{}`)})
		}

		f, ok := openWhenReady(ctx, path, cfg, out)
		if !ok {
			// Cancelled before the file appeared: emit exit so the UI
			// finalizes the turn. The appearWait timeout (ctx not cancelled)
			// already emitted an error, so skip it there.
			if ctx.Err() != nil {
				emitExit()
			}
			return
		}
		defer f.Close()

		if _, err := f.Seek(byteOffset, io.SeekStart); err != nil {
			logger.Warn("tail seek", "path", path, "offset", byteOffset, "err", err)
			return
		}

		reader := bufio.NewReader(f)
		var pending []byte // partial trailing line not yet terminated by '\n'
		ticker := time.NewTicker(cfg.poll)
		defer ticker.Stop()

		// Idle-fallback state (see tailConfig): active latches once a line proves
		// a model turn is in flight; lastGrowth tracks log silence; idleProbes
		// counts consecutive pane sightings without a running-turn indicator.
		active := false
		lastGrowth := time.Now()
		var lastProbe time.Time
		idleProbes := 0

		for {
			for {
				chunk, err := reader.ReadBytes('\n')
				if len(chunk) > 0 {
					pending = append(pending, chunk...)
					lastGrowth = time.Now()
					idleProbes = 0
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					logger.Warn("tail read", "path", path, "err", err)
					return
				}
				// Complete line (ends in '\n').
				line := bytes.TrimRight(pending, "\r\n")
				pending = nil
				if len(line) == 0 {
					continue
				}
				// The turn is done when Claude Code logs its end-of-turn
				// "system/turn_duration" event — NOT when an assistant message
				// carries stop_reason "end_turn" (interactive claude stamps
				// end_turn on intermediate thinking/tool_use messages too, so
				// trusting it ends the turn before the tool even runs — which
				// released ownership and sent permission prompts to the pane).
				if cfg.turnComplete(line) {
					emitExit()
					return
				}
				if !active && cfg.turnActivity(line) {
					active = true
				}
				ev := StreamEvent{Type: lineType(line), Raw: append(json.RawMessage(nil), line...)}
				if !sendEvent(ctx, out, ev) {
					emitExit() // ctx cancelled mid-stream
					return
				}
			}

			select {
			case <-ctx.Done():
				emitExit()
				return
			case <-ticker.C:
				if cfg.paneBusy != nil && !active &&
					time.Since(lastGrowth) >= cfg.quiet && time.Since(lastProbe) >= cfg.probeEvery {
					lastProbe = time.Now()
					if cfg.paneBusy() {
						idleProbes = 0
					} else if idleProbes++; idleProbes >= idleProbeStrikes {
						logger.Debug("turn produced no model activity and the pane is idle; finalizing", "path", path)
						if cfg.onIdleExit != nil {
							cfg.onIdleExit()
						}
						emitExit()
						return
					}
				}
			}
		}
	}()

	return out
}

// openWhenReady opens path, waiting up to cfg.appearWait for it to exist. A
// brand-new session's jsonl is created only once claude writes its first
// event, so SendNew callers race the file into existence.
func openWhenReady(ctx context.Context, path string, cfg tailConfig, out chan<- StreamEvent) (*os.File, bool) {
	deadline := time.NewTimer(cfg.appearWait)
	defer deadline.Stop()
	ticker := time.NewTicker(cfg.poll)
	defer ticker.Stop()

	for {
		if f, err := os.Open(path); err == nil {
			return f, true
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-deadline.C:
			errMsg, _ := json.Marshal(map[string]string{"message": "session jsonl did not appear: " + path})
			sendEvent(context.Background(), out, StreamEvent{Type: "error", Raw: errMsg})
			return nil, false
		case <-ticker.C:
		}
	}
}

func lineType(line []byte) string {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return ""
	}
	return head.Type
}

// isTurnComplete reports whether line is Claude Code's end-of-turn marker: a
// "system" event with subtype "turn_duration" (carries durationMs/messageCount).
// It is written once, after the final assistant message, only when the turn has
// truly finished — so unlike assistant stop_reason it does not fire mid-turn
// (during thinking or a pending tool call).
func isTurnComplete(line []byte) bool {
	var o struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(line, &o); err != nil {
		return false
	}
	return o.Type == "system" && o.Subtype == "turn_duration"
}

// isTurnActivity reports whether a Claude Code jsonl line proves a model turn
// is in flight: an "assistant" line with real model output. TUI-local commands
// never write one. Claude Code's own synthetic assistant lines ("No response
// requested.", API-error messages — model "<synthetic>") don't count: they
// often end a turn with no turn_duration, exactly what the fallback must
// finalize.
func isTurnActivity(line []byte) bool {
	var o struct {
		Type     string `json:"type"`
		IsAPIErr bool   `json:"isApiErrorMessage"`
		Message  struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &o); err != nil {
		return false
	}
	return o.Type == "assistant" && !o.IsAPIErr && o.Message.Model != "<synthetic>"
}
