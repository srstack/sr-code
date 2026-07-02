// Package router glues discovery, sender, broker, and hook together. It is
// the central coordinator that the web layer and the Usher Agent both go
// through, and serves as the type that satisfies the agent's AgentAPI
// contract — keeping the agent's surface a strict subset of usher's
// internals.
package router

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/codexrollout"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/discovery"
	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/jsonl"
	"github.com/nexustar/usher/internal/sender"
	"github.com/nexustar/usher/internal/sessionmeta"
)

// ErrSessionNotFound is returned when an operation targets a session with no
// log on disk (so its path/backend can't be resolved).
var ErrSessionNotFound = errors.New("session not found")

type Router struct {
	discovery *discovery.Discovery
	// senders holds one Sender per backend ("claude", "codex"); usher manages
	// both at once. A send is routed by the session's Backend tag (existing
	// sessions) or the chosen model (new sessions). defaultBackend is the
	// fallback when a backend is unknown/empty.
	senders        map[string]*sender.Sender
	defaultBackend string
	broker         *broker.Broker
	hooks          *hook.Manager
	meta           *sessionmeta.Store

	sendMu     sync.Mutex
	activeSend map[string]*sendToken    // sessionID -> in-flight turn's cancel handle
	sendQueue  map[string][]pendingSend // sessionID -> sends waiting for the turn to end
	creating   map[string]core.Session  // sessions usher is spawning, not yet on disk

	// runTurn overrides the turn executor ((*Router).runSend) in tests, so
	// queue mechanics are testable without a live tmux sender. nil in
	// production.
	runTurn func(ctx context.Context, sessionID, prompt, cwd string, tok *sendToken)
}

// startTurn launches one turn's executor goroutine (the seam runTurn tests
// override).
func (r *Router) startTurn(ctx context.Context, sessionID, prompt, cwd string, tok *sendToken) {
	run := r.runTurn
	if run == nil {
		run = r.runSend
	}
	go run(ctx, sessionID, prompt, cwd, tok)
}

// sendToken pairs a cancel function with a unique pointer identity so that a
// finishing goroutine only deletes its own entry — never the entry of a
// later send that replaced it.
type sendToken struct {
	cancel context.CancelFunc
}

// pendingSend is one send waiting in a session's FIFO queue behind an
// in-flight turn. pre (optional) runs just before the turn starts — after
// every event of the previous turn has been published — so a relay collector
// can subscribe with correct turn attribution. abort (optional) runs instead
// if the queued send is dropped (session deleted, turn cancelled).
type pendingSend struct {
	text  string
	pre   func()
	abort func(err error)
}

// maxQueuedSends bounds one session's send queue — a backstop against a
// looping agent, far above anything a human produces.
const maxQueuedSends = 32

// New builds a Router over the given backends (at least one). senders maps a
// backend name ("claude"/"codex") to its Sender; defaultBackend names the one to
// fall back to for unknown/empty backends and is the new-session default — it
// must be a key in senders.
func New(d *discovery.Discovery, senders map[string]*sender.Sender, defaultBackend string, b *broker.Broker, h *hook.Manager, meta *sessionmeta.Store) *Router {
	return &Router{
		discovery:      d,
		senders:        senders,
		defaultBackend: defaultBackend,
		broker:         b,
		hooks:          h,
		meta:           meta,
		activeSend:     map[string]*sendToken{},
		sendQueue:      map[string][]pendingSend{},
		creating:       map[string]core.Session{},
	}
}

// Backends returns the enabled backend names, sorted ("claude" before "codex").
// The web layer uses it to show only available backends in the model picker.
func (r *Router) Backends() []string {
	out := make([]string, 0, len(r.senders))
	for b := range r.senders {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// senderForBackend returns the Sender for a backend, falling back to the
// default when the backend is empty or unregistered.
func (r *Router) senderForBackend(backend string) *sender.Sender {
	if s, ok := r.senders[backend]; ok {
		return s
	}
	return r.senders[r.defaultBackend]
}

// senderFor returns the Sender owning an existing session, by its Backend tag.
func (r *Router) senderFor(id string) *sender.Sender {
	if s, ok := r.discovery.Get(id); ok {
		return r.senderForBackend(s.Backend)
	}
	return r.senders[r.defaultBackend]
}

// anyHas reports whether any backend's sender holds a live process for id —
// used for hook ownership, where the session's backend may not be resolved yet.
func (r *Router) anyHas(id string) bool {
	for _, s := range r.senders {
		if s.Has(id) {
			return true
		}
	}
	return false
}

// liveSet unions the live-session ids across every backend's sender.
func (r *Router) liveSet() map[string]bool {
	set := map[string]bool{}
	for _, s := range r.senders {
		for _, id := range s.LiveSessions() {
			set[id] = true
		}
	}
	return set
}

// backendOf returns the backend a session belongs to, or the default if the
// session isn't known to discovery yet.
func (r *Router) backendOf(id string) string {
	if s, ok := r.discovery.Get(id); ok && s.Backend != "" {
		return s.Backend
	}
	return r.defaultBackend
}

// readTurnsForBackend parses a session log into display turns using the parser
// for its backend: Codex rollouts vs Claude jsonl. Both return the same shape.
func readTurnsForBackend(path, backend string, limit int) ([]jsonl.Turn, int, error) {
	if backend == "codex" {
		return codexrollout.ReadTurns(path, limit)
	}
	return jsonl.ReadTurns(path, limit)
}

// ReadTurns resolves a session's log path and backend and returns its grouped
// display turns (and the pre-trim total). Returns ErrSessionNotFound when the
// session has no log on disk.
func (r *Router) ReadTurns(id string, limit int) ([]jsonl.Turn, int, error) {
	path, ok := r.discovery.Path(id)
	if !ok {
		return nil, 0, ErrSessionNotFound
	}
	return readTurnsForBackend(path, r.backendOf(id), limit)
}

// BackendForModel exposes backendForModel to other packages (the web layer's
// model gate) — which backend a chosen model routes to.
func BackendForModel(model string) string { return backendForModel(model) }

// backendForModel maps a new-session model choice to its backend. Model names
// are unique across backends except the literal "default" (the UI resolves that
// to an explicit backend); gpt-*/o-series/codex are Codex, everything else
// (claude-*, opus, sonnet, haiku, fable) is Claude.
func backendForModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o1"),
		strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"),
		strings.Contains(m, "codex"):
		return "codex"
	default:
		return "claude"
	}
}

// --- session reads -------------------------------------------------------

// ListSessions returns sessions decorated with their current run state: a
// turn in flight is "running"; otherwise a warm pooled process is "live".
func (r *Router) ListSessions() []core.Session {
	sessions := r.discovery.List()
	live := r.liveSet()
	r.sendMu.Lock()
	known := make(map[string]bool, len(sessions))
	for i := range sessions {
		known[sessions[i].ID] = true
		if _, running := r.activeSend[sessions[i].ID]; running {
			sessions[i].Status = core.StatusRunning
		} else if live[sessions[i].ID] {
			sessions[i].Status = core.StatusLive
		}
		r.applyCustomTitle(&sessions[i])
	}
	var pending []core.Session
	for id, s := range r.creating {
		if !known[id] {
			r.applyCustomTitle(&s)
			pending = append(pending, s)
		}
	}
	r.sendMu.Unlock()
	return append(pending, sessions...)
}

func (r *Router) GetSession(id string) (core.Session, bool) {
	sess, ok := r.discovery.Get(id)
	if !ok {
		// Not on disk yet — fall back to the creating-overlay so a just-spawned
		// session's detail view opens instead of 404ing.
		r.sendMu.Lock()
		sess, ok = r.creating[id]
		r.sendMu.Unlock()
		if ok {
			r.applyCustomTitle(&sess)
		}
		return sess, ok
	}
	r.sendMu.Lock()
	_, running := r.activeSend[id]
	r.sendMu.Unlock()
	if running {
		sess.Status = core.StatusRunning
	} else if r.senderForBackend(sess.Backend).Has(id) {
		sess.Status = core.StatusLive
	}
	r.applyCustomTitle(&sess)
	return sess, true
}

func (r *Router) SessionPath(id string) (string, bool) {
	return r.discovery.Path(id)
}

// ForkSession branches the conversation of srcID into a brand-new session:
// a prefix copy of its jsonl through the turn containing afterUUID (see
// jsonl.ForkCopy), under a fresh id in the same project dir. Pure file
// operation — no process is spawned; the fork is resumed lazily by the pool
// on its first send, like any idle session. Returns the new session id.
func (r *Router) ForkSession(srcID, afterUUID string) (string, error) {
	path, ok := r.discovery.Path(srcID)
	if !ok {
		return "", ErrSessionNotFound
	}
	newID := newUUIDv4()
	dir := filepath.Dir(path)

	var dstPath string
	var err error
	if r.backendOf(srcID) == "codex" {
		// Codex rollout: name the fork rollout-<ts>-<id>.jsonl (the shape discovery
		// matches) and truncate at the turn whose task_complete is afterUUID.
		dstPath = filepath.Join(dir, codexrollout.RolloutFilename(newID, time.Now()))
		err = codexrollout.ForkCopy(path, dstPath, afterUUID, newID, srcID)
	} else {
		dstPath = filepath.Join(dir, newID+".jsonl")
		err = jsonl.ForkCopy(path, dstPath, afterUUID, newID)
	}
	if err != nil {
		return "", err
	}
	// Ingest synchronously so the id resolves the moment the client navigates
	// to it, instead of racing the fsnotify watcher.
	r.discovery.Upsert(dstPath)
	return newID, nil
}

// staleClock is what auto-archive measures inactivity from: last user input,
// falling back to last event. Keying on input (not mtime) means pause/kill and
// streaming don't reset the countdown — mirrors discovery's sort.
func staleClock(s core.Session) time.Time {
	if !s.LastInputAt.IsZero() {
		return s.LastInputAt
	}
	return s.LastEventAt
}

func (r *Router) IsArchived(sessionID string) bool {
	sess, ok := r.discovery.Get(sessionID)
	if !ok {
		return false
	}
	return r.meta.IsArchived(sessionID, staleClock(sess), time.Now())
}

func (r *Router) Archive(sessionID string)       { r.meta.Archive(sessionID) }
func (r *Router) IsPinned(sessionID string) bool { return r.meta.IsPinned(sessionID) }
func (r *Router) Pin(sessionID string)           { r.meta.Pin(sessionID) }
func (r *Router) Unpin(sessionID string)         { r.meta.Unpin(sessionID) }
func (r *Router) Rename(sessionID, title string) { r.meta.Rename(sessionID, title) }

func (r *Router) applyCustomTitle(s *core.Session) {
	if t := r.meta.CustomTitle(s.ID); t != "" {
		s.Title = t
	}
}

func (r *Router) Unarchive(sessionID string) {
	sess, _ := r.discovery.Get(sessionID)
	r.meta.Unarchive(sessionID, staleClock(sess), time.Now())
}

// DeleteSession permanently removes a session: it cancels any in-flight turn,
// kills usher's live window for it (if any), deletes the session jsonl from
// disk, and forgets all per-session state (archive decision, auto-approve,
// remembered permission rules). Irreversible — the conversation is gone with
// the file. Errors if the session is unknown or the file delete fails; the
// live-process teardown is best-effort. Unlike Archive (a reversible sidebar
// hide), this is destructive.
func (r *Router) DeleteSession(id string) error {
	path, ok := r.discovery.Path(id)
	if !ok {
		return errors.New("session not found")
	}
	// Release any in-flight turn first so its tail goroutine stops before the
	// file is pulled out from under it.
	r.stopLive(id)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete session file: %w", err)
	}
	// Forget it synchronously so the id stops resolving immediately instead of
	// racing the fsnotify Remove event (mirror of fork's Upsert).
	r.discovery.Remove(id)
	r.meta.Forget(id)
	r.hooks.SetAutoApprove(id, false)
	r.hooks.ForgetSessionRules(id)
	return nil
}

// PauseSession kills usher's live window for a session without touching its
// jsonl or per-session state: it cancels any in-flight turn and the tmux
// window, dropping the session to "idle". The conversation survives and
// resumes (via --resume) on the next Send. The manual equivalent of LRU
// eviction. Idempotent for an already-idle session; errors only on unknown id.
func (r *Router) PauseSession(id string) error {
	if _, ok := r.discovery.Path(id); !ok {
		return errors.New("session not found")
	}
	r.stopLive(id)
	return nil
}

// stopLive drops a session to idle: cancels any in-flight turn, drops queued
// sends, and kills its live window. Best-effort; idempotent. Shared by
// PauseSession/DeleteSession.
func (r *Router) stopLive(id string) {
	r.flushSendQueue(id, errors.New("session stopped"))
	r.sendMu.Lock()
	tok := r.activeSend[id]
	r.sendMu.Unlock()
	if tok != nil {
		tok.cancel()
	}
	if err := r.senderFor(id).Kill(id); err != nil {
		slog.Warn("kill session window", "session", id, "err", err)
	}
}

// --- session writes ------------------------------------------------------

// SendToSession delivers text to the session as the next turn. If the session
// is idle the turn starts immediately (fire-and-forget; stream events go to
// broker subscribers); if a turn is in flight the send waits in the session's
// FIFO queue and is injected when that turn ends. Returns an error only if
// the session is unknown or the queue is full.
func (r *Router) SendToSession(id, text string) error {
	return r.enqueueSend(id, text, nil, nil)
}

// enqueueSend is the single entry point for turn-tracked sends: run now if
// the session is idle, queue otherwise. Serializing usher-injected turns per
// session is what makes "subscribe, then collect until subprocess.exit" a
// sound way to capture one turn's reply — a send injected mid-turn would
// tail the PREVIOUS turn's remainder instead. Typing directly into an
// attached tmux pane still bypasses this (the manual-attach corner).
func (r *Router) enqueueSend(id, text string, pre func(), abort func(error)) error {
	sess, ok := r.discovery.Get(id)
	if !ok {
		return errors.New("session not found")
	}
	// A '!'-prefixed message is not a model turn: Claude Code runs it as a TUI
	// bash command, which logs no turn_duration for the tailer to wait on
	// (and bracketed paste can't neutralize a leading '!', unlike '/' or
	// '@'). Bash lines skip the queue: claude runs them even mid-turn.
	if strings.HasPrefix(text, "!") {
		if pre != nil {
			pre()
		}
		go r.injectDirect(sess.ID, text, sess.Cwd)
		return nil
	}
	// Reorder the sidebar the instant the user sends, without waiting for the
	// prompt to land in the jsonl (see discovery.MarkInput).
	r.discovery.MarkInput(id, time.Now().UTC())

	r.sendMu.Lock()
	if _, busy := r.activeSend[id]; busy || len(r.sendQueue[id]) > 0 {
		if len(r.sendQueue[id]) >= maxQueuedSends {
			r.sendMu.Unlock()
			return errors.New("send queue full for session")
		}
		r.sendQueue[id] = append(r.sendQueue[id], pendingSend{text: text, pre: pre, abort: abort})
		r.sendMu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	tok := &sendToken{cancel: cancel}
	r.activeSend[id] = tok
	r.sendMu.Unlock()

	if pre != nil {
		pre()
	}
	r.startTurn(ctx, sess.ID, text, sess.Cwd, tok)
	return nil
}

// injectDirect pastes text without turn tracking (see the '!' note in
// SendToSession), then emits the turn.user + subprocess.exit events a normal
// turn would, so the web client adopts the echo and returns to idle with no
// special-casing. No activeSend: nothing to cancel, and the session stays
// "live" rather than "running". The 45s budget covers a cold window's resume.
func (r *Router) injectDirect(sessionID, text, cwd string) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := r.senderFor(sessionID).Inject(ctx, sessionID, text, cwd); err != nil {
		errMsg, _ := json.Marshal(map[string]string{"message": err.Error()})
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: "error", Raw: errMsg})
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: "subprocess.exit", Raw: json.RawMessage(`{}`)})
		return
	}
	uraw, _ := json.Marshal(map[string]any{"role": "user", "content": text, "ts": time.Now().UTC()})
	r.broker.Publish(broker.Event{SessionID: sessionID, Type: "turn.user", Raw: uraw})
	r.broker.Publish(broker.Event{SessionID: sessionID, Type: "subprocess.exit", Raw: json.RawMessage(`{}`)})
}

func (r *Router) runSend(ctx context.Context, sessionID, prompt, cwd string, tok *sendToken) {
	defer r.releaseSend(sessionID, tok)

	ch, err := r.senderFor(sessionID).Send(ctx, sessionID, prompt, cwd)
	if err != nil {
		r.markSendIdle(sessionID, tok)
		errMsg, _ := json.Marshal(map[string]string{"message": err.Error()})
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: "error", Raw: errMsg})
		return
	}
	asm := newStreamAssembler(r.backendOf(sessionID))
	for ev := range ch {
		// Publish BEFORE clearing the running bit: once activeSend is empty a
		// new send's collector may subscribe, and it must not see this
		// turn's exit.
		r.publishStream(sessionID, asm, ev)
		if ev.Type == "subprocess.exit" {
			r.markSendIdle(sessionID, tok)
		}
	}
}

// publishStream forwards one tail event to broker subscribers, deriving the
// display-ready turn events alongside the raw line:
//
//   - the raw event keeps flowing under its jsonl type ("user", "assistant",
//     "system", …) for non-web consumers (other frontends consume these); the
//     web SSE layer filters them out in favour of the derived events, which
//     are far smaller on the wire (no thinking blocks, usage stats, or file
//     snapshots).
//   - "part": one TurnPart appended to the in-progress assistant turn,
//     grouped/rendered server-side by jsonl.Assembler — the same engine
//     behind ReadTurns, so a part streamed live and the same turn fetched
//     later from /transcript can never disagree.
//   - "turn.user": a canonical user turn hit the jsonl (the prompt usher just
//     injected, or one queued from another frontend mid-turn). Carries the
//     persisted timestamp, so clients can stamp their optimistic echo.
//
// Returns the appended part (nil otherwise) so callers that accumulate the
// turn's text (CreateSession) don't re-parse the payload.
// streamAssembler is the cross-backend turn-grouping engine: both
// jsonl.Assembler (Claude) and codexrollout.Assembler (Codex) implement it, so
// publishStream derives the same turn.user/part events from either backend's
// log lines.
type streamAssembler interface {
	FeedLine(raw []byte) (completed []jsonl.Turn, part *jsonl.TurnPart)
	Model() string
}

var (
	_ streamAssembler = (*jsonl.Assembler)(nil)
	_ streamAssembler = (*codexrollout.Assembler)(nil)
)

// newStreamAssembler returns the assembler for a backend's log shape.
func newStreamAssembler(backend string) streamAssembler {
	if backend == "codex" {
		return codexrollout.NewAssembler()
	}
	return jsonl.NewAssembler()
}

// isControlEvent reports whether a StreamEvent is a synthesized control signal
// (not a backend log line) and so must not be fed to the assembler.
func isControlEvent(t string) bool {
	return t == "subprocess.started" || t == "subprocess.exit" || t == "error"
}

// lineTimestamp pulls the top-level "timestamp" from a log line (present on both
// Claude and Codex lines); zero time if absent.
func lineTimestamp(raw json.RawMessage) time.Time {
	var o struct {
		Timestamp time.Time `json:"timestamp"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.Timestamp
}

func (r *Router) publishStream(sessionID string, asm streamAssembler, ev sender.StreamEvent) *jsonl.TurnPart {
	if ev.Type == "subprocess.exit" {
		ev.Raw = r.enrichExitWithTurnTimestamps(sessionID, ev.Raw)
	}
	r.broker.Publish(broker.Event{SessionID: sessionID, Type: ev.Type, Raw: ev.Raw})

	if isControlEvent(ev.Type) {
		return nil
	}
	// Feed every log line; the assembler ignores non-conversational ones
	// (Claude's system events, Codex's session_meta/token_count, etc.).
	completed, part := asm.FeedLine(ev.Raw)
	for _, t := range completed {
		if t.Role != "user" {
			// Assistant turns are finalized client-side by the turn-end
			// transcript truth-up; no event needed.
			continue
		}
		raw, mErr := json.Marshal(map[string]any{"role": "user", "content": t.Content, "ts": t.Time})
		if mErr == nil {
			r.broker.Publish(broker.Event{SessionID: sessionID, Type: "turn.user", Raw: raw})
		}
	}
	if part != nil {
		raw, mErr := json.Marshal(map[string]any{
			"role": "assistant", "ts": lineTimestamp(ev.Raw), "model": asm.Model(), "part": part,
		})
		if mErr == nil {
			r.broker.Publish(broker.Event{SessionID: sessionID, Type: "part", Raw: raw})
		}
	}
	return part
}

// enrichExitWithTurnTimestamps reads the last two user/assistant turns from
// the session jsonl and injects their timestamps into the exit event so
// the web UI can replace its optimistically-stamped chat messages with the
// canonical server timestamps. Best-effort: any read failure leaves the
// payload untouched.
func (r *Router) enrichExitWithTurnTimestamps(sessionID string, raw json.RawMessage) json.RawMessage {
	path, ok := r.discovery.Path(sessionID)
	if !ok {
		return raw
	}
	turns, _, err := readTurnsForBackend(path, r.backendOf(sessionID), 2)
	if err != nil || len(turns) == 0 {
		return raw
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		payload = map[string]any{}
	}
	if len(turns) >= 2 && turns[len(turns)-2].Role == "user" {
		payload["user_ts"] = turns[len(turns)-2].Time
	}
	if turns[len(turns)-1].Role == "assistant" {
		payload["assistant_ts"] = turns[len(turns)-1].Time
		// Fork point of the turn that just finished, so the client can arm the
		// fork control on the promoted-in-place bubble without a refetch.
		payload["assistant_uuid"] = turns[len(turns)-1].UUID
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return b
}

func (r *Router) releaseSend(sessionID string, tok *sendToken) {
	r.sendMu.Lock()
	if cur, ok := r.activeSend[sessionID]; ok && cur == tok {
		delete(r.activeSend, sessionID)
	}
	delete(r.creating, sessionID) // discovery owns it once the turn hit disk

	// Promote the next queued send. runSend's event loop has ended, so the
	// finished turn is fully published — the promoted send's pre() (relay
	// subscription) can't see stale events.
	var next *pendingSend
	var nextTok *sendToken
	var nextCtx context.Context
	if _, busy := r.activeSend[sessionID]; !busy {
		if q := r.sendQueue[sessionID]; len(q) > 0 {
			n := q[0]
			next = &n
			if len(q) == 1 {
				delete(r.sendQueue, sessionID)
			} else {
				r.sendQueue[sessionID] = q[1:]
			}
			ctx, cancel := context.WithCancel(context.Background())
			nextTok = &sendToken{cancel: cancel}
			nextCtx = ctx
			r.activeSend[sessionID] = nextTok
		}
	}
	r.sendMu.Unlock()
	tok.cancel()

	if next == nil {
		return
	}
	sess, ok := r.discovery.Get(sessionID)
	if !ok {
		// Session vanished while this send was queued.
		if next.abort != nil {
			next.abort(errors.New("session no longer exists"))
		}
		r.releaseSend(sessionID, nextTok) // drain the rest of the queue
		return
	}
	if next.pre != nil {
		next.pre()
	}
	r.startTurn(nextCtx, sess.ID, next.text, sess.Cwd, nextTok)
}

// flushSendQueue drops every queued send for sessionID, calling each abort.
// Used when the user cancels the in-flight turn or the session goes away —
// continuing to inject queued messages after an explicit stop would surprise.
func (r *Router) flushSendQueue(sessionID string, reason error) {
	r.sendMu.Lock()
	q := r.sendQueue[sessionID]
	delete(r.sendQueue, sessionID)
	r.sendMu.Unlock()
	for _, p := range q {
		if p.abort != nil {
			p.abort(reason)
		}
	}
}

// markSendIdle clears the running-state bit before publishing a terminal
// event. The creating overlay stays in place until releaseSend, so a just-born
// session remains addressable while the browser receives error/exit.
func (r *Router) markSendIdle(sessionID string, tok *sendToken) {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	cur, ok := r.activeSend[sessionID]
	if ok && cur != tok {
		return
	}
	if ok {
		delete(r.activeSend, sessionID)
	}
	if sess, ok := r.creating[sessionID]; ok {
		sess.Status = core.StatusIdle
		sess.LastEventAt = time.Now()
		r.creating[sessionID] = sess
	}
}

// CancelSend stops the in-flight turn for sessionID. It both cancels usher's
// tail goroutine (tok.cancel) and interrupts the live interactive claude with
// Ctrl-C — the process is persistent now, so cancelling the listener alone
// would leave claude generating into the void.
func (r *Router) CancelSend(sessionID string) error {
	r.sendMu.Lock()
	tok, ok := r.activeSend[sessionID]
	r.sendMu.Unlock()
	if !ok {
		return errors.New("no active send")
	}
	// Cancel means stop: drop queued follow-ups too, before cancelling the
	// turn, so releaseSend finds nothing to promote.
	r.flushSendQueue(sessionID, errors.New("cancelled"))
	if err := r.senderFor(sessionID).Interrupt(sessionID); err != nil {
		slog.Warn("interrupt session turn", "session", sessionID, "err", err)
	}
	tok.cancel()
	return nil
}

func (r *Router) SubscribeSession(id string) (<-chan broker.Event, func()) {
	return r.broker.Subscribe(id)
}

// SubscribeAllSessions returns a stream of events across every session, for
// frontends (the Telegram hub) that mirror all active sessions rather than one
// open one. Counterpart to SubscribeSession for the SSE-per-session web path.
func (r *Router) SubscribeAllSessions() (<-chan broker.Event, func()) {
	return r.broker.SubscribeAll()
}

// SubscribePendingInteractions returns a stream of newly-submitted permission
// requests, so the Telegram hub can push inline allow/deny buttons without
// polling ListPendingInteractions.
func (r *Router) SubscribePendingInteractions() (<-chan hook.Pending, func()) {
	return r.hooks.SubscribePending()
}

// --- terminal mirror -----------------------------------------------------

// CaptureScreen returns the current rendered pane contents (with colour
// escapes) for a session usher holds a live process for — the read-only
// terminal mirror's frame source. Ownership is required: there's no pane to
// mirror unless usher has a live window (sender.Has), and we must not reach
// into the user's own terminal/IDE claude on a shared socket.
func (r *Router) CaptureScreen(id string) (string, error) {
	snd := r.senderFor(id)
	if !snd.Has(id) {
		return "", errors.New("session not live")
	}
	return snd.CapturePane(id)
}

// SendKeys forwards navigation keys to a live session's pane, powering the
// terminal mirror's soft keys. Same ownership gate as CaptureScreen. The web
// layer restricts which key names reach here; this only enforces ownership.
func (r *Router) SendKeys(id string, keys ...string) error {
	snd := r.senderFor(id)
	if !snd.Has(id) {
		return errors.New("session not live")
	}
	if err := snd.SendKeys(id, keys...); err != nil {
		return err
	}
	// Esc while a turn is running interrupts claude in the pane, but an
	// interrupted turn never logs the turn_duration our tailer waits on — so the
	// turn would stick as "running" forever. Release it the same way the cancel
	// button does (cancel the tail ctx); the tailer then emits subprocess.exit
	// and clients recover live. No-op when no turn is in flight.
	for _, k := range keys {
		if k == "Escape" {
			r.sendMu.Lock()
			tok := r.activeSend[id]
			r.sendMu.Unlock()
			if tok != nil {
				tok.cancel()
			}
			break
		}
	}
	return nil
}

// ResizeCanvas sets the mirror's pane to cols×rows (and repairs any
// manual-attach drift). Called when a /screen stream opens, with cols and rows
// derived client-side from the viewer. Same ownership gate; a no-op error for
// unowned sessions is ignored by the caller.
func (r *Router) ResizeCanvas(id string, cols, rows int) error {
	snd := r.senderFor(id)
	if !snd.Has(id) {
		return errors.New("session not live")
	}
	return snd.ResizeCanvas(id, cols, rows)
}

// --- hook / interactions -------------------------------------------------

func (r *Router) ListPendingInteractions() []hook.Pending { return r.hooks.List() }

func (r *Router) RespondInteraction(id string, resp hook.Response) error {
	return r.hooks.Respond(id, resp)
}

// HandleHook applies a remembered per-session decision first, then blocks for
// the web UI when usher owns the session. Ownership = the session has a live
// window in usher's process pool (sender.Has) — a simple membership test, NOT
// whether a turn is currently executing. The old activeSend (turn-in-flight)
// gate raced the send/inject/turn lifecycle and bounced mid-turn tool prompts
// back to the pane; pool membership is the stable signal. It also keeps usher
// from intercepting the user's own terminal/IDE claude (not in our pool), which
// on a shared default socket would otherwise reach this same hook server.
func (r *Router) HandleHook(ctx context.Context, ev hook.Event) (hook.Response, error) {
	if resp, ok := r.hooks.QuickDecide(ev); ok {
		return resp, nil
	}
	if !r.anyHas(ev.SessionID) {
		return hook.Response{}, errors.New("session not owned by usher")
	}
	return r.hooks.Submit(ctx, ev)
}

func (r *Router) SetAutoApprove(sessionID string, enabled bool) {
	r.hooks.SetAutoApprove(sessionID, enabled)
}

func (r *Router) IsAutoApprove(sessionID string) bool {
	return r.hooks.IsAutoApprove(sessionID)
}

// --- transcript / blocking send (v0.2 LLM agent helpers) ----------------

// ReadSessionTranscript projects the most recent N user/assistant turns of
// a session's jsonl into core.TranscriptTurn. limit ≤ 0 returns everything.
func (r *Router) ReadSessionTranscript(id string, limit int) ([]core.TranscriptTurn, error) {
	path, ok := r.discovery.Path(id)
	if !ok {
		return nil, ErrSessionNotFound
	}
	turns, _, err := readTurnsForBackend(path, r.backendOf(id), limit)
	if err != nil {
		return nil, err
	}
	out := make([]core.TranscriptTurn, len(turns))
	for i, t := range turns {
		out[i] = core.TranscriptTurn{Role: t.Role, Content: flattenTurnText(t, true), Time: t.Time}
	}
	return out, nil
}

// ReadSessionTranscriptPage returns one page of the transcript: up to limit
// turns starting at absolute index offset (0-based from the start of the
// session). A negative offset means "the most recent page". It also returns
// the resolved start offset and the total turn count, so a caller can page in
// either direction and know when it has reached an end. Because the jsonl is
// append-only, an absolute index is a stable cursor even as the session grows.
//
// This is the primitive behind read_session_transcript's paging: the per-call
// limit bounds how much enters the agent's context, while offset + total keep
// any depth reachable — there is no hard wall, only a page boundary.
func (r *Router) ReadSessionTranscriptPage(id string, offset, limit int) ([]core.TranscriptTurn, int, int, error) {
	path, ok := r.discovery.Path(id)
	if !ok {
		return nil, 0, 0, ErrSessionNotFound
	}
	turns, _, err := readTurnsForBackend(path, r.backendOf(id), 0)
	if err != nil {
		return nil, 0, 0, err
	}
	total := len(turns)
	start, end := pageBounds(offset, limit, total)
	out := make([]core.TranscriptTurn, end-start)
	for i := start; i < end; i++ {
		t := turns[i]
		out[i-start] = core.TranscriptTurn{Role: t.Role, Content: flattenTurnText(t, true), Time: t.Time}
	}
	return out, start, total, nil
}

// pageBounds resolves [start, end) into a slice of length total for a page of
// up to limit items beginning at offset. A negative offset selects the last
// page (start = total-limit). Everything is clamped to [0, total], so an
// offset past the end yields an empty page rather than a panic.
func pageBounds(offset, limit, total int) (start, end int) {
	if limit <= 0 {
		limit = 1
	}
	start = offset
	if start < 0 {
		start = total - limit
	}
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end = start + limit
	if end > total {
		end = total
	}
	return start, end
}

// flattenTurnText renders a jsonl.Turn to plain text. User turns carry their
// text in Content; assistant turns carry it in Parts (text blocks interleaved
// with tool annotations). When includeTools is set, tool parts are inlined as
// `[tool: Name target]` markers — matching what read_session_transcript
// advertises. Search passes includeTools=false so it only matches the
// user/assistant prose, not tool names or command/file targets.
func flattenTurnText(t jsonl.Turn, includeTools bool) string {
	if t.Role != "assistant" {
		return t.Content
	}
	var b strings.Builder
	for _, p := range t.Parts {
		switch p.Type {
		case "text":
			if p.Content == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Content)
		case "tool":
			if !includeTools {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("[tool: ")
			b.WriteString(p.ToolName)
			if p.ToolTarget != "" {
				b.WriteString(" ")
				b.WriteString(p.ToolTarget)
			}
			b.WriteString("]")
		}
	}
	return b.String()
}

// SearchSessionTranscript scans the whole transcript for a case-insensitive
// substring of query in the user/assistant text (tool annotations excluded),
// returning at most maxHits matching turns with a bounded snippet around each
// first occurrence. The bool reports whether more turns matched than were
// returned. limit-style truncation of read_session_transcript is exactly what
// this avoids: it looks at every turn but returns only small located snippets.
func (r *Router) SearchSessionTranscript(id, query string, maxHits, contextChars int) ([]core.TranscriptSearchHit, bool, error) {
	if strings.TrimSpace(query) == "" {
		return nil, false, errors.New("query is required")
	}
	path, ok := r.discovery.Path(id)
	if !ok {
		return nil, false, ErrSessionNotFound
	}
	turns, _, err := readTurnsForBackend(path, r.backendOf(id), 0)
	if err != nil {
		return nil, false, err
	}
	hits, matched := scanTurnsForQuery(turns, []rune(query), maxHits, contextChars)
	return hits, matched > len(hits), nil
}

// SearchAllSessions runs the same substring search across every discovered
// session and returns one compact result per session that has a match: its
// total hit count and a snippet at the first hit. It is the routing primitive
// for "which session mentioned X?" — the alternative, calling
// SearchSessionTranscript per session, costs a tool round-trip each. Sessions
// are ranked by hit count (most matches first); the bool reports whether more
// matched than maxSessions returned. Every session's jsonl is read once, so
// this is heavier than a single-session search — meant for user-driven lookup.
func (r *Router) SearchAllSessions(query string, maxSessions, contextChars int) ([]core.SessionSearchResult, bool, error) {
	if strings.TrimSpace(query) == "" {
		return nil, false, errors.New("query is required")
	}
	q := []rune(query)
	var results []core.SessionSearchResult
	for _, s := range r.ListSessions() {
		path, ok := r.discovery.Path(s.ID)
		if !ok {
			continue
		}
		turns, _, err := readTurnsForBackend(path, r.backendOf(s.ID), 0)
		if err != nil {
			continue
		}
		hits, matched := scanTurnsForQuery(turns, q, 1, contextChars)
		if matched == 0 {
			continue
		}
		res := core.SessionSearchResult{
			SessionID: s.ID,
			Title:     s.Title,
			Cwd:       s.Cwd,
			HitCount:  matched,
		}
		if len(hits) > 0 {
			res.TurnIndex = hits[0].TurnIndex
			res.Snippet = hits[0].Snippet
		}
		results = append(results, res)
	}
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].HitCount > results[j].HitCount
	})
	truncated := false
	if maxSessions > 0 && len(results) > maxSessions {
		results = results[:maxSessions]
		truncated = true
	}
	return results, truncated, nil
}

// scanTurnsForQuery scans decoded turns for a case-insensitive substring of q
// in the user/assistant prose (tool annotations excluded), returning up to
// maxHits located snippets and the total count of matching turns. Shared by
// the single-session and cross-session searches.
func scanTurnsForQuery(turns []jsonl.Turn, q []rune, maxHits, contextChars int) (hits []core.TranscriptSearchHit, matched int) {
	for i, t := range turns {
		text := []rune(flattenTurnText(t, false))
		first, count := foldFindAll(text, q)
		if first < 0 {
			continue
		}
		matched++
		if len(hits) >= maxHits {
			continue
		}
		hits = append(hits, core.TranscriptSearchHit{
			Role:        t.Role,
			Time:        t.Time,
			TurnIndex:   i,
			Occurrences: count,
			Snippet:     snippetAround(text, first, len(q), contextChars),
		})
	}
	return hits, matched
}

// foldFindAll returns the index of the first case-insensitive occurrence of
// needle in hay (rune slices) and the total non-overlapping occurrence count.
// Rune-based to stay correct with multibyte (e.g. CJK) content, where mixing
// byte offsets from strings.ToLower would risk splitting a character.
func foldFindAll(hay, needle []rune) (first, count int) {
	first = -1
	if len(needle) == 0 {
		return -1, 0
	}
	for i := 0; i+len(needle) <= len(hay); {
		if foldEqualAt(hay, needle, i) {
			if first < 0 {
				first = i
			}
			count++
			i += len(needle)
		} else {
			i++
		}
	}
	return first, count
}

func foldEqualAt(hay, needle []rune, at int) bool {
	for j := range needle {
		if unicode.ToLower(hay[at+j]) != unicode.ToLower(needle[j]) {
			return false
		}
	}
	return true
}

// snippetAround returns matchLen runes at start plus up to ctx runes of
// context on each side, with ellipses marking truncation and newlines
// collapsed to spaces so the result stays a single compact line.
func snippetAround(text []rune, start, matchLen, ctx int) string {
	if ctx < 0 {
		ctx = 0
	}
	lo := start - ctx
	if lo < 0 {
		lo = 0
	}
	hi := start + matchLen + ctx
	if hi > len(text) {
		hi = len(text)
	}
	snip := strings.ReplaceAll(string(text[lo:hi]), "\n", " ")
	if lo > 0 {
		snip = "…" + snip
	}
	if hi < len(text) {
		snip = snip + "…"
	}
	return snip
}

// SendToSessionAndWait spawns the same fire-and-forget send as
// SendToSession but blocks until subprocess.exit (or timeout / ctx cancel),
// returning the accumulated assistant text streamed during this turn.
//
// We subscribe to the broker BEFORE issuing the send so no events are
// missed in the window between SendToSession returning and the subscriber
// being attached.
func (r *Router) SendToSessionAndWait(ctx context.Context, id, text string, timeout time.Duration) (string, error) {
	waitCtx, waitCancel := context.WithTimeout(ctx, timeout)
	defer waitCancel()

	// The subscription is created by the queue's pre hook — at actual turn
	// start, after any earlier turn's events have all been published — so the
	// collected text is this turn's, even when the send had to queue.
	started := make(chan turnSub, 1)
	aborted := make(chan error, 1)
	err := r.enqueueSend(id, text,
		func() {
			ch, cancel := r.broker.Subscribe(id)
			started <- turnSub{ch: ch, cancel: cancel}
		},
		func(err error) { aborted <- err },
	)
	if err != nil {
		return "", err
	}

	select {
	case sub := <-started:
		defer sub.cancel()
		return collectTurnText(waitCtx, sub.ch)
	case err := <-aborted:
		return "", err
	case <-waitCtx.Done():
		// Wait expired while still queued. The send stays queued (delivered
		// in order); only the wait gives up. Drain the eventual subscription
		// so it can't leak.
		go func() {
			select {
			case sub := <-started:
				sub.cancel()
			case <-aborted:
			case <-time.After(relayWaitCeiling):
			}
		}()
		return "", errors.New("timeout (send still queued behind an earlier turn)")
	}
}

// turnSub hands a pre-subscribed broker stream from enqueueSend's pre hook to
// the caller that collects it.
type turnSub struct {
	ch     <-chan broker.Event
	cancel func()
}

// relayWaitCeiling bounds how long a relayed send's collector goroutine may
// outlive its turn. It is not a UX timeout — relays are event-driven and fire
// whenever subprocess.exit lands — only a leak backstop for a session whose
// turn never terminates.
const relayWaitCeiling = 24 * time.Hour

// errNoResponse marks a collect that ended with no assistant text at all
// (ceiling/timeout expiry before any part arrived).
var errNoResponse = errors.New("timeout (no response received)")

// SendToSessionRelayed delivers text like SendToSession and collects the
// turn's assistant text in the background, handing it to onDone (at most
// once, own goroutine; reply may be partial alongside a non-nil error). If
// the ceiling passes with no response at all — killed window, exit never
// observed — onDone is NOT called: a day-late "(relay: timeout)" message is
// noise, and the reply, if any, is in the transcript.
func (r *Router) SendToSessionRelayed(id, text string, onDone func(sessionID, reply string, err error)) error {
	return r.enqueueSend(id, text,
		func() {
			ch, cancel := r.broker.Subscribe(id)
			go func() {
				defer cancel()
				waitCtx, waitCancel := context.WithTimeout(context.Background(), relayWaitCeiling)
				defer waitCancel()
				reply, err := collectTurnText(waitCtx, ch)
				if errors.Is(err, errNoResponse) {
					slog.Warn("relay collector expired with no response; dropping", "session", id)
					return
				}
				onDone(id, reply, err)
			}()
		},
		func(err error) { onDone(id, "", err) },
	)
}

// collectTurnText accumulates one turn's assistant text from a broker
// subscription until subprocess.exit, an error event, channel close, or ctx
// expiry (partial text is returned alongside the timeout error).
func collectTurnText(ctx context.Context, ch <-chan broker.Event) (string, error) {
	var buf strings.Builder
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return buf.String(), nil
			}
			switch ev.Type {
			case "part":
				// Message granularity (interactive claude emits no stream-json
				// token deltas): each text part carries one assistant message's
				// text blocks. Accumulate them across the turn.
				if t := partText(ev.Raw); t != "" {
					if buf.Len() > 0 {
						buf.WriteString("\n")
					}
					buf.WriteString(t)
				}
			case "subprocess.exit":
				return buf.String(), nil
			case "error":
				return buf.String(), errors.New(extractErrorMessage(ev.Raw))
			}
		case <-ctx.Done():
			if buf.Len() == 0 {
				return "", errNoResponse
			}
			return buf.String(), fmt.Errorf("timeout after %d chars (partial response retained)", buf.Len())
		}
	}
}

// partText returns the text content of a "part" broker event — assistant
// text parts only; tool parts and malformed payloads yield "".
func partText(raw json.RawMessage) string {
	var o struct {
		Role string `json:"role"`
		Part struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"part"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return ""
	}
	if o.Role != "assistant" || o.Part.Type != "text" {
		return ""
	}
	return o.Part.Content
}

func extractErrorMessage(raw json.RawMessage) string {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Message == "" {
		return "unknown error"
	}
	return e.Message
}

// StartSession spawns a brand-new Claude Code session in cwd and returns
// immediately with the generated session id. Stream events flow to broker
// subscribers — callers that want the first response inline should use
// CreateSession instead. Registered in activeSend so CancelSend works.
func (r *Router) StartSession(cwd, initialMsg, model string) (string, error) {
	cwd, err := validateCreateInputs(cwd, initialMsg)
	if err != nil {
		return "", err
	}
	backend := backendForModel(model)
	snd, ok := r.senders[backend]
	if !ok {
		backend = r.defaultBackend
		snd = r.senders[backend]
	}
	if !snd.PreAssignsID() {
		// Codex assigns its own id; spawn, discover it, register under it.
		return r.startDiscoveredSession(cwd, initialMsg, model, backend, snd)
	}

	sessionID := newUUIDv4()
	ctx, cancel := context.WithCancel(context.Background())
	tok := &sendToken{cancel: cancel}

	now := time.Now()
	r.sendMu.Lock()
	r.activeSend[sessionID] = tok
	// Surface it now: discovery won't see it until claude writes the first
	// jsonl line, so without this the detail view 404s. Dropped in releaseSend.
	r.creating[sessionID] = core.Session{
		ID:          sessionID,
		Title:       truncateRunes(initialMsg, 60),
		Cwd:         cwd,
		Status:      core.StatusRunning,
		StartedAt:   now,
		LastEventAt: now,
		LastInputAt: now,
		Backend:     backend,
	}
	r.sendMu.Unlock()

	go r.runStart(ctx, sessionID, initialMsg, cwd, model, backend, tok)
	return sessionID, nil
}

// codexDiscoverTimeout bounds how long StartSession blocks waiting for Codex to
// write its new session log (and so reveal the id it assigned itself).
const codexDiscoverTimeout = 20 * time.Second

// startDiscoveredSession creates a new session for a backend that assigns its
// own id (Codex). It spawns under a temporary handle, blocks until the real id
// is discovered, then registers creating/activeSend/broker under that id — first
// and only id, no placeholder, no re-keying. The turn then streams in the
// background like a Claude start.
func (r *Router) startDiscoveredSession(cwd, initialMsg, model, backend string, snd *sender.Sender) (string, error) {
	tempID := newUUIDv4()
	ctx, cancel := context.WithCancel(context.Background())
	realID, ch, err := snd.StartCodexSession(ctx, tempID, initialMsg, cwd, model, codexDiscoverTimeout)
	if err != nil {
		cancel()
		return "", err
	}

	tok := &sendToken{cancel: cancel}
	now := time.Now()
	r.sendMu.Lock()
	r.activeSend[realID] = tok
	r.creating[realID] = core.Session{
		ID:          realID,
		Title:       truncateRunes(initialMsg, 60),
		Cwd:         cwd,
		Status:      core.StatusRunning,
		StartedAt:   now,
		LastEventAt: now,
		LastInputAt: now,
		Backend:     backend,
	}
	r.sendMu.Unlock()

	go func() {
		defer r.releaseSend(realID, tok)
		asm := newStreamAssembler(backend)
		for ev := range ch {
			if ev.Type == "subprocess.exit" {
				r.markSendIdle(realID, tok)
			}
			r.publishStream(realID, asm, ev)
		}
	}()
	return realID, nil
}

func (r *Router) runStart(ctx context.Context, sessionID, prompt, cwd, model, backend string, tok *sendToken) {
	defer r.releaseSend(sessionID, tok)
	ch, err := r.senderForBackend(backend).SendNew(ctx, sessionID, prompt, cwd, model)
	if err != nil {
		r.markSendIdle(sessionID, tok)
		errMsg, _ := json.Marshal(map[string]string{"message": err.Error()})
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: "error", Raw: errMsg})
		return
	}
	asm := newStreamAssembler(backend)
	for ev := range ch {
		// Publish before clearing the running bit — same reasoning as runSend.
		r.publishStream(sessionID, asm, ev)
		if ev.Type == "subprocess.exit" {
			r.markSendIdle(sessionID, tok)
		}
	}
}

// validateCreateInputs checks the new-session inputs and returns the resolved
// cwd: ~ is expanded, the path must otherwise be absolute, and it is created
// if missing.
func validateCreateInputs(cwd, initialMsg string) (string, error) {
	if cwd == "" {
		return "", errors.New("cwd is required")
	}
	if initialMsg == "" {
		return "", errors.New("initial_message is required")
	}
	if cwd == "~" || strings.HasPrefix(cwd, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %w", err)
		}
		cwd = filepath.Join(home, cwd[1:])
	}
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd %q must be an absolute path or start with ~", cwd)
	}
	// MkdirAll no-ops if cwd exists and errors on a non-dir, so it also is-dir-checks.
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return "", fmt.Errorf("cwd %q: %w", cwd, err)
	}
	return cwd, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// CreateSession spawns a brand-new Claude Code session in cwd and waits for
// the assistant's first response (subject to timeout). Returns the
// generated session id and the accumulated assistant text. The session
// will appear in discovery via fsnotify shortly after the subprocess
// starts writing its jsonl.
func (r *Router) CreateSession(ctx context.Context, cwd, initialMsg string, timeout time.Duration) (string, string, error) {
	cwd, err := validateCreateInputs(cwd, initialMsg)
	if err != nil {
		return "", "", err
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sessionID, ch, tok, err := r.startNewSession(waitCtx, cancel, cwd, initialMsg)
	if err != nil {
		return "", "", err
	}

	reply := r.collectNewSessionText(sessionID, ch)
	expired := waitCtx.Err() != nil // before releaseSend, which cancels waitCtx
	r.releaseSend(sessionID, tok)

	if expired && reply == "" {
		return sessionID, "", fmt.Errorf("create_session timeout (no response received within %s)", timeout)
	}
	return sessionID, reply, nil
}

// CreateSessionRelayed starts a new session like CreateSession but returns as
// soon as the session id is known; the first assistant reply is collected in
// the background and handed to onDone (same contract as SendToSessionRelayed;
// onDone also receives the new session id so callers don't have to close over
// the not-yet-assigned return value). For Claude the id is pre-assigned so
// this returns almost immediately; for Codex it returns once the rollout file
// is discovered.
func (r *Router) CreateSessionRelayed(cwd, initialMsg string, onDone func(sessionID, reply string, err error)) (string, error) {
	cwd, err := validateCreateInputs(cwd, initialMsg)
	if err != nil {
		return "", err
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), relayWaitCeiling)

	sessionID, ch, tok, err := r.startNewSession(waitCtx, cancel, cwd, initialMsg)
	if err != nil {
		cancel()
		return "", err
	}

	go func() {
		reply := r.collectNewSessionText(sessionID, ch)
		expired := waitCtx.Err() != nil // before releaseSend, which cancels waitCtx
		r.releaseSend(sessionID, tok)
		if expired && reply == "" {
			onDone(sessionID, "", errors.New("no response received (send expired)"))
			return
		}
		onDone(sessionID, reply, nil)
	}()
	return sessionID, nil
}

// startNewSession spawns a brand-new session in the default backend with its
// default model (empty model → the CLI's own default). Claude lets usher pick
// the id up front; Codex assigns its own, so discover it first (mirrors
// StartSession's preAssignsID split). The new id is registered in
// creating/activeSend — without the overlay the focus link / detail view 404s
// until discovery sees the first jsonl line, and without the token CancelSend
// is a no-op and a follow-up send would interleave with the first turn
// instead of queueing. Callers must end the turn with releaseSend(id, tok).
func (r *Router) startNewSession(ctx context.Context, cancel context.CancelFunc, cwd, initialMsg string) (string, <-chan sender.StreamEvent, *sendToken, error) {
	snd := r.senderForBackend(r.defaultBackend)
	var sessionID string
	var ch <-chan sender.StreamEvent
	var err error
	if snd.PreAssignsID() {
		sessionID = newUUIDv4()
		ch, err = snd.SendNew(ctx, sessionID, initialMsg, cwd, "")
	} else {
		sessionID, ch, err = snd.StartCodexSession(ctx, newUUIDv4(), initialMsg, cwd, "", codexDiscoverTimeout)
	}
	if err != nil {
		return "", nil, nil, err
	}

	tok := &sendToken{cancel: cancel}
	now := time.Now()
	r.sendMu.Lock()
	r.activeSend[sessionID] = tok
	r.creating[sessionID] = core.Session{
		ID:          sessionID,
		Title:       truncateRunes(initialMsg, 60),
		Cwd:         cwd,
		Status:      core.StatusRunning,
		StartedAt:   now,
		LastEventAt: now,
		LastInputAt: now,
		Backend:     r.defaultBackend,
	}
	r.sendMu.Unlock()
	return sessionID, ch, tok, nil
}

// collectNewSessionText drains a new session's first-turn stream, forwarding
// every event to broker subscribers (raw + derived part/turn.user events, so a
// session-detail tab opened on the new id sees the live stream too) while
// accumulating the assistant text parts.
func (r *Router) collectNewSessionText(sessionID string, ch <-chan sender.StreamEvent) string {
	var buf strings.Builder
	asm := newStreamAssembler(r.defaultBackend)
	for ev := range ch {
		if p := r.publishStream(sessionID, asm, ev); p != nil && p.Type == "text" {
			if buf.Len() > 0 {
				buf.WriteString("\n")
			}
			buf.WriteString(p.Content)
		}
	}
	return buf.String()
}

// newUUIDv4 produces a randomly-generated UUIDv4 string. We avoid the
// google/uuid dep — Claude Code accepts any RFC 4122 v4 string.
func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
