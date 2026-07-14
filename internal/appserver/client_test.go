package appserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/hook"
)

func TestThreadParamsCarryPolicySandboxAndNativeMCP(t *testing.T) {
	c := New("codex", hook.New(""), map[string]any{"sandbox": "workspace-write"}, map[string]any{
		"mcp_servers.usher.command":             "/usr/bin/usher",
		"mcp_servers.usher.args":                []string{"mcp-stdio"},
		"mcp_servers.usher.env_vars":            []string{"USHER_HOOK_SOCK"},
		"code_mode.direct_only_tool_namespaces": []string{"usher"},
	}, nil, nil)
	p := c.threadParams("/work/project", "gpt-test")
	if p["approvalPolicy"] != "on-request" || p["sandbox"] != "workspace-write" || p["cwd"] != "/work/project" {
		t.Fatalf("missing thread policy params: %#v", p)
	}
	cfg := p["config"].(map[string]any)
	if cfg["mcp_servers.usher.cwd"] != "/work/project" || cfg["model"] != "gpt-test" {
		t.Fatalf("missing per-thread config: %#v", cfg)
	}
	args, ok := cfg["mcp_servers.usher.args"].([]string)
	if !ok || len(args) != 1 || args[0] != "mcp-stdio" {
		t.Fatalf("MCP args are not a native array: %#v", cfg["mcp_servers.usher.args"])
	}
	direct, ok := cfg["code_mode.direct_only_tool_namespaces"].([]string)
	if !ok || len(direct) != 1 || direct[0] != "usher" {
		t.Fatalf("usher namespace must be direct: %#v", direct)
	}
}

func TestAgentMessageDeltaRoutesByThread(t *testing.T) {
	c := New("codex", nil, nil, nil, nil, nil)
	stream := &turnStream{done: make(chan TurnResult, 1), deltas: make(chan Delta, 2)}
	c.turns["thread-1"] = stream
	c.dispatch(rpcMessage{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"hello"}`)})
	select {
	case d := <-stream.deltas:
		if d.Kind != "text" || d.Text != "hello" {
			t.Fatalf("delta = %+v", d)
		}
	default:
		t.Fatal("agent message delta not routed")
	}
}

func TestInterruptStoppedClientIsNoop(t *testing.T) {
	c := New("definitely-not-a-command", hook.New(""), nil, nil, nil, nil)
	if err := c.Interrupt(context.Background(), "missing"); err != nil {
		t.Fatalf("interrupt stopped client: %v", err)
	}
}

func TestEnsureConcurrentWaitsForInitializedHandshake(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests")
	script := filepath.Join(dir, "fake-codex")
	body := `#!/bin/sh
IFS= read -r init
printf '%s\n' "$init" >> "$FAKE_LOG"
sleep 0.1
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"fake/1"}}'
IFS= read -r initialized
printf '%s\n' "$initialized" >> "$FAKE_LOG"
while IFS= read -r line; do :; done
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	c := New(script, hook.New(""), nil, nil, []string{"FAKE_LOG=" + logPath}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- c.ensure(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var b []byte
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		b, _ = os.ReadFile(logPath)
		if strings.Count(strings.TrimSpace(string(b)), "\n") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.Shutdown()
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"method":"initialized"`) {
		t.Fatalf("handshake requests: %s", b)
	}
}

func TestClientTracksSpontaneousTurnActivity(t *testing.T) {
	c := New("unused", nil, nil, nil, nil, nil)
	c.dispatch(rpcMessage{Method: "turn/started", Params: json.RawMessage(`{"threadId":"session-1","turn":{"id":"turn-1"}}`)})
	if !c.Busy("session-1") {
		t.Fatal("turn/started did not mark the session busy")
	}
	c.dispatch(rpcMessage{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}`)})
	if c.Busy("session-1") {
		t.Fatal("turn/completed did not clear session activity")
	}
}
