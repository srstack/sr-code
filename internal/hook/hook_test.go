package hook

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManager_SubmitAndRespond(t *testing.T) {
	m := New("")

	var (
		gotResp Response
		gotErr  error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		gotResp, gotErr = m.Submit(context.Background(), Event{
			SessionID: "s1",
			Event:     "PreToolUse",
			ToolName:  "Bash",
		})
	}()

	id := waitForPending(t, m)
	if err := m.Respond(id, Response{Behavior: "allow", Reason: "ok"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Submit did not return")
	}
	if gotErr != nil {
		t.Fatalf("err = %v", gotErr)
	}
	if gotResp.Behavior != "allow" || gotResp.Reason != "ok" {
		t.Errorf("Resp = %+v", gotResp)
	}
	if len(m.List()) != 0 {
		t.Errorf("expected empty list after respond, got %d", len(m.List()))
	}
}

func TestManager_ContextCancelReleases(t *testing.T) {
	m := New("")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := m.Submit(ctx, Event{SessionID: "s", Event: "PreToolUse"})
		done <- err
	}()

	waitForPending(t, m)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected ctx error")
		}
	case <-time.After(time.Second):
		t.Fatal("Submit did not unblock on ctx cancel")
	}
}

func TestManager_RespondUnknown(t *testing.T) {
	m := New("")
	if err := m.Respond("nope", Response{Behavior: "allow"}); err == nil {
		t.Error("expected error")
	}
}

func TestManager_RespondTwice(t *testing.T) {
	m := New("")
	go func() { _, _ = m.Submit(context.Background(), Event{SessionID: "s"}) }()
	id := waitForPending(t, m)
	if err := m.Respond(id, Response{Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}
	// the entry will be deleted shortly after; second Respond should fail.
	// Loop a few ms to outlast the deletion goroutine.
	time.Sleep(50 * time.Millisecond)
	if err := m.Respond(id, Response{Behavior: "allow"}); err == nil {
		t.Error("expected error on second respond")
	}
}

func TestManager_RememberRule_Bash(t *testing.T) {
	m := New("")

	// First call: user allows with session scope.
	done := make(chan Response, 1)
	go func() {
		r, _ := m.Submit(context.Background(), Event{
			SessionID: "s",
			Event:     "PreToolUse",
			ToolName:  "Bash",
			ToolInput: []byte(`{"command":"git status"}`),
		})
		done <- r
	}()
	id := waitForPending(t, m)
	if err := m.Respond(id, Response{Behavior: "allow", Scope: "session"}); err != nil {
		t.Fatal(err)
	}
	<-done

	rules := m.ListRules()
	if len(rules) != 1 || rules[0].Matcher != "Bash(git:*)" || rules[0].Behavior != "allow" {
		t.Fatalf("unexpected rules: %+v", rules)
	}

	// Second call to the same session with another git command — should not
	// touch the UI. We assert by ensuring Submit returns immediately and no
	// pending entry is registered.
	r, err := m.Submit(context.Background(), Event{
		SessionID: "s",
		Event:     "PreToolUse",
		ToolName:  "Bash",
		ToolInput: []byte(`{"command":"git log --oneline"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Behavior != "allow" {
		t.Errorf("Behavior = %q", r.Behavior)
	}
	if !strings.Contains(r.Reason, "Bash(git:*)") {
		t.Errorf("Reason should reference rule, got %q", r.Reason)
	}
	if len(m.List()) != 0 {
		t.Errorf("expected no pending interactions; got %d", len(m.List()))
	}
}

func TestManager_RememberRule_NonBash(t *testing.T) {
	m := New("")
	done := make(chan struct{})
	go func() {
		_, _ = m.Submit(context.Background(), Event{
			SessionID: "s",
			ToolName:  "Read",
			ToolInput: []byte(`{"file_path":"/tmp/a.txt"}`),
		})
		close(done)
	}()
	id := waitForPending(t, m)
	if err := m.Respond(id, Response{Behavior: "allow", Scope: "session"}); err != nil {
		t.Fatal(err)
	}
	<-done

	// Another Read on a different file — auto-allowed because the matcher is
	// the bare tool name.
	r, err := m.Submit(context.Background(), Event{
		SessionID: "s",
		ToolName:  "Read",
		ToolInput: []byte(`{"file_path":"/etc/passwd"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Behavior != "allow" {
		t.Errorf("Behavior = %q", r.Behavior)
	}
}

func TestManager_RememberRule_DoesNotLeakAcrossSessions(t *testing.T) {
	m := New("")
	done := make(chan struct{})
	go func() {
		_, _ = m.Submit(context.Background(), Event{
			SessionID: "s1",
			ToolName:  "Bash",
			ToolInput: []byte(`{"command":"rm -rf /"}`),
		})
		close(done)
	}()
	id := waitForPending(t, m)
	_ = m.Respond(id, Response{Behavior: "allow", Scope: "session"})
	<-done

	// Different session — should still prompt.
	go func() { _, _ = m.Submit(context.Background(), Event{
		SessionID: "s2",
		ToolName:  "Bash",
		ToolInput: []byte(`{"command":"rm /tmp/x"}`),
	}) }()

	id2 := waitForPending(t, m)
	if id2 == "" {
		t.Fatal("expected new pending in different session")
	}
}

func TestManager_OnceScopeIsNotRemembered(t *testing.T) {
	m := New("")
	done := make(chan struct{})
	go func() {
		_, _ = m.Submit(context.Background(), Event{SessionID: "s", ToolName: "Read"})
		close(done)
	}()
	id := waitForPending(t, m)
	_ = m.Respond(id, Response{Behavior: "allow"}) // no scope
	<-done
	if rules := m.ListRules(); len(rules) != 0 {
		t.Errorf("once-scope must not be remembered, got %v", rules)
	}
}

func TestManager_ForgetSessionRules(t *testing.T) {
	m := New("")
	go func() { _, _ = m.Submit(context.Background(), Event{SessionID: "s", ToolName: "Read"}) }()
	id := waitForPending(t, m)
	_ = m.Respond(id, Response{Behavior: "allow", Scope: "session"})
	// give goroutine a beat to record
	time.Sleep(20 * time.Millisecond)
	if len(m.ListRules()) == 0 {
		t.Fatal("rule not stored")
	}
	m.ForgetSessionRules("s")
	if len(m.ListRules()) != 0 {
		t.Error("rule not cleared")
	}
}

func TestDeriveMatcher(t *testing.T) {
	cases := []struct {
		tool  string
		input string
		want  string
	}{
		{"Bash", `{"command":"git push origin main"}`, "Bash(git:*)"},
		{"Bash", `{"command":"  ls -la  "}`, "Bash(ls:*)"},
		{"Bash", `{"command":""}`, "Bash"},
		{"Bash", `{}`, "Bash"},
		{"Read", `{"file_path":"/x"}`, "Read"},
		{"", `{}`, ""},
	}
	for _, c := range cases {
		got := deriveMatcher(c.tool, json.RawMessage(c.input))
		if got != c.want {
			t.Errorf("deriveMatcher(%q,%q) = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
}

func TestMatchRule(t *testing.T) {
	cases := []struct {
		rule  Rule
		tool  string
		input string
		want  bool
	}{
		{Rule{Matcher: "Bash(git:*)"}, "Bash", `{"command":"git status"}`, true},
		{Rule{Matcher: "Bash(git:*)"}, "Bash", `{"command":"  git push"}`, true},
		{Rule{Matcher: "Bash(git:*)"}, "Bash", `{"command":"rm -rf /"}`, false},
		{Rule{Matcher: "Bash(git:*)"}, "Read", `{}`, false},
		{Rule{Matcher: "Read"}, "Read", `{"file_path":"/x"}`, true},
		{Rule{Matcher: "Read"}, "Write", `{}`, false},
	}
	for _, c := range cases {
		got := matchRule(c.rule, c.tool, json.RawMessage(c.input))
		if got != c.want {
			t.Errorf("matchRule(%+v, %q, %q) = %v, want %v", c.rule, c.tool, c.input, got, c.want)
		}
	}
}

func TestManager_ConcurrentSubmits(t *testing.T) {
	m := New("")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				_, _ = m.Submit(ctx, Event{SessionID: "s", Event: "PreToolUse"})
				close(done)
			}()
			// wait briefly then cancel
			time.Sleep(10 * time.Millisecond)
			cancel()
			<-done
		}()
	}
	wg.Wait()
	if n := len(m.List()); n != 0 {
		t.Errorf("leaked %d pending entries", n)
	}
}

func TestQuickDecide(t *testing.T) {
	m := New("")

	// Empty manager: no decision yet, caller must block on UI.
	if resp, ok := m.QuickDecide(Event{SessionID: "s", ToolName: "Read"}); ok {
		t.Errorf("expected (zero,false) on empty manager; got (%+v,%v)", resp, ok)
	}

	// Auto-approve settles instantly.
	m.SetAutoApprove("s", true)
	resp, ok := m.QuickDecide(Event{SessionID: "s", ToolName: "Bash"})
	if !ok || resp.Behavior != "allow" || resp.Reason != "auto-approve" {
		t.Errorf("auto-approve QuickDecide = (%+v,%v); want allow/auto-approve/true", resp, ok)
	}
	m.SetAutoApprove("s", false)

	// Remembered rule (deny) wins over absent auto-approve.
	go func() { _, _ = m.Submit(context.Background(), Event{SessionID: "s", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"rm -rf /"}`)}) }()
	id := waitForPending(t, m)
	_ = m.Respond(id, Response{Behavior: "deny", Scope: "session"})
	time.Sleep(20 * time.Millisecond)

	resp, ok = m.QuickDecide(Event{SessionID: "s", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"rm -rf /tmp"}`)})
	if !ok || resp.Behavior != "deny" {
		t.Errorf("remembered-deny QuickDecide = (%+v,%v); want deny/true", resp, ok)
	}

	// Remembered rule beats auto-approve (specific opt-outs survive a
	// later blanket trust toggle).
	m.SetAutoApprove("s", true)
	resp, ok = m.QuickDecide(Event{SessionID: "s", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"rm -rf /tmp"}`)})
	if !ok || resp.Behavior != "deny" {
		t.Errorf("rule-vs-auto-approve QuickDecide = (%+v,%v); want deny/true", resp, ok)
	}
}

// AskUserQuestion must never be settled by QuickDecide: a bare "allow" from
// auto-approve or a remembered rule would let the tool block on the pane TUI
// selector instead of being answered through the web UI.
func TestQuickDecideSkipsAskUserQuestion(t *testing.T) {
	m := New("")
	m.SetAutoApprove("s", true)
	if resp, ok := m.QuickDecide(Event{SessionID: "s", ToolName: "AskUserQuestion"}); ok {
		t.Errorf("AskUserQuestion under auto-approve = (%+v,%v); want (zero,false)", resp, ok)
	}
	// Sanity: a different tool in the same session is still auto-approved.
	if _, ok := m.QuickDecide(Event{SessionID: "s", ToolName: "Read"}); !ok {
		t.Errorf("Read under auto-approve should still settle")
	}
}

func TestAutoApprovePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auto-approve.json")

	// Set two sessions in the first manager.
	m1 := New(path)
	m1.SetAutoApprove("session-a", true)
	m1.SetAutoApprove("session-b", true)
	m1.SetAutoApprove("session-a", false) // remove a; only b should persist

	// A fresh manager pointed at the same file should rehydrate state.
	m2 := New(path)
	if m2.IsAutoApprove("session-a") {
		t.Errorf("session-a should NOT be auto-approve after rehydrate")
	}
	if !m2.IsAutoApprove("session-b") {
		t.Errorf("session-b should remain auto-approve after rehydrate")
	}

	// Toggling on m2 must round-trip via disk.
	m2.SetAutoApprove("session-c", true)
	m3 := New(path)
	if !m3.IsAutoApprove("session-c") {
		t.Errorf("session-c should survive a second rehydrate")
	}
}

func waitForPending(t *testing.T, m *Manager) string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		list := m.List()
		if len(list) > 0 {
			return list[0].ID
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no pending interaction registered")
	return ""
}
