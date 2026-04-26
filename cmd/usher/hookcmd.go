package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// runHook is the worker for `usher hook <event>`. It is invoked by Claude
// Code per the entry installed by `usher setup`. It reads the hook payload
// from stdin, POSTs it to the local usher server, and writes the server's
// JSON response to stdout.
//
// If usher is not running, the hook fails open: prints `{}` and exits 0,
// letting Claude proceed with its default permission flow.
func runHook(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: usher hook <event-name>")
	}
	event := args[0]

	addr := os.Getenv("USHER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:7777"
	}

	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	url := "http://" + addr + "/hook/" + event
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// No client-side timeout: the server holds the request until either the
	// user responds via UI or Claude Code's own hook timeout fires (which
	// will kill our process).
	resp, err := http.DefaultClient.Do(req)
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
