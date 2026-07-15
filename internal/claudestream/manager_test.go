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
		ch, _, fresh, _, err := m.Send(context.Background(), "sid", "hello", "/tmp", "", false)
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
	if !strings.Contains(string(b), "--session-id sid") || !strings.Contains(string(b), "--input-format stream-json") || !strings.Contains(string(b), "--include-partial-messages") {
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
	ch, _, _, _, err := m.Send(context.Background(), "sid", "hello", "/tmp", "", true)
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
	user := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta, 1)}
	p.mu.Lock()
	p.turns = append(p.turns, user)
	p.mu.Unlock()
	_, _ = io.WriteString(w, "{\"type\":\"result\",\"subtype\":\"success\"}\n{\"type\":\"command_lifecycle\"}\n{\"type\":\"rate_limit_event\"}\n")
	select {
	case <-user.done:
		t.Fatal("spontaneous result was delivered to user turn")
	case <-time.After(20 * time.Millisecond):
	}
	_, _ = io.WriteString(w, "{\"type\":\"result\",\"subtype\":\"success\"}\n")
	select {
	case <-user.done:
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

func TestMessageStartMarksSpontaneousTurnButDeltasDoNot(t *testing.T) {
	m := New("", "", "", nil, 2, nil)
	p := &process{id: "s", lastUsed: time.Now()}
	m.processes["s"] = p
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { m.readLoop(p, r); close(done) }()
	_, _ = io.WriteString(w, `{"type":"stream_event","event":{"type":"message_start"}}`+"\n")
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		n := len(p.turns)
		p.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("message_start did not mark a spontaneous turn")
		}
		time.Sleep(time.Millisecond)
	}
	user := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta, 1)}
	p.mu.Lock()
	if p.turns[0] != nil {
		p.mu.Unlock()
		t.Fatal("spontaneous marker is not nil")
	}
	p.turns = append(p.turns, user)
	p.mu.Unlock()
	// The spontaneous turn's deltas must not leak into the queued user turn.
	_, _ = io.WriteString(w, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"spontaneous"}}}`+"\n")
	_, _ = io.WriteString(w, `{"type":"result","subtype":"success"}`+"\n")
	select {
	case <-user.done:
		t.Fatal("spontaneous result was delivered to user turn")
	case d := <-user.deltas:
		t.Fatalf("spontaneous delta leaked to user turn: %+v", d)
	case <-time.After(20 * time.Millisecond):
	}
	_, _ = io.WriteString(w, `{"type":"result","subtype":"success"}`+"\n")
	select {
	case <-user.done:
	case <-time.After(time.Second):
		t.Fatal("user result not delivered")
	}
	_ = w.Close()
	<-done
}

func TestPartialTextDeltaRoutesToCurrentTurn(t *testing.T) {
	m := New("", "", "", nil, 2, nil)
	req := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta, 2)}
	p := &process{id: "s", lastUsed: time.Now(), turns: []*turnRequest{req}}
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { m.readLoop(p, r); close(done) }()
	_, _ = io.WriteString(w, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`+"\n")
	select {
	case d := <-req.deltas:
		if d.Text != "hello" {
			t.Fatalf("delta = %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("delta timeout")
	}
	_, _ = io.WriteString(w, `{"type":"result","subtype":"success"}`+"\n")
	<-req.done
	_ = w.Close()
	<-done
}

func TestResultUsesLastAssistantModelContextWindow(t *testing.T) {
	m := New("", "", "", nil, 2, nil)
	req := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta, 1)}
	p := &process{id: "s", lastUsed: time.Now(), turns: []*turnRequest{req}}
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { m.readLoop(p, r); close(done) }()
	_, _ = io.WriteString(w, `{"type":"assistant","message":{"model":"claude-opus-4-7"}}`+"\n")
	_, _ = io.WriteString(w, `{"type":"result","subtype":"success","modelUsage":{"claude-sonnet-4-6":{"contextWindow":200000},"claude-opus-4-7":{"contextWindow":1000000}}}`+"\n")
	result := <-req.done
	if result.Model != "claude-opus-4-7" || result.ContextWindow != 1000000 {
		t.Fatalf("result runtime = %+v", result)
	}
	_ = w.Close()
	<-done
}

func TestResultSingleModelUsageFallback(t *testing.T) {
	m := New("", "", "", nil, 2, nil)
	req := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta, 1)}
	p := &process{id: "s", lastUsed: time.Now(), turns: []*turnRequest{req}}
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { m.readLoop(p, r); close(done) }()
	_, _ = io.WriteString(w, `{"type":"result","subtype":"success","modelUsage":{"claude-sonnet-4-6":{"contextWindow":200000}}}`+"\n")
	result := <-req.done
	if result.Model != "claude-sonnet-4-6" || result.ContextWindow != 200000 {
		t.Fatalf("result runtime = %+v", result)
	}
	_ = w.Close()
	<-done
}

func TestMaxLiveDoesNotGrowWhenAllProcessesBusy(t *testing.T) {
	m := New("missing", "", "", nil, 1, nil)
	m.processes["busy"] = &process{id: "busy", turns: []*turnRequest{nil}}
	if _, _, err := m.ensure(context.Background(), "new", "/tmp", "", true); err == nil {
		t.Fatal("expected max-live busy error")
	}
	if len(m.processes) != 1 {
		t.Fatalf("process count=%d, want hard cap 1", len(m.processes))
	}
}
