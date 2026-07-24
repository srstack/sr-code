package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/pluginapi"
)

// pluginDialer creates an http.Client that talks to the plugin API Unix socket.
func pluginDialer() (*http.Client, string, error) {
	sockPath := pluginapi.SocketPath(pluginapi.DefaultDataDir())
	if _, err := os.Stat(sockPath); err != nil {
		return nil, "", fmt.Errorf("usher is not running (no plugin socket at %s)", sockPath)
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
	return client, "http://usher", nil
}

// runSendCommand sends a message to a session via the plugin socket.
func runSendCommand(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: usher send <session-id> <text>")
	}
	id := args[0]
	text := strings.Join(args[1:], " ")

	client, baseURL, err := pluginDialer()
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	resp, err := client.Post(baseURL+"/v1/sessions/"+id+"/send", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("send failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// runInterruptCommand interrupts a running session via the plugin socket.
func runInterruptCommand(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: usher interrupt <session-id>")
	}
	id := args[0]

	client, baseURL, err := pluginDialer()
	if err != nil {
		return err
	}
	resp, err := client.Post(baseURL+"/v1/sessions/"+id+"/interrupt", "application/json", nil)
	if err != nil {
		return fmt.Errorf("interrupt failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("interrupt failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
