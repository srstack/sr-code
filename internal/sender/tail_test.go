package sender

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/codexrollout"
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
// between them, simulating claude writing the turn out over time. Runs in a
// goroutine, so it panics rather than calling t.Fatal on error.
func appendLines(path string, gap time.Duration, lines ...string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	for _, ln := range lines {
		time.Sleep(gap)
		if _, err := f.WriteString(ln + "\n"); err != nil {
			panic(err)
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

	go appendLines(path, 20*time.Millisecond,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"system","subtype":"turn_duration","durationMs":42}`,
	)

	ch := tailTurn(context.Background(), path, off.Size(), nil, fastCfg())
	got := collect(t, ch, 5*time.Second)

	want := []string{"user", "assistant", "subprocess.exit"}
	if !eq(types(got), want) {
		t.Fatalf("got %v, want %v", types(got), want)
	}
}

func TestTailTurnSkipsQueuedSpontaneousCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	go appendLines(path, 10*time.Millisecond,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"scheduled"}]}}`,
		`{"type":"system","subtype":"turn_duration"}`,
		`{"type":"user","message":{"role":"user","content":"queued"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}`,
		`{"type":"system","subtype":"turn_duration"}`)
	cfg := fastCfg()
	cfg.skipCompletions = 1
	got := collect(t, tailTurn(context.Background(), path, 0, nil, cfg), 3*time.Second)
	if want := []string{"assistant", "system", "user", "assistant", "subprocess.exit"}; !eq(types(got), want) {
		t.Fatalf("got %v, want %v", types(got), want)
	}
}

func TestTailTurn_EndTurnStopReasonDoesNotEndEarly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Interactive claude stamps stop_reason "end_turn" on the intermediate
	// thinking/tool_use messages too. The turn must NOT end there — only at the
	// system/turn_duration marker after the tool ran and the final answer.
	go appendLines(path, 15*time.Millisecond,
		`{"type":"user","message":{"role":"user","content":"make a file"}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"tool_use","name":"Write"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"system","subtype":"turn_duration","durationMs":42}`,
	)

	ch := tailTurn(context.Background(), path, 0, nil, fastCfg())
	got := collect(t, ch, 5*time.Second)

	// All four turn events flow before the exit — the early end_turn messages
	// did not cut it short.
	want := []string{"user", "assistant", "user", "assistant", "subprocess.exit"}
	if !eq(types(got), want) {
		t.Fatalf("got %v, want %v", types(got), want)
	}
}

func TestTailTurn_ByteOffsetSkipsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	old := `{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"OLD"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	off, _ := os.Stat(path)

	go appendLines(path, 15*time.Millisecond,
		`{"type":"user","message":{"role":"user","content":"new"}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"NEW"}]}}`,
		`{"type":"system","subtype":"turn_duration","durationMs":42}`,
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

// idleCfg arms the idle fallback with a short quiet window and a scripted
// paneBusy: successive probes pop verdicts from busy (true = spinner visible);
// past the end the pane reads idle.
func idleCfg(busy ...bool) tailConfig {
	i := 0
	return tailConfig{
		poll:       10 * time.Millisecond,
		appearWait: 2 * time.Second,
		quiet:      60 * time.Millisecond,
		probeEvery: 10 * time.Millisecond,
		paneBusy: func() bool {
			if i < len(busy) {
				i++
				return busy[i-1]
			}
			return false
		},
	}
}

// A TUI-local command like /model writes nothing to the jsonl at all. With the
// pane idle, the tailer must finalize the turn instead of waiting forever on a
// turn_duration that isn't coming.
func TestTailTurn_IdleFallback_NoJsonlAtAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"mode"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	off, _ := os.Stat(path)

	ch := tailTurn(context.Background(), path, off.Size(), nil, idleCfg())
	got := collect(t, ch, 2*time.Second)
	if !eq(types(got), []string{"subprocess.exit"}) {
		t.Fatalf("got %v, want [subprocess.exit]", types(got))
	}
}

func TestTailTurn_UnconfirmedSubmissionIsVisibleFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := idleCfg()
	cfg.idleReason = "submission_unconfirmed"
	got := collect(t, tailTurn(context.Background(), path, 0, nil, cfg), 2*time.Second)
	if !eq(types(got), []string{"error", "subprocess.exit"}) {
		t.Fatalf("got %v, want visible error followed by exit", types(got))
	}
	if !strings.Contains(string(got[1].Raw), "submission_unconfirmed") {
		t.Fatalf("exit lacks reason: %s", got[1].Raw)
	}
}

func TestTailTurn_CodexAbortTerminatesAfterActivityLatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := idleCfg()
	cfg.turnActivity = codexrollout.IsTurnActivity
	cfg.turnComplete = codexrollout.IsTurnComplete
	cfg.turnAborted = codexrollout.IsTurnAborted
	go appendLines(path, 5*time.Millisecond,
		`{"type":"event_msg","payload":{"type":"task_started"}}`,
		`{"type":"event_msg","payload":{"type":"turn_aborted"}}`,
	)
	got := collect(t, tailTurn(context.Background(), path, 0, nil, cfg), 2*time.Second)
	if !eq(types(got), []string{"event_msg", "event_msg", "error", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
}

// A send whose lines reached the log was definitely submitted: the fallback
// must finalize it as a benign local command, not a submission failure.
func TestTailTurn_UnconfirmedDowngradedWhenLogGrew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := idleCfg()
	cfg.idleReason = "submission_unconfirmed"
	go appendLines(path, 5*time.Millisecond, `{"type":"user","isMeta":true}`)
	got := collect(t, tailTurn(context.Background(), path, 0, nil, cfg), 2*time.Second)
	if !eq(types(got), []string{"user", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
	if !strings.Contains(string(got[1].Raw), "local_command") {
		t.Fatalf("exit reason not downgraded: %s", got[1].Raw)
	}
}

func TestTailTurn_SeekErrorEmitsErrorAndExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := collect(t, tailTurn(context.Background(), path, -1, nil, fastCfg()), 2*time.Second)
	if !eq(types(got), []string{"error", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
}

// Some local commands (! bash mode, /compact's echo) write a few isMeta "user"
// lines but never an assistant line or turn_duration. They must stream through
// and then finalize via the fallback.
func TestTailTurn_IdleFallback_MetaUserLinesOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	go appendLines(path, 5*time.Millisecond,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>…</local-command-caveat>"}}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"<bash-input>ls</bash-input>"}}`,
	)

	ch := tailTurn(context.Background(), path, 0, nil, idleCfg())
	got := collect(t, ch, 2*time.Second)
	if !eq(types(got), []string{"user", "user", "subprocess.exit"}) {
		t.Fatalf("got %v, want [user user subprocess.exit]", types(got))
	}
}

// The submit→first-assistant window routinely runs tens of seconds with a
// silent jsonl; the spinner on the pane is what holds the fallback off. A busy
// pane must never finalize, no matter how long the log is quiet.
func TestTailTurn_IdleFallback_SpinnerHoldsThroughSlowFirstToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := idleCfg()
	cfg.paneBusy = func() bool { return true } // spinner never leaves the pane

	// User line lands, then a long silence (>> quiet) before the model replies.
	go func() {
		appendLines(path, 5*time.Millisecond, `{"type":"user","message":{"role":"user","content":"hi"}}`)
		time.Sleep(300 * time.Millisecond)
		appendLines(path, 5*time.Millisecond,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`,
			`{"type":"system","subtype":"turn_duration","durationMs":42}`,
		)
	}()

	ch := tailTurn(context.Background(), path, 0, nil, cfg)
	got := collect(t, ch, 2*time.Second)
	if !eq(types(got), []string{"user", "assistant", "subprocess.exit"}) {
		t.Fatalf("got %v, want the full turn (no premature exit)", types(got))
	}
}

// Once an assistant line has landed, the latch is armed: even a pane that reads
// idle (e.g. the spinner string changed in a future claude) must not finalize —
// only turn_duration may. The mid-silence assistant line after the quiet window
// proves the tailer was still alive.
func TestTailTurn_IdleFallback_ActivityLatchDisablesFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := idleCfg()
	cfg.paneBusy = func() bool { return false } // pane always reads idle

	go func() {
		appendLines(path, 5*time.Millisecond,
			`{"type":"user","message":{"role":"user","content":"hi"}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}`,
		)
		time.Sleep(300 * time.Millisecond) // long tool run, silent jsonl, idle-looking pane
		appendLines(path, 5*time.Millisecond,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
			`{"type":"system","subtype":"turn_duration","durationMs":42}`,
		)
	}()

	ch := tailTurn(context.Background(), path, 0, nil, cfg)
	got := collect(t, ch, 2*time.Second)
	if !eq(types(got), []string{"user", "assistant", "assistant", "subprocess.exit"}) {
		t.Fatalf("got %v, want the full turn (latch must hold)", types(got))
	}
}

func TestIsTurnActivity(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{`{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"hi"}]}}`, true},
		{`{"type":"assistant","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"No response requested."}]}}`, false},
		{`{"type":"assistant","isApiErrorMessage":true,"message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"API Error: 500"}]}}`, false},
		{`{"type":"user","message":{"role":"user","content":"hi"}}`, false},
		{`{"type":"user","isMeta":true,"message":{"role":"user","content":"<bash-input>ls</bash-input>"}}`, false},
		{`{"type":"system","subtype":"turn_duration","durationMs":42}`, false},
		{`not json`, false},
	} {
		if got := isTurnActivity([]byte(tc.line)); got != tc.want {
			t.Errorf("isTurnActivity(%s) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// A single idle sighting can be a mid-repaint frame with the spinner briefly
// absent; only consecutive idle probes finalize. busy,idle,busy,idle,idle →
// exit on the second consecutive idle, not the first transient one.
func TestTailTurn_IdleFallback_NeedsConsecutiveIdleProbes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	probes := 0
	cfg := idleCfg()
	cfg.paneBusy = func() bool {
		probes++
		return probes == 1 || probes == 3 // idle at 2, then busy again, then idle 4,5…
	}

	ch := tailTurn(context.Background(), path, 0, nil, cfg)
	got := collect(t, ch, 2*time.Second)
	if !eq(types(got), []string{"subprocess.exit"}) {
		t.Fatalf("got %v, want [subprocess.exit]", types(got))
	}
	if probes < 5 {
		t.Fatalf("exited after %d probes; a transient idle frame must not finalize", probes)
	}
}

// Cancelling a send before the jsonl appears (a fast cancel on a brand-new
// session) must still emit subprocess.exit — otherwise the web UI never
// finalizes the turn and leaves send disabled until a manual refresh.
func TestTailTurn_CancelBeforeFileAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	// Long appearWait so the only way out is the cancel, not the timeout.
	ch := tailTurn(ctx, path, 0, nil, tailConfig{poll: 10 * time.Millisecond, appearWait: 10 * time.Second})
	cancel()
	got := collect(t, ch, 2*time.Second)
	if !eq(types(got), []string{"subprocess.exit"}) {
		t.Fatalf("got %v, want [subprocess.exit]", types(got))
	}
}
