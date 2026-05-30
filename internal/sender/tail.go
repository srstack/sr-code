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
}

func (c tailConfig) withDefaults() tailConfig {
	if c.poll <= 0 {
		c.poll = 100 * time.Millisecond
	}
	if c.appearWait <= 0 {
		c.appearWait = 15 * time.Second
	}
	return c
}

// tailTurn follows the session jsonl at path starting from byteOffset and
// streams the events appended during the current turn. byteOffset is the file
// size captured just before the prompt was injected, so the tailer reports
// only this turn's new lines.
//
// Output: each complete jsonl line becomes a StreamEvent{Type, Raw} (Type is
// the line's "type" field). When an assistant message ends the turn
// (stop_reason other than "tool_use" — verified: intermediate tool steps are
// "tool_use", the final answer is "end_turn"), the tailer emits that assistant
// event, then a synthesized "subprocess.exit" carrying the stop_reason, then
// closes the channel.
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

		f, ok := openWhenReady(ctx, path, cfg, out)
		if !ok {
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

		for {
			for {
				chunk, err := reader.ReadBytes('\n')
				if len(chunk) > 0 {
					pending = append(pending, chunk...)
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
				ev := StreamEvent{Type: lineType(line), Raw: append(json.RawMessage(nil), line...)}
				if !sendEvent(ctx, out, ev) {
					return
				}
				if reason, terminal := terminalStopReason(line); terminal {
					exit, _ := json.Marshal(struct {
						StopReason string `json:"stop_reason"`
					}{reason})
					sendEvent(context.Background(), out, StreamEvent{Type: "subprocess.exit", Raw: exit})
					return
				}
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
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

// terminalStopReason reports whether line is an assistant message that ends a
// turn. Intermediate tool-call steps carry stop_reason "tool_use" and are not
// terminal; the final answer is "end_turn" (also "max_tokens"/"stop_sequence").
func terminalStopReason(line []byte) (string, bool) {
	var o struct {
		Type    string `json:"type"`
		Message struct {
			StopReason string `json:"stop_reason"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &o); err != nil || o.Type != "assistant" {
		return "", false
	}
	r := o.Message.StopReason
	return r, r != "" && r != "tool_use"
}
