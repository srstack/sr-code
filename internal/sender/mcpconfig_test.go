package sender

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPConfigArgs(t *testing.T) {
	dir := t.TempDir()
	hookSock := filepath.Join(dir, "hook.sock")
	args := claudeMCPConfigArgs(hookSock, slog.Default())

	if len(args) != 2 || args[0] != "--mcp-config" {
		t.Fatalf("expected [--mcp-config <path>], got %v", args)
	}
	cfgPath := args[1]
	if filepath.Dir(cfgPath) != dir {
		t.Fatalf("config should sit next to hook sock; got %s", cfgPath)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command    string   `json:"command"`
			Args       []string `json:"args"`
			AlwaysLoad bool     `json:"alwaysLoad"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	srv, ok := cfg.MCPServers["usher"]
	if !ok {
		t.Fatalf("config missing usher server: %s", raw)
	}
	if len(srv.Args) != 1 || srv.Args[0] != "mcp-stdio" {
		t.Fatalf("expected args [mcp-stdio], got %v", srv.Args)
	}
	if !srv.AlwaysLoad {
		t.Fatal("server must set alwaysLoad:true to bypass Tool Search deferral")
	}
	if !filepath.IsAbs(srv.Command) || strings.TrimSpace(srv.Command) == "" {
		t.Fatalf("command should be an absolute exe path, got %q", srv.Command)
	}
}

// Empty hook sock disables the feature (returns nil) rather than erroring.
func TestMCPConfigArgs_NoHookSock(t *testing.T) {
	if got := claudeMCPConfigArgs("", slog.Default()); got != nil {
		t.Fatalf("expected nil for empty hook sock, got %v", got)
	}
}
