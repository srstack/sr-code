// Package modelcatalog implements model metadata sources for built-in agent
// backends. Catalogs are account-level capabilities, independent of workers.
package modelcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/opencode"
)

// catalogTTL is effectively "once per process": providers and keys change
// rarely, and main.go pre-warms every catalog in the background at startup,
// so picker requests should never wait on an agent CLI probe. Restart to
// refresh.
const catalogTTL = 24 * time.Hour

// Claude resolves its model list dynamically from the Anthropic-compatible
// /v1/models endpoint when ANTHROPIC_BASE_URL / ANTHROPIC_API_KEY are set
// (gateways like right.codes expose exactly what the account can run), and
// falls back to the stock aliases otherwise. Results are cached briefly.
type Claude struct {
	mu       sync.Mutex
	cached   []backend.Model
	cachedAt time.Time
}

var claudeStaticModels = []backend.Model{
	{ID: "opus", DisplayName: "Opus"},
	{ID: "sonnet", DisplayName: "Sonnet"},
	{ID: "haiku", DisplayName: "Haiku"},
	{ID: "fable", DisplayName: "Fable"},
	{ID: "opusplan", DisplayName: "Opus Plan"},
}

func (c *Claude) Models(ctx context.Context) ([]backend.Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && time.Since(c.cachedAt) < catalogTTL {
		return c.cached, nil
	}
	if models := claudeRemoteModels(ctx); len(models) > 0 {
		// The gateway's exact ids are the only trustworthy list — the stock
		// aliases (opus/sonnet/…) may not exist on a custom gateway at all.
		c.cached = models
		c.cachedAt = time.Now()
		return models, nil
	}
	return claudeStaticModels, nil
}

// claudeRemoteModels queries the Anthropic-compatible model list. Best-effort:
// any failure (no key, no gateway, network) just yields nil.
func claudeRemoteModels(ctx context.Context) []backend.Model {
	base := strings.TrimSuffix(os.Getenv("ANTHROPIC_BASE_URL"), "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&doc) != nil {
		return nil
	}
	var out []backend.Model
	for _, m := range doc.Data {
		if m.ID != "" {
			out = append(out, backend.Model{ID: m.ID, DisplayName: m.ID})
		}
	}
	return out
}

func (Claude) DefaultEffort(context.Context, string) (string, error) { return "", nil }

// ValidateModel accepts the stock aliases, any claude-* id, and (permissively)
// whatever a gateway might offer — a wrong id fails at spawn with a visible
// error in the session rather than blocking creation here.
func (c Claude) ValidateModel(ctx context.Context, model string) error {
	for _, candidate := range claudeStaticModels {
		if candidate.ID == model {
			return nil
		}
	}
	if strings.HasPrefix(model, "claude-") {
		return nil
	}
	return fmt.Errorf("unknown Claude model %q", model)
}

type Codex struct {
	Path       string // models_cache.json (account catalog, may not exist)
	ConfigPath string // config.toml (configured default model + provider)

	mu       sync.Mutex
	cached   []backend.Model
	cachedAt time.Time
}

// codexConfiguredModel scans config.toml for the top-level `model = "…"`
// line (before the first [section]) without pulling in a TOML parser.
func codexConfiguredModel(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			break // past the top-level table
		}
		if !strings.HasPrefix(line, "model") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) != "model" {
			continue
		}
		v := strings.TrimSpace(kv[1])
		v = strings.Trim(v, `"'`)
		return v
	}
	return ""
}

func codexFallbackModels() []backend.Model {
	return []backend.Model{
		{ID: "gpt-5.5", DisplayName: "GPT-5.5"},
		{ID: "gpt-5.4-mini", DisplayName: "GPT-5.4 Mini"},
	}
}

type codexDocument struct {
	Models []struct {
		Slug                  string `json:"slug"`
		DisplayName           string `json:"display_name"`
		Visibility            string `json:"visibility"`
		Priority              int    `json:"priority"`
		DefaultReasoningLevel string `json:"default_reasoning_level"`
	} `json:"models"`
}

func (c Codex) read() (codexDocument, error) {
	var doc codexDocument
	raw, err := os.ReadFile(c.Path)
	if err != nil {
		return doc, err
	}
	err = json.Unmarshal(raw, &doc)
	return doc, err
}

// codexProviderConfig extracts the active provider's base_url and bearer
// token from config.toml: the top-level model_provider names the section,
// [model_providers.<name>] carries base_url and experimental_bearer_token.
// Scan-based (no TOML dep); malformed files just yield empty strings.
func codexProviderConfig(path string) (baseURL, token string) {
	if path == "" {
		return "", ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var provider, section string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = strings.Trim(line, "[] ")
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		switch {
		case section == "" && key == "model_provider":
			provider = val
		case section == "model_providers."+provider && provider != "":
			switch key {
			case "base_url":
				baseURL = val
			case "experimental_bearer_token", "bearer_token", "api_key":
				token = val
			}
		}
	}
	return strings.TrimSuffix(baseURL, "/"), token
}

// codexRemoteModels queries the provider's OpenAI-compatible /models list.
// Best-effort: any failure yields nil.
func codexRemoteModels(ctx context.Context, baseURL, token string) []backend.Model {
	if baseURL == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/models", nil)
	if err != nil {
		return nil
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&doc) != nil {
		return nil
	}
	var out []backend.Model
	for _, m := range doc.Data {
		if m.ID != "" {
			out = append(out, backend.Model{ID: m.ID, DisplayName: m.ID})
		}
	}
	return out
}

func (c *Codex) Models(ctx context.Context) ([]backend.Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && time.Since(c.cachedAt) < catalogTTL {
		return c.cached, nil
	}
	var out []backend.Model
	seen := map[string]bool{}
	// The configured default model first — with custom providers it is often
	// the only id the account can actually run. It is a plain catalog entry;
	// the UI uses the first entry to annotate its Default row.
	if cfg := codexConfiguredModel(c.ConfigPath); cfg != "" {
		out = append(out, backend.Model{ID: cfg, DisplayName: cfg})
		seen[cfg] = true
	}
	// The provider's live catalog (OpenAI-compatible /models).
	baseURL, token := codexProviderConfig(c.ConfigPath)
	for _, m := range codexRemoteModels(ctx, baseURL, token) {
		if seen[m.ID] {
			continue
		}
		out = append(out, m)
		seen[m.ID] = true
	}
	// The account cache (official Codex CLI flow) when no custom provider
	// answered.
	if len(out) <= 1 {
		doc, err := c.read()
		if err == nil && len(doc.Models) > 0 {
			picks := doc.Models[:0:0]
			for _, m := range doc.Models {
				if m.Visibility == "list" && m.Slug != "" {
					picks = append(picks, m)
				}
			}
			sort.SliceStable(picks, func(i, j int) bool { return picks[i].Priority < picks[j].Priority })
			for _, m := range picks {
				if seen[m.Slug] {
					continue
				}
				label := m.DisplayName
				if label == "" {
					label = m.Slug
				}
				out = append(out, backend.Model{ID: m.Slug, DisplayName: label})
				seen[m.Slug] = true
			}
		}
	}
	if len(out) == 0 {
		return codexFallbackModels(), nil
	}
	c.cached = out
	c.cachedAt = time.Now()
	return out, nil
}

// DefaultModel is the CLI-configured default (the catalog's leading entry).
func (c *Codex) DefaultModel(context.Context) (string, error) {
	return codexConfiguredModel(c.ConfigPath), nil
}

// ValidateModel is permissive on purpose: with custom model_providers the
// account cache cannot enumerate what the provider accepts, so a wrong guess
// surfaces as a spawn-time error in the session instead of a creation-time
// refusal here.
func (c Codex) ValidateModel(context.Context, string) error { return nil }

func (c Codex) DefaultEffort(_ context.Context, model string) (string, error) {
	if model == "" {
		return "", nil
	}
	doc, err := c.read()
	if err != nil {
		return "", err
	}
	for _, m := range doc.Models {
		if m.Slug == model {
			return m.DefaultReasoningLevel, nil
		}
	}
	return "", nil
}

// OpenCode lists the models `opencode models` reports (provider/model ids,
// exactly what `opencode run --model` accepts). The list is cached for five
// minutes — it changes only when providers/keys change.
//
// V2 selects the OpenCode 2 flavor: no `models` subcommand exists, so the
// catalog comes from `opencode2 api /api/model` instead.
type OpenCode struct {
	Cmd string
	V2  bool

	mu       sync.Mutex
	cached   []backend.Model
	cachedAt time.Time
}

func (c *OpenCode) Models(ctx context.Context) ([]backend.Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && time.Since(c.cachedAt) < catalogTTL {
		return c.cached, nil
	}
	cmd := c.Cmd
	if cmd == "" {
		cmd = "opencode"
	}
	if c.V2 {
		if models, err := c.modelsV2(ctx, cmd); err == nil {
			c.cached = models
			c.cachedAt = time.Now()
		}
		return c.cached, nil
	}
	out, err := exec.CommandContext(ctx, cmd, "models").Output()
	if err != nil {
		return c.cached, nil // fall back to whatever we had (nil = Default only)
	}
	var models []backend.Model
	for _, line := range strings.Split(string(out), "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		models = append(models, backend.Model{ID: id, DisplayName: id})
	}
	c.cached = models
	c.cachedAt = time.Now()
	return models, nil
}

// modelsV2 reads the catalog from GET /api/model: provider/model ids plus the
// human name, restricted to enabled models whose provider is configured
// (GET /api/provider) — i.e. only providers the user actually connected, the
// same scope `opencode models` reports for v1. When the provider list is
// unavailable the raw catalog is used rather than showing nothing.
func (c *OpenCode) modelsV2(ctx context.Context, cmd string) ([]backend.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var list struct {
		Data []struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
			Name       string `json:"name"`
			Enabled    bool   `json:"enabled"`
			Status     string `json:"status"`
			Variants   []struct {
				ID string `json:"id"`
			} `json:"variants"`
		} `json:"data"`
	}
	if err := opencode2APIJSON(ctx, cmd, "/api/model", &list); err != nil {
		return nil, err
	}
	configured := v2ConfiguredProviders(ctx, cmd)
	var models []backend.Model
	for _, m := range list.Data {
		if m.ID == "" || m.ProviderID == "" || !m.Enabled || m.Status == "deprecated" {
			continue
		}
		if configured != nil && !configured[m.ProviderID] {
			continue
		}
		id := m.ProviderID + "/" + m.ID
		name := m.Name
		if name == "" {
			name = id
		}
		model := backend.Model{ID: id, DisplayName: name}
		// Reasoning-effort variants ride the model id as provider/model#variant
		// (what `opencode2 run --model` accepts); "default" is not a choice.
		for _, v := range m.Variants {
			if v.ID != "" && v.ID != "default" {
				model.ThinkingLevels = append(model.ThinkingLevels, v.ID)
			}
		}
		models = append(models, model)
	}
	return models, nil
}

// opencode2APIJSON gets a v2 API path and decodes it. It delegates to the
// opencode package's direct-HTTP client: the beta `opencode2 api` CLI
// truncates large responses to a 4KiB boundary.
func opencode2APIJSON(ctx context.Context, cmd, path string, out any) error {
	return opencode.V2GetJSON(ctx, cmd, path, out)
}

// v2ConfiguredProviders returns the ids from GET /api/provider — the
// providers v2 resolved from credentials and config. nil on any failure so
// the caller falls back to the unfiltered catalog.
func v2ConfiguredProviders(ctx context.Context, cmd string) map[string]bool {
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := opencode2APIJSON(ctx, cmd, "/api/provider", &list); err != nil || len(list.Data) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, p := range list.Data {
		if p.ID != "" {
			set[p.ID] = true
		}
	}
	return set
}

func (c *OpenCode) ValidateModel(ctx context.Context, model string) error {
	// A reasoning-effort variant rides the id as provider/model#variant;
	// validate the base id, the variant is the CLI's to reject.
	base, _, _ := strings.Cut(model, "#")
	models, _ := c.Models(ctx)
	if len(models) == 0 {
		return nil // catalog unavailable: let opencode validate at spawn
	}
	for _, m := range models {
		if m.ID == base {
			return nil
		}
	}
	return fmt.Errorf("unknown OpenCode model %q", model)
}

func (OpenCode) DefaultEffort(context.Context, string) (string, error) { return "", nil }

// DefaultModel reports the CLI's own default model (v2: /api/model/default),
// so the pickers can name the default instead of showing a bare "Default".
// "" for v1, which has no such query.
func (c *OpenCode) DefaultModel(ctx context.Context) (string, error) {
	if !c.V2 {
		return "", nil
	}
	cmd := c.Cmd
	if cmd == "" {
		cmd = "opencode2"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var doc struct {
		Data struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
		} `json:"data"`
	}
	if err := opencode2APIJSON(ctx, cmd, "/api/model/default", &doc); err != nil || doc.Data.ID == "" {
		return "", nil // unknown: fall back to the plain Default label
	}
	return doc.Data.ProviderID + "/" + doc.Data.ID, nil
}
