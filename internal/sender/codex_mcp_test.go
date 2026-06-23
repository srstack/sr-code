package sender

import (
	"log/slog"
	"strings"
	"testing"
)

func TestCodexMCPConfArgs(t *testing.T) {
	args := codexMCPConfigArgs(slog.Default())
	if len(args) != 3 {
		t.Fatalf("want 3 -c values, got %v", args)
	}
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		`mcp_servers.usher.command="`,
		`mcp_servers.usher.args=["mcp-stdio"]`,
		`mcp_servers.usher.env_vars=["USHER_HOOK_SOCK"]`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

// With injectMCPTools+hookSock, the codex spawn command carries the MCP -c flags.
func TestCodexSpawnInjectsMCP(t *testing.T) {
	s := NewCodex("codex", "/sessions", "", "/tmp/h.sock", nil, 8, true, slog.Default())
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
	s := NewCodex("codex", "/sessions", "", "/tmp/h.sock", nil, 8, false, slog.Default())
	b := s.backend.(codexBackend)
	cmd := b.spawnCommand("id1", "/c", "", false)
	if strings.Contains(cmd, "mcp_servers.usher") {
		t.Errorf("disabled: spawn must not inject MCP flags; got: %s", cmd)
	}
}
