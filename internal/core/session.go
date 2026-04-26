// Package core holds shared types referenced by multiple internal packages.
package core

import "time"

// Status describes whether a session has an active subprocess attached.
type Status string

const (
	StatusIdle               Status = "idle"
	StatusRunning            Status = "running"
	StatusAwaitingPermission Status = "awaiting_permission"
)

// Session is the projection of a Claude Code session that usher manages.
// In v0.1 it is a derived view of a jsonl file at
// ~/.claude/projects/<sanitized-cwd>/<id>.jsonl.
type Session struct {
	ID          string    `json:"id"`
	Cwd         string    `json:"cwd"`
	Title       string    `json:"title"`
	Status      Status    `json:"status"`
	StartedAt   time.Time `json:"started_at"`
	LastEventAt time.Time `json:"last_event_at"`
}
