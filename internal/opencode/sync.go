package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sync mirrors sessions created outside usher (the opencode TUI, other
// frontends) into the shadow tree so discovery sees them. opencode keeps its
// canonical state in SQLite with no stable file layout, so the sync shells
// out to `opencode session list` / `opencode export` and rewrites the shadow
// jsonl whenever a session's updated timestamp moved past the shadow's mtime.
// Sessions with a live usher-driven turn are skipped — the runtime owns those
// files until the turn ends.

const syncInterval = 15 * time.Second

// SyncLoop runs until ctx is cancelled. It syncs once immediately, then on a
// ticker; individual failures are logged and skipped, never fatal.
func SyncLoop(ctx context.Context, rt *Runtime, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if rt.v2 {
		syncOnceV2(ctx, rt, logger)
	} else {
		syncOnce(ctx, rt, logger)
	}
	tick := time.NewTicker(syncInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if rt.v2 {
				syncOnceV2(ctx, rt, logger)
			} else {
				syncOnce(ctx, rt, logger)
			}
		}
	}
}

// DeleteNative removes the session from opencode's own store, invoked when the
// user deletes the shadow session in usher — otherwise the next sync tick
// would export it right back. A tombstone guards the window where the native
// delete hasn't settled yet (and covers a failed delete); it is persisted so
// a restarted usher doesn't resurrect the session either.
func (r *Runtime) DeleteNative(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r.forgotten.Store(id, time.Now())
	r.persistTombstones()
	if r.v2 {
		return apiDelete(ctx, r.Cmd(), "/api/session/"+id)
	}
	// `opencode session delete` asks for interactive confirmation; answer it.
	cmd := exec.CommandContext(ctx, r.Cmd(), "session", "delete", id)
	cmd.Stdin = strings.NewReader("y\n")
	return cmd.Run()
}

// forgotten is the per-runtime tombstone set for usher-deleted sessions.
// Entries expire after a day — long past any reasonable native-delete
// propagation.
func (r *Runtime) forgottenHas(id string) bool {
	v, ok := r.forgotten.Load(id)
	if !ok {
		return false
	}
	if time.Since(v.(time.Time)) > 24*time.Hour {
		r.forgotten.Delete(id)
		return false
	}
	return true
}

// tombstonePath is where tombstones persist across restarts.
func (r *Runtime) tombstonePath() string {
	return filepath.Join(r.root, ".tombstones.json")
}

// LoadTombstones restores the persisted tombstone set at startup.
func (r *Runtime) LoadTombstones() {
	raw, err := os.ReadFile(r.tombstonePath())
	if err != nil {
		return
	}
	var entries map[string]time.Time
	if json.Unmarshal(raw, &entries) != nil {
		return
	}
	for id, ts := range entries {
		r.forgotten.Store(id, ts)
	}
}

func (r *Runtime) persistTombstones() {
	entries := map[string]time.Time{}
	r.forgotten.Range(func(k, v any) bool {
		entries[k.(string)] = v.(time.Time)
		return true
	})
	raw, err := json.Marshal(entries)
	if err != nil {
		return
	}
	_ = os.WriteFile(r.tombstonePath(), raw, 0o644)
}

type sessionEntry struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Updated   int64  `json:"updated"`
	Created   int64  `json:"created"`
	Directory string `json:"directory"`
}

// syncMetaLine is the footer every full shadow rewrite ends with. The
// freshness check compares sourceUpdated against the native session's
// updated timestamp — file mtime is NOT trustworthy (a runtime append or an
// unrelated touch looks like a fresh sync and permanently suppresses it).
func syncMetaLine(s sessionEntry) json.RawMessage {
	return mustMarshal(map[string]any{
		"type":          "system",
		"subtype":       "sync-meta",
		"sessionId":     s.ID,
		"timestamp":     eventTime(s.Updated),
		"uuid":          randomHexID(),
		"sourceUpdated": s.Updated,
	})
}

// readSyncMeta returns the sourceUpdated marker from a shadow's tail.
// ok=false: no marker (pre-marker shadow) → treat as stale and rewrite once.
func readSyncMeta(path string) (updated int64, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	const tail = 8192
	start := int64(0)
	if fi.Size() > tail {
		start = fi.Size() - tail
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	_, _ = f.Seek(start, 0)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if start > 0 {
		sc.Scan() // discard the partial first line
	}
	for sc.Scan() {
		var ev struct {
			Subtype       string `json:"subtype"`
			SourceUpdated int64  `json:"sourceUpdated"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Subtype == "sync-meta" && ev.SourceUpdated > 0 {
			updated, ok = ev.SourceUpdated, true
		}
	}
	return updated, ok
}

// shadowFresh reports whether the shadow at path already mirrors the native
// session state (updated ms).
//
// The native `updated` can LAG the final message commit — observed on v2:
// the last assistant message lands ~1s after the last updated bump, so a
// fetch that races turn end permanently loses the reply. A recently-active
// session is therefore never considered settled; it keeps re-fetching each
// tick until it has been quiet for syncSettleWindow.
const syncSettleWindow = 2 * time.Minute

func shadowFresh(path string, updated int64) bool {
	m, ok := readSyncMeta(path)
	if !ok || m < updated {
		return false
	}
	return time.Since(time.UnixMilli(updated)) > syncSettleWindow
}

func syncOnce(ctx context.Context, rt *Runtime, logger *slog.Logger) {
	// `opencode session list` only covers a slice of projects (observed: a
	// few directories' worth); the session table is global. Query it directly
	// so sessions from every project show up, not just recent ones.
	rows, err := queryRows(ctx, rt.Cmd(),
		"SELECT id, title, directory, time_created, time_updated FROM session WHERE parent_id IS NULL ORDER BY time_updated DESC LIMIT 500")
	if err != nil {
		logger.Warn("opencode sync: session query failed", "err", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		if len(row) < 5 {
			continue
		}
		s := sessionEntry{ID: row[0], Title: row[1], Directory: row[2]}
		s.Created, _ = strconv.ParseInt(row[3], 10, 64)
		s.Updated, _ = strconv.ParseInt(row[4], 10, 64)
		if s.ID == "" || s.Directory == "" {
			continue
		}
		if rt.forgottenHas(s.ID) {
			continue // deleted via usher; don't resurrect
		}
		if rt.Has(s.ID) {
			continue // live turn owns the shadow file
		}
		path := logPath(rt.Root(), s.Directory, s.ID)
		if shadowFresh(path, s.Updated) {
			continue
		}
		// Transcripts are fetched with SQL via `opencode db` (paged rows, no
		// size cap) rather than `opencode export`, whose stdout caps at 128KiB
		// and loses every larger session — including long-running ones.
		if rt.failures.load(s.ID, s.Updated) {
			continue
		}
		if err := fetchSession(ctx, rt, s, path); err != nil {
			rt.failures.store(s.ID, s.Updated)
			logger.Warn("opencode sync: fetch failed", "session", s.ID, "err", err)
		}
	}
}

// syncOnceV2 is syncOnce's v2 counterpart: the session list and transcripts
// come from `opencode2 api` (HTTP) rather than SQLite over `opencode db`.
func syncOnceV2(ctx context.Context, rt *Runtime, logger *slog.Logger) {
	sessions, err := v2ListSessions(ctx, rt.Cmd())
	if err != nil {
		logger.Warn("opencode2 sync: session list failed", "err", err)
		return
	}
	for _, s := range sessions {
		if ctx.Err() != nil {
			return
		}
		if rt.forgottenHas(s.ID) {
			continue
		}
		if rt.Has(s.ID) {
			continue
		}
		path := logPath(rt.Root(), s.Directory, s.ID)
		if shadowFresh(path, s.Updated) {
			continue
		}
		if rt.failures.load(s.ID, s.Updated) {
			continue
		}
		if err := v2FetchSession(ctx, rt, s, path); err != nil {
			rt.failures.store(s.ID, s.Updated)
			logger.Warn("opencode2 sync: fetch failed", "session", s.ID, "err", err)
		}
	}
}

// RefreshSession synchronously re-mirrors one session when its native state
// moved past the shadow — invoked when the UI opens a transcript so
// point-of-use reads never wait for the next sync tick. Cheap when fresh:
// one native row lookup plus a tail read.
func (r *Runtime) RefreshSession(id string) error {
	if r.Has(id) || r.forgottenHas(id) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := r.sessionEntryFor(ctx, id)
	if err != nil || s == nil {
		return err
	}
	path := logPath(r.root, s.Directory, id)
	if shadowFresh(path, s.Updated) {
		return nil
	}
	if r.v2 {
		return v2FetchSession(ctx, r, *s, path)
	}
	return fetchSession(ctx, r, *s, path)
}

// sessionEntryFor looks up one native session row (v1: SQL; v2: HTTP).
func (r *Runtime) sessionEntryFor(ctx context.Context, id string) (*sessionEntry, error) {
	if !sessionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("refusing unexpected session id %q", id)
	}
	if r.v2 {
		var s v2Session
		if err := apiGetJSON(ctx, r.cmd, "/api/session/"+id, &s); err != nil {
			return nil, err
		}
		if s.ID == "" || s.Location.Directory == "" {
			return nil, nil
		}
		return &sessionEntry{
			ID: s.ID, Title: s.Title, Directory: s.Location.Directory,
			Created: s.Time.Created, Updated: s.Time.Updated,
		}, nil
	}
	rows, err := queryRows(ctx, r.cmd,
		"SELECT id, title, directory, time_created, time_updated FROM session WHERE id = '"+id+"' LIMIT 1")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if len(row) < 5 {
			continue
		}
		s := &sessionEntry{ID: row[0], Title: row[1], Directory: row[2]}
		s.Created, _ = strconv.ParseInt(row[3], 10, 64)
		s.Updated, _ = strconv.ParseInt(row[4], 10, 64)
		return s, nil
	}
	return nil, nil
}

// failedSync remembers (id, updated) pairs whose export already failed, so a
// permanently-too-large session isn't re-exported every tick. Entries expire
// after five minutes: a transient failure (service cold start, locked DB)
// must not freeze the session's shadow forever. Per-runtime: v1 and v2
// sessions share the ses_ id space but not stores.
type failedSync struct {
	mu      sync.Mutex
	entries map[string]failedEntry
}

type failedEntry struct {
	updated int64
	at      time.Time
}

const failedSyncTTL = 5 * time.Minute

func (f *failedSync) load(id string, updated int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[id]
	return ok && e.updated == updated && time.Since(e.at) < failedSyncTTL
}

func (f *failedSync) store(id string, updated int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) > 1000 {
		f.entries = map[string]failedEntry{}
	}
	f.entries[id] = failedEntry{updated: updated, at: time.Now()}
}

// sessionIDPattern guards the SQL interpolation below — session ids come from
// `opencode session list` and are always ses_<alphanum>, but a defensive check
// keeps a malformed id out of the query string.
var sessionIDPattern = regexp.MustCompile(`^ses_[A-Za-z0-9]+$`)

// queryRows runs `opencode db <sql> --format tsv` and returns the rows with
// all columns. TSV has a header line and tab-separated columns; JSON payload
// columns are single-line blobs (newlines/tabs stay escaped), so a naive
// split is safe.
func queryRows(ctx context.Context, cmd, sql string) ([][]string, error) {
	out, err := exec.CommandContext(ctx, cmd, "db", sql, "--format", "tsv").Output()
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for i, line := range strings.Split(string(out), "\n") {
		if line == "" || i == 0 {
			continue // header / trailing blank
		}
		rows = append(rows, strings.Split(line, "\t"))
	}
	return rows, nil
}

type exportMessage struct {
	Info struct {
		ID         string      `json:"id"`
		Role       string      `json:"role"`
		ProviderID string      `json:"providerID"`
		ModelID    string      `json:"modelID"`
		Tokens     *tokenUsage `json:"tokens"`
		Time       struct {
			Created int64 `json:"created"`
		} `json:"time"`
	} `json:"info"`
	Parts []exportPart `json:"parts"`
}

type exportPart struct {
	Type   string     `json:"type"`
	Text   string     `json:"text"`
	Tool   string     `json:"tool"`
	CallID string     `json:"callID"`
	State  *toolState `json:"state"`
}

// tokenUsage mirrors opencode's per-message token accounting (DB + stream).
type tokenUsage struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Write int64 `json:"write"`
		Read  int64 `json:"read"`
	} `json:"cache"`
}

type messageData struct {
	Role       string      `json:"role"`
	ModelID    string      `json:"modelID"`
	ProviderID string      `json:"providerID"`
	Tokens     *tokenUsage `json:"tokens"`
	Time       struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

// fetchSession mirrors one session's full transcript into its shadow jsonl by
// querying opencode's SQLite store through `opencode db`. The JSON output
// format caps at 64KiB; TSV streams unbounded, one row per line, with the
// message/part payload as a single-line JSON blob in the last column.
func fetchSession(ctx context.Context, rt *Runtime, s sessionEntry, path string) error {
	if !sessionIDPattern.MatchString(s.ID) {
		return fmt.Errorf("refusing unexpected session id %q", s.ID)
	}
	messages, err := queryRows(ctx, rt.Cmd(),
		"SELECT id, data FROM message WHERE session_id = '"+s.ID+"' ORDER BY time_created ASC LIMIT 100000")
	if err != nil {
		return err
	}
	partRows, err := queryRows(ctx, rt.Cmd(),
		"SELECT message_id, data FROM part WHERE session_id = '"+s.ID+"' ORDER BY time_created ASC LIMIT 100000")
	if err != nil {
		return err
	}
	partsByMsg := make(map[string][]exportPart, len(messages))
	for _, row := range partRows {
		if len(row) < 2 {
			continue
		}
		var p exportPart
		if json.Unmarshal([]byte(row[1]), &p) != nil {
			continue
		}
		partsByMsg[row[0]] = append(partsByMsg[row[0]], p)
	}

	var buf []byte
	write := func(raw json.RawMessage) {
		if raw == nil {
			return
		}
		buf = append(buf, raw...)
		buf = append(buf, '\n')
	}
	sid := s.ID
	cwd := s.Directory
	// The DB title is authoritative (opencode auto-generates it); the shadow's
	// first user line is often a system-reminder injection, which would
	// otherwise become the displayed title. Skip opencode's "New session"
	// placeholder — the first prompt is a better title than that, and the
	// real title lands on a later sync.
	if s.Title != "" && s.Title != "New session" {
		write(mustMarshal(map[string]any{
			"type":      "ai-title",
			"aiTitle":   s.Title,
			"sessionId": sid,
			"timestamp": eventTime(s.Created),
		}))
	}
	for _, row := range messages {
		if len(row) < 2 {
			continue
		}
		var md messageData
		if json.Unmarshal([]byte(row[1]), &md) != nil {
			continue
		}
		ts := eventTime(md.Time.Created)
		msg := exportMessage{}
		msg.Info.ID = row[0]
		msg.Info.Role = md.Role
		msg.Info.ModelID = md.ModelID
		msg.Info.ProviderID = md.ProviderID
		msg.Info.Tokens = md.Tokens
		msg.Info.Time.Created = md.Time.Created
		msg.Parts = partsByMsg[row[0]]

		if md.Role == "user" {
			var texts []string
			for _, p := range msg.Parts {
				if p.Type == "text" && p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
			if len(texts) == 0 {
				continue
			}
			write(userLineWithUUID(s.ID, cwd, joinTexts(texts), ts, row[0]))
			continue
		}
		if md.Role != "assistant" {
			continue
		}
		window := rt.modelWindow(msg)
		for _, p := range msg.Parts {
			switch p.Type {
			case "text":
				write(assistantLineModel(s.ID, textBlocks(p.Text), ts, msg, window))
			case "reasoning":
				write(assistantLineModel(s.ID, thinkingBlocks(p.Text), ts, msg, window))
			case "tool":
				if p.State == nil {
					continue
				}
				pp := partPayload{ID: p.CallID, Tool: p.Tool, CallID: p.CallID, State: p.State}
				write(assistantLineModel(s.ID, toolUseBlocks(pp), ts, msg, window))
				write(toolResultLine(s.ID, cwd, pp, ts))
			}
		}
		write(turnCompleteLine(s.ID, ts))
	}
	write(syncMetaLine(s))
	if len(buf) == 0 {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func joinTexts(texts []string) string {
	if len(texts) == 1 {
		return texts[0]
	}
	out := texts[0]
	for _, t := range texts[1:] {
		out += "\n" + t
	}
	return out
}

func userLineWithUUID(sessionID, cwd, content string, ts time.Time, uuid string) json.RawMessage {
	if uuid == "" {
		uuid = randomHexID()
	}
	return mustMarshal(map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"cwd":       cwd,
		"timestamp": ts,
		"uuid":      uuid,
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	})
}

// assistantLineModel is assistantLine plus the model tag the assembler reads
// for per-turn model display. opencode stamps provider/model per message. It
// also carries token usage in Claude's shape so ReadSessionMeta surfaces
// context usage for the session list/detail views; window (the model's
// context limit from the catalog) rides along as a nonstandard usage field
// so synced sessions can show their limit without a live runtime event.
func assistantLineModel(sessionID string, blocks []map[string]any, ts time.Time, m exportMessage, window int64) json.RawMessage {
	if len(blocks) == 0 {
		return nil
	}
	model := m.Info.ModelID
	if m.Info.ProviderID != "" && model != "" {
		model = m.Info.ProviderID + "/" + model
	}
	uuid := m.Info.ID
	if uuid == "" {
		uuid = randomHexID()
	}
	msg := map[string]any{
		"role":    "assistant",
		"model":   model,
		"content": blocks,
	}
	if m.Info.Tokens != nil || window > 0 {
		usage := map[string]any{}
		if m.Info.Tokens != nil {
			usage["input_tokens"] = m.Info.Tokens.Input
			usage["output_tokens"] = m.Info.Tokens.Output
			usage["cache_read_input_tokens"] = m.Info.Tokens.Cache.Read
			usage["cache_creation_input_tokens"] = m.Info.Tokens.Cache.Write
		}
		if window > 0 {
			usage["context_window"] = window
		}
		msg["usage"] = usage
	}
	return mustMarshal(map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"timestamp": ts,
		"uuid":      uuid,
		"message":   msg,
	})
}

// modelWindow resolves the context limit for a session model reference
// through the runtime's catalog cache (v1: models --verbose; v2: /api/model).
func (r *Runtime) modelWindow(m exportMessage) int64 {
	model := m.Info.ModelID
	if m.Info.ProviderID != "" && model != "" {
		model = m.Info.ProviderID + "/" + model
	}
	return r.contextWindow(model)
}
