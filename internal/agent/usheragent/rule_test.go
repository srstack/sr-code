package usheragent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"usher/internal/core"
	"usher/internal/hook"
)

type fakeAPI struct {
	sessions   []core.Session
	pending    []hook.Pending
	sentTo     []string
	sentText   []string
	resolved   map[string]hook.Response
	sendErr    error
	respondErr error

	transcripts map[string][]core.TranscriptTurn
	waitReplies map[string]string
	waitErrs    map[string]error
	waitedFor   []string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		resolved:    map[string]hook.Response{},
		transcripts: map[string][]core.TranscriptTurn{},
		waitReplies: map[string]string{},
		waitErrs:    map[string]error{},
	}
}

func (f *fakeAPI) ListSessions() []core.Session            { return f.sessions }
func (f *fakeAPI) ListPendingInteractions() []hook.Pending { return f.pending }
func (f *fakeAPI) SendToSession(id, text string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sentTo = append(f.sentTo, id)
	f.sentText = append(f.sentText, text)
	return nil
}
func (f *fakeAPI) RespondInteraction(id string, r hook.Response) error {
	if f.respondErr != nil {
		return f.respondErr
	}
	f.resolved[id] = r
	return nil
}
func (f *fakeAPI) ReadSessionTranscript(id string, limit int) ([]core.TranscriptTurn, error) {
	turns, ok := f.transcripts[id]
	if !ok {
		return nil, fmt.Errorf("no transcript for %q", id)
	}
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, nil
}
func (f *fakeAPI) SendToSessionAndWait(_ context.Context, id, text string, _ time.Duration) (string, error) {
	f.waitedFor = append(f.waitedFor, id)
	_ = text
	if err, ok := f.waitErrs[id]; ok && err != nil {
		return f.waitReplies[id], err
	}
	if reply, ok := f.waitReplies[id]; ok {
		return reply, nil
	}
	return "", nil
}
func (f *fakeAPI) CreateSession(_ context.Context, _, _ string, _ time.Duration) (string, string, error) {
	return "", "", fmt.Errorf("rule agent test fake: CreateSession not used")
}

func handle(t *testing.T, a *RuleAgent, msg string) string {
	t.Helper()
	res, err := a.Handle(context.Background(), nil, "", msg)
	if err != nil {
		t.Fatal(err)
	}
	return res.Reply
}

func handleFull(t *testing.T, a *RuleAgent, msg string) AgentResult {
	t.Helper()
	res, err := a.Handle(context.Background(), nil, "", msg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRule_HelpAndUnknown(t *testing.T) {
	a := NewRule(newFakeAPI())
	if !strings.Contains(handle(t, a, "/help"), "/list") {
		t.Error("/help missing /list")
	}
	if got := handle(t, a, "/whatever"); !strings.Contains(got, "unknown") {
		t.Errorf("unknown got: %q", got)
	}
	if got := handle(t, a, "hello there"); !strings.Contains(got, "natural-language") {
		t.Errorf("nl got: %q", got)
	}
	if handle(t, a, "") != "" {
		t.Error("empty message should produce empty reply")
	}
}

func TestRule_List(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{
		{ID: "abcdef0123", Title: "first", Cwd: "/tmp/a", LastEventAt: time.Now()},
		{ID: "0123abcdef", Title: "second", Cwd: "/tmp/b", LastEventAt: time.Now()},
	}
	a := NewRule(api)
	out := handle(t, a, "/list")
	if !strings.Contains(out, "abcdef01") || !strings.Contains(out, "0123abcd") {
		t.Errorf("list missing ids: %q", out)
	}
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Errorf("list missing titles: %q", out)
	}
}

func TestRule_ListEmpty(t *testing.T) {
	if got := handle(t, NewRule(newFakeAPI()), "/list"); got != "no sessions" {
		t.Errorf("got %q", got)
	}
}

func TestRule_SendByIDPrefix(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{
		{ID: "abc12345", Title: "one", Cwd: "/tmp"},
		{ID: "def67890", Title: "two", Cwd: "/tmp"},
	}
	a := NewRule(api)
	res := handleFull(t, a, "/send abc hello world")
	if !strings.Contains(res.Reply, "sent to abc12345") {
		t.Errorf("got %q", res.Reply)
	}
	if res.FocusSession != "abc12345" {
		t.Errorf("FocusSession = %q, want abc12345", res.FocusSession)
	}
	if len(api.sentTo) != 1 || api.sentTo[0] != "abc12345" {
		t.Errorf("sentTo = %v", api.sentTo)
	}
	if api.sentText[0] != "hello world" {
		t.Errorf("sentText = %q", api.sentText[0])
	}
}

func TestRule_NonSendCommandDoesNotChangeFocus(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{{ID: "abc12345", Title: "one"}}
	a := NewRule(api)
	if res := handleFull(t, a, "/list"); res.FocusSession != "" {
		t.Errorf("/list should not set focus, got %q", res.FocusSession)
	}
	if res := handleFull(t, a, "/help"); res.FocusSession != "" {
		t.Errorf("/help should not set focus, got %q", res.FocusSession)
	}
}

func TestRule_SendByTitleSubstring(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{
		{ID: "abc", Title: "deploy script"},
		{ID: "def", Title: "run tests"},
	}
	a := NewRule(api)
	if got := handle(t, a, "/send deploy ship it"); !strings.Contains(got, "sent to abc") {
		t.Errorf("got %q", got)
	}
}

func TestRule_SendAmbiguous(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{
		{ID: "abc1", Title: "one"},
		{ID: "abc2", Title: "two"},
	}
	a := NewRule(api)
	out := handle(t, a, "/send abc hi")
	if !strings.Contains(out, "ambiguous") {
		t.Errorf("got %q", out)
	}
	if len(api.sentTo) != 0 {
		t.Error("send should not have happened")
	}
}

func TestRule_SendNoMatch(t *testing.T) {
	api := newFakeAPI()
	a := NewRule(api)
	if got := handle(t, a, "/send nope hi"); !strings.Contains(got, "no sessions match") {
		t.Errorf("got %q", got)
	}
}

func TestRule_SendUsage(t *testing.T) {
	a := NewRule(newFakeAPI())
	if got := handle(t, a, "/send"); !strings.Contains(got, "usage") {
		t.Errorf("got %q", got)
	}
	if got := handle(t, a, "/send abc"); !strings.Contains(got, "usage") {
		t.Errorf("got %q", got)
	}
}

func TestRule_SendError(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{{ID: "abc"}}
	api.sendErr = errors.New("boom")
	a := NewRule(api)
	if got := handle(t, a, "/send abc hi"); !strings.Contains(got, "boom") {
		t.Errorf("got %q", got)
	}
}

func TestRule_PendingAndRespond(t *testing.T) {
	api := newFakeAPI()
	api.pending = []hook.Pending{
		{ID: "deadbeefcafef00d", SessionID: "sessabcd", ToolName: "Bash"},
	}
	a := NewRule(api)
	out := handle(t, a, "/pending")
	if !strings.Contains(out, "deadbeef") || !strings.Contains(out, "Bash") {
		t.Errorf("got %q", out)
	}
	out = handle(t, a, "/approve dead")
	if !strings.Contains(out, "allowed") {
		t.Errorf("got %q", out)
	}
	if got := api.resolved["deadbeefcafef00d"]; got.Behavior != "allow" {
		t.Errorf("behavior = %q", got.Behavior)
	}
}

func TestRule_DenyAndAmbiguous(t *testing.T) {
	api := newFakeAPI()
	api.pending = []hook.Pending{
		{ID: "abc11", ToolName: "Bash"},
		{ID: "abc22", ToolName: "Read"},
	}
	a := NewRule(api)
	if got := handle(t, a, "/deny abc"); !strings.Contains(got, "ambiguous") {
		t.Errorf("got %q", got)
	}
	if got := handle(t, a, "/deny abc11"); !strings.Contains(got, "denied") {
		t.Errorf("got %q", got)
	}
}

func TestRule_ApproveNoMatch(t *testing.T) {
	a := NewRule(newFakeAPI())
	if got := handle(t, a, "/approve missing"); !strings.Contains(got, "no pending") {
		t.Errorf("got %q", got)
	}
}
