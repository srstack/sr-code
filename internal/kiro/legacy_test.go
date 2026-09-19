package kiro

import (
	"os"
	"strings"
	"testing"
)

func TestLegacyReadTurns(t *testing.T) {
	turns, total, err := LegacyTranscript{}.ReadTurns("testdata/legacy.jsonl", 0)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("no turns parsed")
	}
	if turns[0].Role != "user" || turns[0].Content != "sample text" {
		t.Fatalf("first turn = %+v", turns[0])
	}
	var haveText, haveThinking, haveTool bool
	for _, tn := range turns {
		for _, p := range tn.Parts {
			switch p.Type {
			case "text":
				haveText = true
			case "thinking":
				haveThinking = true
			case "tool":
				if p.ToolName == "todo_list" {
					haveTool = true
					if !strings.Contains(p.Content, `"ok":true`) {
						t.Fatalf("tool result not joined: %+v", p)
					}
				}
			}
		}
	}
	if !haveText || !haveThinking || !haveTool {
		t.Fatalf("text=%v thinking=%v tool=%v", haveText, haveThinking, haveTool)
	}
	// one user prompt, two assistant messages grouped per record boundaries
	var users int
	for _, tn := range turns {
		if tn.Role == "user" {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("users=%d", users)
	}
}

func TestReadLegacySessionMeta(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(dir+"/"+name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("abc.json", `{"session_id":"abc","cwd":"/work","title":"old chat","created_at":"2026-09-19T10:00:00Z","updated_at":"2026-09-19T11:00:00Z"}`)
	write("abc.jsonl", `{"version":"v1","kind":"Prompt","data":{"message_id":"m1","content":[{"kind":"text","data":"hi"}],"meta":{"timestamp":1789813773}}}`)
	meta, err := ReadLegacySessionMeta(dir + "/abc.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "abc" || meta.Cwd != "/work" || meta.Title != "old chat" {
		t.Fatalf("meta = %+v", meta)
	}
}
