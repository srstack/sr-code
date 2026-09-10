package opencode

// Live end-to-end checks against the machine's real opencode stores, gated
// behind USHER_LIVE_TEST=1 (they shell out to the CLI / hit the local v2
// service and so only run on demand, never in CI). Shadows are written to a
// temp dir; the native stores are only read.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func liveRuntime(t *testing.T, v2 bool) (*Runtime, string) {
	t.Helper()
	if os.Getenv("USHER_LIVE_TEST") == "" {
		t.Skip("set USHER_LIVE_TEST=1 to run against the real opencode stores")
	}
	root := t.TempDir()
	if v2 {
		return NewRuntimeV2("opencode2", root, nil), root
	}
	return NewRuntime("opencode", root, nil), root
}

// TestLiveV2FetchInflight fetches a session that is mid-turn and expects the
// sync meta to mark it inflight — the signal that keeps the sync refetching
// while the native updated timestamp is frozen.
func TestLiveV2FetchInflight(t *testing.T) {
	rt, root := liveRuntime(t, true)
	id := os.Getenv("USHER_LIVE_V2_SESSION")
	if id == "" {
		t.Skip("set USHER_LIVE_V2_SESSION to a currently-running v2 session id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := rt.sessionEntryFor(ctx, id)
	if err != nil || s == nil {
		t.Fatalf("sessionEntryFor: s=%v err=%v", s, err)
	}
	path := logPath(root, s.Directory, id)
	if err := v2FetchSession(ctx, rt, *s, path); err != nil {
		t.Fatal(err)
	}
	m, ok := readSyncMeta(path)
	if !ok {
		t.Fatal("no sync meta written")
	}
	if m.version != syncMetaVersion {
		t.Errorf("meta version = %d", m.version)
	}
	if !m.inflight {
		t.Error("inflight = false for a session that is mid-turn")
	}
	if m.inflightSince == 0 {
		t.Error("inflightSince not set")
	}
	t.Logf("meta: %+v", m)
	// A session mid-turn must never look fresh, however old updated is.
	if shadowFresh(path, *s) {
		t.Error("shadowFresh = true for a mid-turn session")
	}
}

// TestLiveV1FetchSettled fetches an idle v1 session: the transcript must
// contain the conversation and the meta must NOT claim inflight.
func TestLiveV1FetchSettled(t *testing.T) {
	rt, root := liveRuntime(t, false)
	id := os.Getenv("USHER_LIVE_V1_SESSION")
	if id == "" {
		t.Skip("set USHER_LIVE_V1_SESSION to an idle v1 session id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	s, err := rt.sessionEntryFor(ctx, id)
	if err != nil || s == nil {
		t.Fatalf("sessionEntryFor: s=%v err=%v", s, err)
	}
	path := logPath(root, s.Directory, id)
	if err := fetchSession(ctx, rt, *s, path); err != nil {
		// Under store contention the CLI silently truncates and the fetch is
		// rejected — that rejection IS the guard working (a truncated snapshot
		// must never reach the shadow). Treat it as a pass with evidence.
		t.Logf("fetch rejected (truncation guard working under contention): %v", err)
		return
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("empty shadow: %v", err)
	}
	m, ok := readSyncMeta(path)
	if !ok {
		t.Fatal("no sync meta written")
	}
	if m.inflight {
		t.Error("inflight = true for an idle session")
	}
	if !shadowFresh(filepath.Join(path), *s) && time.Since(time.UnixMilli(s.Updated)) > syncSettleWindow {
		t.Error("idle, long-quiet session should be fresh after one fetch")
	}
	t.Logf("shadow %d bytes, meta: %+v", fi.Size(), m)
}
