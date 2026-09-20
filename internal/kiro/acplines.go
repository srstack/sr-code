package kiro

// Synthesized kiro-format transcript lines emitted from ACP updates, so the
// kiro assemblers render live tool cards exactly as they render the native
// transcript at rest.

import (
	"encoding/json"
	"time"

	"github.com/nexustar/usher/internal/acp"
)

func randomID() string {
	return strings36(time.Now().UnixNano())
}

// strings36 is a tiny monotonically-unique id source for synthesized lines.
func strings36(n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var buf [16]byte
	for i := len(buf) - 1; i >= 0; i-- {
		buf[i] = digits[n%36]
		n /= 36
	}
	return string(buf[:])
}

// The v3 assembler parses {"payload":{"type":…}} lines; the legacy assembler
// parses {"kind":…,"data":{…}} lines. Each synthesizer below has both shapes.

func kiroUserLine(sessionID, content string) json.RawMessage {
	return mustJSON(map[string]any{
		"id":        randomID(),
		"timestamp": time.Now().UTC(),
		"payload":   map[string]any{"type": "user", "content": content},
	})
}

func legacyUserLine(content string) json.RawMessage {
	return mustJSON(map[string]any{
		"version": "v1",
		"kind":    "Prompt",
		"data": map[string]any{
			"message_id": randomID(),
			"content":    []map[string]any{{"kind": "text", "data": content}},
			"meta":       map[string]any{"timestamp": time.Now().Unix()},
		},
	})
}

func kiroToolCallLine(sessionID string, u acp.Update) json.RawMessage {
	if u.ToolCallID == "" {
		return nil
	}
	return mustJSON(map[string]any{
		"id":        u.ToolCallID,
		"timestamp": time.Now().UTC(),
		"payload": map[string]any{
			"type":       "tool_call",
			"toolCallId": u.ToolCallID,
			"toolName":   u.Title,
			"args":       json.RawMessage(normalizeRaw(u.RawInput)),
		},
	})
}

func legacyToolCallLine(u acp.Update) json.RawMessage {
	if u.ToolCallID == "" {
		return nil
	}
	return mustJSON(map[string]any{
		"version": "v1",
		"kind":    "AssistantMessage",
		"data": map[string]any{
			"message_id": randomID(),
			"content": []map[string]any{{
				"kind": "toolUse",
				"data": map[string]any{
					"toolUseId": u.ToolCallID,
					"name":      u.Title,
					"input":     json.RawMessage(normalizeRaw(u.RawInput)),
				},
			}},
			"meta": map[string]any{"timestamp": time.Now().Unix()},
		},
	})
}

func kiroToolResultLine(sessionID string, u acp.Update) json.RawMessage {
	if u.ToolCallID == "" {
		return nil
	}
	ok := u.Status == "completed"
	return mustJSON(map[string]any{
		"id":        u.ToolCallID + "-result",
		"timestamp": time.Now().UTC(),
		"payload": map[string]any{
			"type":       "tool_result",
			"toolCallId": u.ToolCallID,
			"content":    u.ToolOutputText(),
			"success":    ok,
		},
	})
}

func legacyToolResultLine(u acp.Update) json.RawMessage {
	if u.ToolCallID == "" {
		return nil
	}
	return mustJSON(map[string]any{
		"version": "v1",
		"kind":    "ToolResults",
		"data": map[string]any{
			"message_id": randomID(),
			"content": []map[string]any{{
				"kind": "toolResult",
				"data": map[string]any{
					"toolUseId": u.ToolCallID,
					"content":   []map[string]any{{"kind": "text", "data": u.ToolOutputText()}},
				},
			}},
			"meta": map[string]any{"timestamp": time.Now().Unix()},
		},
	})
}

func normalizeRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
