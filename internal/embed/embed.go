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

	// A crashed child is restarted on a fresh port a bounded number of times,
	// so a transient boot failure (e.g. dsh aborting on a corrupt session log)
	// self-heals instead of leaving the embed stuck on "starting…". A child
	// that stayed up for restartHealthyAfter resets the budget.
	maxRestarts         = 5
	restartDelay        = 3 * time.Second
	restartHealthyAfter = 60 * time.Second
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
	spec     Spec
	logger   *slog.Logger
	childURL atomic.Value // string, "http://127.0.0.1:<port>" of the live child
	query    atomic.Value // string, start query captured from the child's output
	ready    atomic.Bool
}

// Start spawns the child on a free loopback port and returns immediately. The
// child is supervised: if it exits before usher shuts down, it is restarted on
// a fresh port (bounded, see maxRestarts).
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
	p := &Process{spec: spec, logger: logger}
	p.query.Store("")
	go p.supervise(ctx, urlRe)
	return p, nil
}

// supervise runs the child, restarting it after an early exit until the
// restart budget is exhausted or ctx is cancelled.
func (p *Process) supervise(ctx context.Context, urlRe *regexp.Regexp) {
	restarts := 0
	for {
		started := time.Now()
		err := p.spawn(ctx, urlRe)
		p.ready.Store(false)
		if ctx.Err() != nil {
			return
		}
		p.logger.Info("embed child exited", "name", p.spec.Name, "err", err, "ran", time.Since(started).Round(time.Second))
		if time.Since(started) >= restartHealthyAfter {
			restarts = 0
		}
		if restarts >= maxRestarts {
			p.logger.Error("embed child gave up after repeated exits", "name", p.spec.Name, "restarts", restarts)
			return
		}
		restarts++
		select {
		case <-ctx.Done():
			return
		case <-time.After(restartDelay):
		}
	}
}

// spawn runs one child to completion, returning its exit error. It publishes
// the new child's URL and query before waiting.
func (p *Process) spawn(ctx context.Context, urlRe *regexp.Regexp) error {
	// Reserve a free port and release it before spawning. The child binds
	// immediately after, so the listen/close race is accepted; a port stolen
	// in between surfaces as the child never becoming ready.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("find free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	args := make([]string, len(p.spec.Args))
	for i, a := range p.spec.Args {
		args[i] = strings.ReplaceAll(a, "{port}", strconv.Itoa(port))
	}
	cmd := exec.CommandContext(ctx, p.spec.Cmd, args...)
	cmd.Env = append(os.Environ(), p.spec.Env...)
	if p.spec.Dir != "" {
		cmd.Dir = p.spec.Dir
	}
	p.childURL.Store("http://127.0.0.1:" + strconv.Itoa(port))
	p.query.Store("")

	// Feed each stream through an io.Pipe: cmd.Wait waits for the
	// stdlib-internal copies into these writers to finish, and a writer only
	// completes once our scanner has consumed every line — so trailing output
	// printed just before a quick exit is never truncated.
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		stdoutW.Close()
		stderrW.Close()
		return fmt.Errorf("start: %w", err)
	}

	var captureOnce sync.Once
	p.scanStream(p.spec.Name, urlRe, &captureOnce, stdoutR)
	p.scanStream(p.spec.Name, urlRe, &captureOnce, stderrR)

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	go p.pollReady(runCtx)
	err = cmd.Wait()
	// The child is gone; close our pipe writers so the line scanners see EOF
	// and exit instead of blocking on Read forever (one pair per restart).
	stdoutW.Close()
	stderrW.Close()
	return err
}

// ChildURL returns "http://127.0.0.1:<port>" of the live child.
func (p *Process) ChildURL() string {
	if u, ok := p.childURL.Load().(string); ok {
		return u
	}
	return ""
}

// StartQuery returns the query string captured from stdout (e.g.
// "?token=abc"), or "".
func (p *Process) StartQuery() string {
	if q, ok := p.query.Load().(string); ok {
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
					p.query.Store("?" + u.RawQuery)
				}
			})
		}
	}()
}

// pollReady polls the health endpoint until any HTTP response (even
// 4xx/5xx) arrives, then marks the process ready. It gives up after
// readyTimeout and logs an error; the process stays up and the UI keeps
// showing "starting…". runCtx is cancelled when the child exits, so a stale
// poller never marks a restart's process ready.
func (p *Process) pollReady(runCtx context.Context) {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.After(readyTimeout)
	tick := time.NewTicker(healthPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-deadline:
			p.logger.Error("embed child never became ready", "name", p.spec.Name)
			return
		case <-tick.C:
			resp, err := client.Get(p.ChildURL() + p.spec.HealthPath)
			if err == nil {
				resp.Body.Close()
				if runCtx.Err() == nil {
					p.ready.Store(true)
				}
				return
			}
		}
	}
}
