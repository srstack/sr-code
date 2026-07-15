package sender

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
)

type openCodeBackend struct {
	cmd    string
	root   string
	logger *slog.Logger

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// NewOpenCode builds a Sender that drives opencode through `opencode run` and
// writes usher-owned shadow jsonl transcripts under sessionsDir. opencode's
// native store is SQLite; keeping a jsonl shadow preserves usher's file-derived
// session model without importing SQLite or relying on opencode's DB schema.
func NewOpenCode(openCodeCmd, sessionsDir string, maxLive int, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	_ = maxLive // opencode run is one process per turn; no warm pool yet.
	t := timing{confirm: 8 * time.Second, poll: 150 * time.Millisecond}
	return &Sender{
		opencode: &openCodeBackend{
			cmd:     openCodeCmd,
			root:    sessionsDir,
			logger:  logger,
			running: map[string]context.CancelFunc{},
		},
		preAssignsID: true,
		locateFn:     func(id string) string { return locateOpenCode(sessionsDir, id) },
		logger:       logger,
		t:            t,
	}
}

func (s *Sender) opencodeTurn(ctx context.Context, id, prompt, cwd, model string) (<-chan StreamEvent, error) {
	if s.opencode == nil {
		return nil, errors.New("opencode backend is unavailable")
	}
	return s.opencode.Run(ctx, id, prompt, cwd, model)
}

func (b *openCodeBackend) Run(ctx context.Context, id, prompt, cwd, model string) (<-chan StreamEvent, error) {
	if id == "" {
		return nil, errors.New("opencode session id is empty")
	}
	if strings.TrimSpace(cwd) == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	path := openCodeLogPath(b.root, cwd, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	childCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, b.cmd, b.args(id, prompt, cwd, model)...)
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

	b.mu.Lock()
	b.running[id] = cancel
	b.mu.Unlock()

	out := make(chan StreamEvent, 64)
	go func() {
		defer close(out)
		defer func() {
			b.mu.Lock()
			delete(b.running, id)
			b.mu.Unlock()
			cancel()
		}()

		started, _ := json.Marshal(map[string]any{"cwd": cwd, "fresh": false})
		if !sendEvent(ctx, out, StreamEvent{Type: "subprocess.started", Raw: started}) {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			return
		}
		userRaw := claudeUserLine(id, cwd, prompt, time.Now().UTC())
		if err := appendLogLine(path, userRaw); err != nil {
			emitError(ctx, out, "opencode shadow log write failed: "+err.Error())
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			return
		}
		if !sendEvent(ctx, out, StreamEvent{Type: "user", Raw: userRaw}) {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			return
		}

		var errBuf bytes.Buffer
		errDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(&errBuf, stderr)
			close(errDone)
		}()

		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			text := strings.TrimRight(sc.Text(), "\r")
			if strings.TrimSpace(text) == "" {
				continue
			}
			raw := claudeAssistantLine(id, text, time.Now().UTC())
			if err := appendLogLine(path, raw); err != nil {
				emitError(ctx, out, "opencode shadow log write failed: "+err.Error())
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
				return
			}
			if !sendEvent(ctx, out, StreamEvent{Type: "assistant", Raw: raw}) {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
				return
			}
		}
		if err := sc.Err(); err != nil {
			emitError(ctx, out, "opencode stdout read failed: "+err.Error())
		}
		waitErr := cmd.Wait()
		<-errDone
		if waitErr != nil && childCtx.Err() == nil {
			msg := strings.TrimSpace(errBuf.String())
			if msg == "" {
				msg = waitErr.Error()
			}
			emitError(ctx, out, "opencode turn failed: "+msg)
		}
		systemRaw := claudeTurnCompleteLine(id, time.Now().UTC())
		if err := appendLogLine(path, systemRaw); err != nil {
			emitError(ctx, out, "opencode shadow log write failed: "+err.Error())
			return
		}
		if !sendEvent(ctx, out, StreamEvent{Type: "system", Raw: systemRaw}) {
			return
		}
		exited, _ := json.Marshal(map[string]any{"code": exitCode(waitErr)})
		sendEvent(ctx, out, StreamEvent{Type: "subprocess.exit", Raw: exited})
	}()
	return out, nil
}

func (b *openCodeBackend) args(id, prompt, cwd, model string) []string {
	args := []string{"run", "--session", id, "--dir", cwd}
	if model != "" && model != "default" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)
	return args
}

func (b *openCodeBackend) Has(sessionID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.running[sessionID]
	return ok
}

func (b *openCodeBackend) LiveSessions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.running))
	for id := range b.running {
		out = append(out, id)
	}
	return out
}

func (b *openCodeBackend) Kill(sessionID string) error {
	b.mu.Lock()
	cancel := b.running[sessionID]
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (b *openCodeBackend) Shutdown() {
	b.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(b.running))
	for _, cancel := range b.running {
		cancels = append(cancels, cancel)
	}
	b.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func locateOpenCode(root, id string) string {
	var found string
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

func openCodeLogPath(root, cwd, id string) string {
	return filepath.Join(root, openCodeProjectKey(cwd), id+".jsonl")
}

func openCodeProjectKey(cwd string) string {
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

func claudeUserLine(sessionID, cwd, content string, ts time.Time) json.RawMessage {
	return mustMarshal(map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"cwd":       cwd,
		"timestamp": ts,
		"uuid":      randomHexID(),
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	})
}

func claudeAssistantLine(sessionID, content string, ts time.Time) json.RawMessage {
	return mustMarshal(map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"timestamp": ts,
		"uuid":      randomHexID(),
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]string{{
				"type": "text",
				"text": content,
			}},
		},
	})
}

func claudeTurnCompleteLine(sessionID string, ts time.Time) json.RawMessage {
	return mustMarshal(map[string]any{
		"type":      "system",
		"subtype":   "turn_duration",
		"sessionId": sessionID,
		"timestamp": ts,
		"uuid":      randomHexID(),
	})
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func randomHexID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
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
