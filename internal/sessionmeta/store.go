// Package sessionmeta persists per-session user decisions (archive, pin)
// in archived.json and pinned.json under <data-dir>/.
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

type Store struct {
	archivePath string
	pinPath     string
	autoAfter   time.Duration

	mu       sync.Mutex
	archived map[string]archiveDecision
	pinned   map[string]bool
}

func New(archivePath, pinPath string, autoAfter time.Duration) *Store {
	if autoAfter < 0 {
		autoAfter = 0
	}
	s := &Store{
		archivePath: archivePath,
		pinPath:     pinPath,
		autoAfter:   autoAfter,
		archived:    map[string]archiveDecision{},
		pinned:      map[string]bool{},
	}
	s.loadArchive()
	s.loadPin()
	return s
}


func (s *Store) loadArchive() {
	if s.archivePath == "" {
		return
	}
	data, err := os.ReadFile(s.archivePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("sessionmeta: read archive", "path", s.archivePath, "err", err)
		}
		return
	}
	var loaded map[string]archiveDecision
	if err := json.Unmarshal(data, &loaded); err != nil {
		slog.Warn("sessionmeta: decode archive", "path", s.archivePath, "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range loaded {
		if v == decisionArchived || v == decisionShown {
			s.archived[k] = v
		}
	}
}

func (s *Store) loadPin() {
	if s.pinPath == "" {
		return
	}
	data, err := os.ReadFile(s.pinPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("sessionmeta: read pin", "path", s.pinPath, "err", err)
		}
		return
	}
	var loaded []string
	if err := json.Unmarshal(data, &loaded); err != nil {
		slog.Warn("sessionmeta: decode pin", "path", s.pinPath, "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range loaded {
		s.pinned[id] = true
	}
}

func (s *Store) persistArchive() {
	persistJSON(s.archivePath, s.archived, "archive")
}

func (s *Store) persistPin() {
	ids := make([]string, 0, len(s.pinned))
	for id := range s.pinned {
		ids = append(ids, id)
	}
	persistJSON(s.pinPath, ids, "pin")
}

func persistJSON(path string, v any, label string) {
	if path == "" {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		slog.Warn("sessionmeta: encode "+label, "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Warn("sessionmeta: mkdir "+label, "err", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("sessionmeta: write "+label, "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		slog.Warn("sessionmeta: rename "+label, "err", err)
	}
}


func (s *Store) Archive(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.archived[id] == decisionArchived {
		return
	}
	s.archived[id] = decisionArchived
	s.persistArchive()
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
	s.persistArchive()
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
	s.persistPin()
}

func (s *Store) Unpin(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pinned[id] {
		return
	}
	delete(s.pinned, id)
	s.persistPin()
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
	if hasArchive {
		s.persistArchive()
	}
	if hasPin {
		s.persistPin()
	}
}
