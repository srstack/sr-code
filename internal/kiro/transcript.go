package kiro

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
)

// Transcript owns kiro v3's native session format:
// <sessions-root>/<project-hash>/sess_<uuid>/messages.jsonl, one JSON object
// per line shaped {"id","timestamp","payload":{"type":…}}. The payload types
// we project are user / assistant (Say text, Reasoning thinking) / tool_call /
// tool_result; turn bookkeeping (turn_start, session_metadata, usage_summary,
// session_event, session_start) is skipped except as turn boundaries.
type Transcript struct{}

func (Transcript) ReadTurns(path string, limit int) ([]core.Turn, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	a := NewAssembler()
	var turns []core.Turn
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		completed, _ := a.FeedLine(append([]byte(nil), line...))
		turns = append(turns, completed...)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	if t := a.Flush(); t != nil {
		turns = append(turns, *t)
	}
	total := len(turns)
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, total, nil
}

func (Transcript) NewAssembler() backend.Assembler { return NewAssembler() }

// IsTurnComplete reports a finished agent turn (turn_end record).
func (Transcript) IsTurnComplete(raw []byte) bool {
	var e entry
	return json.Unmarshal(raw, &e) == nil && e.Payload.Type == "turn_end"
}

// IsTurnAborted reports a turn ended by interruption rather than normally.
func (Transcript) IsTurnAborted(raw []byte) bool {
	var e entry
	if json.Unmarshal(raw, &e) != nil || e.Payload.Type != "turn_end" {
		return false
	}
	switch e.Payload.StopReason {
	case "cancelled", "aborted", "interrupted":
		return true
	default:
		return false
	}
}

type entry struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Payload   payload   `json:"payload"`
}

type payload struct {
	Type       string          `json:"type"`
	Content    string          `json:"content"`
	OpType     string          `json:"operationType"`
	StopReason string          `json:"stopReason"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Title      string          `json:"title"`
	Success    *bool           `json:"success"`
}

// Assembler groups kiro's flat record stream into display turns. Assistant
// Say/Reasoning/tool records between two user prompts (or a turn_end) form one
// assistant turn, mirroring the other backends' grouping.
type Assembler struct {
	cur *core.Turn
}

func NewAssembler() *Assembler     { return &Assembler{} }
func (a *Assembler) Model() string { return "" } // kiro records carry no per-message model

func (a *Assembler) FeedLine(raw []byte) ([]core.Turn, *core.TurnPart) {
	completed, parts := a.FeedLineParts(raw)
	if len(parts) == 0 {
		return completed, nil
	}
	return completed, parts[len(parts)-1]
}

func (a *Assembler) FeedLineParts(raw []byte) ([]core.Turn, []*core.TurnPart) {
	var e entry
	if json.Unmarshal(raw, &e) != nil {
		return nil, nil
	}
	p := e.Payload
	switch p.Type {
	case "user":
		var done []core.Turn
		if a.cur != nil {
			done = append(done, *a.cur)
			a.cur = nil
		}
		if strings.TrimSpace(p.Content) == "" {
			return done, nil
		}
		done = append(done, core.Turn{Role: "user", Content: p.Content, Time: e.Timestamp, UUID: e.ID, EndTime: e.Timestamp})
		return done, nil
	case "turn_end":
		// A turn can close without a following user prompt (idle pause).
		if a.cur == nil {
			return nil, nil
		}
		done := *a.cur
		a.cur = nil
		done.Touch(e.Timestamp)
		return []core.Turn{done}, nil
	case "assistant":
		a.ensureCur(e)
		var part core.TurnPart
		switch p.OpType {
		case "Reasoning":
			part = core.TurnPart{Type: "thinking", Content: p.Content, Time: e.Timestamp}
		default: // Say and any future text op
			if strings.TrimSpace(p.Content) == "" {
				return nil, nil
			}
			part = core.TurnPart{Type: "text", Content: p.Content, Time: e.Timestamp}
		}
		a.cur.Parts = append(a.cur.Parts, part)
		a.cur.Touch(e.Timestamp)
		return nil, []*core.TurnPart{&part}
	case "tool_call":
		a.ensureCur(e)
		part := core.TurnPart{
			Type:       "tool",
			ToolName:   p.ToolName,
			ToolUseID:  p.ToolCallID,
			ToolTarget: toolTarget(p.Args),
			Time:       e.Timestamp,
		}
		a.cur.Parts = append(a.cur.Parts, part)
		a.cur.Touch(e.Timestamp)
		return nil, []*core.TurnPart{&part}
	case "tool_result":
		a.ensureCur(e)
		content := p.Content
		if p.Success != nil && !*p.Success && content != "" {
			content = "Error: " + content
		}
		for i := len(a.cur.Parts) - 1; i >= 0; i-- {
			if a.cur.Parts[i].ToolUseID == p.ToolCallID {
				a.cur.Parts[i].Content = renderToolResult(a.cur.Parts[i].ToolName, content)
				part := a.cur.Parts[i]
				a.cur.Touch(e.Timestamp)
				return nil, []*core.TurnPart{&part}
			}
		}
		part := core.TurnPart{Type: "tool", Content: renderToolResult(p.ToolName, content), ToolName: p.ToolName, ToolUseID: p.ToolCallID, Time: e.Timestamp}
		a.cur.Parts = append(a.cur.Parts, part)
		a.cur.Touch(e.Timestamp)
		return nil, []*core.TurnPart{&part}
	}
	return nil, nil
}

func (a *Assembler) ensureCur(e entry) {
	if a.cur == nil {
		a.cur = &core.Turn{Role: "assistant", Time: e.Timestamp, UUID: e.ID}
	}
}

func (a *Assembler) Flush() *core.Turn {
	if a.cur == nil {
		return nil
	}
	t := a.cur
	a.cur = nil
	if len(t.Parts) == 0 && strings.TrimSpace(t.Content) == "" {
		return nil
	}
	return t
}

// toolTarget picks the display target (path/command) out of a tool_call's args.
func toolTarget(args json.RawMessage) string {
	var v map[string]any
	if json.Unmarshal(args, &v) != nil {
		return ""
	}
	for _, key := range []string{"command", "path", "file_path", "query", "pattern", "url"} {
		if s, ok := v[key].(string); ok {
			return s
		}
	}
	return ""
}

// renderToolResult fences terminal-style output before shared Markdown rendering.
func renderToolResult(name, body string) string {
	if body == "" || !terminalOutputTool(name) {
		return body
	}
	return fence(clampBody(body))
}

func terminalOutputTool(name string) bool {
	switch strings.ToLower(name) {
	case "execute_bash", "fs_read", "read", "bash", "grep", "find", "ls":
		return true
	default:
		return false
	}
}

func fence(body string) string {
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
	return ticks + "\n" + body + "\n" + ticks
}

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
