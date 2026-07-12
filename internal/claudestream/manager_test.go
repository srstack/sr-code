package claudestream

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeClaude(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	script := filepath.Join(dir, "claude")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_CLAUDE_LOG"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "${FAKE_CLAUDE_LOG}.input"
  case "$line" in
    *control_request*) ;;
    *) printf '%s\n' '{"type":"result","subtype":"success","is_error":false}' ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, log
}

func TestLongRunningProcessServesMultipleTurns(t *testing.T) {
	bin, log := fakeClaude(t)
	m := New(bin, `{"hooks":{}}`, "/tmp/h.sock", nil, 4, nil)
	m.processes = map[string]*process{}
	os.Setenv("FAKE_CLAUDE_LOG", log)
	t.Cleanup(func() { os.Unsetenv("FAKE_CLAUDE_LOG"); m.Shutdown() })
	for i := 0; i < 2; i++ {
		ch, fresh, _, err := m.Send(context.Background(), "sid", "hello", "/tmp", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if fresh != (i == 0) {
			t.Fatalf("turn %d fresh=%v", i, fresh)
		}
		select {
		case r := <-ch:
			if r.IsError {
				t.Fatalf("result=%+v", r)
			}
		case <-time.After(time.Second):
			t.Fatal("result timeout")
		}
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(b)), "\n") + 1; lines != 1 {
		t.Fatalf("spawn count=%d args=%s", lines, b)
	}
	if !strings.Contains(string(b), "--session-id sid") || !strings.Contains(string(b), "--input-format stream-json") {
		t.Fatalf("args=%s", b)
	}
	in, _ := os.ReadFile(log + ".input")
	if !strings.Contains(string(in), `"content":[{"text":"hello","type":"text"}]`) {
		t.Fatalf("user message is not content-block array: %s", in)
	}
}

func TestResumeUsesResumeFlag(t *testing.T) {
	bin, log := fakeClaude(t)
	os.Setenv("FAKE_CLAUDE_LOG", log)
	defer os.Unsetenv("FAKE_CLAUDE_LOG")
	m := New(bin, "", "", nil, 4, nil)
	defer m.Shutdown()
	ch, _, _, err := m.Send(context.Background(), "sid", "hello", "/tmp", "", true)
	if err != nil {
		t.Fatal(err)
	}
	<-ch
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "--resume sid") {
		t.Fatalf("args=%s", b)
	}
}

func TestSpontaneousTurnTailEventsDoNotStickOrStealNextResult(t *testing.T) {
	m := New("", "", "", nil, 2, nil)
	p := &process{id: "s", lastUsed: time.Now()}
	m.processes["s"] = p
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { m.readLoop(p, r); close(done) }()
	_, _ = io.WriteString(w, "{\"type\":\"assistant\"}\n")
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		n := len(p.turns)
		p.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("spontaneous marker not queued")
		}
		time.Sleep(time.Millisecond)
	}
	user := make(chan Result, 1)
	p.mu.Lock()
	p.turns = append(p.turns, user)
	p.mu.Unlock()
	_, _ = io.WriteString(w, "{\"type\":\"result\",\"subtype\":\"success\"}\n{\"type\":\"command_lifecycle\"}\n{\"type\":\"rate_limit_event\"}\n")
	select {
	case <-user:
		t.Fatal("spontaneous result was delivered to user turn")
	case <-time.After(20 * time.Millisecond):
	}
	_, _ = io.WriteString(w, "{\"type\":\"result\",\"subtype\":\"success\"}\n")
	select {
	case <-user:
	case <-time.After(time.Second):
		t.Fatal("user result not delivered")
	}
	_ = w.Close()
	<-done
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.turns) != 0 {
		t.Fatalf("turn queue stuck: %d", len(p.turns))
	}
}

func TestMaxLiveDoesNotGrowWhenAllProcessesBusy(t *testing.T) {
	m := New("missing", "", "", nil, 1, nil)
	m.processes["busy"] = &process{id: "busy", turns: []chan Result{nil}}
	if _, _, err := m.ensure(context.Background(), "new", "/tmp", "", true); err == nil {
		t.Fatal("expected max-live busy error")
	}
	if len(m.processes) != 1 {
		t.Fatalf("process count=%d, want hard cap 1", len(m.processes))
	}
}
