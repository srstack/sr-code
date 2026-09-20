// Package acp is a minimal Agent Client Protocol client over newline-delimited
// JSON-RPC stdio — enough to drive kiro (`kiro-cli acp`) and dsh
// (`dsh --profile acp`): initialize, session new/load/resume, prompt with
// streamed session/update notifications, cancellation, and permission
// requests relayed to a handler.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// Conn is one ACP child process's stdio transport.
type Conn struct {
	in  io.WriteCloser
	out *bufio.Scanner

	mu      sync.Mutex
	pending map[int64]chan rpcResult
	nextID  int64

	onUpdate   func(Update)
	onRequest  func(id int64, m ServerRequest) // server→client requests (permissions)
	writeErrMu sync.Mutex
	writeErr   error
	done       chan struct{}
	closeOnce  sync.Once
	updateMu   sync.RWMutex
	errTail    *tailBuffer
}

// tailBuffer keeps the last bytes of a stream for error messages.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2048 {
		t.buf = t.buf[len(t.buf)-2048:]
	}
	return len(p), nil
}

// StderrTail returns the child's recent stderr for diagnostics.
func (c *Conn) StderrTail() string {
	if c.errTail == nil {
		return ""
	}
	c.errTail.mu.Lock()
	defer c.errTail.mu.Unlock()
	return string(c.errTail.buf)
}

// SetUpdateHandler swaps the session/update handler (nil disables).
func (c *Conn) SetUpdateHandler(h func(Update)) {
	c.updateMu.Lock()
	c.onUpdate = h
	c.updateMu.Unlock()
}

type rpcResult struct {
	Result json.RawMessage
	Error  *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// ServerRequest is a server→client request (e.g. session/request_permission).
type ServerRequest struct {
	Method string
	Params json.RawMessage
}

// PermissionParams is the session/request_permission payload.
type PermissionParams struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		Kind       string          `json:"kind"`
		RawInput   json.RawMessage `json:"rawInput"`
	} `json:"toolCall"`
	Options []struct {
		OptionID string `json:"optionId"`
		Name     string `json:"name"`
		Kind     string `json:"kind"` // allow_once | allow_always | reject_once | reject_always
	} `json:"options"`
}

// Start launches cmd and reads its stdout as the ACP stream. onUpdate receives
// session/update notifications; onRequest receives server→client requests and
// must answer via Respond.
func Start(ctx context.Context, name string, args []string, dir string, onUpdate func(Update), onRequest func(id int64, m ServerRequest)) (*Conn, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Conn{
		in:      stdin,
		out:     bufio.NewScanner(stdout),
		pending: map[int64]chan rpcResult{},
		done:    make(chan struct{}),
	}
	c.out.Buffer(make([]byte, 64<<10), 32<<20)
	// Agent logs (diagnostics) — drain so the child never blocks on stderr;
	// keep the tail for error reporting.
	var errTail tailBuffer
	go io.Copy(&errTail, stderr)
	c.errTail = &errTail
	go c.readLoop(onUpdate, onRequest)
	go func() {
		_ = cmd.Wait()
		c.closeOnce.Do(func() { close(c.done) })
	}()
	return c, nil
}

func (c *Conn) readLoop(onUpdate func(Update), onRequest func(id int64, m ServerRequest)) {
	for c.out.Scan() {
		line := c.out.Bytes()
		if len(line) == 0 {
			continue
		}
		var head struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &head) != nil {
			continue
		}
		switch {
		case head.ID != nil && head.Method == "":
			// response
			var m struct {
				Result json.RawMessage `json:"result"`
				Error  *rpcError       `json:"error"`
			}
			_ = json.Unmarshal(line, &m)
			c.mu.Lock()
			ch := c.pending[*head.ID]
			delete(c.pending, *head.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- rpcResult{Result: m.Result, Error: m.Error}
			}
		case head.ID != nil && head.Method != "":
			// server→client request
			if onRequest != nil {
				go onRequest(*head.ID, ServerRequest{Method: head.Method, Params: head.Params})
			}
		case head.Method == "session/update":
			var m struct {
				Params struct {
					SessionID string `json:"sessionId"`
					Update    Update `json:"update"`
				} `json:"params"`
			}
			c.updateMu.RLock()
			h := c.onUpdate
			c.updateMu.RUnlock()
			if json.Unmarshal(line, &m) == nil && h != nil {
				m.Params.Update.SessionID = m.Params.SessionID
				h(m.Params.Update)
			}
		}
	}
}

// Done closes when the child process exits.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Call issues one request and waits for its response.
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	c.writeErrMu.Lock()
	werr := c.writeErr
	c.writeErrMu.Unlock()
	if werr != nil {
		return nil, werr
	}
	if _, err := c.in.Write(append(body, '\n')); err != nil {
		c.writeErrMu.Lock()
		c.writeErr = err
		c.writeErrMu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return nil, r.Error
		}
		return r.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("acp: child exited")
	}
}

// Respond answers a server→client request.
func (c *Conn) Respond(id int64, result any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if err != nil {
		return err
	}
	_, err = c.in.Write(append(body, '\n'))
	return err
}

// RespondError answers a server→client request with an error.
func (c *Conn) RespondError(id int64, code int, message string) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	if err != nil {
		return err
	}
	_, err = c.in.Write(append(body, '\n'))
	return err
}

// Close terminates the child.
func (c *Conn) Close() error { return c.in.Close() }

// --- session lifecycle helpers ------------------------------------------------

// Initialize performs the ACP handshake.
func (c *Conn) Initialize(ctx context.Context) error {
	_, err := c.Call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}},
	})
	return err
}

// SessionNew creates a session and returns its id.
func (c *Conn) SessionNew(ctx context.Context, cwd string) (string, error) {
	raw, err := c.Call(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		return "", err
	}
	var r struct {
		SessionID string `json:"sessionId"`
		Meta      struct {
			ID string `json:"id"`
		} `json:"_meta"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return "", fmt.Errorf("acp: bad session/new response: %.200s", raw)
	}
	if r.SessionID != "" {
		return r.SessionID, nil
	}
	return r.Meta.ID, nil // kiro answers in _meta
}

// SessionLoad attaches to an existing session (ACP session/load).
func (c *Conn) SessionLoad(ctx context.Context, id, cwd string) error {
	_, err := c.Call(ctx, "session/load", map[string]any{"sessionId": id, "cwd": cwd, "mcpServers": []any{}})
	return err
}

// SessionResume attaches to an existing session (dsh's session/resume).
func (c *Conn) SessionResume(ctx context.Context, id, cwd string) error {
	_, err := c.Call(ctx, "session/resume", map[string]any{"sessionId": id, "cwd": cwd})
	return err
}

// Prompt sends one user turn; the call returns when the turn ends.
func (c *Conn) Prompt(ctx context.Context, sessionID, text string) (string, error) {
	raw, err := c.Call(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": text}},
	})
	if err != nil {
		return "", err
	}
	var r struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.StopReason, nil
}

// Cancel interrupts the in-flight turn.
func (c *Conn) Cancel(ctx context.Context, sessionID string) error {
	// session/cancel is a notification; no response is expected, so write
	// directly rather than waiting on Call.
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sessionID}})
	if err != nil {
		return err
	}
	_, err = c.in.Write(append(body, '\n'))
	return err
}
