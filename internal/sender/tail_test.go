package sender

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func types(evs []StreamEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// appendLines appends each line (a '\n' is added) to path with a small delay
// between them, simulating claude writing the turn out over time.
func appendLines(t *testing.T, path string, gap time.Duration, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, ln := range lines {
		time.Sleep(gap)
		if _, err := f.WriteString(ln + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func fastCfg() tailConfig {
	return tailConfig{poll: 10 * time.Millisecond, appearWait: 2 * time.Second}
}

func TestTailTurn_SimpleTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"mode"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	off, _ := os.Stat(path)

	go appendLines(t, path, 20*time.Millisecond,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"hello"}]}}`,
	)

	ch := tailTurn(context.Background(), path, off.Size(), nil, fastCfg())
	got := collect(t, ch, 5*time.Second)

	want := []string{"user", "assistant", "subprocess.exit"}
	if !eq(types(got), want) {
		t.Fatalf("got %v, want %v", types(got), want)
	}
}

func TestTailTurn_ToolUseDoesNotEndEarly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	go appendLines(t, path, 15*time.Millisecond,
		`{"type":"user","message":{"role":"user","content":"make a file"}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","name":"Write"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`,
	)

	ch := tailTurn(context.Background(), path, 0, nil, fastCfg())
	got := collect(t, ch, 5*time.Second)

	want := []string{"user", "assistant", "user", "assistant", "subprocess.exit"}
	if !eq(types(got), want) {
		t.Fatalf("got %v, want %v", types(got), want)
	}
	// The exit must carry the terminal stop_reason, not the tool_use one.
	last := got[len(got)-1]
	if string(last.Raw) == "" || !strings.Contains(string(last.Raw), "end_turn") {
		t.Fatalf("exit event missing end_turn stop_reason: %s", last.Raw)
	}
}

func TestTailTurn_ByteOffsetSkipsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	old := `{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"OLD"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	off, _ := os.Stat(path)

	go appendLines(t, path, 15*time.Millisecond,
		`{"type":"user","message":{"role":"user","content":"new"}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"NEW"}]}}`,
	)

	ch := tailTurn(context.Background(), path, off.Size(), nil, fastCfg())
	got := collect(t, ch, 5*time.Second)

	for _, e := range got {
		if strings.Contains(string(e.Raw), "OLD") {
			t.Fatalf("tailer leaked pre-offset history: %s", e.Raw)
		}
	}
	if !eq(types(got), []string{"user", "assistant", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
}

func TestTailTurn_FileNeverAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.jsonl")
	ch := tailTurn(context.Background(), path, 0, nil, tailConfig{poll: 10 * time.Millisecond, appearWait: 80 * time.Millisecond})
	got := collect(t, ch, 2*time.Second)
	if !eq(types(got), []string{"error"}) {
		t.Fatalf("got %v, want [error]", types(got))
	}
}
