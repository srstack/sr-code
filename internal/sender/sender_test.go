package sender

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func absFake(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestSender(t *testing.T, fakeName string) *Sender {
	t.Helper()
	return New(absFake(t, fakeName), "", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

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
			t.Fatalf("timed out after %s collecting events; got %d so far", timeout, len(got))
		}
	}
}

func TestSender_StreamsAndExits(t *testing.T) {
	s := newTestSender(t, "fake-claude")
	ch, err := s.Send(context.Background(), "fake-session", "hi", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, ch, 5*time.Second)

	wantTypes := []string{
		"subprocess.started",
		"system",
		"stream_event",
		"stream_event",
		"assistant",
		"result",
		"subprocess.exit",
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(wantTypes), eventTypes(events))
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event[%d] type = %q, want %q", i, events[i].Type, want)
		}
		if len(events[i].Raw) == 0 {
			t.Errorf("event[%d] (%s) Raw is empty", i, events[i].Type)
		}
	}
}

func TestSender_BadCommandReturnsError(t *testing.T) {
	s := New("/nonexistent/binary", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := s.Send(context.Background(), "x", "hi", t.TempDir())
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestSender_CancelInterruptsSubprocess(t *testing.T) {
	s := newTestSender(t, "fake-claude-slow")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := s.Send(ctx, "fake-slow", "hi", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the first real event so we know the subprocess is running.
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no first event")
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no second event")
	}

	cancel()

	// The subprocess sleeps 30s; with SIGINT + WaitDelay, it should exit
	// quickly. Allow some margin.
	deadline := time.After(8 * time.Second)
	var sawExit bool
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				if !sawExit {
					t.Fatal("channel closed without subprocess.exit event")
				}
				return
			}
			if ev.Type == "subprocess.exit" {
				sawExit = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for cancel to take effect")
		}
	}
}

func TestParseStreamLine(t *testing.T) {
	ev, err := parseStreamLine([]byte(`{"type":"system","subtype":"init"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "system" {
		t.Errorf("Type = %q", ev.Type)
	}
	if string(ev.Raw) != `{"type":"system","subtype":"init"}` {
		t.Errorf("Raw = %s", ev.Raw)
	}
}

func TestParseStreamLine_BadJSON(t *testing.T) {
	if _, err := parseStreamLine([]byte("not json")); err == nil {
		t.Error("expected error")
	}
}

func eventTypes(evs []StreamEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}
