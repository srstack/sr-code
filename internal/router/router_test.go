package router

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/discovery"
	"github.com/nexustar/usher/internal/sender"
)

// TestPublishStreamDerivesCodexTurns proves the live path: fed Codex rollout
// lines through the codex assembler, publishStream derives the backend-neutral
// turn.user / part broker events the web client renders (same as for Claude).
func TestPublishStreamDerivesCodexTurns(t *testing.T) {
	b := broker.New()
	r := &Router{broker: b}
	sub, unsub := b.Subscribe("s1")
	defer unsub()

	asm := newStreamAssembler("codex")
	lines := []string{
		`{"timestamp":"2026-06-14T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"hello codex"}}`,
		`{"timestamp":"2026-06-14T00:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"hi there"}}`,
		`{"timestamp":"2026-06-14T00:00:09Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`,
	}
	go func() {
		for _, ln := range lines {
			r.publishStream("s1", asm, sender.StreamEvent{Type: "event_msg", Raw: json.RawMessage(ln)}, time.Time{})
		}
		b.Publish(broker.Event{SessionID: "s1", Type: "done"})
	}()

	var sawUser, sawPart bool
	for ev := range sub {
		switch {
		case ev.Type == "turn.user" && strings.Contains(string(ev.Raw), "hello codex"):
			sawUser = true
		case ev.Type == "part" && strings.Contains(string(ev.Raw), "hi there"):
			sawPart = true
		case ev.Type == "done":
			if !sawUser {
				t.Error("no turn.user derived from codex user_message")
			}
			if !sawPart {
				t.Error("no part derived from codex agent_message")
			}
			return
		}
	}
}

const codexLog = `{"timestamp":"2026-06-14T00:00:00Z","type":"session_meta","payload":{"id":"id1","cwd":"/c","timestamp":"2026-06-14T00:00:00Z"}}
{"timestamp":"2026-06-14T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"hello codex"}}
{"timestamp":"2026-06-14T00:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"hi"}}
{"timestamp":"2026-06-14T00:00:09Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}
`

const claudeLog = `{"type":"user","message":{"role":"user","content":"hello claude"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}
{"type":"system","subtype":"turn_duration"}
`

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReadTurnsForBackend proves the dispatch: each backend's parser
// understands its own log shape and yields nothing from the other's.
func TestReadTurnsForBackend(t *testing.T) {
	codexPath := writeTemp(t, "rollout.jsonl", codexLog)
	claudePath := writeTemp(t, "claude.jsonl", claudeLog)

	turns, _, err := readTurnsForBackend(codexPath, "codex", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || turns[0].Role != "user" || turns[0].Content != "hello codex" {
		t.Fatalf("codex parser: got %+v", turns)
	}
	// The Claude parser must not understand a Codex rollout (event_msg lines are
	// not user/assistant) — proving the dispatch matters.
	if wrong, _, _ := readTurnsForBackend(codexPath, "claude", 0); len(wrong) != 0 {
		t.Errorf("claude parser should yield nothing from a codex log; got %+v", wrong)
	}

	turns, _, err = readTurnsForBackend(claudePath, "claude", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || turns[0].Content != "hello claude" {
		t.Fatalf("claude parser: got %+v", turns)
	}
	if wrong, _, _ := readTurnsForBackend(claudePath, "codex", 0); len(wrong) != 0 {
		t.Errorf("codex parser should yield nothing from a claude log; got %+v", wrong)
	}
}

func TestBackendForModel(t *testing.T) {
	cases := map[string]string{
		"gpt-5.5":           "codex",
		"gpt-4.1":           "codex",
		"o3":                "codex",
		"o4-mini":           "codex",
		"codex-mini":        "codex",
		"claude-opus-4-8":   "claude",
		"opus":              "claude",
		"sonnet":            "claude",
		"haiku":             "claude",
		"claude-fable-5":    "claude",
		"":                  "claude", // unspecified → default backend
		"default":           "claude", // ambiguous name resolves to the default backend
		"GPT-5.5":           "codex",  // case-insensitive
		"  gpt-5.5  ":       "codex",  // trimmed
		"something-unknown": "claude",
	}
	for model, want := range cases {
		if got := backendForModel(model); got != want {
			t.Errorf("backendForModel(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestSenderForBackendFallsBackToDefault(t *testing.T) {
	r := &Router{
		senders:        map[string]*sender.Sender{"claude": nil},
		defaultBackend: "claude",
	}
	// An unregistered backend falls back to the default (here the claude entry).
	if _, ok := r.senders["codex"]; ok {
		t.Fatal("precondition: codex should be unregistered")
	}
	// senderForBackend returns the default entry for an unknown backend; we only
	// assert it does not panic and returns the same (nil) default value.
	if got := r.senderForBackend("codex"); got != r.senders["claude"] {
		t.Errorf("unknown backend did not fall back to default")
	}
	if got := r.senderForBackend("claude"); got != r.senders["claude"] {
		t.Errorf("registered backend not returned")
	}
}

// --- collectTurnText (the relayed/waited send's accumulate loop) ----------

func partEvent(sid, role, typ, content string) broker.Event {
	raw, _ := json.Marshal(map[string]any{
		"role": role,
		"part": map[string]string{"type": typ, "content": content},
	})
	return broker.Event{SessionID: sid, Type: "part", Raw: raw}
}

func TestCollectTurnTextAccumulatesUntilExit(t *testing.T) {
	b := broker.New()
	ch, unsub := b.Subscribe("s1")
	defer unsub()

	go func() {
		b.Publish(partEvent("s1", "assistant", "text", "hello"))
		b.Publish(partEvent("s1", "assistant", "tool", "ignored tool part"))
		b.Publish(partEvent("s1", "user", "text", "ignored user echo"))
		b.Publish(partEvent("s1", "assistant", "text", "world"))
		b.Publish(broker.Event{SessionID: "s1", Type: "subprocess.exit", Raw: json.RawMessage(`{}`)})
	}()

	got, err := collectTurnText(context.Background(), ch)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello\nworld" {
		t.Errorf("collected %q, want %q", got, "hello\nworld")
	}
}

func TestCollectTurnTextErrorEvent(t *testing.T) {
	b := broker.New()
	ch, unsub := b.Subscribe("s1")
	defer unsub()

	go func() {
		b.Publish(partEvent("s1", "assistant", "text", "partial"))
		b.Publish(broker.Event{SessionID: "s1", Type: "error", Raw: json.RawMessage(`{"message":"boom"}`)})
	}()

	got, err := collectTurnText(context.Background(), ch)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want boom", err)
	}
	if got != "partial" {
		t.Errorf("partial text lost: %q", got)
	}
}

func TestCollectTurnTextTimeout(t *testing.T) {
	b := broker.New()
	ch, unsub := b.Subscribe("s1")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already expired: no events will ever arrive

	if _, err := collectTurnText(ctx, ch); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v, want timeout", err)
	}
}

func TestSendToSessionRelayedUnknownSession(t *testing.T) {
	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	r := &Router{discovery: d, broker: broker.New()}
	if err := r.SendToSessionRelayed("nope", "hi", func(string, string, error) {}); err == nil {
		t.Error("expected error for unknown session")
	}
}

// --- send queue (per-session turn serialization) ---------------------------

// newQueueTestRouter builds a Router over a real discovery holding one session
// (id "abc12345"), with runTurn overridden to simulate each turn: publish one
// assistant part echoing the prompt, publish exit, release the send. That is
// exactly the event pattern a real turn produces, so the relay collectors'
// attribution can be asserted end to end without tmux.
func newQueueTestRouter(t *testing.T) *Router {
	t.Helper()
	tmp := t.TempDir()
	line := `{"type":"user","sessionId":"abc12345","cwd":"/tmp/x","timestamp":"2026-07-01T10:00:00.000Z","message":{"role":"user","content":"seed"},"uuid":"u1"}` + "\n"
	p := filepath.Join(tmp, "-tmp-x", "abc12345.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(tmp))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r := &Router{
		discovery:  d,
		broker:     broker.New(),
		activeSend: map[string]*sendToken{},
		sendQueue:  map[string][]pendingSend{},
		creating:   map[string]core.Session{},
	}
	r.runTurn = func(_ context.Context, sessionID, prompt, _ string, tok *sendToken) {
		r.broker.Publish(partEvent(sessionID, "assistant", "text", "re: "+prompt))
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: "subprocess.exit", Raw: json.RawMessage(`{}`)})
		r.releaseSend(sessionID, tok)
	}
	return r
}

// TestEnrichExitSkipsStaleExchange: a turn that logged nothing (idle-fallback
// exit for a TUI-local command, or a cancel before first output) must not pick
// up the PREVIOUS exchange's timestamps — IM mirrors would show its duration
// as the current turn's "responded in".
func TestEnrichExitSkipsStaleExchange(t *testing.T) {
	r := newQueueTestRouter(t)
	path, _ := r.discovery.Path("abc12345")
	appendFile(t, path, `{"type":"assistant","timestamp":"2026-07-01T10:00:08.000Z","message":{"role":"assistant","content":[{"type":"text","text":"prev reply"}]},"uuid":"uu-prev"}`+"\n")

	stamps := func(raw json.RawMessage) bool {
		var p struct {
			UserTS      time.Time `json:"user_ts"`
			AssistantTS time.Time `json:"assistant_ts"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		return !p.UserTS.IsZero() && !p.AssistantTS.IsZero()
	}

	// Send began after the exchange completed → it is a previous turn's: skip.
	after := time.Date(2026, 7, 1, 10, 1, 0, 0, time.UTC)
	if stamps(r.enrichExitWithTurnTimestamps("abc12345", json.RawMessage(`{}`), after)) {
		t.Error("stale exchange stamped onto an empty turn's exit")
	}
	// Send began before the exchange completed → it is this turn's: stamp.
	before := time.Date(2026, 7, 1, 10, 0, 1, 0, time.UTC)
	if !stamps(r.enrichExitWithTurnTimestamps("abc12345", json.RawMessage(`{}`), before)) {
		t.Error("current turn's exchange not stamped")
	}
	// Zero started disables the check (first-turn paths).
	if !stamps(r.enrichExitWithTurnTimestamps("abc12345", json.RawMessage(`{}`), time.Time{})) {
		t.Error("zero started should stamp unconditionally")
	}
}

func TestEnrichExitSkipsConsecutiveUsersAndZeroEndTime(t *testing.T) {
	r := newQueueTestRouter(t)
	path, _ := r.discovery.Path("abc12345")
	appendFile(t, path, `{"type":"user","timestamp":"2026-07-01T10:00:01Z","message":{"role":"user","content":"A"}}`+"\n")
	appendFile(t, path, `{"type":"user","timestamp":"2026-07-01T10:00:02Z","message":{"role":"user","content":"B"}}`+"\n")
	raw := r.enrichExitWithTurnTimestamps("abc12345", json.RawMessage(`{}`), time.Date(2026, 7, 1, 10, 0, 1, 500000000, time.UTC))
	if string(raw) != `{}` {
		t.Fatalf("consecutive-user exit was enriched: %s", raw)
	}

	r2 := newQueueTestRouter(t)
	path2, _ := r2.discovery.Path("abc12345")
	appendFile(t, path2, `{"type":"user","timestamp":"2026-07-01T10:00:01Z","message":{"role":"user","content":"current"}}`+"\n")
	appendFile(t, path2, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"undated"}]}}`+"\n")
	raw = r2.enrichExitWithTurnTimestamps("abc12345", json.RawMessage(`{}`), time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC))
	if string(raw) != `{}` {
		t.Fatalf("zero-time assistant exit was enriched: %s", raw)
	}
}

// TestSendQueueSerializesAndAttributesReplies: three rapid relayed sends into
// one session must run as three ordered turns, each collector receiving ITS
// OWN turn's reply — not the previous turn's tail (the busy-session
// misattribution bug).
func TestSendQueueSerializesAndAttributesReplies(t *testing.T) {
	r := newQueueTestRouter(t)

	type got struct{ prompt, reply string }
	results := make(chan got, 3)
	for _, prompt := range []string{"one", "two", "three"} {
		p := prompt
		err := r.SendToSessionRelayed("abc12345", p, func(_, reply string, err error) {
			if err != nil {
				t.Errorf("send %q relay err: %v", p, err)
			}
			results <- got{p, reply}
		})
		if err != nil {
			t.Fatalf("send %q: %v", p, err)
		}
	}

	for i := 0; i < 3; i++ {
		select {
		case g := <-results:
			if g.reply != "re: "+g.prompt {
				t.Errorf("prompt %q got reply %q — misattributed turn", g.prompt, g.reply)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for relayed replies")
		}
	}
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if len(r.sendQueue["abc12345"]) != 0 || len(r.activeSend) != 0 {
		t.Errorf("queue not drained: %+v %+v", r.sendQueue, r.activeSend)
	}
}

// TestSendQueueWaitCorrelatesAcrossQueue: SendToSessionAndWait issued while a
// turn is in flight must return the QUEUED send's reply, not the in-flight
// turn's.
func TestSendQueueWaitCorrelatesAcrossQueue(t *testing.T) {
	r := newQueueTestRouter(t)

	// Occupy the session: a manual active token makes send #2 queue. Finish
	// the fake turn after a short delay, publishing its own exit.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx
	tok := &sendToken{cancel: func() {}}
	r.sendMu.Lock()
	r.activeSend["abc12345"] = tok
	r.sendMu.Unlock()
	go func() {
		time.Sleep(50 * time.Millisecond)
		r.broker.Publish(partEvent("abc12345", "assistant", "text", "stale turn tail"))
		r.broker.Publish(broker.Event{SessionID: "abc12345", Type: "subprocess.exit", Raw: json.RawMessage(`{}`)})
		r.releaseSend("abc12345", tok)
	}()

	reply, err := r.SendToSessionAndWait(context.Background(), "abc12345", "fresh question", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reply != "re: fresh question" {
		t.Errorf("reply = %q, want the queued turn's own reply", reply)
	}
}

func TestFlushSendQueueAborts(t *testing.T) {
	r := newQueueTestRouter(t)
	r.sendMu.Lock()
	r.activeSend["abc12345"] = &sendToken{cancel: func() {}}
	r.sendMu.Unlock()

	aborted := make(chan error, 1)
	if err := r.enqueueSend("abc12345", "queued", nil, func(err error) { aborted <- err }); err != nil {
		t.Fatal(err)
	}
	r.flushSendQueue("abc12345", errors.New("cancelled"))
	select {
	case err := <-aborted:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Errorf("abort err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("abort callback never ran")
	}
}

// TestValidateCreateInputsResolvesSymlinks: a cwd reached through a symlink
// (macOS /tmp → /private/tmp) must come back resolved, or Codex id discovery
// never matches the rollout's recorded cwd.
func TestValidateCreateInputsResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := validateCreateInputs(link, "hi")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("cwd = %q, want %q", got, want)
	}
}
