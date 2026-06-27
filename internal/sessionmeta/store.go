// Package sessionmeta persists per-session user decisions (archive, pin)
// in sessions.json under <data-dir>/.
package sessionmeta

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const DefaultAutoArchiveAfter = 7 * 24 * time.Hour

type archiveDecision string

const (
	decisionArchived archiveDecision = "archived"
	decisionShown    archiveDecision = "shown"
)

type fileFormat struct {
	Archived map[string]archiveDecision `json:"archived,omitempty"`
	Pinned   []string                   `json:"pinned,omitempty"`
}

type Store struct {
	path      string
	autoAfter time.Duration

	mu       sync.Mutex
	archived map[string]archiveDecision
	pinned   map[string]bool
}

func New(path string, autoAfter time.Duration) *Store {
	if autoAfter < 0 {
		autoAfter = 0
	}
	s := &Store{
		path:      path,
		autoAfter: autoAfter,
		archived:  map[string]archiveDecision{},
		pinned:    map[string]bool{},
	}
	s.load()
	return s
}

func (s *Store) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("sessionmeta: read", "path", s.path, "err", err)
		}
		return
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		slog.Warn("sessionmeta: decode", "path", s.path, "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range f.Archived {
		if v == decisionArchived || v == decisionShown {
			s.archived[k] = v
		}
	}
	for _, id := range f.Pinned {
		s.pinned[id] = true
	}
}

func (s *Store) persist() {
	if s.path == "" {
		return
	}
	pinned := make([]string, 0, len(s.pinned))
	for id := range s.pinned {
		pinned = append(pinned, id)
	}
	data, err := json.Marshal(fileFormat{Archived: s.archived, Pinned: pinned})
	if err != nil {
		slog.Warn("sessionmeta: encode", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		slog.Warn("sessionmeta: mkdir", "err", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("sessionmeta: write", "err", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		slog.Warn("sessionmeta: rename", "err", err)
	}
}

func (s *Store) Archive(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.archived[id] == decisionArchived {
		return
	}
	s.archived[id] = decisionArchived
	s.persist()
}

func (s *Store) Unarchive(id string, lastEventAt time.Time, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fresh := s.autoAfter == 0 ||
		(!lastEventAt.IsZero() && now.Sub(lastEventAt) <= s.autoAfter)
	if fresh {
		if _, ok := s.archived[id]; !ok {
			return
		}
		delete(s.archived, id)
	} else {
		if s.archived[id] == decisionShown {
			return
		}
		s.archived[id] = decisionShown
	}
	s.persist()
}

func (s *Store) IsArchived(id string, lastEventAt time.Time, now time.Time) bool {
	s.mu.Lock()
	d, ok := s.archived[id]
	s.mu.Unlock()
	if ok {
		return d == decisionArchived
	}
	if s.autoAfter == 0 || lastEventAt.IsZero() {
		return false
	}
	return now.Sub(lastEventAt) > s.autoAfter
}

func (s *Store) Pin(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pinned[id] {
		return
	}
	s.pinned[id] = true
	s.persist()
}

func (s *Store) Unpin(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pinned[id] {
		return
	}
	delete(s.pinned, id)
	s.persist()
}

func (s *Store) IsPinned(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pinned[id]
}

// Forget drops all state for id.
func (s *Store) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, hasArchive := s.archived[id]
	hasPin := s.pinned[id]
	if !hasArchive && !hasPin {
		return
	}
	delete(s.archived, id)
	delete(s.pinned, id)
	s.persist()
}
