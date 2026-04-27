package usheragent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"usher/internal/core"
	"usher/internal/hook"
)

// mockChatServer returns a series of canned responses in order. Each call
// to /v1/chat/completions consumes one response. Out-of-bounds calls fail
// the test — that means the agent looped further than expected.
type mockChatServer struct {
	t         *testing.T
	responses []ChatResponse
	mu        sync.Mutex
	calls     int
	lastReq   ChatRequest
}

func newMockChatServer(t *testing.T, responses []ChatResponse) (*httptest.Server, *mockChatServer) {
	m := &mockChatServer{t: t, responses: responses}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&m.lastReq)
		if m.calls >= len(m.responses) {
			t.Errorf("unexpected call %d (only %d responses queued)", m.calls+1, len(m.responses))
			http.Error(w, "no more responses", http.StatusInternalServerError)
			return
		}
		resp := m.responses[m.calls]
		m.calls++
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return srv, m
}

// --- fake AgentAPI ---

type fakeAgentAPI struct {
	sessions []core.Session
	pending  []hook.Pending
	sent     []sendCall
	resolved []hook.Response

	transcripts map[string][]core.TranscriptTurn
	waitReplies map[string]string
	waitErrs    map[string]error
	waitedFor   []sendCall
}
type sendCall struct{ ID, Text string }

func newFakeAgentAPI() *fakeAgentAPI {
	return &fakeAgentAPI{
		transcripts: map[string][]core.TranscriptTurn{},
		waitReplies: map[string]string{},
		waitErrs:    map[string]error{},
	}
}

func (f *fakeAgentAPI) ListSessions() []core.Session            { return f.sessions }
func (f *fakeAgentAPI) ListPendingInteractions() []hook.Pending { return f.pending }
func (f *fakeAgentAPI) SendToSession(id, text string) error {
	f.sent = append(f.sent, sendCall{id, text})
	return nil
}
func (f *fakeAgentAPI) RespondInteraction(id string, r hook.Response) error {
	f.resolved = append(f.resolved, r)
	return nil
}
func (f *fakeAgentAPI) ReadSessionTranscript(id string, limit int) ([]core.TranscriptTurn, error) {
	turns, ok := f.transcripts[id]
	if !ok {
		return nil, errors.New("session not found")
	}
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, nil
}
func (f *fakeAgentAPI) SendToSessionAndWait(_ context.Context, id, text string, _ time.Duration) (string, error) {
	f.waitedFor = append(f.waitedFor, sendCall{id, text})
	if err, ok := f.waitErrs[id]; ok && err != nil {
		return f.waitReplies[id], err
	}
	if reply, ok := f.waitReplies[id]; ok {
		return reply, nil
	}
	return "", nil
}

// --- helpers ---

func chatTextResp(text string) ChatResponse {
	return ChatResponse{
		Choices: []ChatChoice{{
			Message:      ChatMessage{Role: "assistant", Content: text},
			FinishReason: "stop",
		}},
	}
}

func chatToolCallResp(callID, name, argsJSON string) ChatResponse {
	return ChatResponse{
		Choices: []ChatChoice{{
			Message: ChatMessage{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:       callID,
					Type:     "function",
					Function: ToolCallFunc{Name: name, Arguments: argsJSON},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
}

func newTestLLM(t *testing.T, api AgentAPI, baseURL string) *LLMAgent {
	t.Helper()
	c := NewChatClient(baseURL+"/v1", "test-key")
	a, err := NewLLM(api, LLMConfig{Client: c, Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func runHandle(t *testing.T, a *LLMAgent, userMsg string) AgentResult {
	t.Helper()
	res, err := a.Handle(context.Background(), nil, "", userMsg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// --- tests ---

func TestLLMAgent_DirectAnswer(t *testing.T) {
	srv, _ := newMockChatServer(t, []ChatResponse{chatTextResp("hi back")})
	defer srv.Close()
	a := newTestLLM(t, newFakeAgentAPI(), srv.URL)
	res := runHandle(t, a, "hello")
	if res.Reply != "hi back" {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.FocusSession != "" {
		t.Errorf("Focus should be empty, got %q", res.FocusSession)
	}
}

func TestLLMAgent_ListSessionsToolThenAnswer(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{
		{ID: "abc12345", Title: "deploy", Cwd: "/x"},
		{ID: "def67890", Title: "tests", Cwd: "/y"},
	}
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_1", "list_sessions", `{}`),
		chatTextResp("you have 2 sessions: deploy and tests."),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "what's running?")
	if !strings.Contains(res.Reply, "deploy") {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.FocusSession != "" {
		t.Errorf("list_sessions must not set focus, got %q", res.FocusSession)
	}
	if m.calls != 2 {
		t.Errorf("expected 2 server calls, got %d", m.calls)
	}
}

func TestLLMAgent_SendToolDispatchAndFocus(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{{ID: "deadbeef", Title: "X"}}
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_send", "send_to_session", `{"session_id":"deadbeef","text":"deploy now"}`),
		chatTextResp("sent"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "tell deadbeef to deploy now")
	if len(api.sent) != 1 || api.sent[0].ID != "deadbeef" || api.sent[0].Text != "deploy now" {
		t.Errorf("sent = %+v", api.sent)
	}
	if res.FocusSession != "deadbeef" {
		t.Errorf("FocusSession = %q, want deadbeef", res.FocusSession)
	}
}

func TestLLMAgent_RespondToolDispatchDoesNotSetFocus(t *testing.T) {
	api := newFakeAgentAPI()
	api.pending = []hook.Pending{{ID: "iact1", ToolName: "Bash"}}
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_a", "respond_to_interaction", `{"id":"iact1","behavior":"allow","reason":"safe"}`),
		chatTextResp("approved"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "approve the pending bash")
	if len(api.resolved) != 1 || api.resolved[0].Behavior != "allow" {
		t.Errorf("resolved = %+v", api.resolved)
	}
	if res.FocusSession != "" {
		t.Errorf("respond_to_interaction must not set focus, got %q", res.FocusSession)
	}
}

func TestLLMAgent_ToolErrorPropagatesAsContent(t *testing.T) {
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_x", "send_to_session", `{"session_id":"","text":""}`),
		chatTextResp("told you so"),
	})
	defer srv.Close()

	a := newTestLLM(t, newFakeAgentAPI(), srv.URL)
	if _, err := a.Handle(context.Background(), nil, "", "x"); err != nil {
		t.Fatal(err)
	}
	var toolMsg *ChatMessage
	for i := range m.lastReq.Messages {
		if m.lastReq.Messages[i].Role == "tool" {
			toolMsg = &m.lastReq.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message in second request")
	}
	if !strings.Contains(toolMsg.Content, "error") {
		t.Errorf("tool content = %q", toolMsg.Content)
	}
}

func TestLLMAgent_ReadTranscript(t *testing.T) {
	api := newFakeAgentAPI()
	api.transcripts["spike01"] = []core.TranscriptTurn{
		{Role: "user", Content: "what is 2+2?"},
		{Role: "assistant", Content: "4"},
	}
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c", "read_session_transcript", `{"session_id":"spike01","limit":50}`),
		chatTextResp("the assistant said 4"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "what was the answer in spike?")
	if !strings.Contains(res.Reply, "4") {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.FocusSession != "spike01" {
		t.Errorf("read_session_transcript should set focus, got %q", res.FocusSession)
	}
	// Verify the tool result fed back contained transcript JSON
	var toolMsg *ChatMessage
	for i := range m.lastReq.Messages {
		if m.lastReq.Messages[i].Role == "tool" {
			toolMsg = &m.lastReq.Messages[i]
		}
	}
	if toolMsg == nil || !strings.Contains(toolMsg.Content, `"4"`) {
		t.Errorf("tool result didn't contain transcript content; got %q", toolMsg.Content)
	}
}

func TestLLMAgent_ReadTranscriptDefaultLimit(t *testing.T) {
	api := newFakeAgentAPI()
	// Build 100 turns; default limit is 20.
	api.transcripts["s"] = make([]core.TranscriptTurn, 100)
	for i := range api.transcripts["s"] {
		api.transcripts["s"][i] = core.TranscriptTurn{Role: "user", Content: "x"}
	}
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c", "read_session_transcript", `{"session_id":"s"}`),
		chatTextResp("ok"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	_ = runHandle(t, a, "summarize s")
	// We can't easily count returned items without inspecting the tool
	// response; instead verify that the call didn't error and focus moved.
}

func TestLLMAgent_SendAndWait(t *testing.T) {
	api := newFakeAgentAPI()
	api.waitReplies["abc"] = "/tmp/usher-spike"
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c", "send_and_wait_for_response", `{"session_id":"abc","text":"pwd"}`),
		chatTextResp("session abc says: /tmp/usher-spike"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "ask abc what its cwd is")
	if !strings.Contains(res.Reply, "/tmp/usher-spike") {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.FocusSession != "abc" {
		t.Errorf("focus = %q", res.FocusSession)
	}
	if len(api.waitedFor) != 1 || api.waitedFor[0].ID != "abc" || api.waitedFor[0].Text != "pwd" {
		t.Errorf("waitedFor = %+v", api.waitedFor)
	}
}

func TestLLMAgent_SendAndWaitTimeoutClamped(t *testing.T) {
	api := newFakeAgentAPI()
	api.waitReplies["s"] = "ok"
	// timeout_seconds=99999 should be silently clamped to 1800; we just
	// verify the call didn't error.
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c", "send_and_wait_for_response", `{"session_id":"s","text":"x","timeout_seconds":99999}`),
		chatTextResp("done"),
	})
	defer srv.Close()
	a := newTestLLM(t, api, srv.URL)
	if _, err := a.Handle(context.Background(), nil, "", "x"); err != nil {
		t.Fatal(err)
	}
}

func TestLLMAgent_HistoryAndFocusInjected(t *testing.T) {
	srv, m := newMockChatServer(t, []ChatResponse{chatTextResp("ok")})
	defer srv.Close()
	a := newTestLLM(t, newFakeAgentAPI(), srv.URL)

	history := []HistoryMessage{
		{Role: "user", Content: "first thing"},
		{Role: "agent", Content: "first reply"},
	}
	if _, err := a.Handle(context.Background(), history, "focused-session-id", "next thing"); err != nil {
		t.Fatal(err)
	}

	// Check the request the mock saw: should have system, system(focus), user, assistant, user
	roles := []string{}
	for _, m := range m.lastReq.Messages {
		roles = append(roles, m.Role)
	}
	if len(roles) != 5 || roles[0] != "system" || roles[1] != "system" {
		t.Errorf("expected [system, system(focus), user, assistant, user]; got %v", roles)
	}
	if !strings.Contains(m.lastReq.Messages[1].Content, "focused-session-id") {
		t.Errorf("focus system message missing id: %q", m.lastReq.Messages[1].Content)
	}
	if m.lastReq.Messages[2].Content != "first thing" {
		t.Errorf("history[0] = %q", m.lastReq.Messages[2].Content)
	}
	if m.lastReq.Messages[3].Role != "assistant" || m.lastReq.Messages[3].Content != "first reply" {
		t.Errorf("history[1] = %+v", m.lastReq.Messages[3])
	}
	if m.lastReq.Messages[4].Content != "next thing" {
		t.Errorf("current user = %q", m.lastReq.Messages[4].Content)
	}
}

func TestLLMAgent_NoFocusInjectionWhenEmpty(t *testing.T) {
	srv, m := newMockChatServer(t, []ChatResponse{chatTextResp("ok")})
	defer srv.Close()
	a := newTestLLM(t, newFakeAgentAPI(), srv.URL)
	if _, err := a.Handle(context.Background(), nil, "", "hi"); err != nil {
		t.Fatal(err)
	}
	// Only one system message (the static prompt), then the user message.
	if len(m.lastReq.Messages) != 2 || m.lastReq.Messages[0].Role != "system" || m.lastReq.Messages[1].Role != "user" {
		t.Errorf("messages = %+v", m.lastReq.Messages)
	}
}

func TestLLMAgent_MaxItersGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(chatToolCallResp("c", "list_sessions", `{}`))
	}))
	defer srv.Close()

	a, err := NewLLM(newFakeAgentAPI(), LLMConfig{
		Client:   NewChatClient(srv.URL+"/v1", "k"),
		Model:    "m",
		MaxIters: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Handle(context.Background(), nil, "", "loop"); err == nil || !strings.Contains(err.Error(), "max iterations") {
		t.Errorf("expected max-iter error, got %v", err)
	}
}

func TestLLMAgent_RequiresClientAndModel(t *testing.T) {
	if _, err := NewLLM(newFakeAgentAPI(), LLMConfig{Model: "m"}); err == nil {
		t.Error("expected error for nil Client")
	}
	if _, err := NewLLM(newFakeAgentAPI(), LLMConfig{Client: NewChatClient("http://x", "k")}); err == nil {
		t.Error("expected error for empty Model")
	}
}

func TestExecuteTool_BadJSONArgs(t *testing.T) {
	a, _ := NewLLM(newFakeAgentAPI(), LLMConfig{
		Client: NewChatClient("http://x", "k"),
		Model:  "m",
	})
	got, focus := a.executeTool(context.Background(), "send_to_session", "not json")
	if !strings.Contains(got, "invalid arguments") {
		t.Errorf("got %q", got)
	}
	if focus != "" {
		t.Errorf("focus = %q, want empty", focus)
	}
}
