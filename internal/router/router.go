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
	"strings"
	"sync"
	"time"

	"usher/internal/archive"
	"usher/internal/broker"
	"usher/internal/core"
	"usher/internal/discovery"
	"usher/internal/hook"
	"usher/internal/jsonl"
	"usher/internal/sender"
)

type Router struct {
	discovery *discovery.Discovery
	sender    *sender.Sender
	broker    *broker.Broker
	hooks     *hook.Manager
	archive   *archive.Store

	sendMu     sync.Mutex
	activeSend map[string]*sendToken   // sessionID -> latest send's cancel handle
	creating   map[string]core.Session // sessions usher is spawning, not yet on disk
}

// sendToken pairs a cancel function with a unique pointer identity so that a
// finishing goroutine only deletes its own entry — never the entry of a
// later send that replaced it.
type sendToken struct {
	cancel context.CancelFunc
}

func New(d *discovery.Discovery, s *sender.Sender, b *broker.Broker, h *hook.Manager, archiveStore *archive.Store) *Router {
	return &Router{
		discovery:  d,
		sender:     s,
		broker:     b,
		hooks:      h,
		archive:    archiveStore,
		activeSend: map[string]*sendToken{},
		creating:   map[string]core.Session{},
	}
}

// --- session reads -------------------------------------------------------

// ListSessions returns sessions decorated with their current run state: a
// turn in flight is "running"; otherwise a warm pooled process is "live".
func (r *Router) ListSessions() []core.Session {
	sessions := r.discovery.List()
	live := sliceToSet(r.sender.LiveSessions())
	r.sendMu.Lock()
	known := make(map[string]bool, len(sessions))
	for i := range sessions {
		known[sessions[i].ID] = true
		if _, running := r.activeSend[sessions[i].ID]; running {
			sessions[i].Status = core.StatusRunning
		} else if live[sessions[i].ID] {
			sessions[i].Status = core.StatusLive
		}
	}
	// Prepend sessions still being created (newest, not yet on disk) so a
	// just-created session shows in the list before its first jsonl write.
	var pending []core.Session
	for id, s := range r.creating {
		if !known[id] {
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
		return sess, ok
	}
	r.sendMu.Lock()
	_, running := r.activeSend[id]
	r.sendMu.Unlock()
	if running {
		sess.Status = core.StatusRunning
	} else if r.sender.Has(id) {
		sess.Status = core.StatusLive
	}
	return sess, true
}

func sliceToSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func (r *Router) SessionPath(id string) (string, bool) {
	return r.discovery.Path(id)
}

// IsArchived reports whether sessionID is archived in the default
// sidebar view. Wraps archive.Store.IsArchived with the session's
// last_event_at from discovery; returns false for unknown ids.
func (r *Router) IsArchived(sessionID string) bool {
	if r.archive == nil {
		return false
	}
	sess, ok := r.discovery.Get(sessionID)
	if !ok {
		return false
	}
	return r.archive.IsArchived(sessionID, sess.LastEventAt, time.Now())
}

// Archive marks sessionID as manually archived.
func (r *Router) Archive(sessionID string) {
	if r.archive != nil {
		r.archive.Archive(sessionID)
	}
}

// Unarchive removes the archive decision for sessionID. Looks up the
// session's last_event_at from discovery so the store can pick between
// "delete entry" (fresh — let auto-archive resume later) and "write DecisionShown"
// (stale — would otherwise re-archive on next IsArchived call). A missing
// session leaves lastEventAt zero, which the store treats as stale.
func (r *Router) Unarchive(sessionID string) {
	if r.archive == nil {
		return
	}
	sess, _ := r.discovery.Get(sessionID)
	r.archive.Unarchive(sessionID, sess.LastEventAt, time.Now())
}

// --- session writes ------------------------------------------------------

// SendToSession spawns a fire-and-forget subprocess for the session. Stream
// events go to broker subscribers. Returns immediately once the subprocess
// is started, or with an error if the session is unknown.
func (r *Router) SendToSession(id, text string) error {
	sess, ok := r.discovery.Get(id)
	if !ok {
		return errors.New("session not found")
	}
	ctx, cancel := context.WithCancel(context.Background())
	tok := &sendToken{cancel: cancel}

	r.sendMu.Lock()
	r.activeSend[id] = tok
	r.sendMu.Unlock()

	go r.runSend(ctx, sess.ID, text, sess.Cwd, tok)
	return nil
}

func (r *Router) runSend(ctx context.Context, sessionID, prompt, cwd string, tok *sendToken) {
	defer r.releaseSend(sessionID, tok)

	ch, err := r.sender.Send(ctx, sessionID, prompt, cwd)
	if err != nil {
		errMsg, _ := json.Marshal(map[string]string{"message": err.Error()})
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: "error", Raw: errMsg})
		return
	}
	for ev := range ch {
		if ev.Type == "subprocess.exit" {
			ev.Raw = r.enrichExitWithTurnTimestamps(sessionID, ev.Raw)
		}
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: ev.Type, Raw: ev.Raw})
	}
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
	turns, _, err := jsonl.ReadTurns(path, 2)
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
	r.sendMu.Unlock()
	tok.cancel()
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
	if err := r.sender.Interrupt(sessionID); err != nil {
		slog.Warn("interrupt session turn", "session", sessionID, "err", err)
	}
	tok.cancel()
	return nil
}

func (r *Router) SubscribeSession(id string) (<-chan broker.Event, func()) {
	return r.broker.Subscribe(id)
}

// --- terminal mirror -----------------------------------------------------

// CaptureScreen returns the current rendered pane contents (with colour
// escapes) for a session usher holds a live process for — the read-only
// terminal mirror's frame source. Ownership is required: there's no pane to
// mirror unless usher has a live window (sender.Has), and we must not reach
// into the user's own terminal/IDE claude on a shared socket.
func (r *Router) CaptureScreen(id string) (string, error) {
	if !r.sender.Has(id) {
		return "", errors.New("session not live")
	}
	return r.sender.CapturePane(id)
}

// SendKeys forwards navigation keys to a live session's pane, powering the
// terminal mirror's soft keys. Same ownership gate as CaptureScreen. The web
// layer restricts which key names reach here; this only enforces ownership.
func (r *Router) SendKeys(id string, keys ...string) error {
	if !r.sender.Has(id) {
		return errors.New("session not live")
	}
	if err := r.sender.SendKeys(id, keys...); err != nil {
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
	if !r.sender.Has(id) {
		return errors.New("session not live")
	}
	return r.sender.ResizeCanvas(id, cols, rows)
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
	if !r.sender.Has(ev.SessionID) {
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
		return nil, errors.New("session not found")
	}
	turns, _, err := jsonl.ReadTurns(path, limit)
	if err != nil {
		return nil, err
	}
	out := make([]core.TranscriptTurn, len(turns))
	for i, t := range turns {
		out[i] = core.TranscriptTurn{Role: t.Role, Content: t.Content, Time: t.Time}
	}
	return out, nil
}

// SendToSessionAndWait spawns the same fire-and-forget send as
// SendToSession but blocks until subprocess.exit (or timeout / ctx cancel),
// returning the accumulated assistant text streamed during this turn.
//
// We subscribe to the broker BEFORE issuing the send so no events are
// missed in the window between SendToSession returning and the subscriber
// being attached.
func (r *Router) SendToSessionAndWait(ctx context.Context, id, text string, timeout time.Duration) (string, error) {
	if _, ok := r.discovery.Get(id); !ok {
		return "", errors.New("session not found")
	}
	ch, cancel := r.broker.Subscribe(id)
	defer cancel()

	if err := r.SendToSession(id, text); err != nil {
		return "", err
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, timeout)
	defer waitCancel()

	var buf strings.Builder
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return buf.String(), nil
			}
			switch ev.Type {
			case "assistant":
				// Message granularity now (interactive claude emits no
				// stream-json token deltas): each assistant event carries its
				// full text blocks. Accumulate them across the turn.
				if t := extractAssistantText(ev.Raw); t != "" {
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
		case <-waitCtx.Done():
			if buf.Len() == 0 {
				return "", errors.New("timeout (no response received)")
			}
			return buf.String(), fmt.Errorf("timeout after %d chars (partial response retained)", buf.Len())
		}
	}
}

// extractAssistantText concatenates the text blocks of an "assistant" jsonl
// line's message. Non-text blocks (thinking, tool_use) are skipped, so an
// assistant turn that only calls a tool yields "".
func extractAssistantText(raw json.RawMessage) string {
	var o struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range o.Message.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
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
func (r *Router) StartSession(cwd, initialMsg string) (string, error) {
	if err := validateCreateInputs(cwd, initialMsg); err != nil {
		return "", err
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
		Cwd:         cwd,
		Status:      core.StatusRunning,
		StartedAt:   now,
		LastEventAt: now,
	}
	r.sendMu.Unlock()

	go r.runStart(ctx, sessionID, initialMsg, cwd, tok)
	return sessionID, nil
}

func (r *Router) runStart(ctx context.Context, sessionID, prompt, cwd string, tok *sendToken) {
	defer r.releaseSend(sessionID, tok)
	ch, err := r.sender.SendNew(ctx, sessionID, prompt, cwd)
	if err != nil {
		errMsg, _ := json.Marshal(map[string]string{"message": err.Error()})
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: "error", Raw: errMsg})
		return
	}
	for ev := range ch {
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: ev.Type, Raw: ev.Raw})
	}
}

func validateCreateInputs(cwd, initialMsg string) error {
	if cwd == "" {
		return errors.New("cwd is required")
	}
	if info, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("cwd %q: %w", cwd, err)
	} else if !info.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", cwd)
	}
	if initialMsg == "" {
		return errors.New("initial_message is required")
	}
	return nil
}

// CreateSession spawns a brand-new Claude Code session in cwd and waits for
// the assistant's first response (subject to timeout). Returns the
// generated session id and the accumulated assistant text. The session
// will appear in discovery via fsnotify shortly after the subprocess
// starts writing its jsonl.
func (r *Router) CreateSession(ctx context.Context, cwd, initialMsg string, timeout time.Duration) (string, string, error) {
	if err := validateCreateInputs(cwd, initialMsg); err != nil {
		return "", "", err
	}

	sessionID := newUUIDv4()

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ch, err := r.sender.SendNew(waitCtx, sessionID, initialMsg, cwd)
	if err != nil {
		return "", "", err
	}

	var buf strings.Builder
	for ev := range ch {
		// Forward to broker so any session-detail subscriber that opens
		// the new tab sees the live stream too.
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: ev.Type, Raw: ev.Raw})
		if ev.Type == "assistant" {
			if t := extractAssistantText(ev.Raw); t != "" {
				if buf.Len() > 0 {
					buf.WriteString("\n")
				}
				buf.WriteString(t)
			}
		}
	}

	if waitCtx.Err() != nil && buf.Len() == 0 {
		return sessionID, "", fmt.Errorf("create_session timeout (no response received within %s)", timeout)
	}
	return sessionID, buf.String(), nil
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
