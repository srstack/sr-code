package opencode

import (
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
	syncOnce(ctx, rt, logger)
	tick := time.NewTicker(syncInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			syncOnce(ctx, rt, logger)
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
	forgotten.Store(id, time.Now())
	r.persistTombstones()
	// `opencode session delete` asks for interactive confirmation; answer it.
	cmd := exec.CommandContext(ctx, r.Cmd(), "session", "delete", id)
	cmd.Stdin = strings.NewReader("y\n")
	return cmd.Run()
}

// forgotten is the tombstone set for usher-deleted sessions. Entries expire
// after a day — long past any reasonable native-delete propagation.
var forgotten sync.Map // id -> time.Time

func forgottenHas(id string) bool {
	v, ok := forgotten.Load(id)
	if !ok {
		return false
	}
	if time.Since(v.(time.Time)) > 24*time.Hour {
		forgotten.Delete(id)
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
		forgotten.Store(id, ts)
	}
}

func (r *Runtime) persistTombstones() {
	entries := map[string]time.Time{}
	forgotten.Range(func(k, v any) bool {
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
		if forgottenHas(s.ID) {
			continue // deleted via usher; don't resurrect
		}
		if rt.Has(s.ID) {
			continue // live turn owns the shadow file
		}
		path := logPath(rt.Root(), s.Directory, s.ID)
		if fi, err := os.Stat(path); err == nil && !fi.ModTime().Before(time.UnixMilli(s.Updated)) {
			continue // shadow is already at least as fresh as the session
		}
		// Transcripts are fetched with SQL via `opencode db` (paged rows, no
		// size cap) rather than `opencode export`, whose stdout caps at 128KiB
		// and loses every larger session — including long-running ones.
		if failedSyncLoad(s.ID, s.Updated) {
			continue
		}
		if err := fetchSession(ctx, rt, s, path); err != nil {
			failedSyncStore(s.ID, s.Updated)
			logger.Warn("opencode sync: fetch failed", "session", s.ID, "err", err)
		}
	}
}

// failedSync remembers (id, updated) pairs whose export already failed, so a
// permanently-too-large session isn't re-exported every tick.
type failedSync struct {
	mu      sync.Mutex
	entries map[string]int64
}

var failures = &failedSync{entries: map[string]int64{}}

func failedSyncLoad(id string, updated int64) bool {
	failures.mu.Lock()
	defer failures.mu.Unlock()
	ts, ok := failures.entries[id]
	return ok && ts == updated
}

func failedSyncStore(id string, updated int64) {
	failures.mu.Lock()
	defer failures.mu.Unlock()
	if len(failures.entries) > 1000 {
		failures.entries = map[string]int64{}
	}
	failures.entries[id] = updated
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
	// otherwise become the displayed title.
	if s.Title != "" {
		write(mustMarshal(map[string]any{
			"type":      "ai-title",
			"aiTitle":   s.Title,
			"sessionId": sid,
			"timestamp": eventTime(s.Created),
		}))
	}
	for _, row := range messages {
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
		for _, p := range msg.Parts {
			switch p.Type {
			case "text":
				write(assistantLineModel(s.ID, textBlocks(p.Text), ts, msg))
			case "reasoning":
				write(assistantLineModel(s.ID, thinkingBlocks(p.Text), ts, msg))
			case "tool":
				if p.State == nil {
					continue
				}
				pp := partPayload{ID: p.CallID, Tool: p.Tool, CallID: p.CallID, State: p.State}
				write(assistantLineModel(s.ID, toolUseBlocks(pp), ts, msg))
				write(toolResultLine(s.ID, cwd, pp, ts))
			}
		}
		write(turnCompleteLine(s.ID, ts))
	}
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
// context usage for the session list/detail views.
func assistantLineModel(sessionID string, blocks []map[string]any, ts time.Time, m exportMessage) json.RawMessage {
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
	if m.Info.Tokens != nil {
		msg["usage"] = map[string]any{
			"input_tokens":                m.Info.Tokens.Input,
			"output_tokens":               m.Info.Tokens.Output,
			"cache_read_input_tokens":     m.Info.Tokens.Cache.Read,
			"cache_creation_input_tokens": m.Info.Tokens.Cache.Write,
		}
	}
	return mustMarshal(map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"timestamp": ts,
		"uuid":      uuid,
		"message":   msg,
	})
}
