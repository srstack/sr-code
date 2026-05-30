package sender

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func collect(t *testing.T, ch <-chan StreamEvent, timeout time.Duration) []StreamEvent {
	t.Helper()
	var got []StreamEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out after %s; got %d events so far: %v", timeout, len(got), types(got))
		}
	}
}

// testSender wires a Sender to a fake tmux runner and a temp projects dir,
// with timings shrunk to milliseconds. Returns the sender and the jsonl path
// the session's file should live at.
func testSender(t *testing.T, runner tmuxRunner, id string) (*Sender, string) {
	t.Helper()
	dir := t.TempDir()
	sub := filepath.Join(dir, "proj")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Sender{
		pool:        newPool(runner, "claude", nil, 8, quietLogger()),
		projectsDir: dir,
		logger:      quietLogger(),
		t: timing{
			spawnSettle:   10 * time.Millisecond,
			trustToInject: 5 * time.Millisecond,
			warmSettle:    5 * time.Millisecond,
			confirm:       1 * time.Second,
			poll:          10 * time.Millisecond,
			attempts:      2,
		},
		tail: tailConfig{poll: 10 * time.Millisecond, appearWait: 2 * time.Second},
	}
	return s, filepath.Join(sub, id+".jsonl")
}

var turnLines = []string{
	`{"type":"user","message":{"role":"user","content":"hi"}}`,
	`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"hello"}]}}`,
}

func TestSend_ResumeStreamsTurn(t *testing.T) {
	f := &fakeTmux{}
	s, path := testSender(t, f, "sess-1")
	// Pre-existing history so this is a resume with a non-zero offset.
	if err := os.WriteFile(path, []byte(`{"type":"mode"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ch, err := s.Send(context.Background(), "sess-1", "hi", "/work")
	if err != nil {
		t.Fatal(err)
	}
	go appendLines(path, 30*time.Millisecond, turnLines...)

	got := collect(t, ch, 6*time.Second)
	want := []string{"subprocess.started", "user", "assistant", "subprocess.exit"}
	if !eq(types(got), want) {
		t.Fatalf("got %v, want %v", types(got), want)
	}
	// The pre-existing "mode" line must not leak (offset respected).
	for _, e := range got {
		if strings.Contains(string(e.Raw), `"mode"`) {
			t.Fatalf("history leaked: %s", e.Raw)
		}
	}
}

func TestSend_NewSessionWaitsForFileAndTrust(t *testing.T) {
	f := &fakeTmux{}
	s, path := testSender(t, f, "new-1")

	ch, err := s.SendNew(context.Background(), "new-1", "hi", "/work")
	if err != nil {
		t.Fatal(err)
	}
	// jsonl is created lazily, only after the prompt is submitted.
	go func() {
		time.Sleep(60 * time.Millisecond)
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			panic(err)
		}
		appendLines(path, 20*time.Millisecond, turnLines...)
	}()

	got := collect(t, ch, 6*time.Second)
	if !eq(types(got), []string{"subprocess.started", "user", "assistant", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
	// A fresh window spawns via new-session, runs claude with --session-id,
	// and receives a trust-accept Enter.
	if f.countCmd("new-session") != 1 {
		t.Fatalf("expected one new-session, got %d", f.countCmd("new-session"))
	}
	if !cmdMatches(f, "new-session", "--session-id") {
		t.Fatal("new session should launch claude with --session-id")
	}
	if !cmdMatches(f, "send-keys", "Enter") {
		t.Fatal("fresh window should receive a trust-accept Enter")
	}
}

func TestSend_RetriesWhenPromptMissed(t *testing.T) {
	f := &fakeTmux{}
	s, path := testSender(t, f, "retry-1")
	s.t.confirm = 200 * time.Millisecond // make the first attempt time out fast
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	ch, err := s.Send(context.Background(), "retry-1", "hi", "/work")
	if err != nil {
		t.Fatal(err)
	}
	// Only write the turn after the SECOND inject (second paste-buffer),
	// forcing the retry path.
	go func() {
		for f.countCmd("paste-buffer") < 2 {
			time.Sleep(5 * time.Millisecond)
		}
		appendLines(path, 10*time.Millisecond, turnLines...)
	}()

	got := collect(t, ch, 6*time.Second)
	if !eq(types(got), []string{"subprocess.started", "user", "assistant", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
	if n := f.countCmd("paste-buffer"); n != 2 {
		t.Fatalf("expected 2 inject attempts, got %d", n)
	}
}

func TestSend_SpawnErrorPropagates(t *testing.T) {
	f := &fakeTmux{failSpawn: true}
	s, _ := testSender(t, f, "boom")
	if _, err := s.Send(context.Background(), "boom", "hi", "/work"); err == nil {
		t.Fatal("expected error when window spawn fails")
	}
}

// cmdMatches reports whether some recorded command starting with verb has an
// argument containing sub.
func cmdMatches(f *fakeTmux, verb, sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if len(c) == 0 || c[0] != verb {
			continue
		}
		for _, a := range c {
			if strings.Contains(a, sub) {
				return true
			}
		}
	}
	return false
}
