package hook

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestManager_SubmitAndRespond(t *testing.T) {
	m := New()

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
	m := New()
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
	m := New()
	if err := m.Respond("nope", Response{Behavior: "allow"}); err == nil {
		t.Error("expected error")
	}
}

func TestManager_RespondTwice(t *testing.T) {
	m := New()
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

func TestManager_ConcurrentSubmits(t *testing.T) {
	m := New()
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
