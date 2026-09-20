package acp

import "encoding/json"

// Update is one session/update notification. Fields are the union of the
// update kinds usher consumes; SessionUpdate discriminates.
type Update struct {
	SessionID     string `json:"-"` // stamped from the notification envelope
	SessionUpdate string `json:"sessionUpdate"`

	// RawContent is the update's content payload: an object {type,text} for
	// message/thought chunks, an array of blocks for tool_call_update.
	RawContent json.RawMessage `json:"content"`

	// tool_call / tool_call_update
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"` // pending | in_progress | completed | failed
	RawInput   json.RawMessage `json:"rawInput"`

	// kiro session_info_update carries context usage under _meta.
	Meta json.RawMessage `json:"_meta"`
}

// Text returns the chunk text for message/thought updates.
func (u *Update) Text() string {
	var c struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(u.RawContent, &c) != nil {
		return ""
	}
	return c.Text
}

// ToolOutputText flattens a tool_call_update's content blocks to text.
// Blocks are {type:"content", content:{type:"text",text}} (ACP standard);
// a bare {text} block is also accepted.
func (u *Update) ToolOutputText() string {
	var blocks []struct {
		Content *struct {
			Text string `json:"text"`
		} `json:"content"`
		Text string `json:"text"`
	}
	if json.Unmarshal(u.RawContent, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Content != nil && b.Content.Text != "" {
			parts = append(parts, b.Content.Text)
		} else if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out
}

// ContextUsagePct extracts kiro's _meta.kiro.contextUsage.usagePercentage.
func (u Update) ContextUsagePct() float64 {
	var m struct {
		Kiro struct {
			ContextUsage struct {
				UsagePercentage float64 `json:"usagePercentage"`
			} `json:"contextUsage"`
		} `json:"kiro"`
	}
	if json.Unmarshal(u.Meta, &m) != nil {
		return 0
	}
	return m.Kiro.ContextUsage.UsagePercentage
}
