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
	"fmt"
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

	// ToolUseResult is a line-level sibling of Message that Claude Code attaches
	// to tool_result events. It carries the rich payload — Edit/Write diff
	// (structuredPatch), Read file content, Bash stdout/stderr — that the inline
	// message.content does not, so it is the source for rendering tool turns.
	ToolUseResult json.RawMessage `json:"toolUseResult,omitempty"`

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

// collectToolUseNames records tool_use id→name mappings from an assistant message.
func collectToolUseNames(msg json.RawMessage, dst map[string]string) {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return
	}
	var blocks []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return
	}
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID != "" && b.Name != "" {
			dst[b.ID] = b.Name
		}
	}
}

// matchToolName finds the tool name for a tool_result turn by looking up its
// tool_use_id in the accumulated mapping.
func matchToolName(msg json.RawMessage, names map[string]string) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return ""
	}
	var blocks []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "tool_result" && b.ToolUseID != "" {
			return names[b.ToolUseID]
		}
	}
	return ""
}

// messageModel extracts the model id from a message body (assistant events).
func messageModel(msg json.RawMessage) string {
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return ""
	}
	return m.Model
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
	Role     string    `json:"role"`               // "user" | "assistant" | "tool"
	Content  string    `json:"content"`             // human-readable text (tool calls inlined)
	Time     time.Time `json:"ts"`
	Model    string    `json:"model,omitempty"`
	ToolName string    `json:"toolName,omitempty"` // for role=="tool": which tool produced this result
}

// ReadTurns returns the user/assistant turns of the session at path,
// projecting tool uses and tool results into bracketed inline annotations
// so the transcript reads top-to-bottom in chronological order. limit > 0
// keeps only the most recent N turns. total is the turn count before that
// trim, so callers can tell whether older turns exist beyond the window.
func ReadTurns(path string, limit int) (turns []Turn, total int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	toolNames := map[string]string{} // tool_use id → tool name
	for sc.Scan() {
		ev, err := ParseLine(sc.Bytes())
		if err != nil {
			continue
		}
		if ev.Type != "user" && ev.Type != "assistant" {
			continue
		}
		if ev.Type == "assistant" {
			collectToolUseNames(ev.Message, toolNames)
		}
		role := turnRole(ev.Type, ev.Message)
		var content string
		if role == "tool" {
			content = renderToolResult(ev)
		} else {
			content = extractTextContent(ev.Message)
		}
		if content == "" {
			continue
		}
		t := Turn{Role: role, Content: content, Time: ev.Timestamp}
		if role == "assistant" {
			t.Model = messageModel(ev.Message)
		}
		if role == "tool" {
			t.ToolName = matchToolName(ev.Message, toolNames)
		}
		turns = append(turns, t)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	total = len(turns)
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, total, nil
}

// turnRole maps a jsonl event type to a transcript role. user/assistant keep
// their type, except a "user" event whose content is a tool_result: Claude
// Code records tool output as a user-role message, so without this it would
// render as something the user typed. Reclassify it as "tool".
func turnRole(eventType string, msg json.RawMessage) string {
	if eventType == "user" && hasToolResult(msg) {
		return "tool"
	}
	return eventType
}

func hasToolResult(msg json.RawMessage) bool {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return false
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

// extractTextContent flattens a message body (string OR array of blocks) into
// a single readable string. Tool uses become "`tool: Name arg`" annotations
// (the call/req, shown inline in the assistant turn) so transcripts read
// top-to-bottom even when the assistant turn was mostly tool-driven; the tool's
// result is a separate "tool" turn rendered by renderToolResult.
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
		Input   json.RawMessage `json:"input"`
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
			// The call (req) shown inline in the assistant turn, with its key
			// argument (path / command / pattern) appended so it reads like
			// "tool: Read /repo/foo.go". Wrapped in backticks so the markdown
			// renderer treats it as inline code, not a malformed reference link.
			if b.Name != "" {
				label := "tool: " + b.Name
				if t := toolTarget(b.Input); t != "" {
					label += " " + t
				}
				parts = append(parts, "`"+strings.ReplaceAll(label, "`", "'")+"`")
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

// renderToolResult produces the display body for a tool_result ("tool") turn.
// It is built from the line-level toolUseResult, which carries the rich payload
// (Edit/Write diff, Read file content, Bash output) that the inline
// message.content does not; tools whose shape we do not special-case fall back
// to the inline tool_result text.
func renderToolResult(ev Event) string {
	var tur toolUseResultData
	if len(ev.ToolUseResult) > 0 {
		_ = json.Unmarshal(ev.ToolUseResult, &tur)
	}
	if body := tur.render(); body != "" {
		return body
	}
	return clampBody(flattenToolResult(firstToolResultContent(ev.Message)))
}

// firstToolResultContent returns the raw content of the first tool_result block
// in a message body.
func firstToolResultContent(msg json.RawMessage) json.RawMessage {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return nil
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return b.Content
		}
	}
	return nil
}

// patchHunk is one hunk of a Claude Code structuredPatch. Its Lines already
// carry the unified-diff prefix (' ', '+', '-'), so they drop straight into a
// diff fence.
type patchHunk struct {
	OldStart int      `json:"oldStart"`
	OldLines int      `json:"oldLines"`
	NewStart int      `json:"newStart"`
	NewLines int      `json:"newLines"`
	Lines    []string `json:"lines"`
}

// toolUseResultData decodes the shapes of toolUseResult we render richly. Edit
// and Write carry structuredPatch; Read carries File; Bash carries Stdout/
// Stderr. Unknown shapes leave every field zero and render() returns "".
type toolUseResultData struct {
	StructuredPatch []patchHunk `json:"structuredPatch"`
	File            *struct {
		Content string `json:"content"`
	} `json:"file"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// render turns the structured payload into a fenced markdown block, or "" when
// the shape is not one we special-case (caller falls back to inline text).
func (t toolUseResultData) render() string {
	switch {
	case len(t.StructuredPatch) > 0:
		return fence("diff", clampBody(patchBody(t.StructuredPatch)))
	case t.File != nil && t.File.Content != "":
		return fence("", clampBody(t.File.Content))
	case t.Stdout != "" || t.Stderr != "":
		out := t.Stdout
		if t.Stderr != "" {
			if out != "" {
				out += "\n"
			}
			out += t.Stderr
		}
		return fence("", clampBody(out))
	}
	return ""
}

// patchBody renders structuredPatch hunks as unified-diff text (no fence).
func patchBody(hunks []patchHunk) string {
	var b strings.Builder
	for i, h := range hunks {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		for _, ln := range h.Lines {
			b.WriteByte('\n')
			b.WriteString(ln)
		}
	}
	return b.String()
}

// toolTarget picks the most informative argument to show beside a tool name: a
// file path, else a shell command (first line), else a search pattern.
func toolTarget(input json.RawMessage) string {
	if p := inputString(input, "file_path"); p != "" {
		return p
	}
	if cmd := inputString(input, "command"); cmd != "" {
		return firstLine(cmd)
	}
	if pat := inputString(input, "pattern"); pat != "" {
		return pat
	}
	return ""
}

// inputString reads a string field from a tool_use input object, "" if absent.
func inputString(input json.RawMessage, key string) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(m[key], &s); err != nil {
		return ""
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// fence wraps body in a markdown code fence whose backtick run is widened past
// any run inside body, so a payload containing ``` cannot close the block early.
func fence(lang, body string) string {
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
	return ticks + lang + "\n" + body + "\n" + ticks
}

// clampBody caps a tool body so one huge file or output cannot bloat the
// transcript payload. Generous, because the block is collapsed by default.
func clampBody(s string) string {
	const maxBytes = 32 * 1024
	const maxLines = 400
	if len(s) > maxBytes {
		s = s[:maxBytes] + "\n… (truncated)"
	}
	if lines := strings.Split(s, "\n"); len(lines) > maxLines {
		s = strings.Join(append(lines[:maxLines], "… (truncated)"), "\n")
	}
	return s
}
