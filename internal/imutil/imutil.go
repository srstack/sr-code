// Package imutil holds helpers shared by usher's IM frontends (the in-process
// Telegram hub and out-of-process IM plugins): flattening session jsonl events
// into plain text, and IM-agnostic text utilities.
package imutil

import (
	"encoding/json"
	"strings"
	"time"
)

// TurnUserText extracts the display text from router's backend-neutral
// "turn.user" event. This is the event IM frontends should render for prompt
// echoes; it is derived from either Claude jsonl or Codex rollout logs.
func TurnUserText(raw json.RawMessage) string {
	var o struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &o); err != nil || o.Role != "user" {
		return ""
	}
	return o.Content
}

// PartText extracts assistant text from router's backend-neutral "part" event.
// Tool parts and malformed payloads yield "".
func PartText(raw json.RawMessage) string {
	var o struct {
		Role string `json:"role"`
		Part struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"part"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return ""
	}
	if o.Role != "assistant" || o.Part.Type != "text" {
		return ""
	}
	return o.Part.Content
}

// ImageExts mirrors the show_image allowlist (cmd/usher/mcpcmd.go mcpImageExts
// and web's imageContentTypes): raster only, SVG excluded.
var ImageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// PartImageRefs extracts show_image file paths from a backend-neutral tool
// "part" event. The router puts a tool's path-like argument into toolTarget,
// so this covers streamed show_image calls without tying IM frontends to one
// backend's raw log shape.
func PartImageRefs(raw json.RawMessage) []string {
	var o struct {
		Role string `json:"role"`
		Part struct {
			Type       string `json:"type"`
			ToolName   string `json:"toolName"`
			ToolTarget string `json:"toolTarget"`
		} `json:"part"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil
	}
	if o.Role != "assistant" || o.Part.Type != "tool" || o.Part.ToolTarget == "" || !isShowImage(o.Part.ToolName) {
		return nil
	}
	return []string{o.Part.ToolTarget}
}

func isShowImage(name string) bool {
	return name == "show_image" || strings.HasSuffix(name, "__show_image")
}

// AskQuestion is the subset of an AskUserQuestion question IM frontends
// render: the prompt text, its short header, and the option labels.
type AskQuestion struct {
	Header      string `json:"header"`
	Question    string `json:"question"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label string `json:"label"`
	} `json:"options"`
}

// ParseQuestions extracts the questions of an AskUserQuestion tool input.
func ParseQuestions(raw json.RawMessage) []AskQuestion {
	var in struct {
		Questions []AskQuestion `json:"questions"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil
	}
	return in.Questions
}

// TurnDuration reads the elapsed time of a turn from a subprocess.exit event,
// using the user_ts / assistant_ts the router stamps onto it. Returns false
// when either timestamp is missing (best-effort enrichment) or out of order.
func TurnDuration(raw json.RawMessage) (time.Duration, bool) {
	var x struct {
		UserTS      time.Time `json:"user_ts"`
		AssistantTS time.Time `json:"assistant_ts"`
	}
	if err := json.Unmarshal(raw, &x); err != nil {
		return 0, false
	}
	if x.UserTS.IsZero() || x.AssistantTS.IsZero() || !x.AssistantTS.After(x.UserTS) {
		return 0, false
	}
	return x.AssistantTS.Sub(x.UserTS), true
}

// HumanizeDuration renders a turn duration compactly (e.g. "8s", "1m12s").
func HumanizeDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	return d.Round(time.Second).String()
}

// CompactInput flattens a tool input to a one-or-few-line summary. Bash shows
// its command; everything else shows compact JSON.
func CompactInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asObj map[string]any
	if err := json.Unmarshal(raw, &asObj); err != nil || asObj == nil {
		return string(raw) // not a JSON object (array/scalar) → show it verbatim
	}
	if cmd, ok := asObj["command"].(string); ok {
		return cmd
	}
	compact, err := json.Marshal(asObj)
	if err != nil {
		return string(raw)
	}
	return string(compact)
}

// ShortID returns the first 8 characters of a session id.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// SplitMessage breaks text into chunks of at most max runes, preferring to cut
// at a newline boundary so code/paragraphs stay intact. A non-positive max is
// nonsense from a miscomputed limit; return the text whole rather than loop.
func SplitMessage(text string, max int) []string {
	runes := []rune(text)
	if max <= 0 || len(runes) <= max {
		return []string{text}
	}
	var chunks []string
	for len(runes) > 0 {
		n := max
		if n > len(runes) {
			n = len(runes)
		} else if idx := lastNewline(runes[:n]); idx > max/2 {
			n = idx
		}
		chunks = append(chunks, string(runes[:n]))
		runes = runes[n:]
		// Skip a single leading newline left at the cut point.
		if len(runes) > 0 && runes[0] == '\n' {
			runes = runes[1:]
		}
	}
	return chunks
}

func lastNewline(runes []rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == '\n' {
			return i
		}
	}
	return -1
}

// Truncate caps s at max runes.
func Truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
