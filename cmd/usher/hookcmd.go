package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// hookRetryInterval is how long `usher hook` waits between reconnect attempts
// while the usher server is unreachable (e.g. mid-restart).
const hookRetryInterval = time.Second

// runHook is the worker for `usher hook <event>`. It is invoked by Claude Code
// per the entry installed by `usher setup`. It reads the hook payload from
// stdin, POSTs it to the usher server over its Unix domain socket, and writes
// the server's JSON decision to stdout.
//
// Ownership is decided locally from the environment — no server round-trip
// needed. usher spawns its sessions with USHER_HOOK_SOCK set (pool spawn -e),
// so a claude that carries it is usher-managed; one that doesn't is the user's
// own (terminal/IDE). For the latter we fail open immediately (print `{}`) and
// let claude use its own permission flow — usher neither blocks nor awaits it.
//
// For a usher-managed session the only resolver is usher's web UI, so if the
// server is unreachable we retry the connection indefinitely rather than fail
// open into a tmux pane TUI nobody is watching. This rides out a usher restart
// (it rebinds the same socket and re-adopts the session). The retry is bounded
// by Claude Code's own hook timeout, which SIGKILLs us if usher never returns.
func runHook(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: usher hook <event-name>")
	}
	event := args[0]

	sockPath := os.Getenv("USHER_HOOK_SOCK")
	if sockPath == "" {
		// Not a usher-managed session — fail open at once.
		fmt.Println("{}")
		return nil
	}

	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	client := &http.Client{
		// No client-side timeout: the server holds the request until the user
		// responds via the UI (or Claude Code's hook timeout kills us).
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
	// "http://hook" is a placeholder authority; the unix dialer ignores it.
	url := "http://hook/hook/" + event

	for {
		// Rebuild per attempt — the body reader is consumed on use.
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			// usher down / restarting → wait and retry (managed session).
			time.Sleep(hookRetryInterval)
			continue
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			// Reachable but declined (shouldn't happen for an env-tagged
			// session) → fail open.
			fmt.Fprintln(os.Stderr, "usher hook: server returned", resp.StatusCode, ":", strings.TrimSpace(string(out)))
			fmt.Println("{}")
			return nil
		}
		fmt.Println(strings.TrimRight(string(out), "\n"))
		return nil
	}
}
