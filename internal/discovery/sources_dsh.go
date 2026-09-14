package discovery

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/nexustar/usher/internal/codexrollout"
	"github.com/nexustar/usher/internal/core"
)

// DshSource scans dsh's session tree:
// <sessions-dir>/<project-slug>/<session-id>/session.vN.jsonl[.zstd]. The
// session id is the directory name; project dir names are LOSSY slugs, so the
// cwd is always read from the header frame, never reversed from the path. A
// session dir can hold several generation files (session.vN…); only the
// highest N is the live log. dsh appends standard zstd frames (frame 0 = the
// header line), so reads go through codexrollout.Open, which decodes
// transparently and drops a torn trailing frame like a torn trailing line.
type DshSource struct {
	root          string // <dsh-home>/sessions
	cacheDir      string // <dsh-home>/storages/session_projcache/sessions
	quarantineDir string // <dsh-home>/quarantine
}

// NewDshSource builds a source over dsh's sessions dir; the projection cache
// (fast title path) sits at the sibling storages/ tree of the same DSH_HOME.
func NewDshSource(sessionsDir string) DshSource {
	return DshSource{
		root:          sessionsDir,
		cacheDir:      filepath.Join(filepath.Dir(sessionsDir), "storages", "session_projcache", "sessions"),
		quarantineDir: filepath.Join(filepath.Dir(sessionsDir), "quarantine"),
	}
}

func (s DshSource) Backend() string { return "dsh" }
func (s DshSource) Root() string    { return s.root }

// dshGenRe matches dsh generation files: session.v3.jsonl or
// session.v3.jsonl.zstd. A deployment uses one compression form consistently.
var dshGenRe = regexp.MustCompile(`^session\.v([0-9]+)\.jsonl(\.zstd)?$`)

// parseDshGen extracts the generation number and compression form from a
// generation file name.
func parseDshGen(name string) (gen int, zstd bool, ok bool) {
	m := dshGenRe.FindStringSubmatch(name)
	if m == nil {
		return 0, false, false
	}
	gen, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false, false
	}
	return gen, m[2] != "", true
}

// IsSessionFile accepts only the highest generation file in each session dir
// (plain preferred over .zstd at the same generation, a deterministic tiebreak
// for a form mix dsh never produces in practice).
func (s DshSource) IsSessionFile(path string) bool {
	gen, zstd, ok := parseDshGen(filepath.Base(path))
	if !ok {
		return false
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return true // best effort; ReadMeta surfaces any real problem
	}
	for _, e := range entries {
		g2, z2, ok := parseDshGen(e.Name())
		if !ok {
			continue
		}
		if g2 > gen || (g2 == gen && zstd && !z2) {
			return false
		}
	}
	return true
}

// SessionID is the session directory name (a UUID, with dsh's occasional
// "session-" prefix preserved — the projection cache keys on the same name).
func (s DshSource) SessionID(path string) string {
	if _, _, ok := parseDshGen(filepath.Base(path)); !ok {
		return ""
	}
	return filepath.Base(filepath.Dir(path))
}

// dshHeader is frame 0 of a dsh session log.
type dshHeader struct {
	Type          string `json:"type"`
	CreatedAt     int64  `json:"createdAt"` // epoch ms
	Cwd           string `json:"cwd"`
	ParentSession string `json:"parentSession"`
	Origin        string `json:"origin"`
}

func (s DshSource) ReadMeta(path string) (core.SessionMeta, error) {
	var meta core.SessionMeta
	rc, err := codexrollout.Open(path)
	if err != nil {
		return meta, err
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	if !sc.Scan() {
		return meta, fmt.Errorf("dsh session log %s has no header frame", path)
	}
	var hdr dshHeader
	if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil {
		return meta, fmt.Errorf("dsh session header: %w", err)
	}
	if hdr.Type != "session" {
		// dsh occasionally writes a session log whose first frame is an event
		// instead of the header (observed twice). dsh's own session/list then
		// fails wholesale — "first frame is not exactly one header line" — so
		// every workspace looks empty until the bad log is moved aside. Quarantine
		// it (lossless: the dir is renamed, never deleted) so both dsh and usher
		// recover. The age guard avoids racing a log dsh is still writing.
		if dest, ok := s.quarantineCorrupt(path); ok {
			return meta, fmt.Errorf(
				"dsh session %s: corrupt header frame (first line is %q); quarantined to %s",
				path, string(bytes.TrimSpace(sc.Bytes()[:min(len(sc.Bytes()), 80)])), dest)
		}
		return meta, fmt.Errorf("dsh session %s: unexpected header type %q", path, hdr.Type)
	}
	meta.Cwd = hdr.Cwd
	if hdr.CreatedAt > 0 {
		meta.StartedAt = time.UnixMilli(hdr.CreatedAt)
	}
	if hdr.Origin == "subagent" {
		meta.IsSubagent = true
		meta.ParentID = hdr.ParentSession
	}
	// Title: dsh's projection cache (last-wins session/title events) is the
	// cheap path; scanning whole logs for titles is too expensive for listing.
	// Fall back to the first real user message near the head of the log.
	if meta.Title = s.cachedTitle(s.SessionID(path)); meta.Title == "" {
		meta.Title = dshFirstUserMessage(sc)
	}
	return meta, nil
}

// quarantineCorrupt moves a session dir whose log has no valid header frame
// aside, under <dsh-home>/quarantine. A headerless first line is definitive
// corruption — dsh always writes the header as frame 0 — and dsh itself fails
// to boot while one exists (its workspace init aborts), so this is done
// immediately rather than after a grace period. A file still being written
// either has no complete line yet (a different, non-quarantining error path)
// or already carries its header. Returns the destination and whether the move
// happened.
func (s DshSource) quarantineCorrupt(path string) (string, bool) {
	if s.quarantineDir == "" {
		return "", false
	}
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	src := filepath.Dir(path)
	dest := filepath.Join(s.quarantineDir, filepath.Base(src))
	if _, err := os.Stat(dest); err == nil {
		dest = fmt.Sprintf("%s-%d", dest, time.Now().UnixNano())
	}
	if err := os.MkdirAll(s.quarantineDir, 0o700); err != nil {
		return "", false
	}
	if err := os.Rename(src, dest); err != nil {
		return "", false
	}
	return dest, true
}

// cachedTitle reads dsh's advisory projection cache for the session's latest
// title. Missing or malformed caches yield "" (the caller falls back).
func (s DshSource) cachedTitle(id string) string {
	if id == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.cacheDir, id+".json"))
	if err != nil {
		return ""
	}
	type projRow struct {
		Val json.RawMessage `json:"val"`
	}
	var cache struct {
		Rows   map[string]projRow `json:"rows"`
		Record struct {
			Rows map[string]projRow `json:"rows"`
		} `json:"record"`
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return ""
	}
	rows := cache.Rows
	if rows == nil {
		rows = cache.Record.Rows
	}
	row, ok := rows["title"]
	if !ok || len(row.Val) == 0 {
		return ""
	}
	// The title projection's val is a bare string in current dsh builds;
	// accept a snapshot object carrying a title field as well.
	var title string
	if err := json.Unmarshal(row.Val, &title); err == nil {
		return title
	}
	var snapshot struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(row.Val, &snapshot); err == nil {
		return snapshot.Title
	}
	return ""
}

// dshTitleScanLimit bounds the first-user-message title fallback to the head
// of the event stream — enough to cover seeded system preamble before the
// first real user message, cheap enough for listing.
const dshTitleScanLimit = 64 * 1024

// dshFirstUserMessage scans the event stream (already positioned past the
// header) for the first user-typed message and returns its first text block,
// truncated. "" when none appears within the scan limit.
func dshFirstUserMessage(sc *bufio.Scanner) string {
	scanned := 0
	for sc.Scan() {
		line := sc.Bytes()
		scanned += len(line)
		if scanned > dshTitleScanLimit {
			break
		}
		if !bytes.Contains(line, []byte(`"user/message"`)) {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				Source struct {
					Kind string `json:"kind"`
				} `json:"source"`
			} `json:"data"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Type != "user/message" || ev.Data.Source.Kind != "user" {
			continue
		}
		for _, c := range ev.Data.Content {
			if c.Type == "text" && c.Text != "" {
				return truncateRunes(c.Text, 80)
			}
		}
	}
	return ""
}

// truncateRunes shortens s to n runes, mirroring jsonl's title convention.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
