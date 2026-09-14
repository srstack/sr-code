package dsh

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
)

// ms builds the local-zone time ReadTurns derives from a dsh epoch-ms stamp.
func ms(v int64) time.Time { return time.UnixMilli(v) }

func readFixture(t *testing.T, name string) ([]core.Turn, int) {
	t.Helper()
	turns, total, err := Transcript{}.ReadTurns(filepath.Join("testdata", name), 0)
	if err != nil {
		t.Fatalf("ReadTurns(%s): %v", name, err)
	}
	return turns, total
}

func partTexts(parts []core.TurnPart) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p.Type+":"+p.Content)
	}
	return out
}

func TestReadTurnsBasic(t *testing.T) {
	turns, total := readFixture(t, "basic.jsonl")
	if total != 2 || len(turns) != 2 {
		t.Fatalf("got %d turns (total %d), want 2", len(turns), total)
	}
	u := turns[0]
	if u.Role != "user" || u.Content != "hello dsh" {
		t.Errorf("turn0 = %+v, want user %q", u, "hello dsh")
	}
	if !u.Time.Equal(ms(1789064460604)) {
		t.Errorf("user time = %v, want %v", u.Time, ms(1789064460604))
	}

	a := turns[1]
	if a.Role != "assistant" {
		t.Fatalf("turn1 role = %q, want assistant", a.Role)
	}
	if a.Model != "deepseek-chat" {
		t.Errorf("model = %q, want deepseek-chat", a.Model)
	}
	if len(a.Parts) != 1 || a.Parts[0].Type != "text" || a.Parts[0].Content != "hi there" {
		t.Errorf("parts = %+v, want a single text part", a.Parts)
	}
	if a.Usage == nil || *a.Usage != (core.TokenUsage{Input: 100, Output: 20, CacheRead: 5, CacheWrite: 2}) {
		t.Errorf("usage = %+v, want {100 20 5 2}", a.Usage)
	}
}

func TestReadTurnsToolsUsageDurations(t *testing.T) {
	turns, _ := readFixture(t, "tools.jsonl")
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 (user + one grouped assistant turn)", len(turns))
	}
	a := turns[1]
	// Model comes from the LAST assistant message; usage sums across the turn.
	if a.Model != "m2" {
		t.Errorf("model = %q, want m2 (last message wins)", a.Model)
	}
	if a.Usage == nil || *a.Usage != (core.TokenUsage{Input: 250, Output: 40, CacheRead: 7}) {
		t.Errorf("usage = %+v, want {250 40 7 0}", a.Usage)
	}
	if len(a.Parts) != 4 {
		t.Fatalf("parts = %+v, want 4", a.Parts)
	}
	if a.Parts[0].Type != "thinking" || a.Parts[0].Content != "need to look" {
		t.Errorf("part0 = %+v, want thinking", a.Parts[0])
	}
	if a.Parts[1].Type != "text" || a.Parts[1].Content != "checking" {
		t.Errorf("part1 = %+v, want text 'checking'", a.Parts[1])
	}
	tool := a.Parts[2]
	if tool.Type != "tool" || tool.ToolName != "bash" || tool.ToolTarget != "ls -la" || tool.ToolUseID != "c1" {
		t.Errorf("part2 = %+v, want bash tool targeting ls -la", tool)
	}
	if tool.Content != "out1\nout2" {
		t.Errorf("tool content = %q, want joined result text", tool.Content)
	}
	if tool.DurationMs != 500 {
		t.Errorf("tool duration = %d, want 500 (result.time - tool/call.time)", tool.DurationMs)
	}
	if a.Parts[3].Type != "text" || a.Parts[3].Content != "all good" {
		t.Errorf("part3 = %+v, want text 'all good'", a.Parts[3])
	}
}

// TestReadTurnsReasoningSpan proves a thinking part is timestamped so
// StampPartDurations yields a reasoning span to the next part.
func TestReadTurnsReasoningSpan(t *testing.T) {
	turns, _ := readFixture(t, "reasoning.jsonl")
	a := turns[1]
	if len(a.Parts) != 2 {
		t.Fatalf("parts = %+v, want 2", a.Parts)
	}
	if a.Parts[0].Type != "thinking" {
		t.Fatalf("part0 = %+v, want thinking", a.Parts[0])
	}
	if a.Parts[0].DurationMs != 3000 {
		t.Errorf("thinking duration = %d, want 3000", a.Parts[0].DurationMs)
	}
}

func TestReadTurnsContextInjection(t *testing.T) {
	turns, _ := readFixture(t, "context.jsonl")
	if len(turns) != 4 {
		t.Fatalf("got %d turns, want 4", len(turns))
	}
	if turns[1].Role != "system" || turns[1].Content != "[plugin] runtime ctx" {
		t.Errorf("turn1 = %+v, want system [plugin] injection", turns[1])
	}
	if turns[2].Role != "system" || turns[2].Content != "[skill-catalog] skill list" {
		t.Errorf("turn2 = %+v, want system [skill-catalog] injection", turns[2])
	}
}

func TestReadTurnsCompaction(t *testing.T) {
	turns, total := readFixture(t, "compaction.jsonl")
	if total != 4 {
		t.Fatalf("total = %d, want 4 (summary + non-compact replacement add nothing)", total)
	}
	if turns[1].Role != "assistant" || len(turns[1].Parts) != 1 || turns[1].Parts[0].Content != "ok" {
		t.Errorf("assistant turn = %+v, want only the append-origin 'ok'", turns[1])
	}
	if turns[2].Role != "system" || turns[2].Content != "Context compacted" {
		t.Errorf("turn2 = %+v, want compaction marker", turns[2])
	}
	if turns[3].Role != "user" || turns[3].Content != "next" {
		t.Errorf("turn3 = %+v, want user 'next'", turns[3])
	}
}

func TestReadTurnsUnknownEventsSkipped(t *testing.T) {
	turns, total := readFixture(t, "unknown.jsonl")
	if total != 2 || len(turns) != 2 {
		t.Fatalf("got %d turns (total %d), want 2: unknown events must be skipped", len(turns), total)
	}
	if turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Errorf("turns = %+v, want user then assistant", turns)
	}
}

func TestReadTurnsOrphanResult(t *testing.T) {
	turns, _ := readFixture(t, "orphan.jsonl")
	a := turns[1]
	if len(a.Parts) != 2 {
		t.Fatalf("parts = %+v, want text + orphan tool", a.Parts)
	}
	orphan := a.Parts[1]
	if orphan.Type != "tool" || orphan.ToolUseID != "ghost" || orphan.Content != "ghost output" {
		t.Errorf("orphan = %+v, want unmatched tool/result preserved as a tool part", orphan)
	}
}

func TestReadTurnsInterrupted(t *testing.T) {
	turns, _ := readFixture(t, "interrupted.jsonl")
	// Empty interrupted message: a stopped marker keeps the turn non-empty.
	if len(turns[1].Parts) != 1 || turns[1].Parts[0].Content != "— stopped —" {
		t.Errorf("empty interrupted parts = %+v, want stopped marker", turns[1].Parts)
	}
	// Interrupted with content: keep the content, do not add the marker.
	if len(turns[2].Parts) != 1 || turns[2].Parts[0].Content != "partial" {
		t.Errorf("partial interrupted parts = %+v, want only 'partial'", turns[2].Parts)
	}
}

func TestReadTurnsErrorResults(t *testing.T) {
	turns, _ := readFixture(t, "errorresult.jsonl")
	a := turns[1]
	if len(a.Parts) != 2 {
		t.Fatalf("parts = %+v, want two tool parts", a.Parts)
	}
	if got := a.Parts[0].Content; got != "error: no such file" {
		t.Errorf("isError content = %q, want error-prefixed", got)
	}
	if got := a.Parts[1].Content; got != "error: partial" {
		t.Errorf("data.error content = %q, want error-prefixed", got)
	}
}

// TestReadTurnsTornTail proves a truncated final line drops silently instead of
// failing the whole read.
func TestReadTurnsTornTail(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "basic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	data = data[:len(data)-20]
	p := filepath.Join(t.TempDir(), "torn.jsonl")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	turns, total, err := Transcript{}.ReadTurns(p, 0)
	if err != nil {
		t.Fatalf("torn tail must read cleanly: %v", err)
	}
	if total != 1 || len(turns) != 1 || turns[0].Content != "hello dsh" {
		t.Fatalf("torn read = %+v (total %d), want the intact user turn only", turns, total)
	}
}

func TestReadTurnsLimitAndGrouping(t *testing.T) {
	full, total := readFixture(t, "multi.jsonl")
	if total != 6 || len(full) != 6 {
		t.Fatalf("full read = %d turns (total %d), want 6", len(full), total)
	}
	// Two assistant messages of turn 2 group into one turn with two parts.
	if full[3].Role != "assistant" || len(full[3].Parts) != 2 {
		t.Errorf("turn2 = %+v, want one assistant turn with two parts", full[3])
	}

	turns, total, err := Transcript{}.ReadTurns(filepath.Join("testdata", "multi.jsonl"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 6 {
		t.Errorf("total after trim = %d, want pre-trim 6", total)
	}
	if len(turns) != 2 {
		t.Fatalf("limited read = %d turns, want 2", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Content != "three" || turns[1].Parts[0].Content != "r3" {
		t.Errorf("limited turns = %+v, want the last user + assistant pair", turns)
	}
}

func TestReadTurnsZstdRoundTrip(t *testing.T) {
	plain, err := os.ReadFile(filepath.Join("testdata", "basic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "session.v3.jsonl.zstd")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got, total, err := Transcript{}.ReadTurns(p, 0)
	if err != nil {
		t.Fatalf("zstd read: %v", err)
	}
	want, _, err := Transcript{}.ReadTurns(filepath.Join("testdata", "basic.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != len(want) || !reflect.DeepEqual(got, want) {
		t.Errorf("zstd turns = %+v, want identical to plain %+v", got, want)
	}
}

func TestToolTarget(t *testing.T) {
	cases := []struct {
		name, args, want string
	}{
		{"bash", `{"command":"ls -la"}`, "ls -la"},
		{"shell", `{"command":"echo hi"}`, "echo hi"},
		{"read", `{"file_path":"/a/b.go","offset":1}`, "/a/b.go"},
		{"read", `{"path":"/a/c.go"}`, "/a/c.go"},
		{"read", `{"url":"https://x/y"}`, "https://x/y"},
		{"write", `{"file_path":"/w.txt","content":"x"}`, "/w.txt"},
		{"edit", `{"path":"/e.txt","old_string":"a"}`, "/e.txt"},
		{"grep", `{"pattern":"foo.*bar","path":"."}`, "foo.*bar"},
		{"glob", `{"pattern":"**/*.go"}`, "**/*.go"},
		{"webfetch", `{"url":"https://a"}`, "https://a"},
		{"web_search", `{"query":"golang"}`, "golang"},
		// Unknown tool: first string value in document order, not key order.
		{"run_code", `{"description":"list files","code":"..."}`, "list files"},
		{"mystery", `{"b":"second","a":"first"}`, "second"},
		{"mystery", `{"n":1,"s":"str"}`, "str"},
		{"bash", `not json`, ""},
		{"bash", `{"command":123}`, ""},
		{"bash", `{}`, ""},
	}
	for _, tc := range cases {
		if got := toolTarget(tc.name, tc.args); got != tc.want {
			t.Errorf("toolTarget(%q, %s) = %q, want %q", tc.name, tc.args, got, tc.want)
		}
	}
}

func TestToolTargetTruncates(t *testing.T) {
	long := strings.Repeat("x", 300)
	got := toolTarget("bash", `{"command":"`+long+`"}`)
	if n := len([]rune(got)); n != 120 {
		t.Errorf("truncated target has %d runes, want 120", n)
	}
}

// TestAssemblerStub pins the read-only contract: the stub consumes nothing and
// never produces a turn, and the turn predicates are inert.
func TestAssemblerStub(t *testing.T) {
	var tr backend.Transcript = Transcript{}
	a := tr.NewAssembler()
	if a == nil {
		t.Fatal("NewAssembler returned nil")
	}
	if c, p := a.FeedLine([]byte(`{"type":"user/message"}`)); c != nil || p != nil {
		t.Errorf("stub FeedLine = (%v, %v), want (nil, nil)", c, p)
	}
	if a.Flush() != nil {
		t.Error("stub Flush = non-nil, want nil")
	}
	if a.Model() != "" {
		t.Errorf("stub Model = %q, want empty", a.Model())
	}
	if tr.IsTurnComplete(nil) || tr.IsTurnAborted(nil) {
		t.Error("IsTurnComplete/IsTurnAborted = true, want false")
	}
}
