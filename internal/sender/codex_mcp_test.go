package sender

import (
	"log/slog"
	"testing"
)

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
