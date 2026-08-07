package opencode

// OpenCode 2 (opencode2) keeps the same session model as v1 but exposes no
// `db`/`export`/`session`/`models` subcommands — everything goes through the
// background service's HTTP API. This file is the v2 store adapter: session
// list, transcript fetch, delete, and model metadata, all normalized onto
// the same shadow format as v1.
//
// The adapter talks HTTP directly rather than shelling `opencode2 api`: the
// beta CLI intermittently truncates large responses to a 4KiB boundary (and
// silently capped message pages at 50), while the service itself streams
// fine. The service registers at $XDG_STATE_HOME/opencode/service.json (or
// ~/.local/state) with its URL and Basic-auth password; a missing or dead
// service is (re)started via `opencode2 service start`.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// v2Service is the background service registration (service.json).
type v2Service struct {
	URL      string `json:"url"`
	Password string `json:"password"`
	PID      int    `json:"pid"`
}

func v2ServicePath() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "service.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "opencode", "service.json")
}

func v2ReadService() (*v2Service, error) {
	raw, err := os.ReadFile(v2ServicePath())
	if err != nil {
		return nil, err
	}
	var svc v2Service
	if json.Unmarshal(raw, &svc) != nil || svc.URL == "" {
		return nil, fmt.Errorf("bad opencode2 service registration")
	}
	return &svc, nil
}

// v2HTTPClient talks to loopback only; env proxies (a local Clash on 7890
// here) must be bypassed explicitly or large responses get mangled.
var v2HTTPClient = &http.Client{
	Transport: &http.Transport{Proxy: nil},
}

// v2API performs one service request, restarting a missing/dead service
// between attempts. method is GET or DELETE; query may be nil.
func v2API(ctx context.Context, cmd, method, path string, query url.Values) ([]byte, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		svc, err := v2ReadService()
		if err == nil {
			u := strings.TrimSuffix(svc.URL, "/") + path
			if len(query) > 0 {
				u += "?" + query.Encode()
			}
			req, rerr := http.NewRequestWithContext(ctx, method, u, nil)
			if rerr == nil {
				req.SetBasicAuth("opencode", svc.Password)
				if body, herr := v2Do(req); herr == nil {
					return body, nil
				} else {
					last = herr
				}
			} else {
				last = rerr
			}
		} else {
			last = err
		}
		// Service missing or unhealthy: (re)start it, then re-read the
		// registration (the port can change across restarts).
		startCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_ = exec.CommandContext(startCtx, cmd, "service", "start").Run()
		cancel()
	}
	return nil, last
}

func v2Do(req *http.Request) ([]byte, error) {
	resp, err := v2HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("opencode2 service: %s %s → %s", req.Method, req.URL.Path, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// apiGetJSON gets a v2 API path and decodes the payload into out, with
// retries covering transient service hiccups.
func apiGetJSON(ctx context.Context, cmd, path string, out any, params ...string) error {
	q := url.Values{}
	for _, p := range params {
		if k, v, ok := strings.Cut(p, "="); ok {
			q.Set(k, v)
		}
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 400 * time.Millisecond):
			}
		}
		raw, err := v2API(ctx, cmd, http.MethodGet, path, q)
		if err != nil {
			last = err
			continue
		}
		if err := json.Unmarshal(raw, out); err == nil {
			return nil
		} else {
			last = err
		}
	}
	return fmt.Errorf("opencode2 api: %w", last)
}

// V2GetJSON exposes the direct-HTTP v2 API client to sibling packages (the
// model catalog) that must not go through the truncation-prone beta CLI
// either. params are "k=v" query pairs.
func V2GetJSON(ctx context.Context, cmd, path string, out any, params ...string) error {
	return apiGetJSON(ctx, cmd, path, out, params...)
}

func apiDelete(ctx context.Context, cmd, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := v2API(ctx, cmd, http.MethodDelete, path, nil)
	return err
}

// v2Session is one entry of GET /api/session.
type v2Session struct {
	ID       string `json:"id"`
	ParentID string `json:"parentID"`
	Title    string `json:"title"`
	Time     struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

type v2Page struct {
	Cursor struct {
		Next string `json:"next"`
	} `json:"cursor"`
}

// v2ListSessions returns root sessions (subagent children excluded) across
// all projects, newest first, capped at 500 — mirroring the v1 SQL listing.
//
// The API's cursor.next is always set — even past the end, where it returns
// the same page again — so pagination stops when a page adds no new ids.
func v2ListSessions(ctx context.Context, cmd string) ([]sessionEntry, error) {
	var out []sessionEntry
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 10 && len(out) < 500; pages++ {
		params := []string{"limit=200"}
		if cursor != "" {
			params = append(params, "cursor="+cursor)
		}
		var page struct {
			Data []v2Session `json:"data"`
			v2Page
		}
		if err := apiGetJSON(ctx, cmd, "/api/session", &page, params...); err != nil {
			return nil, err
		}
		added := 0
		for _, s := range page.Data {
			if seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			if s.ID == "" || s.ParentID != "" || s.Location.Directory == "" {
				continue
			}
			added++
			out = append(out, sessionEntry{
				ID:        s.ID,
				Title:     s.Title,
				Directory: s.Location.Directory,
				Created:   s.Time.Created,
				Updated:   s.Time.Updated,
			})
		}
		if page.Cursor.Next == "" || len(page.Data) == 0 || added == 0 {
			break
		}
		cursor = page.Cursor.Next
	}
	return out, nil
}

// v2Message is one entry of GET /api/session/{id}/message. Only the fields
// the shadow converter needs are modeled; the other message kinds
// (agent-switched, compaction, …) fall through Type and are skipped.
type v2Message struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text"`
	Time struct {
		Created int64 `json:"created"`
	} `json:"time"`
	Model *struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
		Variant    string `json:"variant"`
	} `json:"model"`
	Content []v2Content `json:"content"`
	Tokens  *tokenUsage `json:"tokens"`
}

type v2Content struct {
	Type  string       `json:"type"` // text | reasoning | tool
	Text  string       `json:"text"`
	ID    string       `json:"id"`   // tool call id
	Name  string       `json:"name"` // tool name
	State *v2ToolState `json:"state"`
}

type v2ToolState struct {
	Status string          `json:"status"` // streaming | running | completed | error
	Input  json.RawMessage `json:"input"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	Content []struct {
		Type string `json:"type"` // text | file
		Text string `json:"text"`
		URI  string `json:"uri"`
	} `json:"content"`
}

// modelRef renders a v2 model reference the way `--model` accepts it:
// provider/model, with a non-default variant appended as #variant.
func v2ModelRef(providerID, modelID, variant string) string {
	if modelID == "" {
		return ""
	}
	ref := modelID
	if providerID != "" {
		ref = providerID + "/" + modelID
	}
	if variant != "" && variant != "default" {
		ref += "#" + variant
	}
	return ref
}

// v2SessionModel is the v2 store lookup for Runtime.sessionModel: the
// session's own model ref from GET /api/session/{id}.
func v2SessionModel(ctx context.Context, cmd, id string) string {
	var s struct {
		Model *struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
			Variant    string `json:"variant"`
		} `json:"model"`
	}
	if apiGetJSON(ctx, cmd, "/api/session/"+id, &s) != nil || s.Model == nil {
		return ""
	}
	return v2ModelRef(s.Model.ProviderID, s.Model.ID, s.Model.Variant)
}

// v2ContextWindows maps provider/model → context limit from GET /api/model.
func v2ContextWindows(ctx context.Context, cmd string) (map[string]int64, error) {
	var list struct {
		Data []struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
			Limit      struct {
				Context int64 `json:"context"`
			} `json:"limit"`
		} `json:"data"`
	}
	if err := apiGetJSON(ctx, cmd, "/api/model", &list); err != nil {
		return nil, err
	}
	m := map[string]int64{}
	for _, e := range list.Data {
		if e.Limit.Context > 0 {
			m[e.ProviderID+"/"+e.ID] = e.Limit.Context
		}
	}
	return m, nil
}

// v2ListMessages pages through a session's full message timeline and returns
// it oldest-first. Pages arrive newest-first (the API's fixed order); the
// cursor never empties (past the end it replays the same page), so paging
// stops when a page adds no new ids.
func v2ListMessages(ctx context.Context, cmd, sessionID string) ([]v2Message, error) {
	var out []v2Message
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 500; pages++ {
		params := []string{"limit=200"}
		if cursor != "" {
			params = append(params, "cursor="+cursor)
		}
		var page struct {
			Data []v2Message `json:"data"`
			v2Page
		}
		if err := apiGetJSON(ctx, cmd, "/api/session/"+sessionID+"/message", &page, params...); err != nil {
			return nil, err
		}
		added := 0
		for _, m := range page.Data {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			added++
			out = append(out, m)
		}
		if page.Cursor.Next == "" || len(page.Data) == 0 || added == 0 {
			break
		}
		cursor = page.Cursor.Next
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Time.Created < out[j].Time.Created
	})
	return out, nil
}

// v2ToolOutput flattens a completed tool state's content blocks into the
// single output string the shadow format expects.
func v2ToolOutput(st *v2ToolState) string {
	var parts []string
	for _, c := range st.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		case "file":
			if c.URI != "" {
				parts = append(parts, c.URI)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// v2FetchSession mirrors one v2 session's transcript into its shadow jsonl,
// the v1 fetchSession counterpart fed by the HTTP API instead of SQL.
func v2FetchSession(ctx context.Context, rt *Runtime, s sessionEntry, path string) error {
	if !sessionIDPattern.MatchString(s.ID) {
		return fmt.Errorf("refusing unexpected session id %q", s.ID)
	}
	messages, err := v2ListMessages(ctx, rt.Cmd(), s.ID)
	if err != nil {
		return err
	}

	var buf []byte
	write := func(raw json.RawMessage) {
		if raw == nil {
			return
		}
		buf = append(buf, raw...)
		buf = append(buf, '\n')
	}
	sid := s.ID
	cwd := s.Directory
	// v2's placeholder is "New session - <timestamp>"; the real title lands
	// on a later sync once generated.
	if s.Title != "" && !strings.HasPrefix(s.Title, "New session") {
		write(mustMarshal(map[string]any{
			"type":      "ai-title",
			"aiTitle":   s.Title,
			"sessionId": sid,
			"timestamp": eventTime(s.Created),
		}))
	}
	for _, m := range messages {
		ts := eventTime(m.Time.Created)
		switch m.Type {
		case "user":
			if strings.TrimSpace(m.Text) == "" {
				continue
			}
			write(userLineWithUUID(sid, cwd, m.Text, ts, m.ID))
		case "assistant":
			em := exportMessage{}
			em.Info.ID = m.ID
			em.Info.Role = "assistant"
			em.Info.Tokens = m.Tokens
			em.Info.Time.Created = m.Time.Created
			if m.Model != nil {
				em.Info.ModelID = v2ModelRef(m.Model.ProviderID, m.Model.ID, m.Model.Variant)
			}
			window := rt.modelWindow(em)
			for _, c := range m.Content {
				switch c.Type {
				case "text":
					write(assistantLineModel(sid, textBlocks(c.Text), ts, em, window))
				case "reasoning":
					write(assistantLineModel(sid, thinkingBlocks(c.Text), ts, em, window))
				case "tool":
					if c.State == nil || (c.State.Status != "completed" && c.State.Status != "error") {
						continue
					}
					st := &toolState{
						Status: c.State.Status,
						Input:  c.State.Input,
						Output: v2ToolOutput(c.State),
					}
					if c.State.Error != nil {
						st.Error = c.State.Error.Message
					}
					pp := partPayload{ID: c.ID, Tool: c.Name, CallID: c.ID, State: st}
					write(assistantLineModel(sid, toolUseBlocks(pp), ts, em, window))
					write(toolResultLine(sid, cwd, pp, ts))
				}
			}
			write(turnCompleteLine(sid, ts))
		}
	}
	write(syncMetaLine(s))
	if len(buf) == 0 {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
