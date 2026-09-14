// Package embed manages the lifecycle of embedded agent web UIs —
// child processes (dsh/opencode/kimi) spawned on free loopback ports and
// surfaced behind usher's reverse proxy.
package embed

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	healthPollInterval = 250 * time.Millisecond
	readyTimeout       = 60 * time.Second
)

// Spec describes one embedded agent UI.
type Spec struct {
	Name       string // "dsh"
	Title      string // "DeepSeek Harness" (sidebar label)
	Cmd        string
	Args       []string // {port} placeholder is replaced with the chosen child port
	Env        []string // extra KEY=VAL appended to os.Environ
	HealthPath string   // HTTP path polled for readiness, e.g. "/"
	URLPattern string   // regexp with one capture group; first stdout/stderr line matching yields the start URL (may carry ?token=). Empty = no capture.
	Dir        string   // child working directory; empty inherits usher's. Agents scope their UI to the cwd workspace (dsh lists sessions per cwd).
	// RootAPI marks the child as the fallback for unknown root-absolute /api
	// requests. UIs whose Workers bypass the URL-rebasing path proxy and call
	// /api/... on the page origin (dsh's RPC stream and WebSocket) need it;
	// at most one embed may set it.
	RootAPI bool
}

// Process is a running embedded child.
type Process struct {
	spec       Spec
	childURL   string
	logger     *slog.Logger
	startQuery atomic.Value // string, query captured from the child's output
	ready      atomic.Bool
}

// Start spawns the child on a free loopback port and returns immediately.
func Start(ctx context.Context, spec Spec, logger *slog.Logger) (*Process, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if _, err := exec.LookPath(spec.Cmd); err != nil {
		return nil, fmt.Errorf("embed %s: %w", spec.Name, err)
	}
	var urlRe *regexp.Regexp
	if spec.URLPattern != "" {
		re, err := regexp.Compile(spec.URLPattern)
		if err != nil {
			return nil, fmt.Errorf("embed %s: URLPattern: %w", spec.Name, err)
		}
		urlRe = re
	}

	// Reserve a free port and release it before spawning. The child binds
	// immediately after, so the listen/close race is accepted; a port stolen
	// in between surfaces as the child never becoming ready.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("embed %s: find free port: %w", spec.Name, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	args := make([]string, len(spec.Args))
	for i, a := range spec.Args {
		args[i] = strings.ReplaceAll(a, "{port}", strconv.Itoa(port))
	}
	cmd := exec.CommandContext(ctx, spec.Cmd, args...)
	cmd.Env = append(os.Environ(), spec.Env...)
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}

	p := &Process{
		spec:     spec,
		childURL: "http://127.0.0.1:" + strconv.Itoa(port),
		logger:   logger,
	}

	// Feed each stream through an io.Pipe: cmd.Wait waits for the
	// stdlib-internal copies into these writers to finish, and a writer only
	// completes once our scanner has consumed every line — so trailing output
	// printed just before a quick exit is never truncated.
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("embed %s: start: %w", spec.Name, err)
	}

	var captureOnce sync.Once
	p.scanStream(spec.Name, urlRe, &captureOnce, stdoutR)
	p.scanStream(spec.Name, urlRe, &captureOnce, stderrR)
	go p.pollReady(ctx)
	go func() {
		err := cmd.Wait()
		p.ready.Store(false)
		logger.Info("embed child exited", "name", spec.Name, "err", err)
	}()
	return p, nil
}

// ChildURL returns "http://127.0.0.1:<port>" once the process is running.
func (p *Process) ChildURL() string { return p.childURL }

// StartQuery returns the query string captured from stdout (e.g.
// "?token=abc"), or "".
func (p *Process) StartQuery() string {
	if q, ok := p.startQuery.Load().(string); ok {
		return q
	}
	return ""
}

// Ready reports readiness (health endpoint returned any HTTP status).
func (p *Process) Ready() bool { return p.ready.Load() }

// Spec returns the original spec (for the /api/embeds endpoint).
func (p *Process) Spec() Spec { return p.spec }

// scanStream logs each child output line and, on the first line matching
// urlRe, stores the capture group's query as the start URL's query.
func (p *Process) scanStream(name string, urlRe *regexp.Regexp, captureOnce *sync.Once, r io.Reader) {
	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			p.logger.Debug("embed child", "name", name, "line", line)
			if urlRe == nil {
				continue
			}
			m := urlRe.FindStringSubmatch(line)
			if len(m) < 2 {
				continue
			}
			u, err := url.Parse(m[1])
			if err != nil {
				continue
			}
			captureOnce.Do(func() {
				if u.RawQuery != "" {
					p.startQuery.Store("?" + u.RawQuery)
				}
			})
		}
	}()
}

// pollReady polls the health endpoint until any HTTP response (even
// 4xx/5xx) arrives, then marks the process ready. It gives up after
// readyTimeout and logs an error; the process stays up and the UI keeps
// showing "starting…".
func (p *Process) pollReady(ctx context.Context) {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.After(readyTimeout)
	tick := time.NewTicker(healthPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			p.logger.Error("embed child never became ready", "name", p.spec.Name)
			return
		case <-tick.C:
			resp, err := client.Get(p.childURL + p.spec.HealthPath)
			if err == nil {
				resp.Body.Close()
				p.ready.Store(true)
				return
			}
		}
	}
}
