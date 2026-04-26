// Package router glues discovery, sender, broker, and hook together. It is
// the central coordinator that the web layer and the Usher Agent both go
// through, and serves as the type that satisfies the agent's AgentAPI
// contract — keeping the agent's surface a strict subset of usher's
// internals.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"usher/internal/broker"
	"usher/internal/core"
	"usher/internal/discovery"
	"usher/internal/hook"
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
