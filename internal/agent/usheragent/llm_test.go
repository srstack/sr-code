package usheragent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
}
type sendCall struct{ ID, Text string }

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

// --- tests ---

func TestLLMAgent_DirectAnswer(t *testing.T) {
	srv, _ := newMockChatServer(t, []ChatResponse{chatTextResp("hi back")})
	defer srv.Close()
	a := newTestLLM(t, &fakeAgentAPI{}, srv.URL)
	got, err := a.Handle(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hi back" {
		t.Errorf("got %q", got)
	}
}

func TestLLMAgent_ListSessionsToolThenAnswer(t *testing.T) {
	api := &fakeAgentAPI{
		sessions: []core.Session{
			{ID: "abc12345", Title: "deploy", Cwd: "/x"},
			{ID: "def67890", Title: "tests", Cwd: "/y"},
		},
	}
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_1", "list_sessions", `{}`),
		chatTextResp("you have 2 sessions: deploy and tests."),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	out, err := a.Handle(context.Background(), "what's running?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deploy") {
		t.Errorf("got %q", out)
	}
	if m.calls != 2 {
		t.Errorf("expected 2 server calls, got %d", m.calls)
	}

	// On the second turn the tool result must have been appended.
	roles := []string{}
	for _, msg := range m.lastReq.Messages {
		roles = append(roles, msg.Role)
	}
	if !sliceContains(roles, "tool") {
		t.Errorf("second turn missing tool message: roles=%v", roles)
	}
}

func TestLLMAgent_SendToolDispatch(t *testing.T) {
	api := &fakeAgentAPI{
		sessions: []core.Session{{ID: "deadbeef", Title: "X"}},
	}
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_send", "send_to_session", `{"session_id":"deadbeef","text":"deploy now"}`),
		chatTextResp("sent to deadbeef"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	if _, err := a.Handle(context.Background(), "tell deadbeef to deploy now"); err != nil {
		t.Fatal(err)
	}
	if len(api.sent) != 1 || api.sent[0].ID != "deadbeef" || api.sent[0].Text != "deploy now" {
		t.Errorf("sent = %+v", api.sent)
	}
}

func TestLLMAgent_RespondToolDispatch(t *testing.T) {
	api := &fakeAgentAPI{
		pending: []hook.Pending{{ID: "iact1", ToolName: "Bash"}},
	}
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_a", "respond_to_interaction", `{"id":"iact1","behavior":"allow","reason":"safe"}`),
		chatTextResp("approved"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	if _, err := a.Handle(context.Background(), "approve the pending bash"); err != nil {
		t.Fatal(err)
	}
	if len(api.resolved) != 1 || api.resolved[0].Behavior != "allow" {
		t.Errorf("resolved = %+v", api.resolved)
	}
}

func TestLLMAgent_ToolErrorPropagatesAsContent(t *testing.T) {
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_x", "send_to_session", `{"session_id":"","text":""}`), // bad args
		chatTextResp("told you so"),
	})
	defer srv.Close()

	a := newTestLLM(t, &fakeAgentAPI{}, srv.URL)
	if _, err := a.Handle(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	// Second call should include a tool-role message whose content is a JSON
	// error envelope — proves we surface tool errors back to the model
	// rather than aborting the turn.
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

func TestLLMAgent_UnknownToolNameSurfacesError(t *testing.T) {
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c1", "rm_rf_root", `{}`),
		chatTextResp("won't do that"),
	})
	defer srv.Close()

	a := newTestLLM(t, &fakeAgentAPI{}, srv.URL)
	if _, err := a.Handle(context.Background(), "do bad things"); err != nil {
		t.Fatal(err)
	}
	var toolMsg ChatMessage
	for _, m := range m.lastReq.Messages {
		if m.Role == "tool" {
			toolMsg = m
		}
	}
	if !strings.Contains(toolMsg.Content, "unknown tool") {
		t.Errorf("expected unknown-tool error, got %q", toolMsg.Content)
	}
}

func TestLLMAgent_MaxItersGuard(t *testing.T) {
	// Server always returns a tool call → loops forever → bounded by MaxIters
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(chatToolCallResp("c", "list_sessions", `{}`))
	}))
	defer srv.Close()

	a, err := NewLLM(&fakeAgentAPI{}, LLMConfig{
		Client:   NewChatClient(srv.URL+"/v1", "k"),
		Model:    "m",
		MaxIters: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Handle(context.Background(), "loop"); err == nil || !strings.Contains(err.Error(), "max iterations") {
		t.Errorf("expected max-iter error, got %v", err)
	}
}

func TestLLMAgent_RequiresClientAndModel(t *testing.T) {
	if _, err := NewLLM(&fakeAgentAPI{}, LLMConfig{Model: "m"}); err == nil {
		t.Error("expected error for nil Client")
	}
	if _, err := NewLLM(&fakeAgentAPI{}, LLMConfig{Client: NewChatClient("http://x", "k")}); err == nil {
		t.Error("expected error for empty Model")
	}
}

func TestExecuteTool_BadJSONArgs(t *testing.T) {
	a, _ := NewLLM(&fakeAgentAPI{}, LLMConfig{
		Client: NewChatClient("http://x", "k"),
		Model:  "m",
	})
	got := a.executeTool("send_to_session", "not json")
	if !strings.Contains(got, "invalid arguments") {
		t.Errorf("got %q", got)
	}
}

func sliceContains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
