// Package discovery scans Claude Code's projects directory and watches it for
// changes, exposing the set of known sessions.
//
// Sessions are not "owned" by usher: discovery is purely observational. A
// session jsonl file appearing on disk is enough; usher does not need to have
// launched the Claude Code process.
package discovery

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"

	"usher/internal/core"
	"usher/internal/jsonl"
)

type Discovery struct {
	rootDir string
	logger  *slog.Logger
	watcher *fsnotify.Watcher

	mu       sync.RWMutex
	sessions map[string]core.Session // by id
	paths    map[string]string       // id -> path
}

func New(rootDir string, logger *slog.Logger) (*Discovery, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Discovery{
		rootDir:  rootDir,
		logger:   logger,
		watcher:  w,
		sessions: map[string]core.Session{},
		paths:    map[string]string{},
	}, nil
}

// Start performs an initial scan, registers fsnotify watches on the root and
// every existing project subdirectory, and spawns the watch goroutine.
func (d *Discovery) Start(ctx context.Context) error {
	if err := d.scan(); err != nil {
		return err
	}
	if err := d.addWatches(); err != nil {
		return err
	}
	go d.run(ctx)
	return nil
}

// scan walks the root once and upserts every session jsonl found.
func (d *Discovery) scan() error {
	return filepath.Walk(d.rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Best-effort: skip unreadable subtrees.
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if d.isSessionJSONL(path) {
			d.upsert(path)
		}
		return nil
	})
}

// isSessionJSONL accepts only top-level project session files, i.e. paths
// shaped like <root>/<sanitized-cwd>/<id>.jsonl. Subagent transcripts
// (<root>/<cwd>/<session-id>/subagents/agent-<id>.jsonl), tool-results
// .txt files, and Claude's auto-memory .md files all live deeper and are
// filtered out so they don't show up as fake "sessions" — subagents in
// particular have non-UUID ids that `claude -p --resume` refuses.
func (d *Discovery) isSessionJSONL(path string) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	rel, err := filepath.Rel(d.rootDir, path)
	if err != nil {
		return false
	}
	return strings.Count(rel, string(os.PathSeparator)) == 1
}

// addWatches registers watches on the root dir and every existing subdir, so
// new files are seen no matter which project they appear under.
func (d *Discovery) addWatches() error {
	if err := d.watcher.Add(d.rootDir); err != nil {
		return err
	}
	return filepath.Walk(d.rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && path != d.rootDir {
			if err := d.watcher.Add(path); err != nil {
				d.logger.Warn("watch subdir", "path", path, "err", err)
			}
		}
		return nil
	})
}

// upsert reads a jsonl file's metadata if unknown, or just bumps the
// last-event timestamp from file mtime if already known. The full jsonl is
// scanned once at first sight; subsequent writes during streaming only touch
// mtime, avoiding repeated full-file reads.
func (d *Discovery) upsert(path string) {
	id := sessionIDFromPath(path)
	if id == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}

	d.mu.RLock()
	existing, known := d.sessions[id]
	d.mu.RUnlock()

	if known {
		existing.LastEventAt = info.ModTime()
		// cwd/title come from jsonl content written after the file appears, so
		// the first read can miss them; re-read while either is still empty
		// (self-limiting once both are set — no re-parse on every later write).
		if existing.Cwd == "" || existing.Title == "" {
			if meta, err := jsonl.ReadSessionMeta(path); err == nil {
				if existing.Cwd == "" {
					existing.Cwd = meta.Cwd
				}
				if existing.Title == "" {
					existing.Title = meta.Title
				}
				if existing.StartedAt.IsZero() {
					existing.StartedAt = meta.StartedAt
				}
			}
		}
		d.mu.Lock()
		d.sessions[id] = existing
		d.mu.Unlock()
		return
	}

	meta, err := jsonl.ReadSessionMeta(path)
	if err != nil {
		d.logger.Warn("read session meta", "path", path, "err", err)
		return
	}
	sess := core.Session{
		ID:          id,
		Cwd:         meta.Cwd,
		Title:       meta.Title,
		Status:      core.StatusIdle,
		StartedAt:   meta.StartedAt,
		LastEventAt: info.ModTime(),
	}
	if sess.StartedAt.IsZero() {
		sess.StartedAt = info.ModTime()
	}

	d.mu.Lock()
	d.sessions[id] = sess
	d.paths[id] = path
	d.mu.Unlock()
}

func (d *Discovery) remove(path string) {
	id := sessionIDFromPath(path)
	d.mu.Lock()
	delete(d.sessions, id)
	delete(d.paths, id)
	d.mu.Unlock()
}

func (d *Discovery) run(ctx context.Context) {
	defer d.watcher.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-d.watcher.Events:
			if !ok {
				return
			}
			d.handle(ev)
		case err, ok := <-d.watcher.Errors:
			if !ok {
				return
			}
			d.logger.Warn("fsnotify error", "err", err)
		}
	}
}

func (d *Discovery) handle(ev fsnotify.Event) {
	switch {
	case ev.Op.Has(fsnotify.Create):
		info, err := os.Stat(ev.Name)
		if err != nil {
			return
		}
		if info.IsDir() {
			if err := d.watcher.Add(ev.Name); err != nil {
				d.logger.Warn("watch new subdir", "path", ev.Name, "err", err)
			}
			return
		}
		if d.isSessionJSONL(ev.Name) {
			d.upsert(ev.Name)
		}
	case ev.Op.Has(fsnotify.Write):
		if d.isSessionJSONL(ev.Name) {
			d.upsert(ev.Name)
		}
	case ev.Op.Has(fsnotify.Remove), ev.Op.Has(fsnotify.Rename):
		if d.isSessionJSONL(ev.Name) {
			d.remove(ev.Name)
		}
	}
}

// List returns sessions sorted by most recent activity first.
func (d *Discovery) List() []core.Session {
	d.mu.RLock()
	out := make([]core.Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		out = append(out, s)
	}
	d.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastEventAt.After(out[j].LastEventAt)
	})
	return out
}

// Get returns a session by ID. The bool is false if not found.
func (d *Discovery) Get(id string) (core.Session, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.sessions[id]
	return s, ok
}

// Path returns the on-disk jsonl path for a session ID.
func (d *Discovery) Path(id string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.paths[id]
	return p, ok
}

func sessionIDFromPath(path string) string {
	name := filepath.Base(path)
	if !strings.HasSuffix(name, ".jsonl") {
		return ""
	}
	return strings.TrimSuffix(name, ".jsonl")
}
