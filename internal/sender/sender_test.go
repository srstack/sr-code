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
	tm := timing{
		spawnSettle:   10 * time.Millisecond,
		trustToInject: 5 * time.Millisecond,
		warmSettle:    5 * time.Millisecond,
		resumeReady:   100 * time.Millisecond,
		confirm:       1 * time.Second,
		poll:          10 * time.Millisecond,
	}
	p := newPool(runner, "claude", nil, nil, 8, quietLogger())
	s := &Sender{
		pool:        p,
		backend:     claudeBackend{p: p, t: tm, projectsDir: dir, claudeCmd: "claude"},
		projectsDir: dir,
		logger:      quietLogger(),
		t:           tm,
		tail:        tailConfig{poll: 10 * time.Millisecond, appearWait: 2 * time.Second},
	}
	return s, filepath.Join(sub, id+".jsonl")
}

// idleComposer is a fake capture of claude's mounted empty input box in its
// real shape: two "─" rules around the "❯" prompt, a footer, then the blank
// rows tmux pads below the content. composerReady keys off the rule/❯/rule
// sandwich, not the footer.
const idleComposer = "────────────────────────────────────\n" +
	"❯\u00a0\n" + // real claude renders a non-breaking space after the prompt
	"────────────────────────────────────\n" +
	"  ? for shortcuts · ← for agents\n" +
	"\n\n\n\n\n"

var turnLines = []string{
	`{"type":"user","message":{"role":"user","content":"hi"}}`,
	`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"hello"}]}}`,
	`{"type":"system","subtype":"turn_duration","durationMs":42}`,
}

func TestSend_ResumeStreamsTurn(t *testing.T) {
	// Pane already at the idle input box: no resume chooser to answer.
	f := &fakeTmux{captureOut: idleComposer}
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

	ch, err := s.SendNew(context.Background(), "new-1", "hi", "/work", "")
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

func TestSend_ResumeAnswersChooserWithFullSession(t *testing.T) {
	// A long resume opens the chooser with the "summary" option highlighted;
	// usher must step the arrow down to "full session as-is", never a bare
	// Enter (which would pick the highlighted summary default).
	// The chooser shows first; once usher steps the arrow Down toward
	// full-session, the pane gives way to the mounted composer (readiness).
	f := &fakeTmux{
		captureOut: "❯ 1. Resume from summary (recommended)\n" +
			"  2. " + resumeChooserMarker + "\n  3. Don't ask me again\n",
		captureAfterDown: idleComposer,
	}
	s, path := testSender(t, f, "resume-1")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	ch, err := s.Send(context.Background(), "resume-1", "hi", "/work")
	if err != nil {
		t.Fatal(err)
	}
	// Write the turn only after the prompt is injected, as claude would — so the
	// pre-inject offset is captured before these lines (the chooser wait delays
	// the inject, which would otherwise race the turn past the offset).
	go func() {
		for f.countCmd("paste-buffer") < 1 {
			time.Sleep(2 * time.Millisecond)
		}
		appendLines(path, 10*time.Millisecond, turnLines...)
	}()

	got := collect(t, ch, 6*time.Second)
	if !eq(types(got), []string{"subprocess.started", "user", "assistant", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
	// Down is unique to the chooser path (inject never sends it), so its
	// presence proves the chooser was detected and answered toward full-session.
	if !cmdMatches(f, "send-keys", "Down") {
		t.Fatalf("resume chooser should be answered by stepping to full-session; cmds=%v", f.cmds)
	}
}

func TestChooserArrowOn(t *testing.T) {
	// The chooser's option strings appear verbatim in a session's own
	// transcript; a loose Contains(marker) match fired keystrokes into the
	// prompt box during the boot frames before the input footer rendered. The
	// arrow-row match must require the selection arrow on the SAME line.
	transcript := "  we replaced the blind Down with Resume full session as-is handling\n" +
		"❯\u00a0\n" + // the idle prompt's own arrow, on its own line
		"  ? for shortcuts"
	if chooserArrowOn(transcript, resumeChooserMarker) {
		t.Fatal("must not match option text in transcript that lacks the arrow on its line")
	}
	chooser := "❯ 1. Resume from summary (recommended)\n" +
		"  2. Resume full session as-is\n  3. Don't ask me again"
	if !chooserArrowOn(chooser, resumeSummaryMarker) {
		t.Fatal("must match the arrow on the highlighted summary default")
	}
	if chooserArrowOn(chooser, resumeChooserMarker) {
		t.Fatal("must not report full-session selected while the arrow is on summary")
	}
}

func TestSend_ResumeNotReadyEmitsError(t *testing.T) {
	// The composer never appears (and there's no chooser to answer): waitReady
	// must time out and the turn must surface a visible error rather than
	// blind-pasting the prompt into an unknown screen.
	f := &fakeTmux{captureOut: "loading session…\n"}
	s, path := testSender(t, f, "stuck-1")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	ch, err := s.Send(context.Background(), "stuck-1", "hi", "/work")
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch, 6*time.Second)
	if !eq(types(got), []string{"subprocess.started", "error"}) {
		t.Fatalf("got %v, want a visible error and no turn", types(got))
	}
	if n := f.countCmd("paste-buffer"); n != 0 {
		t.Fatalf("must not paste a prompt when the box never readied; got %d injects", n)
	}
}

func TestSend_CancelDuringWaitReadyEmitsExit(t *testing.T) {
	// A cold resume still settling (composer not yet up) when the user hits ESC
	// / cancel: waitReady is interrupted by the cancelled ctx before the tailer
	// ever runs. The turn must still end with subprocess.exit, or the web UI
	// leaves send disabled until a manual refresh.
	f := &fakeTmux{captureOut: "loading session…\n"}
	s, path := testSender(t, f, "esc-1")
	// Keep waitReady blocked (not timing out) until the cancel lands, so we
	// exercise the cancel path, not the error path.
	tm := s.t
	tm.resumeReady = 5 * time.Second
	s.t = tm
	s.backend = claudeBackend{p: s.pool, t: tm, projectsDir: s.projectsDir, claudeCmd: "claude"}
	if err := os.WriteFile(path, []byte(`{"type":"mode"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := s.Send(ctx, "esc-1", "hi", "/work")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(60 * time.Millisecond) // let it emit started and enter waitReady
		cancel()
	}()

	got := collect(t, ch, 6*time.Second)
	ts := types(got)
	if len(ts) == 0 || ts[len(ts)-1] != "subprocess.exit" {
		t.Fatalf("cancel during waitReady must end with subprocess.exit; got %v", ts)
	}
	if n := f.countCmd("paste-buffer"); n != 0 {
		t.Fatalf("must not paste a prompt when cancelled before readiness; got %d injects", n)
	}
}

func TestSend_CancelMidTurnEmitsExit(t *testing.T) {
	// The case actually hit in practice: a warm/active session streaming a turn
	// when the user cancels (ESC). The tailer emits subprocess.exit, but the
	// run goroutine forwards events through the now-cancelled ctx, whose select
	// can drop that exit (~50% — "sometimes the button doesn't recover"). The
	// cancel-path defer must still deliver a terminal exit.
	f := &fakeTmux{captureOut: idleComposer}
	s, path := testSender(t, f, "mid-1")
	if err := os.WriteFile(path, []byte(`{"type":"mode"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := s.Send(ctx, "mid-1", "hi", "/work")
	if err != nil {
		t.Fatal(err)
	}
	// A partial turn with NO turn_duration, so the tailer stays running until we
	// cancel it.
	go appendLines(path, 20*time.Millisecond,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}`,
	)
	go func() {
		time.Sleep(120 * time.Millisecond) // well past waitReady; mid-tail
		cancel()
	}()

	got := collect(t, ch, 6*time.Second)
	ts := types(got)
	if len(ts) == 0 || ts[len(ts)-1] != "subprocess.exit" {
		t.Fatalf("cancel mid-turn must end with subprocess.exit; got %v", ts)
	}
}

func TestComposerReady(t *testing.T) {
	// The mounted composer — even though tmux pads ~28 blank rows below it, so
	// it sits at the bottom of the CONTENT, not the captured grid (real case).
	if !composerReady(idleComposer) {
		t.Fatal("the mounted empty composer must be detected past the blank padding")
	}
	// A rendered transcript command keeps the "❯" glyph but carries trailing
	// text, so the strict prompt match must not treat it as the live prompt even
	// when rules happen to sit around it.
	cmd := strings.Repeat("─", 40) + "\n❯ /exit\n" + strings.Repeat("─", 40) + "\n"
	if composerReady(cmd) {
		t.Fatal(`"❯ /exit" with trailing text must not match the empty prompt`)
	}
	// A transcript user line "❯ ────" (prompt glyph + a dash run) sandwiched by
	// real rules must not match: the cutset must not strip "─", or it would
	// collapse to a bare "❯" and false-positive (the test-script "────" case).
	dashed := strings.Repeat("─", 40) + "\n❯ " + strings.Repeat("─", 12) + "\n" + strings.Repeat("─", 40) + "\n"
	if composerReady(dashed) {
		t.Fatal(`"❯ ────" must not collapse to the empty prompt`)
	}
	// A ">" blockquote replayed in the transcript, with no rules around it, must
	// not look like the composer — the failure mode the footer-string match had.
	quote := "  Earlier the model wrote:\n  > do the thing\n  and then stopped\n"
	if composerReady(quote) {
		t.Fatal("a bare > line without rules must not match")
	}
	// The composer scrolled above the scan window (lots of content below it)
	// must not match — only the live composer sits at the bottom of the content.
	scrolled := idleComposer + strings.Repeat("filler\n", composerScanLines)
	if composerReady(scrolled) {
		t.Fatal("a composer outside the bottom scan window must not match")
	}
	// The resume chooser is not a composer (its "❯" carries the option text).
	chooser := "❯ 1. Resume from summary\n  2. Resume full session as-is\n"
	if composerReady(chooser) {
		t.Fatal("the resume chooser must not match the composer")
	}
}

func TestSend_InjectsOnceNoRetry(t *testing.T) {
	// The landed-oracle/re-inject loop is gone: exactly one paste per send, even
	// when the user turn lands slowly (here, after a delay).
	f := &fakeTmux{captureOut: idleComposer}
	s, path := testSender(t, f, "once-1")
	if err := os.WriteFile(path, []byte(`{"type":"mode"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ch, err := s.Send(context.Background(), "once-1", "hi", "/work")
	if err != nil {
		t.Fatal(err)
	}
	go appendLines(path, 120*time.Millisecond, turnLines...)

	got := collect(t, ch, 6*time.Second)
	if !eq(types(got), []string{"subprocess.started", "user", "assistant", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
	if n := f.countCmd("paste-buffer"); n != 1 {
		t.Fatalf("expected exactly one inject, got %d", n)
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
