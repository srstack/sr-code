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
	"log/slog"
	"os"
	"path/filepath"
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
	Behavior string `json:"behavior"` // allow | deny
	Reason   string `json:"reason,omitempty"`
	// Scope is "once" (default) or "session". A "session"-scope decision is
	// remembered for the originating session and reapplied to matching
	// future hook events without prompting the UI again.
	Scope string `json:"scope,omitempty"`
	// Answers resolves an AskUserQuestion tool call: each entry maps a
	// question (verbatim from the tool input) to the option label the user
	// chose in the web UI. When set, the server merges it into the tool's
	// updatedInput so claude proceeds with the answer instead of blocking on
	// the pane TUI selector. Behavior is "allow" in this case.
	Answers map[string]string `json:"answers,omitempty"`
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
	ToolUseID string
	Event     string
	ToolName  string
	ToolInput json.RawMessage
	Cwd       string
}

// Manager owns the in-memory map of pending interactions, the per-session
// remembered-rule list, and the per-session auto-approve flag. Pending
// and remembered are process-lifetime only; autoApprove is persisted to
// disk (autoPath) when set, so the trust boundary survives restarts.
type Manager struct {
	mu      sync.Mutex
	pending map[string]*pendingEntry

	rememberMu sync.Mutex
	remembered map[string][]Rule // sessionID → rules

	autoMu      sync.Mutex
	autoApprove map[string]bool // sessionID → true when blanket-allow is on
	autoPath    string          // empty = no disk persistence (tests)

	subMu       sync.Mutex
	pendingSubs map[*pendingSub]struct{} // notified when a new interaction is submitted

	dedupMu sync.Mutex
	dedup   map[string]*dedupDecision
}

type dedupDecision struct {
	done     chan struct{}
	response Response
	err      error
}

// pendingSub subscribes to new-pending notifications. Buffered and drop-on-full,
// like the broker: a stalled consumer never blocks Submit.
type pendingSub struct {
	ch chan Pending
}

// New constructs a Manager. autoPath is the file backing the auto-approve
// flag map; pass "" to disable persistence (e.g. in tests). If the file
// exists at construction time, its state is loaded; subsequent calls to
// SetAutoApprove rewrite it atomically.
func New(autoPath string) *Manager {
	m := &Manager{
		pending:     map[string]*pendingEntry{},
		remembered:  map[string][]Rule{},
		autoApprove: map[string]bool{},
		autoPath:    autoPath,
		pendingSubs: map[*pendingSub]struct{}{},
		dedup:       map[string]*dedupDecision{},
	}
	if autoPath != "" {
		m.loadAutoApprove()
	}
	return m
}

// loadAutoApprove reads autoPath into m.autoApprove. Best-effort: a missing
// file is normal on first run; a corrupt file is logged and treated as empty.
func (m *Manager) loadAutoApprove() {
	data, err := os.ReadFile(m.autoPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("auto-approve: read", "path", m.autoPath, "err", err)
		}
		return
	}
	var loaded map[string]bool
	if err := json.Unmarshal(data, &loaded); err != nil {
		slog.Warn("auto-approve: decode", "path", m.autoPath, "err", err)
		return
	}
	m.autoMu.Lock()
	defer m.autoMu.Unlock()
	for k, v := range loaded {
		if v {
			m.autoApprove[k] = true
		}
	}
}

// persistAutoApprove writes the current map to autoPath via temp-file +
// rename so a partial write can't corrupt the on-disk state. Caller must
// hold m.autoMu. Best-effort: failures are logged but don't surface.
func (m *Manager) persistAutoApprove() {
	if m.autoPath == "" {
		return
	}
	data, err := json.Marshal(m.autoApprove)
	if err != nil {
		slog.Warn("auto-approve: encode", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.autoPath), 0o700); err != nil {
		slog.Warn("auto-approve: mkdir", "err", err)
		return
	}
	tmp := m.autoPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("auto-approve: write tmp", "err", err)
		return
	}
	if err := os.Rename(tmp, m.autoPath); err != nil {
		slog.Warn("auto-approve: rename", "err", err)
	}
}

// QuickDecide settles ev from per-session state (remembered rule or
// auto-approve) without UI; returns (zero, false) when input is needed.
// Remembered rules win over auto-approve so explicit deny-always opt-outs
// survive later blanket trust toggles.
func (m *Manager) QuickDecide(ev Event) (Response, bool) {
	// AskUserQuestion can't be settled without a chosen answer: a bare "allow"
	// (from a remembered rule or blanket auto-approve) just lets the tool run
	// and block on the pane TUI selector — the very thing usher routes around.
	// Always defer it to the web UI for an explicit per-call choice.
	if ev.ToolName == "AskUserQuestion" {
		return Response{}, false
	}
	if rule := m.findMatchingRule(ev); rule != nil {
		return Response{
			Behavior: rule.Behavior,
			Reason:   "remembered: " + rule.Matcher,
		}, true
	}
	if m.IsAutoApprove(ev.SessionID) {
		return Response{
			Behavior: "allow",
			Reason:   "auto-approve",
		}, true
	}
	return Response{}, false
}

// Submit blocks until the user responds via Respond or ctx is cancelled.
// Short-circuits via QuickDecide first. A Scope="session" response is
// stored as a rule before returning.
func (m *Manager) Submit(ctx context.Context, ev Event) (Response, error) {
	if ev.ToolUseID == "" {
		return m.submit(ctx, ev)
	}
	key := ev.SessionID + "\x00" + ev.ToolUseID
	m.dedupMu.Lock()
	d := m.dedup[key]
	if d == nil {
		d = &dedupDecision{done: make(chan struct{})}
		m.dedup[key] = d
		m.dedupMu.Unlock()
		d.response, d.err = m.submit(ctx, ev)
		close(d.done)
		time.AfterFunc(time.Minute, func() {
			m.dedupMu.Lock()
			if m.dedup[key] == d {
				delete(m.dedup, key)
			}
			m.dedupMu.Unlock()
		})
		return d.response, d.err
	}
	m.dedupMu.Unlock()
	select {
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case <-d.done:
		return d.response, d.err
	}
}

func (m *Manager) submit(ctx context.Context, ev Event) (Response, error) {
	if resp, ok := m.QuickDecide(ev); ok {
		return resp, nil
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
	m.notifyPending(p.Pending)

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

// SubscribePending returns a buffered channel that receives each new pending
// interaction as it is submitted, plus a cancel function. The web UI polls
// List() instead; this push path lets the web-push dispatcher surface permission
// prompts as notifications without polling. Drop-on-full, so a slow consumer
// never blocks Submit.
func (m *Manager) SubscribePending() (<-chan Pending, func()) {
	sub := &pendingSub{ch: make(chan Pending, 64)}
	m.subMu.Lock()
	m.pendingSubs[sub] = struct{}{}
	m.subMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			m.subMu.Lock()
			delete(m.pendingSubs, sub)
			m.subMu.Unlock()
			close(sub.ch)
		})
	}
	return sub.ch, cancel
}

func (m *Manager) notifyPending(p Pending) {
	m.subMu.Lock()
	chans := make([]chan Pending, 0, len(m.pendingSubs))
	for s := range m.pendingSubs {
		chans = append(chans, s.ch)
	}
	m.subMu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- p:
		default:
		}
	}
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
// session and persists the change to disk so it survives restarts.
func (m *Manager) SetAutoApprove(sessionID string, enabled bool) {
	m.autoMu.Lock()
	defer m.autoMu.Unlock()
	if enabled {
		m.autoApprove[sessionID] = true
	} else {
		delete(m.autoApprove, sessionID)
	}
	m.persistAutoApprove()
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
