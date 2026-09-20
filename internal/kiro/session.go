package kiro

import (
	"encoding/json"
	"os"
	"path/filepath"
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
