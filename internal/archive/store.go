// Package archive stores per-session visibility decisions for the sidebar.
//
// The persisted state is *only* the user's manual decisions (a session id
// mapped to "archived" or "shown"). Time-based auto-archive for stale
// sessions is derived at read time from the session's last_event_at —
// we never write "archived" for those, so freshly-active sessions
// reappear automatically without us having to scan and update on every
// tick. The staleness threshold is configurable per Store (see New);
// passing 0 disables auto-archive entirely so only manual decisions
// affect visibility.
//
// Layout mirrors hook.autoApprove: a single JSON map persisted via
// tmp-file + rename under <data-dir>/archived.json.
package archive

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultAutoArchiveAfter is the 7-day default for the time-based
// archive rule when callers don't override.
const DefaultAutoArchiveAfter = 7 * 24 * time.Hour

// Decision is the user's persisted choice for a session id.
type Decision string

const (
	DecisionArchived Decision = "archived"
	DecisionShown    Decision = "shown"
)

type Store struct {
	path string
	// autoAfter is how long a session must be quiet before the default
	// view treats it as archived. Zero disables time-based auto-archive
	// entirely — only manual decisions affect visibility.
	autoAfter time.Duration

	mu     sync.Mutex
	manual map[string]Decision
}

// New constructs a Store. path is the JSON file backing the manual
// decisions; pass "" to disable persistence (e.g. in tests). autoAfter
// is the staleness threshold; pass 0 to disable auto-archive (manual
// decisions still work). A missing file is normal on first run; a
// corrupt file is logged and treated as empty so a bad write can't
// lock the user out of their sidebar.
func New(path string, autoAfter time.Duration) *Store {
	if autoAfter < 0 {
		autoAfter = 0
	}
	s := &Store{
		path:      path,
		autoAfter: autoAfter,
		manual:    map[string]Decision{},
	}
	if path != "" {
		s.load()
	}
	return s
}

func (s *Store) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("archive: read", "path", s.path, "err", err)
		}
		return
	}
	var loaded map[string]Decision
	if err := json.Unmarshal(data, &loaded); err != nil {
		slog.Warn("archive: decode", "path", s.path, "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range loaded {
		if v == DecisionArchived || v == DecisionShown {
			s.manual[k] = v
		}
	}
}

// persist writes the current map to disk atomically. Caller must hold
// s.mu. Best-effort: failures are logged but don't surface.
func (s *Store) persist() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.manual)
	if err != nil {
		slog.Warn("archive: encode", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		slog.Warn("archive: mkdir", "err", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("archive: write tmp", "err", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		slog.Warn("archive: rename", "err", err)
	}
}

// Archive records that the user explicitly archived id. The session
// stays archived until Unarchive is called, regardless of recent activity.
func (s *Store) Archive(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manual[id] == DecisionArchived {
		return
	}
	s.manual[id] = DecisionArchived
	s.persist()
}

// Unarchive removes the user's archive decision. For sessions still
// within the auto-archive window we just delete the entry — the
// natural rule keeps them visible AND lets auto-archive resume when
// they eventually go stale, instead of leaving a permanent DecisionShown
// override. For stale sessions we must write DecisionShown, otherwise
// the very next IsArchived call would immediately re-archive them.
// When auto-archive is disabled (s.autoAfter == 0) there is no "stale",
// so we always just delete the entry.
func (s *Store) Unarchive(id string, lastEventAt time.Time, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fresh := s.autoAfter == 0 ||
		(!lastEventAt.IsZero() && now.Sub(lastEventAt) <= s.autoAfter)
	if fresh {
		if _, ok := s.manual[id]; !ok {
			return
		}
		delete(s.manual, id)
	} else {
		if s.manual[id] == DecisionShown {
			return
		}
		s.manual[id] = DecisionShown
	}
	s.persist()
}

// Forget drops any manual decision for id, for when the session is deleted
// outright. Without it a deleted session's entry would linger in archived.json
// forever (harmless, since ids never recur, but it accumulates). A no-op when
// there is no entry.
func (s *Store) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.manual[id]; !ok {
		return
	}
	delete(s.manual, id)
	s.persist()
}

// IsArchived reports whether a session should be archived in the
// default view. Manual decisions win; otherwise the session is
// archived iff auto-archive is enabled and the session has been
// inactive longer than s.autoAfter.
func (s *Store) IsArchived(id string, lastEventAt time.Time, now time.Time) bool {
	s.mu.Lock()
	d, ok := s.manual[id]
	s.mu.Unlock()
	if ok {
		return d == DecisionArchived
	}
	if s.autoAfter == 0 || lastEventAt.IsZero() {
		return false
	}
	return now.Sub(lastEventAt) > s.autoAfter
}
