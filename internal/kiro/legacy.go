package kiro

// Legacy kiro-cli (v1/v2 engine) sessions live in a flat store:
// <sessions-root>/cli/<uuid>.jsonl with a <uuid>.json metadata sidecar.
// The v3 engine cannot resume them, so usher renders them read-only.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
)

// LegacyTranscript projects the legacy line format
// {"version":"v1","kind":"Prompt|AssistantMessage|ToolResults","data":{…}}
// into display turns.
type LegacyTranscript struct{}

func (LegacyTranscript) ReadTurns(path string, limit int) ([]core.Turn, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	a := NewLegacyAssembler()
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

func (LegacyTranscript) NewAssembler() backend.Assembler { return NewLegacyAssembler() }
func (LegacyTranscript) IsTurnComplete([]byte) bool      { return false }
func (LegacyTranscript) IsTurnAborted([]byte) bool       { return false }

type legacyEntry struct {
	Kind string `json:"kind"`
	Data struct {
		MessageID string `json:"message_id"`
		Content   []struct {
			Kind string          `json:"kind"` // text | thinking | toolUse | toolResult
			Data json.RawMessage `json:"data"`
		} `json:"content"`
		Meta struct {
			Timestamp int64 `json:"timestamp"` // unix seconds
		} `json:"meta"`
	} `json:"data"`
}

func (e legacyEntry) ts() time.Time {
	if e.Data.Meta.Timestamp > 0 {
		return time.Unix(e.Data.Meta.Timestamp, 0).UTC()
	}
	return time.Time{}
}

// LegacyAssembler groups the legacy record stream into display turns.
type LegacyAssembler struct {
	cur *core.Turn
}

func NewLegacyAssembler() *LegacyAssembler { return &LegacyAssembler{} }
func (a *LegacyAssembler) Model() string   { return "" }

func (a *LegacyAssembler) FeedLine(raw []byte) ([]core.Turn, *core.TurnPart) {
	completed, parts := a.FeedLineParts(raw)
	if len(parts) == 0 {
		return completed, nil
	}
	return completed, parts[len(parts)-1]
}

func (a *LegacyAssembler) FeedLineParts(raw []byte) ([]core.Turn, []*core.TurnPart) {
	var e legacyEntry
	if json.Unmarshal(raw, &e) != nil {
		return nil, nil
	}
	ts := e.ts()
	switch e.Kind {
	case "Prompt":
		var done []core.Turn
		if a.cur != nil {
			done = append(done, *a.cur)
			a.cur = nil
		}
		text := legacyText(e, "text")
		if text == "" {
			return done, nil
		}
		done = append(done, core.Turn{Role: "user", Content: text, Time: ts, UUID: e.Data.MessageID, EndTime: ts})
		return done, nil
	case "AssistantMessage":
		a.ensureCur(ts, e.Data.MessageID)
		var parts []*core.TurnPart
		for _, c := range e.Data.Content {
			var p core.TurnPart
			switch c.Kind {
			case "text":
				var s string
				if json.Unmarshal(c.Data, &s) != nil || strings.TrimSpace(s) == "" {
					continue
				}
				p = core.TurnPart{Type: "text", Content: s, Time: ts}
			case "thinking":
				var d struct {
					Text string `json:"text"`
				}
				if json.Unmarshal(c.Data, &d) != nil || strings.TrimSpace(d.Text) == "" {
					continue
				}
				p = core.TurnPart{Type: "thinking", Content: d.Text, Time: ts}
			case "toolUse":
				var d struct {
					ToolUseID string          `json:"toolUseId"`
					Name      string          `json:"name"`
					Input     json.RawMessage `json:"input"`
				}
				if json.Unmarshal(c.Data, &d) != nil {
					continue
				}
				p = core.TurnPart{Type: "tool", ToolName: d.Name, ToolUseID: d.ToolUseID, ToolTarget: toolTarget(d.Input), Time: ts}
			default:
				continue
			}
			a.cur.Parts = append(a.cur.Parts, p)
			cp := p
			parts = append(parts, &cp)
		}
		a.cur.Touch(ts)
		return nil, parts
	case "ToolResults":
		a.ensureCur(ts, e.Data.MessageID)
		var parts []*core.TurnPart
		for _, c := range e.Data.Content {
			if c.Kind != "toolResult" {
				continue
			}
			var d struct {
				ToolUseID string `json:"toolUseId"`
				Content   []struct {
					Kind string          `json:"kind"` // text | json
					Data json.RawMessage `json:"data"`
				} `json:"content"`
			}
			if json.Unmarshal(c.Data, &d) != nil {
				continue
			}
			body := legacyResultBody(d.Content)
			joined := false
			for i := len(a.cur.Parts) - 1; i >= 0; i-- {
				if a.cur.Parts[i].ToolUseID == d.ToolUseID {
					a.cur.Parts[i].Content = renderToolResult(a.cur.Parts[i].ToolName, body)
					p := a.cur.Parts[i]
					parts = append(parts, &p)
					joined = true
					break
				}
			}
			if !joined {
				p := core.TurnPart{Type: "tool", Content: renderToolResult("", body), ToolUseID: d.ToolUseID, Time: ts}
				a.cur.Parts = append(a.cur.Parts, p)
				parts = append(parts, &p)
			}
		}
		a.cur.Touch(ts)
		return nil, parts
	}
	return nil, nil
}

func (a *LegacyAssembler) ensureCur(ts time.Time, id string) {
	if a.cur == nil {
		a.cur = &core.Turn{Role: "assistant", Time: ts, UUID: id}
	}
}

func (a *LegacyAssembler) Flush() *core.Turn {
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

// legacyText concatenates the string blocks of the given content kind.
func legacyText(e legacyEntry, kind string) string {
	var b strings.Builder
	for _, c := range e.Data.Content {
		if c.Kind != kind {
			continue
		}
		var s string
		if json.Unmarshal(c.Data, &s) == nil {
			b.WriteString(s)
		}
	}
	return b.String()
}

// legacyResultBody flattens a toolResult's content blocks into one string;
// json blocks are re-encoded compactly.
func legacyResultBody(blocks []struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}) string {
	var parts []string
	for _, c := range blocks {
		switch c.Kind {
		case "text":
			var s string
			if json.Unmarshal(c.Data, &s) == nil && s != "" {
				parts = append(parts, s)
			}
		case "json":
			var v any
			if json.Unmarshal(c.Data, &v) == nil {
				if raw, err := json.Marshal(v); err == nil {
					parts = append(parts, string(raw))
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

// legacySidecar is the <uuid>.json metadata file next to a legacy transcript.
type legacySidecar struct {
	SessionID     string    `json:"session_id"`
	Cwd           string    `json:"cwd"`
	Title         string    `json:"title"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	CreatedReason string    `json:"session_created_reason"` // "subagent" for agent-spawned sessions
}

// ReadLegacySessionMeta builds the discovery descriptor for a legacy session
// from its sidecar json plus the transcript mtime.
func ReadLegacySessionMeta(path string) (core.SessionMeta, error) {
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	meta := core.SessionMeta{ID: id}
	raw, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".json")
	if err != nil {
		return meta, err
	}
	var sc legacySidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		return meta, err
	}
	meta.Title = sc.Title
	meta.Cwd = sc.Cwd
	meta.StartedAt = sc.CreatedAt
	meta.LastInputAt = sc.UpdatedAt
	// Subagent spawns carry no parent linkage in the sidecar; marking them
	// keeps them out of the sidebar's root list (they're internal artifacts
	// of the parent's pipeline, and kiro can't resume them anyway).
	if sc.CreatedReason == "subagent" {
		meta.IsSubagent = true
	}
	if st, err := os.Stat(path); err == nil {
		meta.LastEventAt = st.ModTime()
	}
	if meta.LastEventAt.IsZero() {
		meta.LastEventAt = sc.UpdatedAt
	}
	return meta, nil
}
