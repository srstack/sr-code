// Package pi adapts pi coding-agent sessions and its RPC protocol to usher.
package pi

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/core"
)

// SessionIDFromPath reads the stable id from a pi session header.
func SessionIDFromPath(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return ""
	}
	var h header
	if json.Unmarshal(sc.Bytes(), &h) != nil || h.Type != "session" {
		return ""
	}
	return h.ID
}

type header struct {
	Type          string    `json:"type"`
	ID            string    `json:"id"`
	Cwd           string    `json:"cwd"`
	Timestamp     time.Time `json:"timestamp"`
	ParentSession string    `json:"parentSession"`
}

type entry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Timestamp time.Time       `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Model      string          `json:"model"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	IsError    bool            `json:"isError"`
	Timestamp  int64           `json:"timestamp"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func ReadSessionMeta(path string) (core.SessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return core.SessionMeta{}, err
	}
	defer f.Close()
	var meta core.SessionMeta
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Type == "session" {
			var h header
			if json.Unmarshal(sc.Bytes(), &h) == nil {
				meta.ID, meta.Cwd, meta.StartedAt = h.ID, h.Cwd, h.Timestamp
				meta.ParentID = sessionIDFromParent(h.ParentSession)
			}
			continue
		}
		if !e.Timestamp.IsZero() {
			meta.LastEventAt = e.Timestamp
		}
		if e.Type != "message" {
			continue
		}
		var m message
		if json.Unmarshal(e.Message, &m) != nil {
			continue
		}
		if m.Role == "user" {
			text := contentText(m.Content)
			if meta.Prompt == "" {
				meta.Prompt = truncate(text, 60)
			}
			meta.LastInputAt = entryTime(e, m)
		}
		if m.Role == "assistant" && m.Model != "" {
			meta.Runtime.Model = m.Model
		}
	}
	return meta, sc.Err()
}

func sessionIDFromParent(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return ""
	}
	var h header
	if json.Unmarshal(sc.Bytes(), &h) != nil {
		return ""
	}
	return h.ID
}

func entryTime(e entry, m message) time.Time {
	if m.Timestamp > 0 {
		return time.UnixMilli(m.Timestamp)
	}
	return e.Timestamp
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, "\n")
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
