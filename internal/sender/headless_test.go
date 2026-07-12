package sender

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestDrainTailKeepsFinalPartsAfterProtocolResult(t *testing.T) {
	events := make(chan StreamEvent, 2)
	out := make(chan StreamEvent, 2)
	tailCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		// Model a protocol result arriving just before the tailer's next poll.
		time.Sleep(20 * time.Millisecond)
		events <- StreamEvent{Type: "part"}
		events <- StreamEvent{Type: "subprocess.exit"}
		close(events)
	}()

	drainTail(context.Background(), out, events, cancel, time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)), "timeout", "session_id", "s1")

	if tailCtx.Err() != nil {
		t.Fatal("tail was cancelled before its completion marker")
	}
	if first, second := (<-out).Type, (<-out).Type; first != "part" || second != "subprocess.exit" {
		t.Fatalf("events = [%s %s], want [part subprocess.exit]", first, second)
	}
}
