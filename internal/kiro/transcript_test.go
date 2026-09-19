package kiro

import (
	"strings"
	"testing"
)

func TestReadTurnsWithTools(t *testing.T) {
	turns, total, err := Transcript{}.ReadTurns("testdata/with-tools.jsonl", 0)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || len(turns) == 0 {
		t.Fatal("no turns parsed")
	}
	if turns[0].Role != "user" || !strings.Contains(turns[0].Content, "kprobe.txt") {
		t.Fatalf("first turn = %+v", turns[0])
	}
	var assistant *struct{ parts int }
	_ = assistant
	var toolPart, textPart, thinkPart bool
	for _, tn := range turns {
		if tn.Role != "assistant" {
			continue
		}
		for _, p := range tn.Parts {
			switch p.Type {
			case "tool":
				if p.ToolName == "fs_write" {
					toolPart = true
					if p.ToolTarget != "/tmp/opencode/qtest/kprobe.txt" {
						t.Fatalf("tool target = %q", p.ToolTarget)
					}
					if !strings.Contains(p.Content, "Created") {
						t.Fatalf("tool result not joined: %+v", p)
					}
				}
			case "text":
				if strings.Contains(p.Content, "done") {
					textPart = true
				}
			case "thinking":
				thinkPart = true
			}
		}
	}
	if !toolPart || !textPart || !thinkPart {
		t.Fatalf("toolPart=%v textPart=%v thinkPart=%v", toolPart, textPart, thinkPart)
	}
}

func TestTurnBoundaries(t *testing.T) {
	turns, _, err := Transcript{}.ReadTurns("testdata/with-tools.jsonl", 0)
	if err != nil {
		t.Fatal(err)
	}
	// one user prompt, one assistant turn (turn_end closes it)
	var users, assistants int
	for _, tn := range turns {
		if tn.Role == "user" {
			users++
		} else if tn.Role == "assistant" {
			assistants++
		}
	}
	if users != 1 || assistants != 1 {
		t.Fatalf("users=%d assistants=%d (turns=%d)", users, assistants, len(turns))
	}
}

func TestIsTurnCompleteAndAborted(t *testing.T) {
	tr := Transcript{}
	if !tr.IsTurnComplete([]byte(`{"id":"x","payload":{"type":"turn_end","stopReason":"end_turn"}}`)) {
		t.Fatal("turn_end not detected")
	}
	if tr.IsTurnComplete([]byte(`{"id":"x","payload":{"type":"assistant","content":"hi"}}`)) {
		t.Fatal("assistant line misdetected as turn end")
	}
	if tr.IsTurnAborted([]byte(`{"id":"x","payload":{"type":"turn_end","stopReason":"end_turn"}}`)) {
		t.Fatal("clean end detected as abort")
	}
	if !tr.IsTurnAborted([]byte(`{"id":"x","payload":{"type":"turn_end","stopReason":"cancelled"}}`)) {
		t.Fatal("cancelled end not detected as abort")
	}
}
