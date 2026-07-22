package discovery

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nexustar/usher/internal/codexrollout"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/jsonl"
	piagent "github.com/nexustar/usher/internal/pi"
)

// Source describes where one backend's session logs live on disk and how to
// read them, so Discovery can scan Claude Code and Codex side by side without
// knowing either layout. Discovery handles the watching/caching; a Source only
// answers "is this file a session, what's its id, and what's its metadata".
type Source interface {
	// Backend names the agent CLI this source's sessions belong to
	// ("claude" or "codex"); stamped onto each discovered core.Session.
	Backend() string
	// Root is the directory to scan and recursively watch.
	Root() string
	// IsSessionFile reports whether path is a root or subagent session log, as
	// opposed to a tool-result blob or unrelated nested artifact.
	IsSessionFile(path string) bool
	// SessionID extracts the session id from a session file path, or "" if none.
	SessionID(path string) string
	// ReadMeta reads the lightweight descriptor used for listing.
	ReadMeta(path string) (core.SessionMeta, error)
}

// TailMetaReader is an optional Source capability: refresh only the volatile
// tail-derived fields (last activity, latest usage) without re-parsing the
// whole transcript. Discovery prefers it on the streaming hot path.
type TailMetaReader interface {
	ReadMetaTail(path string) (core.SessionMeta, error)
}

// ClaudeSource scans Claude Code's projects tree:
// <root>/<sanitized-cwd>/<id>.jsonl, where the id is the bare filename.
type ClaudeSource struct{ root string }

func NewClaudeSource(root string) ClaudeSource { return ClaudeSource{root: root} }

func (s ClaudeSource) Backend() string { return "claude" }
func (s ClaudeSource) Root() string    { return s.root }

// IsSessionFile accepts top-level sessions and their nested subagent
// transcripts. Other nested JSONL artifacts remain excluded.
func (s ClaudeSource) IsSessionFile(path string) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) == 2 {
		return true
	}
	return len(parts) >= 4 && parts[2] == "subagents" && strings.HasPrefix(filepath.Base(path), "agent-")
}

// subagentParent returns the parent session id when path is a Claude subagent
// transcript (<cwd>/<parent-id>/subagents/agent-*.jsonl), else "", false.
func (s ClaudeSource) subagentParent(path string) (string, bool) {
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return "", false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) >= 4 && parts[2] == "subagents" {
		return parts[1], true
	}
	return "", false
}

func (s ClaudeSource) SessionID(path string) string {
	name := filepath.Base(path)
	if !strings.HasSuffix(name, ".jsonl") {
		return ""
	}
	id := strings.TrimSuffix(name, ".jsonl")
	if parent, ok := s.subagentParent(path); ok {
		return parent + "::" + id
	}
	return id
}

func (s ClaudeSource) ReadMeta(path string) (core.SessionMeta, error) {
	meta, err := jsonl.ReadSessionMeta(path)
	if err != nil {
		return meta, err
	}
	if parent, ok := s.subagentParent(path); ok {
		meta.ParentID = parent
		meta.IsSubagent = true
		// The id must stay unique, so it's always the agent-<hash> filename;
		// AgentName is the human label (attributionAgent, captured by
		// ReadSessionMeta) and falls back to that same hash only when absent.
		fileID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		meta.ID = meta.ParentID + "::" + fileID
		if meta.AgentName == "" {
			meta.AgentName = fileID
		}
	}
	return meta, nil
}

// ReadMetaTail refreshes only tail-derived fields for a known session.
func (s ClaudeSource) ReadMetaTail(path string) (core.SessionMeta, error) {
	return jsonl.ReadSessionMetaTail(path)
}

// CodexSource scans Codex CLI's rollout tree:
// <root>/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl, where the id is the UUID embedded
// in the filename. The date partitioning is handled by Discovery's recursive
// watch, so a session is recognized purely by its filename shape rather than by
// depth — robust to Codex reorganizing the tree.
type CodexSource struct{ root string }

func NewCodexSource(root string) CodexSource { return CodexSource{root: root} }

func (s CodexSource) Backend() string { return "codex" }
func (s CodexSource) Root() string    { return s.root }

// IsSessionFile accepts rollout files by their name (rollout-…-<uuid>.jsonl).
// Codex keeps archived sessions under a sibling ~/.codex/archived_sessions, a
// different root that is simply not scanned, so every rollout under Root is a
// live session.
func (s CodexSource) IsSessionFile(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "rollout-") &&
		strings.HasSuffix(base, ".jsonl") &&
		codexrollout.SessionIDFromPath(base) != ""
}

func (s CodexSource) SessionID(path string) string {
	return codexrollout.SessionIDFromPath(path)
}

func (s CodexSource) ReadMeta(path string) (core.SessionMeta, error) {
	return codexrollout.ReadSessionMeta(path)
}

// PiSource scans pi's per-working-directory session tree. Directory names are
// an implementation detail; the stable session id and cwd live in the header.
type PiSource struct{ root string }

func NewPiSource(root string) PiSource { return PiSource{root: root} }
func (s PiSource) Backend() string     { return "pi" }
func (s PiSource) Root() string        { return s.root }
func (s PiSource) IsSessionFile(path string) bool {
	return strings.HasSuffix(filepath.Base(path), ".jsonl")
}
func (s PiSource) SessionID(path string) string { return piagent.SessionIDFromPath(path) }
func (s PiSource) ReadMeta(path string) (core.SessionMeta, error) {
	return piagent.ReadSessionMeta(path)
}

// OpenCodeSource scans usher's shadow transcripts for opencode sessions:
// <root>/<sanitized-cwd>/<id>.jsonl. opencode stores its native state in
// SQLite, so usher writes this small Claude-shaped jsonl for sessions it
// drives instead of binding discovery to opencode's internal database schema.
type OpenCodeSource struct{ root string }

func NewOpenCodeSource(root string) OpenCodeSource { return OpenCodeSource{root: root} }

func (s OpenCodeSource) Backend() string { return "opencode" }
func (s OpenCodeSource) Root() string    { return s.root }

func (s OpenCodeSource) IsSessionFile(path string) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	return len(parts) == 2
}

func (s OpenCodeSource) SessionID(path string) string {
	name := filepath.Base(path)
	if !strings.HasSuffix(name, ".jsonl") {
		return ""
	}
	return strings.TrimSuffix(name, ".jsonl")
}

func (s OpenCodeSource) ReadMeta(path string) (core.SessionMeta, error) {
	return jsonl.ReadSessionMeta(path)
}

// ReadMetaTail refreshes only tail-derived fields for a known session.
func (s OpenCodeSource) ReadMetaTail(path string) (core.SessionMeta, error) {
	return jsonl.ReadSessionMetaTail(path)
}
