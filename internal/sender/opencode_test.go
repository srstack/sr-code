package sender

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeSendNewWritesShadowLogAndStreamsEvents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	tmp := t.TempDir()
	calls := filepath.Join(tmp, "calls")
	cmd := filepath.Join(tmp, "opencode")
	script := `#!/bin/sh
printf '%s\n' "$@" > ` + calls + `
printf 'hello from opencode\n'
`
	if err := os.WriteFile(cmd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cwd := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(tmp, "sessions")
	s := NewOpenCode(cmd, sessions, 2, nil)
	ch, err := s.SendNew(context.Background(), "ses_test", "say hi", cwd, "anthropic/claude")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for ev := range ch {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "subprocess.started,user,assistant,system,subprocess.exit" {
		t.Fatalf("events = %v", types)
	}

	args, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs := string(args)
	for _, want := range []string{"run", "--session", "ses_test", "--dir", cwd, "--model", "anthropic/claude", "say hi"} {
		if !strings.Contains(gotArgs, want) {
			t.Fatalf("opencode args %q missing %q", gotArgs, want)
		}
	}

	path := openCodeLogPath(sessions, cwd, "ses_test")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "say hi") || !strings.Contains(string(raw), "hello from opencode") {
		t.Fatalf("shadow log missing prompt or output:\n%s", raw)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("invalid jsonl line %q: %v", line, err)
		}
	}
}

func TestLocateOpenCode(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "x", "ses_1.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := locateOpenCode(tmp, "ses_1"); got != path {
		t.Fatalf("locateOpenCode = %q, want %q", got, path)
	}
	if got := locateOpenCode(tmp, "missing"); got != "" {
		t.Fatalf("locateOpenCode missing = %q, want empty", got)
	}
}

func TestOpenCodeSenderIsNotPreassignBlocked(t *testing.T) {
	s := NewOpenCode("opencode", t.TempDir(), 1, nil)
	if !s.PreAssignsID() {
		t.Fatal("OpenCode sender should use usher-assigned session ids")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	_, _ = s.Send(ctx, "missing", "hi", t.TempDir())
}
