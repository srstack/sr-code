package opencode

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The shadow log is Claude-shaped so the Claude transcript reader, live
// assembler, and fork machinery all parse it unmodified.

// toolState and toolMeta are the opencode tool-call payload shapes, shared by
// the live stream (partPayload) and the export converter (exportPart).
type toolState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input"`
	Output   string          `json:"output"`
	Error    string          `json:"error"`
	Metadata *toolMeta       `json:"metadata"`
}

type toolMeta struct {
	Patch string `json:"patch"`
	Diff  string `json:"diff"`
}

func userLine(sessionID, cwd, content string, ts time.Time) json.RawMessage {
	return mustMarshal(map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"cwd":       cwd,
		"timestamp": ts,
		"uuid":      randomHexID(),
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	})
}

func textBlocks(text string) []map[string]any {
	if text == "" {
		return nil
	}
	return []map[string]any{{"type": "text", "text": text}}
}

func thinkingBlocks(text string) []map[string]any {
	if text == "" {
		return nil
	}
	return []map[string]any{{"type": "thinking", "thinking": text}}
}

// toolUseBlocks renders a completed opencode tool call as a Claude tool_use
// content block. The assembler records id→name+target from it; the matching
// tool_result arrives on the next line.
func toolUseBlocks(p partPayload) []map[string]any {
	input := p.State.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return []map[string]any{{
		"type":  "tool_use",
		"id":    p.CallID,
		"name":  p.Tool,
		"input": input,
	}}
}

// toolResultLine pairs a completed tool_use with its result as a Claude
// user-role tool_result line. Edit/Write results carry the unified diff as a
// fenced diff block so the transcript renders it like a native Edit.
func toolResultLine(sessionID, cwd string, p partPayload, ts time.Time) json.RawMessage {
	body := p.State.Output
	if p.State.Status == "error" {
		if p.State.Error != "" {
			body = p.State.Error
		}
		if body == "" {
			body = "tool call failed"
		}
	}
	if p.State.Metadata != nil {
		diff := p.State.Metadata.Patch
		if diff == "" {
			diff = p.State.Metadata.Diff
		}
		if diff != "" {
			body = fenceDiff(stripDiffHeader(diff))
		}
	}
	if body == "" {
		body = "(no output)"
	}
	return mustMarshal(map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"cwd":       cwd,
		"timestamp": ts,
		"uuid":      randomHexID(),
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": p.CallID,
				"content":     body,
			}},
		},
	})
}

// stripDiffHeader drops opencode's "Index: …" banner and ---/+++ file lines,
// keeping only hunks, so the fence body matches what Claude's structuredPatch
// renders (hunk headers + prefixed lines only).
func stripDiffHeader(diff string) string {
	var b strings.Builder
	for _, ln := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(ln, "Index: "),
			strings.HasPrefix(ln, "==="),
			strings.HasPrefix(ln, "--- "),
			strings.HasPrefix(ln, "+++ "):
			continue
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// fenceDiff wraps a unified-diff body in a markdown diff fence, widening the
// backtick run past any run inside the body.
func fenceDiff(body string) string {
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	ticks := strings.Repeat("`", max(3, longest+1))
	return ticks + "diff\n" + body + "\n" + ticks
}

// withModel stamps message.model on an assistant line so the transcript and
// live UI show which model produced the turn. No-op for user/system lines or
// an empty model.
func withModel(raw json.RawMessage, model string) json.RawMessage {
	if model == "" || model == "default" {
		return raw
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return raw
	}
	if doc["type"] != "assistant" {
		return raw
	}
	msg, ok := doc["message"].(map[string]any)
	if !ok {
		return raw
	}
	msg["model"] = model
	return mustMarshal(doc)
}

func assistantLine(sessionID string, blocks []map[string]any, ts time.Time) json.RawMessage {
	if len(blocks) == 0 {
		return nil
	}
	return mustMarshal(map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"timestamp": ts,
		"uuid":      randomHexID(),
		"message": map[string]any{
			"role":    "assistant",
			"content": blocks,
		},
	})
}

func turnCompleteLine(sessionID string, ts time.Time) json.RawMessage {
	return mustMarshal(map[string]any{
		"type":      "system",
		"subtype":   "turn_duration",
		"sessionId": sessionID,
		"timestamp": ts,
		"uuid":      randomHexID(),
	})
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func randomHexID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
