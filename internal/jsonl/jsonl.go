// Package jsonl parses Claude Code's session log files.
//
// Each session lives at ~/.claude/projects/<sanitized-cwd>/<id>.jsonl, one
// JSON object per line. Lines have heterogeneous shape — type values seen so
// far include: queue-operation, user, assistant, last-prompt, attachment,
// ai-title, file-history-snapshot, permission-mode. We unmarshal common fields
// into Event and keep Raw for downstream typed projections.
package jsonl

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Event is one line of a session jsonl. Common fields are extracted; the full
// raw payload is retained for type-specific decoding by callers.
type Event struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId,omitempty"`
	Timestamp time.Time       `json:"timestamp,omitempty"`
	Cwd       string          `json:"cwd,omitempty"`
	UUID      string          `json:"uuid,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`

	// Title appears on type=ai-title events. Field name is opportunistic —
	// if Claude Code uses a different key we will fall back to other heuristics
	// to produce a session title.
	Title string `json:"title,omitempty"`

	Raw json.RawMessage `json:"-"`
}

// ParseLine decodes one jsonl line into an Event.
func ParseLine(line []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return ev, err
	}
	ev.Raw = append(json.RawMessage(nil), line...)
	return ev, nil
}

// SessionMeta is the lightweight descriptor of a session, suitable for listing.
type SessionMeta struct {
	ID          string
	Cwd         string
	Title       string
	StartedAt   time.Time
	LastEventAt time.Time
}

// ReadSessionMeta scans the file at path and produces a SessionMeta. It walks
// every line because cwd, title, and the first-prompt fallback can each appear
// at different positions; long sessions are read once at discovery and cached
// by the discovery layer.
func ReadSessionMeta(path string) (SessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionMeta{}, err
	}
	defer f.Close()

	meta := SessionMeta{
		ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"),
	}

	sc := bufio.NewScanner(f)
	// Some events (assistant message with usage stats, large attachments)
	// can exceed bufio's default 64K line limit.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var firstUserPrompt string
	for sc.Scan() {
		ev, err := ParseLine(sc.Bytes())
		if err != nil {
			continue // skip malformed lines, do not fail the whole read
		}
		if meta.StartedAt.IsZero() && !ev.Timestamp.IsZero() {
			meta.StartedAt = ev.Timestamp
		}
		if !ev.Timestamp.IsZero() {
			meta.LastEventAt = ev.Timestamp
		}
		if meta.Cwd == "" && ev.Cwd != "" {
			meta.Cwd = ev.Cwd
		}
		if ev.Type == "ai-title" && ev.Title != "" {
			meta.Title = ev.Title
		}
		if firstUserPrompt == "" && ev.Type == "user" && len(ev.Message) > 0 {
			firstUserPrompt = extractUserContent(ev.Message)
		}
	}

	if meta.Title == "" && firstUserPrompt != "" {
		meta.Title = truncate(firstUserPrompt, 60)
	}
	return meta, sc.Err()
}

// extractUserContent pulls a representative text from a user message body. The
// body's content can be either a plain string or an array of content blocks.
func extractUserContent(msg json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Turn is a flattened, display-ready projection of a session jsonl line.
type Turn struct {
	Role    string    `json:"role"`    // "user" | "assistant"
	Content string    `json:"content"` // human-readable text (tool calls inlined)
	Time    time.Time `json:"ts"`
}

// ReadTurns returns the user/assistant turns of the session at path,
// projecting tool uses and tool results into bracketed inline annotations
// so the transcript reads top-to-bottom in chronological order. limit > 0
// keeps only the most recent N turns.
func ReadTurns(path string, limit int) ([]Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var turns []Turn
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		ev, err := ParseLine(sc.Bytes())
		if err != nil {
			continue
		}
		if ev.Type != "user" && ev.Type != "assistant" {
			continue
		}
		content := extractTextContent(ev.Message)
		if content == "" {
			continue
		}
		turns = append(turns, Turn{Role: ev.Type, Content: content, Time: ev.Timestamp})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, nil
}

// extractTextContent flattens a message body (string OR array of blocks) into
// a single readable string. Tool uses become "[tool: Name]" annotations and
// tool results become "[result: ...]" so transcripts remain useful even when
// the assistant turn was mostly tool-driven.
func extractTextContent(msg json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "tool_use":
			// Wrap in backticks so the markdown renderer treats it as inline
			// code (distinct visual + non-link). snarkdown otherwise sees
			// `[tool: Bash]` as a malformed reference link.
			if b.Name != "" {
				parts = append(parts, "`tool: "+b.Name+"`")
			}
		case "tool_result":
			if txt := flattenToolResult(b.Content); txt != "" {
				if len([]rune(txt)) > 200 {
					txt = string([]rune(txt)[:200]) + "…"
				}
				// Strip backticks from the inner text so wrapping survives.
				txt = strings.ReplaceAll(txt, "`", "'")
				parts = append(parts, "`result: "+txt+"`")
			}
		}
	}
	return strings.Join(parts, "\n")
}

func flattenToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
