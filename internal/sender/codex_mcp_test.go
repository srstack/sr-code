package sender

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/nexustar/usher/internal/hook"
)

func TestCodexMCPConfArgs(t *testing.T) {
	args := codexMCPConfigArgs(slog.Default())
	if len(args) != 4 {
		t.Fatalf("want 4 -c values, got %v", args)
	}
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		`mcp_servers.usher.command="`,
		`mcp_servers.usher.args=["mcp-stdio"]`,
		`mcp_servers.usher.env_vars=["USHER_HOOK_SOCK"]`,
		`code_mode.direct_only_tool_namespaces=["usher"]`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestCodexMCPConfigUsesNativeValues(t *testing.T) {
	cfg := codexMCPConfig(slog.Default())
	if _, ok := cfg["mcp_servers.usher.command"].(string); !ok {
		t.Fatalf("command: %#v", cfg)
	}
	if args, ok := cfg["mcp_servers.usher.args"].([]string); !ok || len(args) != 1 || args[0] != "mcp-stdio" {
		t.Fatalf("args must be a native string array: %#v", cfg["mcp_servers.usher.args"])
	}
	direct, ok := cfg["code_mode.direct_only_tool_namespaces"].([]string)
	if !ok || len(direct) != 1 || direct[0] != "usher" {
		t.Fatalf("usher namespace must bypass deferred loading: %#v", direct)
	}
}

func TestCodexHeadlessParams(t *testing.T) {
	sandbox, cfg := codexHeadlessParams([]string{"--sandbox=read-only", "-c", "model_reasoning_effort=high", "-c", "features.foo=true"}, slog.Default())
	if sandbox["sandbox"] != "read-only" || cfg["model_reasoning_effort"] != "high" || cfg["features.foo"] != true {
		t.Fatalf("sandbox=%#v config=%#v", sandbox, cfg)
	}
}

// With injectMCPTools+hookSock, the codex spawn command carries the MCP -c flags.
func TestCodexSpawnInjectsMCP(t *testing.T) {
	s := NewCodex("codex", "/sessions", "", "/tmp/h.sock", nil, 8, true, hook.New(""), slog.Default())
	b := s.backend.(codexBackend)
	cmd := b.spawnCommand("id1", "/work/proj", "", false)
	if !strings.Contains(cmd, "mcp_servers.usher.command=") {
		t.Errorf("spawn should inject usher MCP -c flags; got: %s", cmd)
	}
	// cwd override so the MCP server's path checks match the session cwd.
	if !strings.Contains(cmd, `mcp_servers.usher.cwd=`) || !strings.Contains(cmd, "/work/proj") {
		t.Errorf("spawn should inject the session cwd; got: %s", cmd)
	}
}

// --disable-usher-tools (injectMCPTools=false) ⇒ no MCP flags.
func TestCodexSpawnNoMCPWhenDisabled(t *testing.T) {
	s := NewCodex("codex", "/sessions", "", "/tmp/h.sock", nil, 8, false, hook.New(""), slog.Default())
	b := s.backend.(codexBackend)
	cmd := b.spawnCommand("id1", "/c", "", false)
	if strings.Contains(cmd, "mcp_servers.usher") {
		t.Errorf("disabled: spawn must not inject MCP flags; got: %s", cmd)
	}
}
