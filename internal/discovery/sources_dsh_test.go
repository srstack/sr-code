package discovery

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	dshUUID1 = "11111111-2222-3333-4444-555555555555"
	dshUUID2 = "aaaaaaaa-1111-2222-3333-bbbbbbbbbbbb"
	dshUUID3 = "cccccccc-1111-2222-3333-dddddddddddd"
)

func dshHeaderLine(id, cwd string) string {
	return `{"type":"session","version":3,"id":"` + id + `","createdAt":1789064460603,"cwd":"` + cwd + `","isSeeded":false}`
}

func dshUserMessage(text string) string {
	return `{"type":"user/message","seq":8,"time":1789064460604,"data":{"content":[{"type":"text","text":"` + text + `"}],"source":{"kind":"user"}}}`
}

// writeDshLog writes a dsh generation file; when compress is true the lines
// are written as a .jsonl.zstd log, flushed per line the way dsh's streaming
// writer does, so a truncated tail only loses the trailing chunk.
func writeDshLog(t *testing.T, dir, name string, compress bool, lines ...string) string {
	t.Helper()
	data := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !compress {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range strings.SplitAfter(data, "\n") {
		if _, err := zw.Write([]byte(ln)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDshSource_GenerationSelection proves only the highest generation file
// in a session dir is the session log: v2 must be ignored when v3 exists.
func TestDshSource_GenerationSelection(t *testing.T) {
	dir := t.TempDir()
	v2 := writeDshLog(t, dir, "session.v2.jsonl.zstd", true, dshHeaderLine(dshUUID1, "/tmp/old"))
	v3 := writeDshLog(t, dir, "session.v3.jsonl.zstd", true, dshHeaderLine(dshUUID1, "/tmp/new"))

	s := NewDshSource(filepath.Join(filepath.Dir(dir), "sessions"))
	if s.IsSessionFile(v2) {
		t.Errorf("IsSessionFile(v2) = true, want false (v3 present)")
	}
	if !s.IsSessionFile(v3) {
		t.Errorf("IsSessionFile(v3) = false, want true")
	}
	if got := s.SessionID(v3); got != filepath.Base(dir) {
		t.Errorf("SessionID = %q, want dir name %q", got, filepath.Base(dir))
	}
	if got := s.SessionID(filepath.Join(dir, "notes.jsonl")); got != "" {
		t.Errorf("SessionID(non-generation) = %q, want empty", got)
	}
}

// TestDshSource_TornTail proves a truncated trailing zstd frame (a live dsh
// writer mid-append) degrades to the decoded prefix: header meta still reads.
func TestDshSource_TornTail(t *testing.T) {
	dir := t.TempDir()
	path := writeDshLog(t, dir, "session.v3.jsonl.zstd", true,
		dshHeaderLine(dshUUID1, "/tmp/proj"), dshUserMessage("hello dsh"))

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, fi.Size()-7); err != nil {
		t.Fatal(err)
	}

	s := NewDshSource(dir)
	meta, err := s.ReadMeta(path)
	if err != nil {
		t.Fatalf("torn tail: %v", err)
	}
	if meta.Cwd != "/tmp/proj" {
		t.Errorf("Cwd = %q, want /tmp/proj", meta.Cwd)
	}
	if meta.Title != "hello dsh" {
		t.Errorf("Title = %q, want first user message 'hello dsh'", meta.Title)
	}
}

// TestDshSource_TitleFromCache proves the projection cache is the fast title
// path, winning over the first user message in the log.
func TestDshSource_TitleFromCache(t *testing.T) {
	home := t.TempDir()
	sessionsDir := filepath.Join(home, "sessions")
	dir := filepath.Join(sessionsDir, "--tmp-proj--", dshUUID1)
	path := writeDshLog(t, dir, "session.v3.jsonl.zstd", true,
		dshHeaderLine(dshUUID1, "/tmp/proj"), dshUserMessage("raw first message"))
	writeFile(t, filepath.Join(home, "storages", "session_projcache", "sessions", dshUUID1+".json"),
		`{"version":7,"record":{"identity":{},"rows":{"title":{"ver":1,"seq":9,"val":"cached dsh title"}}}}`)

	s := NewDshSource(sessionsDir)
	meta, err := s.ReadMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "cached dsh title" {
		t.Errorf("Title = %q, want cached 'cached dsh title'", meta.Title)
	}
	if meta.StartedAt.UnixMilli() != 1789064460603 {
		t.Errorf("StartedAt = %v, want createdAt 1789064460603", meta.StartedAt)
	}
	// A malformed cache is advisory only: fall back to the log.
	writeFile(t, filepath.Join(home, "storages", "session_projcache", "sessions", dshUUID1+".json"), `{not json`)
	if got := s.cachedTitle(dshUUID1); got != "" {
		t.Errorf("cachedTitle(malformed) = %q, want empty", got)
	}
}

// TestDshSource_TitleFallback proves that without a cache the title comes
// from the first user-typed message — skipping non-user sources (seeded
// subagent prompts) — and that a cache object-shaped val is also accepted.
func TestDshSource_TitleFallback(t *testing.T) {
	home := t.TempDir()
	sessionsDir := filepath.Join(home, "sessions")
	long := strings.Repeat("x", 100)
	dir := filepath.Join(sessionsDir, "--tmp-proj--", dshUUID2)
	writeDshLog(t, dir, "session.v3.jsonl", false,
		dshHeaderLine(dshUUID2, "/tmp/proj"),
		`{"type":"user/message","seq":1,"time":1789064460604,"data":{"content":[{"type":"text","text":"seeded prompt"}]}}`,
		dshUserMessage(long))

	s := NewDshSource(sessionsDir)
	meta, err := s.ReadMeta(filepath.Join(dir, "session.v3.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat("x", 80) + "…"; meta.Title != want {
		t.Errorf("Title = %q, want 80-rune truncation %q", meta.Title, want)
	}

	writeFile(t, filepath.Join(home, "storages", "session_projcache", "sessions", dshUUID2+".json"),
		`{"rows":{"title":{"ver":1,"seq":3,"val":{"title":"object title"}}}}`)
	if got := s.cachedTitle(dshUUID2); got != "object title" {
		t.Errorf("cachedTitle(object val) = %q, want 'object title'", got)
	}
}

// TestDshSource_ScanIgnoresNonSessions proves a Discovery scan over a dsh
// tree lists generation files only: lock files, stray jsonl artifacts, and
// unrelated nesting are ignored, and subagent sessions are hidden from List.
func TestDshSource_ScanIgnoresNonSessions(t *testing.T) {
	home := t.TempDir()
	sessionsDir := filepath.Join(home, "sessions")
	writeDshLog(t, filepath.Join(sessionsDir, "--tmp-proj--", dshUUID1), "session.v3.jsonl.zstd", true,
		dshHeaderLine(dshUUID1, "/tmp/proj"), dshUserMessage("root session"))
	writeDshLog(t, filepath.Join(sessionsDir, "--tmp-proj--", dshUUID3), "session.v3.jsonl.zstd", true,
		`{"type":"session","version":3,"id":"`+dshUUID3+`","createdAt":1789064460603,"cwd":"/tmp/proj","isSeeded":false,"origin":"subagent","parentSession":"`+dshUUID1+`"}`)
	// Stray artifacts that must not list.
	writeFile(t, filepath.Join(sessionsDir, "--tmp-proj--", dshUUID1, "session.lock"), "")
	writeFile(t, filepath.Join(sessionsDir, "--tmp-proj--", "notes.jsonl"), "{}\n")

	d, err := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), NewDshSource(sessionsDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.watcher.Close() })
	if err := d.scan(); err != nil {
		t.Fatal(err)
	}

	roots := d.List()
	if len(roots) != 1 || roots[0].ID != dshUUID1 {
		t.Fatalf("List = %+v, want only root session %s", roots, dshUUID1)
	}
	if roots[0].Backend != "dsh" {
		t.Errorf("Backend = %q, want dsh", roots[0].Backend)
	}
	sub, ok := d.Get(dshUUID3)
	if !ok || !sub.IsSubagent || sub.ParentID != dshUUID1 {
		t.Errorf("subagent = %+v ok=%v, want IsSubagent with parent %s", sub, ok, dshUUID1)
	}
}

// TestDshSource_QuarantinesCorruptLog: a session whose log starts with an
// event instead of the header poisons dsh's own session/list. ReadMeta must
// move the session dir aside (lossless) once it is old enough that dsh cannot
// still be writing it.
func TestDshSource_QuarantinesCorruptLog(t *testing.T) {
	home := t.TempDir()
	sessionsDir := filepath.Join(home, "sessions")
	sessDir := filepath.Join(sessionsDir, "--tmp-proj--", dshUUID2)
	path := writeDshLog(t, sessDir, "session.v3.jsonl.zstd", true,
		`{"type":"agent/inbox/spliced","seq":3,"time":1789064460604,"data":{}}`)

	// Old enough to quarantine.
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	src := NewDshSource(sessionsDir)
	if _, err := src.ReadMeta(path); err == nil {
		t.Fatal("ReadMeta succeeded on a headerless log; want an error")
	}
	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Errorf("session dir still present after quarantine (stat err = %v)", err)
	}
	qdir := filepath.Join(home, "quarantine", dshUUID2)
	if _, err := os.Stat(qdir); err != nil {
		t.Errorf("quarantined dir %s: %v", qdir, err)
	}
}

// TestDshSource_QuarantinesFreshCorruptLog: a headerless log blocks dsh's own
// startup, so quarantine is immediate — no age grace applies once the first
// complete line is an event rather than the header.
func TestDshSource_QuarantinesFreshCorruptLog(t *testing.T) {
	home := t.TempDir()
	sessionsDir := filepath.Join(home, "sessions")
	sessDir := filepath.Join(sessionsDir, "--tmp-proj--", dshUUID2)
	path := writeDshLog(t, sessDir, "session.v3.jsonl.zstd", true,
		`{"type":"agent/inbox/spliced","seq":3,"time":1789064460604,"data":{}}`)

	src := NewDshSource(sessionsDir)
	if _, err := src.ReadMeta(path); err == nil {
		t.Fatal("ReadMeta succeeded on a headerless log; want an error")
	}
	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Errorf("fresh corrupt session dir not quarantined (stat err = %v)", err)
	}
}
