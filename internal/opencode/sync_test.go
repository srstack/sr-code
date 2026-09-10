package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeShadow writes a fake shadow transcript: one content line plus the
// given sync-meta fields (nil metaMap = pre-marker shadow).
func writeShadow(t *testing.T, metaMap map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ses_test.jsonl")
	content := `{"type":"user","sessionId":"ses_test","message":{"role":"user","content":"hi"}}` + "\n"
	if metaMap != nil {
		metaMap["type"] = "system"
		metaMap["subtype"] = "sync-meta"
		metaMap["sessionId"] = "ses_test"
		if _, ok := metaMap["metaV"]; !ok {
			metaMap["metaV"] = syncMetaVersion
		}
		raw, err := json.Marshal(metaMap)
		if err != nil {
			t.Fatal(err)
		}
		content += string(raw) + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadSyncMeta(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, ok := readSyncMeta(filepath.Join(t.TempDir(), "nope.jsonl")); ok {
			t.Error("ok=true for missing file")
		}
	})
	t.Run("no marker", func(t *testing.T) {
		if _, ok := readSyncMeta(writeShadow(t, nil)); ok {
			t.Error("ok=true for marker-less shadow")
		}
	})
	t.Run("round trip", func(t *testing.T) {
		path := writeShadow(t, map[string]any{
			"sourceUpdated":  1234567890,
			"sourceActivity": 42,
			"inflight":       true,
			"inflightSince":  1234560000,
			"fetchHash":      "deadbeef",
		})
		m, ok := readSyncMeta(path)
		if !ok {
			t.Fatal("marker not found")
		}
		if m.sourceUpdated != 1234567890 || m.sourceActivity != 42 ||
			!m.inflight || m.inflightSince != 1234560000 || m.fetchHash != "deadbeef" {
			t.Errorf("parsed meta = %+v", m)
		}
	})
}

func TestShadowFresh(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour).UnixMilli()      // settled: long past syncSettleWindow
	recent := now.Add(-time.Minute).UnixMilli() // inside syncSettleWindow

	cases := []struct {
		name  string
		meta  map[string]any
		entry sessionEntry
		want  bool
	}{
		{
			name:  "no marker is stale",
			meta:  nil,
			entry: sessionEntry{Updated: old},
			want:  false,
		},
		{
			// Pre-inflight-tracking meta (written by an older usher): force
			// one rewrite so the session gets re-evaluated instead of sitting
			// settled behind a frozen native updated.
			name:  "old-format marker is stale once",
			meta:  map[string]any{"metaV": 1, "sourceUpdated": old},
			entry: sessionEntry{Updated: old},
			want:  false,
		},
		{
			name:  "native moved past shadow",
			meta:  map[string]any{"sourceUpdated": old - 1000, "contentChangedAt": old},
			entry: sessionEntry{Updated: old},
			want:  false,
		},
		{
			// updated froze an hour ago, but the fetched content changed a
			// minute ago — a turn is plainly in progress; keep refetching.
			name:  "recent content change keeps refetching",
			meta:  map[string]any{"sourceUpdated": old, "contentChangedAt": recent},
			entry: sessionEntry{Updated: old},
			want:  false,
		},
		{
			name:  "settled shadow is fresh",
			meta:  map[string]any{"sourceUpdated": old, "contentChangedAt": old},
			entry: sessionEntry{Updated: old},
			want:  true,
		},
		{
			// The core regression: updated froze an hour ago, but the last
			// fetch saw the turn still running — the shadow is by definition
			// behind and must keep refetching.
			name: "inflight turn never settles on frozen updated",
			meta: map[string]any{
				"sourceUpdated": old,
				"inflight":      true,
				"inflightSince": now.UnixMilli(),
				"fetchHash":     "abc",
			},
			entry: sessionEntry{Updated: old},
			want:  false,
		},
		{
			// A dead turn (killed mid-tool, status frozen) must eventually
			// settle, or it would be refetched every tick forever.
			name: "inflight past the ceiling settles",
			meta: map[string]any{
				"sourceUpdated": old,
				"inflight":      true,
				"inflightSince": now.Add(-time.Hour).UnixMilli(),
				"fetchHash":     "abc",
			},
			entry: sessionEntry{Updated: old},
			want:  true,
		},
		{
			name:  "activity drift refetches",
			meta:  map[string]any{"sourceUpdated": old, "sourceActivity": 90, "contentChangedAt": old},
			entry: sessionEntry{Updated: old, Activity: 100},
			want:  false,
		},
		{
			name:  "activity match stays fresh",
			meta:  map[string]any{"sourceUpdated": old, "sourceActivity": 100, "contentChangedAt": old},
			entry: sessionEntry{Updated: old, Activity: 100},
			want:  true,
		},
		{
			name:  "activity check skipped without signals",
			meta:  map[string]any{"sourceUpdated": old},
			entry: sessionEntry{Updated: old, Activity: 100},
			want:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeShadow(t, tc.meta)
			if got := shadowFresh(path, tc.entry); got != tc.want {
				t.Errorf("shadowFresh = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSyncMetaLineInflightClock(t *testing.T) {
	s := sessionEntry{ID: "ses_x", Updated: 1000}
	parse := func(raw json.RawMessage) syncMeta {
		t.Helper()
		var ev struct {
			Inflight      bool   `json:"inflight"`
			InflightSince int64  `json:"inflightSince"`
			FetchHash     string `json:"fetchHash"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		return syncMeta{inflight: ev.Inflight, inflightSince: ev.InflightSince, fetchHash: ev.FetchHash}
	}

	// First inflight observation starts the clock.
	m1 := parse(syncMetaLine(s, true, syncMeta{}, "h1"))
	if !m1.inflight || m1.inflightSince == 0 {
		t.Fatalf("first inflight meta = %+v", m1)
	}
	// No content progress: the clock keeps running from the first observation.
	stalled := syncMeta{inflight: true, inflightSince: 12345, fetchHash: "h1"}
	m2 := parse(syncMetaLine(s, true, stalled, "h1"))
	if m2.inflightSince != 12345 {
		t.Errorf("stalled turn reset the clock: %d", m2.inflightSince)
	}
	// Content progress resets the clock.
	m3 := parse(syncMetaLine(s, true, stalled, "h2"))
	if m3.inflightSince == 12345 {
		t.Error("progress did not reset the clock")
	}
	// Turn completed: no inflight marker.
	m4 := parse(syncMetaLine(s, false, stalled, "h2"))
	if m4.inflight || m4.inflightSince != 0 {
		t.Errorf("completed turn meta = %+v", m4)
	}
}

func v1Msg(role string, parts ...exportPart) exportMessage {
	var m exportMessage
	m.Info.ID = "msg_1"
	m.Info.Role = role
	m.Parts = parts
	return m
}

func TestV1TurnInflight(t *testing.T) {
	runningTool := exportPart{Type: "tool", State: &toolState{Status: "running"}}
	doneTool := exportPart{Type: "tool", State: &toolState{Status: "completed"}}
	stepStart := exportPart{Type: "step-start"}
	stepFinish := exportPart{Type: "step-finish"}

	cases := []struct {
		name string
		last exportMessage
		want bool
	}{
		{"no messages", exportMessage{}, false},
		{"unanswered user prompt", v1Msg("user"), true},
		{"assistant mid first step", v1Msg("assistant", stepStart), true},
		{"assistant message row without parts yet", v1Msg("assistant"), true},
		{"running tool", v1Msg("assistant", stepStart, runningTool), true},
		{"pending tool", v1Msg("assistant", stepStart, exportPart{Type: "tool", State: &toolState{Status: "pending"}}), true},
		{"completed turn", v1Msg("assistant", stepStart, doneTool, stepFinish), false},
		{"completed multi-step turn", v1Msg("assistant", stepStart, stepFinish, stepStart, doneTool, stepFinish), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := v1TurnInflight(tc.last); got != tc.want {
				t.Errorf("v1TurnInflight = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestV2TurnInflight(t *testing.T) {
	cases := []struct {
		name string
		msgs []v2Message
		want bool
	}{
		{"no messages", nil, false},
		{"unanswered user prompt", []v2Message{{Type: "user"}}, true},
		{"assistant without content yet", []v2Message{{Type: "user"}, {Type: "assistant"}}, true},
		{
			"running tool",
			[]v2Message{{Type: "assistant", Content: []v2Content{
				{Type: "text", Text: "working"},
				{Type: "tool", State: &v2ToolState{Status: "running"}},
			}}},
			true,
		},
		{
			"completed turn",
			[]v2Message{{Type: "user"}, {Type: "assistant", Content: []v2Content{
				{Type: "text", Text: "done"},
				{Type: "tool", State: &v2ToolState{Status: "completed"}},
			}}},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := v2TurnInflight(tc.msgs); got != tc.want {
				t.Errorf("v2TurnInflight = %v, want %v", got, tc.want)
			}
		})
	}
}
