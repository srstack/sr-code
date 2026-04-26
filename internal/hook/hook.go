// Package hook brokers Claude Code permission decisions through usher's web
// UI. The CLI subcommand `usher hook <event>` POSTs a hook payload to the
// running server, which holds a pending interaction until a UI client decides
// allow / deny, then returns the decision back to Claude Code on stdout.
package hook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// Pending describes a permission request waiting for a user decision.
type Pending struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Event     string          `json:"event"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Cwd       string          `json:"cwd,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type pendingEntry struct {
	Pending
	response chan Response
}

// Response is the user's decision on a pending interaction.
type Response struct {
	Behavior string `json:"behavior"` // allow | deny
	Reason   string `json:"reason,omitempty"`
}

// Event is the input usher receives from `usher hook` and forwards to Submit.
type Event struct {
	SessionID string
	Event     string
	ToolName  string
	ToolInput json.RawMessage
	Cwd       string
}

// Manager owns the in-memory map of pending interactions and serializes
// access through Submit/Respond/List. There is no on-disk persistence:
// pending interactions survive only as long as the process and the hook's
// own timeout (Claude Code defaults to 60s, usher's `usher setup` raises
// that to 600s).
type Manager struct {
	mu      sync.Mutex
	pending map[string]*pendingEntry
}

func New() *Manager {
	return &Manager{pending: map[string]*pendingEntry{}}
}

// Submit registers a pending interaction and blocks until either the user
// responds via Respond or ctx is cancelled. It cleans up the entry before
// returning.
func (m *Manager) Submit(ctx context.Context, ev Event) (Response, error) {
	p := &pendingEntry{
		Pending: Pending{
			ID:        newID(),
			SessionID: ev.SessionID,
			Event:     ev.Event,
			ToolName:  ev.ToolName,
			ToolInput: ev.ToolInput,
			Cwd:       ev.Cwd,
			CreatedAt: time.Now().UTC(),
		},
		response: make(chan Response, 1),
	}
	m.mu.Lock()
	m.pending[p.ID] = p
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.pending, p.ID)
		m.mu.Unlock()
	}()

	select {
	case r := <-p.response:
		return r, nil
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// List returns a snapshot of all currently-pending interactions.
func (m *Manager) List() []Pending {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Pending, 0, len(m.pending))
	for _, p := range m.pending {
		out = append(out, p.Pending)
	}
	return out
}

// Respond delivers the user's decision to the matching pending interaction.
// Returns an error if the ID is unknown or the entry has already been resolved.
func (m *Manager) Respond(id string, r Response) error {
	m.mu.Lock()
	p, ok := m.pending[id]
	m.mu.Unlock()
	if !ok {
		return errors.New("interaction not found")
	}
	select {
	case p.response <- r:
		return nil
	default:
		return errors.New("already resolved")
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
