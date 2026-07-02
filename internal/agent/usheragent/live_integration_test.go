package usheragent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/core"
)

// Live tests that a real reasoning model's multi-turn tool loop survives our
// field round-trip. Each skips unless its API key is set, so `go test ./...`
// stays offline-safe. Run: go test ./internal/agent/usheragent -run TestLive -v
type liveProvider struct {
	name    string
	keyEnv  string
	baseURL string
	model   string
}

var liveProviders = []liveProvider{
	// DeepSeek V4 defaults to thinking-on → emits message-level reasoning_content.
	{"deepseek", "DEEPSEEK_KEY", "https://api.deepseek.com/v1", "deepseek-v4-flash"},
	// Gemini 3 enforces thought_signature round-trip on tool calls.
	{"gemini", "GEMINI_API_KEY", "https://generativelanguage.googleapis.com/v1beta/openai", "gemini-3.5-flash"},
	// Anthropic's OpenAI-compatible endpoint — smoke test the loop end to end.
	{"anthropic", "CLAUDE_KEY", "https://api.anthropic.com/v1", "claude-haiku-4-5-20251001"},
}

func TestLive_ToolLoopRoundTrip(t *testing.T) {
	for _, p := range liveProviders {
		p := p
		t.Run(p.name, func(t *testing.T) {
			key := os.Getenv(p.keyEnv)
			if key == "" {
				t.Skipf("%s not set", p.keyEnv)
			}

			api := newFakeAgentAPI()
			api.sessions = []core.Session{
				{ID: "aaaa1111", Title: "deploy", Cwd: "/srv/deploy"},
				{ID: "bbbb2222", Title: "tests", Cwd: "/srv/tests"},
			}

			a, err := NewLLM(api, LLMConfig{
				Client: NewChatClient(p.baseURL, key),
				Model:  p.model,
			})
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()

			// Force a tool call so the loop runs ≥2 iterations: turn 1 = the
			// (reasoning + tool_call) assistant turn we must replay, turn 2 =
			// the answer that 400s if the replay dropped the thinking state.
			res, err := a.Handle(ctx, nil, "",
				"Call the list_sessions tool, then tell me exactly how many sessions there are.", nil)
			if err != nil {
				t.Fatalf("%s tool loop failed (round-trip regression?): %v", p.name, err)
			}
			if strings.TrimSpace(res.Reply) == "" {
				t.Errorf("%s returned empty reply", p.name)
			}
			t.Logf("%s reply: %s", p.name, res.Reply)
		})
	}
}
