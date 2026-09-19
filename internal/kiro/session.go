package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// sessionJSON is kiro v3's per-session descriptor (session.json next to
// messages.jsonl).
type sessionJSON struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	WorkspacePaths []string  `json:"workspacePaths"`
	RootPaths      []string  `json:"rootPaths"`
	CreatedAt      time.Time `json:"createdAt"`
	LastModifiedAt time.Time `json:"lastModifiedAt"`
	ModelID        string    `json:"modelId"`
	Status         string    `json:"status"`
}

func readSessionJSON(dir string) (sessionJSON, error) {
	var s sessionJSON
	raw, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(raw, &s)
	return s, err
}

// listSessions runs `kiro-cli --v3 chat --list-sessions --format json` in cwd
// and returns the session ids it reports. Best-effort: any failure yields nil.
func listSessions(ctx context.Context, cmd, cwd string) []string {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, cmd, "--v3", "chat", "--list-sessions", "--format", "json")
	if cwd != "" {
		c.Dir = cwd
	}
	out, err := c.Output()
	if err != nil {
		return nil
	}
	// Envelope: [{"cwd":…, "sessions":[{"sessionId":…}], "complete":true}, …]
	var envelopes []struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
		} `json:"sessions"`
	}
	if json.Unmarshal(out, &envelopes) != nil {
		return nil
	}
	var ids []string
	for _, env := range envelopes {
		for _, s := range env.Sessions {
			if s.SessionID != "" {
				ids = append(ids, s.SessionID)
			}
		}
	}
	return ids
}

// stderrTail returns the last bytes of a process's stderr for error messages.
func stderrTail(buf *bytes.Buffer) string {
	s := strings.TrimSpace(buf.String())
	if s == "" {
		return "(no stderr)"
	}
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return s
}
