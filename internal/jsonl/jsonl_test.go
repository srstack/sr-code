package jsonl

import (
	"strings"
	"testing"
)

func TestParseLine_User(t *testing.T) {
	line := []byte(`{"type":"user","sessionId":"s1","cwd":"/tmp/x","timestamp":"2026-04-26T10:00:00.000Z","message":{"role":"user","content":"hi"},"uuid":"u1"}`)
	ev, err := ParseLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "user" {
		t.Errorf("Type = %q, want user", ev.Type)
	}
	if ev.SessionID != "s1" {
		t.Errorf("SessionID = %q", ev.SessionID)
	}
	if ev.Cwd != "/tmp/x" {
		t.Errorf("Cwd = %q", ev.Cwd)
	}
	if ev.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
	if len(ev.Raw) == 0 {
		t.Error("Raw should not be empty")
	}
}

func TestParseLine_Malformed(t *testing.T) {
	if _, err := ParseLine([]byte(`not json`)); err == nil {
		t.Error("expected error")
	}
}

func TestExtractUserContent_StringContent(t *testing.T) {
	got := extractUserContent([]byte(`{"role":"user","content":"hello world"}`))
	if got != "hello world" {
		t.Errorf("got %q", got)
	}
}

func TestExtractUserContent_BlockArray(t *testing.T) {
	got := extractUserContent([]byte(`{"role":"user","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}`))
	if got != "first" {
		t.Errorf("got %q", got)
	}
}

func TestExtractUserContent_NoText(t *testing.T) {
	got := extractUserContent([]byte(`{"role":"user","content":[{"type":"image","source":{}}]}`))
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestReadSessionMeta(t *testing.T) {
	meta, err := ReadSessionMeta("testdata/sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "sample" {
		t.Errorf("ID = %q, want sample", meta.ID)
	}
	if meta.Cwd != "/tmp/test-project" {
		t.Errorf("Cwd = %q", meta.Cwd)
	}
	if meta.Title != "Hand-titled Session" {
		t.Errorf("Title = %q", meta.Title)
	}
	if meta.StartedAt.IsZero() {
		t.Error("StartedAt should not be zero")
	}
	if meta.LastEventAt.Before(meta.StartedAt) {
		t.Errorf("LastEventAt %v before StartedAt %v", meta.LastEventAt, meta.StartedAt)
	}
}

func TestReadSessionMeta_TitleFallback(t *testing.T) {
	meta, err := ReadSessionMeta("testdata/no-title.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title == "" {
		t.Error("expected first-prompt fallback title")
	}
	if meta.Title != "Reply with exactly: APPLE" {
		t.Errorf("Title = %q", meta.Title)
	}
}

func TestReadSessionMeta_Missing(t *testing.T) {
	if _, err := ReadSessionMeta("testdata/does-not-exist.jsonl"); err == nil {
		t.Error("expected error")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("short: got %q", got)
	}
	if got := truncate("abcdefghij", 5); got != "abcde…" {
		t.Errorf("long: got %q", got)
	}
	// rune-aware: each Chinese char is one rune
	if got := truncate("一二三四五六七八九十", 3); got != "一二三…" {
		t.Errorf("rune: got %q", got)
	}
}

func TestReadTurns(t *testing.T) {
	turns, err := ReadTurns("testdata/sample.jsonl", 0)
	if err != nil {
		t.Fatal(err)
	}
	// sample.jsonl has 2 user prompts and 1 assistant response.
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want 3: %+v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Content != "first prompt" {
		t.Errorf("turns[0] = %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Content != "first response" {
		t.Errorf("turns[1] = %+v", turns[1])
	}
	if turns[2].Role != "user" || turns[2].Content != "second prompt" {
		t.Errorf("turns[2] = %+v", turns[2])
	}
}

func TestReadTurns_Limit(t *testing.T) {
	turns, err := ReadTurns("testdata/sample.jsonl", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d, want 1", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Content != "second prompt" {
		t.Errorf("limited turn = %+v", turns[0])
	}
}

func TestReadTurns_Missing(t *testing.T) {
	if _, err := ReadTurns("testdata/does-not-exist.jsonl", 0); err == nil {
		t.Error("expected error")
	}
}

func TestExtractTextContent_ToolUseAndResult(t *testing.T) {
	msg := []byte(`{"role":"assistant","content":[
		{"type":"text","text":"running ls"},
		{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"ls"}}
	]}`)
	got := extractTextContent(msg)
	if !strings.Contains(got, "running ls") || !strings.Contains(got, "[tool: Bash]") {
		t.Errorf("got %q", got)
	}

	// tool_result with string content
	msg2 := []byte(`{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"tu1","content":"file1.txt\nfile2.txt"}
	]}`)
	if got := extractTextContent(msg2); !strings.Contains(got, "[result: file1.txt") {
		t.Errorf("string result got %q", got)
	}

	// tool_result with block content
	msg3 := []byte(`{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"some output"}]}
	]}`)
	if got := extractTextContent(msg3); !strings.Contains(got, "[result: some output") {
		t.Errorf("block result got %q", got)
	}
}
