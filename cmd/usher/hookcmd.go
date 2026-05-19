package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// runHook is the worker for `usher hook <event>`. It is invoked by Claude
// Code per the entry installed by `usher setup`. It reads the hook payload
// from stdin, POSTs it to the running usher server over its Unix domain
// socket, and writes the server's JSON response to stdout.
//
// If usher is not running (socket missing or refusing connections), the
// hook fails open: prints `{}` and exits 0, letting Claude proceed with
// its default permission flow.
func runHook(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: usher hook <event-name>")
	}
	event := args[0]

	sockPath := os.Getenv("USHER_HOOK_SOCK")
	if sockPath == "" {
		sockPath = filepath.Join(defaultDataDir(), "hook.sock")
	}

	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
	// "http://hook" is a placeholder authority; the unix dialer ignores it.
	url := "http://hook/hook/" + event
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// No client-side timeout: the server holds the request until either the
	// user responds via UI or Claude Code's own hook timeout fires (which
	// will kill our process).
	resp, err := client.Do(req)
	if err != nil {
		// Server unreachable → fail open.
		fmt.Println("{}")
		return nil
	}
	defer resp.Body.Close()

	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintln(os.Stderr, "usher hook: server returned", resp.StatusCode, ":", strings.TrimSpace(string(out)))
		fmt.Println("{}")
		return nil
	}

	fmt.Println(strings.TrimRight(string(out), "\n"))
	return nil
}
