// Package hook brokers Claude Code permission decisions through usher's web
// UI. The CLI subcommand `usher hook <event>` POSTs a hook payload to the
// running server, which holds a pending interaction until a UI client decides
// allow / deny, then returns the decision back to Claude Code on stdout.
//
// "Remember this choice" decisions are kept per-session in memory: when the
// user picks Allow / Deny "always", the manager derives a matcher from the
// payload (Bash uses a "Bash(<first-word>:*)" pattern, every other tool just
// matches by name) and stores it; subsequent identical hook events for the
// same session are answered automatically without bothering the UI.
package hook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
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
	Behavior string `json:"behavior"`        // allow | deny
	Reason   string `json:"reason,omitempty"`
	// Scope is "once" (default) or "session". A "session"-scope decision is
	// remembered for the originating session and reapplied to matching
	// future hook events without prompting the UI again.
	Scope string `json:"scope,omitempty"`
}

// Rule is a remembered allow/deny decision derived from a Response with
// Scope="session". Matcher is either an exact tool name or a Bash subset
// pattern "Bash(<prefix>:*)" — same shape as claudecodeui's allowedTools
// entries.
type Rule struct {
	SessionID string `json:"session_id"`
	Behavior  string `json:"behavior"` // allow | deny
	Matcher   string `json:"matcher"`
}

// Event is the input usher receives from `usher hook` and forwards to Submit.
type Event struct {
	SessionID string
	Event     string
	ToolName  string
	ToolInput json.RawMessage
	Cwd       string
}

// Manager owns the in-memory map of pending interactions, the per-session
// remembered-rule list, and the per-session auto-approve flag. All are
// process-lifetime only — a server restart re-arms the consent boundary.
type Manager struct {
	mu      sync.Mutex
	pending map[string]*pendingEntry

	rememberMu sync.Mutex
	remembered map[string][]Rule // sessionID → rules

	autoMu      sync.Mutex
	autoApprove map[string]bool // sessionID → true when blanket-allow is on
}

func New() *Manager {
	return &Manager{
		pending:     map[string]*pendingEntry{},
		remembered:  map[string][]Rule{},
		autoApprove: map[string]bool{},
	}
}

// Submit registers a pending interaction and blocks until either the user
// responds via Respond or ctx is cancelled. If a remembered rule already
// matches the incoming event, it returns that decision immediately without
// touching the UI. If the user's response carries Scope="session", the
// derived rule is stored before returning.
func (m *Manager) Submit(ctx context.Context, ev Event) (Response, error) {
	if rule := m.findMatchingRule(ev); rule != nil {
		return Response{
			Behavior: rule.Behavior,
			Reason:   "remembered: " + rule.Matcher,
		}, nil
	}
	// Per-session auto-approve runs *after* matchers — a deliberate
	// "deny always X" rule still wins over the blanket flag, which keeps
	// users' specific opt-outs intact when they later toggle auto-approve
	// on globally for the session.
	if m.IsAutoApprove(ev.SessionID) {
		return Response{
			Behavior: "allow",
			Reason:   "auto-approve",
		}, nil
	}

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
		if r.Scope == "session" {
			m.rememberRule(ev, r.Behavior)
		}
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

// --- Remembered rules ----------------------------------------------------

// ListRules returns a snapshot of all remembered rules across all sessions.
func (m *Manager) ListRules() []Rule {
	m.rememberMu.Lock()
	defer m.rememberMu.Unlock()
	var out []Rule
	for _, rules := range m.remembered {
		out = append(out, rules...)
	}
	return out
}

// ForgetSessionRules clears all remembered rules for sessionID.
func (m *Manager) ForgetSessionRules(sessionID string) {
	m.rememberMu.Lock()
	delete(m.remembered, sessionID)
	m.rememberMu.Unlock()
}

// SetAutoApprove flips the blanket "allow every tool call" flag for a
// session. Process-lifetime only.
func (m *Manager) SetAutoApprove(sessionID string, enabled bool) {
	m.autoMu.Lock()
	defer m.autoMu.Unlock()
	if enabled {
		m.autoApprove[sessionID] = true
	} else {
		delete(m.autoApprove, sessionID)
	}
}

// IsAutoApprove reports whether sessionID is currently in blanket-allow mode.
func (m *Manager) IsAutoApprove(sessionID string) bool {
	m.autoMu.Lock()
	defer m.autoMu.Unlock()
	return m.autoApprove[sessionID]
}

func (m *Manager) findMatchingRule(ev Event) *Rule {
	m.rememberMu.Lock()
	rules := m.remembered[ev.SessionID]
	m.rememberMu.Unlock()
	for i := range rules {
		if matchRule(rules[i], ev.ToolName, ev.ToolInput) {
			return &rules[i]
		}
	}
	return nil
}

func (m *Manager) rememberRule(ev Event, behavior string) {
	matcher := deriveMatcher(ev.ToolName, ev.ToolInput)
	if matcher == "" {
		return
	}
	rule := Rule{SessionID: ev.SessionID, Behavior: behavior, Matcher: matcher}

	m.rememberMu.Lock()
	defer m.rememberMu.Unlock()
	for _, existing := range m.remembered[ev.SessionID] {
		if existing.Matcher == matcher && existing.Behavior == behavior {
			return // dedupe
		}
	}
	m.remembered[ev.SessionID] = append(m.remembered[ev.SessionID], rule)
}

var bashPrefixRE = regexp.MustCompile(`^Bash\((.+):\*\)$`)

// matchRule reports whether a remembered rule applies to the given event.
func matchRule(rule Rule, toolName string, toolInput json.RawMessage) bool {
	if rule.Matcher == toolName {
		return true
	}
	if m := bashPrefixRE.FindStringSubmatch(rule.Matcher); m != nil && toolName == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(toolInput, &in); err != nil {
			return false
		}
		return strings.HasPrefix(strings.TrimSpace(in.Command), m[1])
	}
	return false
}

// deriveMatcher picks a session-scope matcher for ev. Bash commands turn
// into "Bash(<first-word>:*)" so e.g. "git push" remembered means future
// "git status" / "git log" / "git pull" are also auto-allowed. Every other
// tool collapses to the bare tool name (a single Read decision applies to
// all later Read calls in this session).
func deriveMatcher(toolName string, toolInput json.RawMessage) string {
	if toolName == "" {
		return ""
	}
	if toolName == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(toolInput, &in); err == nil {
			cmd := strings.TrimSpace(in.Command)
			if cmd != "" {
				first := strings.SplitN(cmd, " ", 2)[0]
				if first != "" {
					return fmt.Sprintf("Bash(%s:*)", first)
				}
			}
		}
	}
	return toolName
}
