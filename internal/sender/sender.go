// Package sender launches headless `claude -p --resume` subprocesses and
// streams their output as parsed events.
//
// Each Send call runs an independent subprocess in the session's original cwd
// and pipes prompt input via stdin. Concurrent sends to the same session are
// safe: Claude Code internally serializes them via filesystem queue events
// (see /home/dev/.claude/projects/<dir>/<id>.jsonl `queue-operation` rows),
// so usher does no locking of its own.
package sender

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// StreamEvent is one event from the subprocess. Type is the top-level "type"
// field of a stream-json line, or one of the synthesized values
// "subprocess.started" / "subprocess.exit" / "subprocess.error".
type StreamEvent struct {
	Type string
	Raw  json.RawMessage
}

type Sender struct {
	claudeCmd      string
	permissionMode string
	logger         *slog.Logger
}

func New(claudeCmd, permissionMode string, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	return &Sender{
		claudeCmd:      claudeCmd,
		permissionMode: permissionMode,
		logger:         logger,
	}
}

// Send runs `claude -p --resume <sessionID>` from cwd, feeding prompt on
// stdin. Events stream on the returned channel; the channel is closed once
// the subprocess exits or ctx is cancelled. Cancelling ctx sends SIGINT and,
// after a 5s grace period, SIGKILL via exec.WaitDelay.
func (s *Sender) Send(ctx context.Context, sessionID, prompt, cwd string) (<-chan StreamEvent, error) {
	args := []string{
		"-p",
		"--resume", sessionID,
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
	}
	if s.permissionMode != "" {
		args = append(args, "--permission-mode", s.permissionMode)
	}

	cmd := exec.CommandContext(ctx, s.claudeCmd, args...)
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(prompt)
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGINT)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	pid := cmd.Process.Pid
	s.logger.Info("send started", "session", sessionID, "cwd", cwd, "pid", pid)

	out := make(chan StreamEvent, 64)

	go func() {
		defer close(out)

		started, _ := json.Marshal(struct {
			PID int    `json:"pid"`
			Cwd string `json:"cwd"`
		}{pid, cwd})
		sendEvent(ctx, out, StreamEvent{Type: "subprocess.started", Raw: started})

		sc := bufio.NewScanner(stdout)
		// Some events (assistant message with full usage stats, large
		// attachments) easily exceed bufio's default 64K line limit.
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

		for sc.Scan() {
			line := sc.Bytes()
			ev, err := parseStreamLine(line)
			if err != nil {
				s.logger.Warn("parse stream line", "err", err)
				continue
			}
			if !sendEvent(ctx, out, ev) {
				break
			}
		}
		if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
			s.logger.Warn("stream scanner error", "err", err)
		}

		waitErr := cmd.Wait()
		exitCode := 0
		var waitMsg string
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				waitMsg = waitErr.Error()
			}
		}
		stderr := stderrBuf.String()
		if exitCode != 0 || waitMsg != "" {
			s.logger.Warn("send subprocess ended non-zero",
				"session", sessionID, "exit_code", exitCode, "stderr", truncate(stderr, 500))
		}

		exit, _ := json.Marshal(struct {
			ExitCode int    `json:"exit_code"`
			Stderr   string `json:"stderr,omitempty"`
			Error    string `json:"error,omitempty"`
		}{exitCode, truncate(stderr, 1000), waitMsg})
		sendEvent(context.Background(), out, StreamEvent{Type: "subprocess.exit", Raw: exit})
	}()

	return out, nil
}

func parseStreamLine(line []byte) (StreamEvent, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return StreamEvent{}, err
	}
	raw := append(json.RawMessage(nil), line...)
	return StreamEvent{Type: head.Type, Raw: raw}, nil
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
