package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
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
	// Activity is an extra liveness signal alongside Updated: v2's cumulative
	// session token total, which moves at step boundaries while Updated stays
	// frozen for whole turns. 0 for v1 (no cheap equivalent) — the check is
	// skipped then.
	Activity int64 `json:"activity"`
}

// syncMeta is the parsed footer of a full shadow rewrite. Beyond the native
// timestamps it records whether the fetch observed a turn in flight, because
// opencode freezes session.updated (and every message/part timestamp) for the
// whole duration of a turn — proven on both v1 and v2 — so a timestamp-only
// freshness check stops syncing minutes into any long turn and the UI freezes
// at the turn's start.
type syncMeta struct {
	version          int
	sourceUpdated    int64
	sourceActivity   int64
	inflight         bool
	inflightSince    int64  // ms; last time the fetched content changed while inflight
	contentChangedAt int64  // ms; last time the fetched content changed at all
	fetchHash        string // FNV-1a of the rewritten body, detects content progress
}

// syncMetaVersion bumps force one rewrite per session after an upgrade, so
// shadows synced by an older format (no inflight tracking) get re-evaluated
// instead of sitting "settled" behind a frozen native updated.
const syncMetaVersion = 2

// syncMetaLine is the footer every full shadow rewrite ends with. The
// freshness check compares sourceUpdated against the native session's
// updated timestamp — file mtime is NOT trustworthy (a runtime append or an
// unrelated touch looks like a fresh sync and permanently suppresses it).
func syncMetaLine(s sessionEntry, inflight bool, prev syncMeta, hash string) json.RawMessage {
	// Any content change (not just native timestamps — those freeze mid-turn)
	// resets the settle clock; an unchanged body keeps it running.
	changedAt := prev.contentChangedAt
	if prev.fetchHash != hash || changedAt == 0 {
		changedAt = time.Now().UnixMilli()
	}
	var since int64
	if inflight {
		// Keep the clock from the previous fetch when the content didn't move
		// (stalled/dead turn); reset it on progress so a long live turn keeps
		// refetching indefinitely while a dead one settles after the ceiling.
		since = prev.inflightSince
		if !prev.inflight || prev.fetchHash != hash || since == 0 {
			since = time.Now().UnixMilli()
		}
	}
	return mustMarshal(map[string]any{
		"type":             "system",
		"subtype":          "sync-meta",
		"sessionId":        s.ID,
		"timestamp":        eventTime(s.Updated),
		"uuid":             randomHexID(),
		"metaV":            syncMetaVersion,
		"sourceUpdated":    s.Updated,
		"sourceActivity":   s.Activity,
		"inflight":         inflight,
		"inflightSince":    since,
		"contentChangedAt": changedAt,
		"fetchHash":        hash,
	})
}

// readSyncMeta returns the marker from a shadow's tail.
// ok=false: no marker (pre-marker shadow) → treat as stale and rewrite once.
func readSyncMeta(path string) (meta syncMeta, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return syncMeta{}, false
	}
	const tail = 8192
	start := int64(0)
	if fi.Size() > tail {
		start = fi.Size() - tail
	}
	f, err := os.Open(path)
	if err != nil {
		return syncMeta{}, false
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
			Subtype          string `json:"subtype"`
			MetaV            int    `json:"metaV"`
			SourceUpdated    int64  `json:"sourceUpdated"`
			SourceActivity   int64  `json:"sourceActivity"`
			Inflight         bool   `json:"inflight"`
			InflightSince    int64  `json:"inflightSince"`
			ContentChangedAt int64  `json:"contentChangedAt"`
			FetchHash        string `json:"fetchHash"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Subtype == "sync-meta" && ev.SourceUpdated > 0 {
			meta = syncMeta{
				version:          ev.MetaV,
				sourceUpdated:    ev.SourceUpdated,
				sourceActivity:   ev.SourceActivity,
				inflight:         ev.Inflight,
				inflightSince:    ev.InflightSince,
				contentChangedAt: ev.ContentChangedAt,
				fetchHash:        ev.FetchHash,
			}
			ok = true
		}
	}
	return meta, ok
}

// shadowFresh reports whether the shadow at path already mirrors the native
// session state.
//
// The native `updated` LAGS real activity badly: it freezes for the whole
// duration of a turn (both v1 and v2) and on v2 trails the final message
// commit by minutes. Timestamps alone therefore cannot declare a shadow
// settled; the settle clock is driven by the fetched CONTENT instead:
//   - contentChangedAt: the shadow keeps refetching until its body has been
//     byte-identical for syncSettleWindow — a turn in progress keeps changing
//     the content (step finishes, tool completions), however frozen updated
//     and the token counter are.
//   - the inflight flag: the last fetch saw the turn still running
//     (unanswered user message, unfinished step, running tool) — long tool
//     calls produce no new content, so this keeps polling until they finish;
//     bounded by inflightRefetchCeiling after the last content change so a
//     dead turn (killed mid-tool, status frozen "running") settles.
//   - sourceActivity (v2's cumulative tokens) and sourceUpdated drift always
//     force a refetch, catching step boundaries the content check precedes.
const syncSettleWindow = 2 * time.Minute
const inflightRefetchCeiling = 30 * time.Minute

func shadowFresh(path string, s sessionEntry) bool {
	m, ok := readSyncMeta(path)
	if !ok || m.version < syncMetaVersion || m.sourceUpdated < s.Updated {
		return false
	}
	if s.Activity > 0 && m.sourceActivity > 0 && m.sourceActivity != s.Activity {
		return false
	}
	if m.inflight && time.Since(time.UnixMilli(m.inflightSince)) < inflightRefetchCeiling {
		return false
	}
	return time.Since(time.UnixMilli(m.contentChangedAt)) > syncSettleWindow
}

// fetchHashSum is a cheap content fingerprint of one shadow rewrite, recorded
// in the sync meta to tell "turn stalled" apart from "turn progressing".
func fetchHashSum(buf []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(buf)
	return strconv.FormatUint(h.Sum64(), 16)
}

func syncOnce(ctx context.Context, rt *Runtime, logger *slog.Logger) {
	// `opencode session list` only covers a slice of projects (observed: a
	// few directories' worth); the session table is global. Read it directly
	// so sessions from every project show up, not just recent ones.
	db, err := rt.v1DB(ctx)
	if err != nil {
		logger.Warn("opencode sync: store open failed", "err", err)
		return
	}
	rows, err := queryTextRows(ctx, db,
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
		if shadowFresh(path, s) {
			continue
		}
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
		if shadowFresh(path, s) {
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
	if shadowFresh(path, *s) {
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
		// The single-session endpoint wraps its payload in {"data": …} like
		// the list endpoint (and returns an error object for unknown ids).
		var resp struct {
			Data v2Session `json:"data"`
		}
		if err := apiGetJSON(ctx, r.cmd, "/api/session/"+id, &resp); err != nil {
			return nil, err
		}
		s := resp.Data
		if s.ID == "" || s.Location.Directory == "" {
			return nil, nil
		}
		return &sessionEntry{
			ID: s.ID, Title: s.Title, Directory: s.Location.Directory,
			Created: s.Time.Created, Updated: s.Time.Updated,
			Activity: s.activityTotal(),
		}, nil
	}
	db, err := r.v1DB(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := queryTextRows(ctx, db,
		"SELECT id, title, directory, time_created, time_updated FROM session WHERE id = ? LIMIT 1", id)
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

// sessionIDPattern guards against malformed ids even though queries are now
// parameterized — it also rejects ids that were never ses_-shaped early.
var sessionIDPattern = regexp.MustCompile(`^ses_[A-Za-z0-9]+$`)

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

// fetchSession mirrors one session's full transcript into its shadow jsonl,
// reading opencode's SQLite store directly (see db.go). The old `opencode db`
// shell-out could silently truncate the newest rows under store contention;
// direct reads are complete by construction, so no paging or completeness
// machinery is needed here.
func fetchSession(ctx context.Context, rt *Runtime, s sessionEntry, path string) error {
	if !sessionIDPattern.MatchString(s.ID) {
		return fmt.Errorf("refusing unexpected session id %q", s.ID)
	}
	db, err := rt.v1DB(ctx)
	if err != nil {
		return err
	}
	messages, err := queryTextRows(ctx, db,
		"SELECT id, data FROM message WHERE session_id = ? ORDER BY time_created ASC LIMIT 100000", s.ID)
	if err != nil {
		return err
	}
	partRows, err := queryTextRows(ctx, db,
		"SELECT id, message_id, data FROM part WHERE session_id = ? ORDER BY time_created ASC LIMIT 100000", s.ID)
	if err != nil {
		return err
	}
	partsByMsg := make(map[string][]exportPart, len(messages))
	for _, row := range partRows {
		if len(row) < 3 {
			continue
		}
		var p exportPart
		if json.Unmarshal([]byte(row[2]), &p) != nil {
			continue
		}
		partsByMsg[row[1]] = append(partsByMsg[row[1]], p)
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
	var last exportMessage // last raw message row, for inflight detection
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
		last = msg

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
	prev, _ := readSyncMeta(path)
	write(syncMetaLine(s, v1TurnInflight(last), prev, fetchHashSum(buf)))
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

// v1TurnInflight reports whether a session's native transcript ends mid-turn:
// the last message is an unanswered user prompt, or the last assistant
// message has an unfinished step (step-start without step-finish) or a
// running/pending tool. Native timestamps freeze while a turn runs, so this
// content signal is what keeps the sync refetching until the turn completes.
func v1TurnInflight(last exportMessage) bool {
	if last.Info.ID == "" {
		return false
	}
	if last.Info.Role == "user" {
		return true
	}
	if last.Info.Role != "assistant" {
		return false
	}
	var starts, finishes int
	for _, p := range last.Parts {
		switch p.Type {
		case "step-start":
			starts++
		case "step-finish":
			finishes++
		case "tool":
			if p.State != nil && (p.State.Status == "running" || p.State.Status == "pending") {
				return true
			}
		}
	}
	// A completed assistant message closes every step it opened; zero
	// finishes means the message row exists but its first step hasn't
	// committed yet (turn just started).
	return starts > finishes || finishes == 0
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
