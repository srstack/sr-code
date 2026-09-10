package opencode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestShadowFetchDeterministic guards the sync settle clock: fetching the
// same native state twice must produce byte-identical shadow output. When
// tool_result / turn_duration / sync-meta lines stamped randomHexID() into
// every fetched body, fetchHashSum changed on every fetch, contentChangedAt
// reset every tick, shadowFresh never returned true, and every opencode
// session was re-exported and rewritten every syncInterval forever (each
// rewrite also bumping the shadow mtime discovery sorts by).
func TestShadowFetchDeterministic(t *testing.T) {
	s := sessionEntry{ID: "ses_x", Title: "t", Directory: "/tmp/proj", Created: 1000, Updated: 2000}
	ts := eventTime(1500)
	st := &toolState{Status: "completed", Input: json.RawMessage(`{}`), Output: "done"}
	pp := partPayload{ID: "call_1", Tool: "read", CallID: "call_1", State: st}
	var msg exportMessage
	msg.Info.ID = "msg_1"
	msg.Info.Role = "assistant"

	// build mirrors the lines fetchSession writes for one user prompt plus
	// one assistant message with a completed tool call, ending in the
	// sync-meta footer.
	build := func(prev syncMeta) []byte {
		var buf []byte
		write := func(raw json.RawMessage) {
			if raw == nil {
				return
			}
			buf = append(buf, raw...)
			buf = append(buf, '\n')
		}
		write(userLineWithUUID(s.ID, s.Directory, "hi", ts, "msg_0"))
		write(assistantLineModel(s.ID, toolUseBlocks(pp), ts, msg, 0))
		write(toolResultLine(s.ID, s.Directory, pp, ts))
		write(turnCompleteLine(s.ID, ts, msg.Info.ID))
		write(syncMetaLine(s, false, prev, fetchHashSum(buf)))
		return buf
	}

	// First fetch: no prior meta.
	first := build(syncMeta{})
	path := filepath.Join(t.TempDir(), "ses_x.jsonl")
	if err := os.WriteFile(path, first, 0o644); err != nil {
		t.Fatal(err)
	}

	// Second fetch of the same native state: meta comes from the shadow tail.
	prev, ok := readSyncMeta(path)
	if !ok {
		t.Fatal("no sync meta after first fetch")
	}
	second := build(prev)
	if !bytes.Equal(first, second) {
		t.Errorf("re-fetch of unchanged state not byte-identical:\nfirst:  %s\nsecond: %s", first, second)
	}
	m1, _ := readSyncMeta(path)
	if err := os.WriteFile(path, second, 0o644); err != nil {
		t.Fatal(err)
	}
	m2, _ := readSyncMeta(path)
	if m1.fetchHash == "" || m1.fetchHash != m2.fetchHash {
		t.Errorf("fetchHash unstable: %q then %q", m1.fetchHash, m2.fetchHash)
	}
	if m2.contentChangedAt != m1.contentChangedAt {
		t.Errorf("unchanged content reset the settle clock: %d -> %d", m1.contentChangedAt, m2.contentChangedAt)
	}

	// With the settle window past and content unchanged, the shadow must
	// read as fresh — the state the bug made unreachable.
	settledPrev := syncMeta{
		version:          syncMetaVersion,
		sourceUpdated:    s.Updated,
		contentChangedAt: time.Now().Add(-2 * syncSettleWindow).UnixMilli(),
		fetchHash:        m1.fetchHash,
	}
	if err := os.WriteFile(path, build(settledPrev), 0o644); err != nil {
		t.Fatal(err)
	}
	if !shadowFresh(path, s) {
		t.Error("shadowFresh = false for a settled, unchanged shadow")
	}
}

// TestStableUUID pins the derivation contract: same keys in, same uuid out;
// an empty key (live runtime path with no stable identity) falls back to
// random so distinct lines never collapse onto one uuid.
func TestStableUUID(t *testing.T) {
	a := stableUUID("tool-result", "ses_x", "call_1")
	b := stableUUID("tool-result", "ses_x", "call_1")
	if a != b {
		t.Errorf("stableUUID not deterministic: %q vs %q", a, b)
	}
	if len(a) != 32 {
		t.Errorf("stableUUID = %q, want 32 hex chars like randomHexID", a)
	}
	if c := stableUUID("tool-result", "ses_x", "call_2"); c == a {
		t.Errorf("distinct keys collided: %q", a)
	}
	if r1, r2 := stableUUID("turn-complete", "ses_x", ""), stableUUID("turn-complete", "ses_x", ""); r1 == r2 {
		t.Error("empty key must fall back to random, got identical uuids")
	}
}
