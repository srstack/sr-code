package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runSetup wires usher into ~/.claude/settings.json by adding a PreToolUse
// hook that runs `usher hook PreToolUse`. Existing user-defined hooks are
// preserved; previous usher entries (identified by command suffix) are
// replaced so re-running is idempotent.
func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	addr := fs.String("addr", "", "USHER_ADDR to set in the hook command (defaults to 127.0.0.1:7777 at runtime)")
	remove := fs.Bool("remove", false, "remove the usher hook from settings.json")
	if err := fs.Parse(args); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}

	settings, err := readSettings(settingsPath)
	if err != nil {
		return err
	}

	hooksRoot, _ := settings["hooks"].(map[string]any)
	if hooksRoot == nil {
		hooksRoot = map[string]any{}
	}

	preToolUse, _ := hooksRoot["PreToolUse"].([]any)
	preToolUse = stripUsherEntries(preToolUse)

	if !*remove {
		cmd := exe + " hook PreToolUse"
		if *addr != "" {
			cmd = "USHER_ADDR=" + *addr + " " + cmd
		}
		preToolUse = append(preToolUse, map[string]any{
			"matcher": "",
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": cmd,
					"timeout": 600,
				},
			},
		})
	}

	if len(preToolUse) == 0 {
		delete(hooksRoot, "PreToolUse")
	} else {
		hooksRoot["PreToolUse"] = preToolUse
	}
	if len(hooksRoot) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooksRoot
	}

	if err := writeSettings(settingsPath, settings); err != nil {
		return err
	}
	if *remove {
		fmt.Printf("removed usher hook from %s\n", settingsPath)
	} else {
		cmd := exe + " hook PreToolUse"
		fmt.Printf("installed usher hook in %s\n", settingsPath)
		fmt.Printf("  matcher: (all tools)\n")
		fmt.Printf("  command: %s\n", cmd)
		fmt.Printf("  timeout: 600s\n")
		fmt.Println()
		fmt.Println("Re-run with --remove to uninstall.")
	}
	return nil
}

const usherHookCmdSuffix = " hook PreToolUse"

// stripUsherEntries returns the slice with any matcher group whose only
// command is usher's hook removed; groups containing both ours and other
// hooks have only ours filtered out.
func stripUsherEntries(groups []any) []any {
	var kept []any
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			kept = append(kept, g)
			continue
		}
		inner, _ := gm["hooks"].([]any)
		var keptInner []any
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			cmd, _ := hm["command"].(string)
			if isUsherHookCmd(cmd) {
				continue
			}
			keptInner = append(keptInner, h)
		}
		if len(keptInner) == 0 {
			continue
		}
		gm["hooks"] = keptInner
		kept = append(kept, gm)
	}
	return kept
}

func isUsherHookCmd(cmd string) bool {
	if cmd == "" {
		return false
	}
	// Tolerate optional leading env var assignment: "USHER_ADDR=... <exe> hook PreToolUse"
	return strings.HasSuffix(strings.TrimSpace(cmd), usherHookCmdSuffix)
}

func readSettings(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s == nil {
		s = map[string]any{}
	}
	return s, nil
}

func writeSettings(path string, s map[string]any) error {
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}
