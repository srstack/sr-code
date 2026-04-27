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
	"os"
	"strings"
	"sync"
	"time"

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

	sendMu     sync.Mutex
	activeSend map[string]*sendToken // sessionID -> latest send's cancel handle
}

// sendToken pairs a cancel function with a unique pointer identity so that a
// finishing goroutine only deletes its own entry — never the entry of a
// later send that replaced it.
type sendToken struct {
	cancel context.CancelFunc
}

func New(d *discovery.Discovery, s *sender.Sender, b *broker.Broker, h *hook.Manager) *Router {
	return &Router{
		discovery:  d,
		sender:     s,
		broker:     b,
		hooks:      h,
		activeSend: map[string]*sendToken{},
	}
}

// --- session reads -------------------------------------------------------

// ListSessions returns sessions decorated with their current run state.
func (r *Router) ListSessions() []core.Session {
	sessions := r.discovery.List()
	r.sendMu.Lock()
	for i := range sessions {
		if _, running := r.activeSend[sessions[i].ID]; running {
			sessions[i].Status = core.StatusRunning
		}
	}
	r.sendMu.Unlock()
	return sessions
}

func (r *Router) GetSession(id string) (core.Session, bool) {
	sess, ok := r.discovery.Get(id)
	if !ok {
		return sess, false
	}
	r.sendMu.Lock()
	if _, running := r.activeSend[id]; running {
		sess.Status = core.StatusRunning
	}
	r.sendMu.Unlock()
	return sess, true
}

func (r *Router) SessionPath(id string) (string, bool) {
	return r.discovery.Path(id)
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
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: ev.Type, Raw: ev.Raw})
	}
}

func (r *Router) releaseSend(sessionID string, tok *sendToken) {
	r.sendMu.Lock()
	if cur, ok := r.activeSend[sessionID]; ok && cur == tok {
		delete(r.activeSend, sessionID)
	}
	r.sendMu.Unlock()
	tok.cancel()
}

// CancelSend cancels the most recent send for sessionID. Subprocesses claude
// has already serialized into its own queue keep running; we only stop the
// one currently driving stdout.
func (r *Router) CancelSend(sessionID string) error {
	r.sendMu.Lock()
	tok, ok := r.activeSend[sessionID]
	r.sendMu.Unlock()
	if !ok {
		return errors.New("no active send")
	}
	tok.cancel()
	return nil
}

func (r *Router) SubscribeSession(id string) (<-chan broker.Event, func()) {
	return r.broker.Subscribe(id)
}

// --- hook / interactions -------------------------------------------------

func (r *Router) ListPendingInteractions() []hook.Pending { return r.hooks.List() }

func (r *Router) RespondInteraction(id string, resp hook.Response) error {
	return r.hooks.Respond(id, resp)
}

func (r *Router) HandleHook(ctx context.Context, ev hook.Event) (hook.Response, error) {
	return r.hooks.Submit(ctx, ev)
}

// --- transcript / blocking send (v0.2 LLM agent helpers) ----------------

// ReadSessionTranscript projects the most recent N user/assistant turns of
// a session's jsonl into core.TranscriptTurn. limit ≤ 0 returns everything.
func (r *Router) ReadSessionTranscript(id string, limit int) ([]core.TranscriptTurn, error) {
	path, ok := r.discovery.Path(id)
	if !ok {
		return nil, errors.New("session not found")
	}
	turns, err := jsonl.ReadTurns(path, limit)
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
			case "stream_event":
				if t := extractTextDelta(ev.Raw); t != "" {
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

func extractTextDelta(raw json.RawMessage) string {
	var d struct {
		Event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		} `json:"event"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return ""
	}
	if d.Event.Type == "content_block_delta" && d.Event.Delta.Type == "text_delta" {
		return d.Event.Delta.Text
	}
	return ""
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

// CreateSession spawns a brand-new Claude Code session in cwd and waits for
// the assistant's first response (subject to timeout). Returns the
// generated session id and the accumulated assistant text. The session
// will appear in discovery via fsnotify shortly after the subprocess
// starts writing its jsonl.
func (r *Router) CreateSession(ctx context.Context, cwd, initialMsg string, timeout time.Duration) (string, string, error) {
	if cwd == "" {
		return "", "", errors.New("cwd is required")
	}
	if info, err := os.Stat(cwd); err != nil {
		return "", "", fmt.Errorf("cwd %q: %w", cwd, err)
	} else if !info.IsDir() {
		return "", "", fmt.Errorf("cwd %q is not a directory", cwd)
	}
	if initialMsg == "" {
		return "", "", errors.New("initial_message is required")
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
		if ev.Type == "stream_event" {
			if t := extractTextDelta(ev.Raw); t != "" {
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
