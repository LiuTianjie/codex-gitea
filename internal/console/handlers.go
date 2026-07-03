package console

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/turning4th/codex-gitea/internal/config"
	"github.com/turning4th/codex-gitea/internal/model"
	_ "modernc.org/sqlite"
)

// consoleDist embeds the Vite-built React console so the service remains a
// single self-contained binary at runtime.
//
//go:embed all:static/dist
var consoleDist embed.FS

// StatusFunc reports codex auth status. The orchestrator injects
// codex.Runner.Status; tests inject a stub. A nil StatusFunc means "not wired".
type StatusFunc func(context.Context) (string, error)

type StatusProvider func() map[string]StatusFunc

type ConfigProvider func() *config.Config

type SettingsApplyFunc func(context.Context, map[string]string) error

type SkillGeneratorFunc func(context.Context, model.SkillGenerationInput) (string, error)

type ChatProbeInput struct {
	Reviewer        string `json:"reviewer"`
	Prompt          string `json:"prompt"`
	Model           string `json:"model"`
	BaseURL         string `json:"base_url"`
	ProviderID      string `json:"provider_id"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type ChatProbeFunc func(context.Context, ChatProbeInput) (string, error)

// Console serves the authenticated admin panel and its JSON API.
type Console struct {
	store           model.Store
	cfg             *config.Config
	configProvider  ConfigProvider
	codexHome       string
	statusFns       map[string]StatusFunc
	statusProvider  StatusProvider
	onSettingsApply SettingsApplyFunc
	skillGenerator  SkillGeneratorFunc
	chatProbe       ChatProbeFunc
	skillTasksMu    sync.Mutex
	skillTasks      map[string]*skillGenerationTask
}

// New builds a Console. codexHome is the directory where auth.json uploads are written.
func New(store model.Store, cfg *config.Config, codexHome string, statusArgs ...any) *Console {
	c := &Console{store: store, cfg: cfg, codexHome: codexHome, statusFns: map[string]StatusFunc{}, skillTasks: map[string]*skillGenerationTask{}}
	for _, arg := range statusArgs {
		switch v := arg.(type) {
		case StatusFunc:
			c.statusFns["codex"] = v
		case map[string]StatusFunc:
			for name, fn := range v {
				c.statusFns[name] = fn
			}
		case StatusProvider:
			c.statusProvider = v
		case ConfigProvider:
			c.configProvider = v
		case SettingsApplyFunc:
			c.onSettingsApply = v
		case SkillGeneratorFunc:
			c.skillGenerator = v
		case ChatProbeFunc:
			c.chatProbe = v
		}
	}
	return c
}

func (c *Console) currentConfig() *config.Config {
	if c.configProvider != nil {
		return c.configProvider()
	}
	if c.cfg != nil {
		return c.cfg.Clone()
	}
	return nil
}

func (c *Console) currentStatusFns() map[string]StatusFunc {
	if c.statusProvider != nil {
		return c.statusProvider()
	}
	out := make(map[string]StatusFunc, len(c.statusFns))
	for name, fn := range c.statusFns {
		out[name] = fn
	}
	return out
}

// Routes returns the console handler tree wrapped in Basic Auth. Mount it under
// whatever prefix the caller chooses; the internal mux uses absolute /admin/...
// paths so the page's fetch() calls resolve correctly.
func (c *Console) Routes() http.Handler {
	mux := http.NewServeMux()
	if assets, err := fs.Sub(consoleDist, "static/dist"); err == nil {
		mux.Handle("GET /admin/assets/", http.StripPrefix("/admin/", http.FileServer(http.FS(assets))))
	}
	mux.HandleFunc("GET /admin/", c.handleIndex)
	mux.HandleFunc("GET /admin/api/settings", c.handleGetSettings)
	mux.HandleFunc("POST /admin/api/settings", c.handlePostSettings)
	mux.HandleFunc("POST /admin/api/authfile", c.handlePostAuthFile)
	mux.HandleFunc("GET /admin/api/status", c.handleStatus)
	mux.HandleFunc("GET /admin/api/effective-config", c.handleEffectiveConfig)
	mux.HandleFunc("POST /admin/api/chat-probe", c.handleChatProbe)
	mux.HandleFunc("GET /admin/api/cc-switch/codex-options", c.handleCCSwitchCodexOptions)
	mux.HandleFunc("POST /admin/api/cc-switch/codex-options/fetch", c.handleFetchCCSwitchCodexModels)
	mux.HandleFunc("GET /admin/api/jobs", c.handleJobs)
	mux.HandleFunc("GET /admin/api/jobs/stats", c.handleJobStats)
	mux.HandleFunc("GET /admin/api/jobs/{id}", c.handleJobDetail)
	mux.HandleFunc("POST /admin/api/jobs/{id}/rerun", c.handleJobRerun)
	mux.HandleFunc("POST /admin/api/jobs/{id}/cancel", c.handleJobCancel)
	mux.HandleFunc("POST /admin/api/analytics/reports", c.handleCreateAnalysisReport)
	mux.HandleFunc("GET /admin/api/analytics/reports/latest", c.handleLatestAnalysisReport)
	mux.HandleFunc("GET /admin/api/analytics/reports", c.handleListAnalysisReports)
	mux.HandleFunc("GET /admin/api/analytics/trend", c.handleAnalysisTrend)
	mux.HandleFunc("GET /admin/api/skills/projects", c.handleSkillProjects)
	mux.HandleFunc("GET /admin/api/skills/{owner}/{repo}", c.handleSkillDetail)
	mux.HandleFunc("POST /admin/api/skills/{owner}/{repo}/generate", c.handleSkillGenerate)
	mux.HandleFunc("GET /admin/api/skills/{owner}/{repo}/generate/{taskID}", c.handleSkillGenerateStatus)

	return consoleAuthMiddleware(func() string {
		if cfg := c.currentConfig(); cfg != nil {
			return cfg.AdminPassword
		}
		return ""
	}, mux)
}

func (c *Console) PublicRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skills/{owner}/{repo}/SKILL.md", c.handlePublicSkillDownload)
	return mux
}

// ---------- handlers ----------

func (c *Console) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data, err := consoleDist.ReadFile("static/dist/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "console assets are not built")
		return
	}
	w.Write(data)
}

// handleGetSettings returns all settings with secret values redacted. Secret
// keys never leak their value to the browser; the UI only learns whether a key
// is set.
func (c *Console) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := c.store.AllSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make(map[string]string, len(all))
	for k, v := range all {
		if isSecretKey(k) {
			if strings.TrimSpace(v) != "" {
				out[k] = redacted
			} else {
				out[k] = ""
			}
			continue
		}
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// settingsPayload accepts either a single {key,value} pair or a {settings:{...}}
// batch. Both forms may be present; both are applied.
type settingsPayload struct {
	Key      string            `json:"key"`
	Value    string            `json:"value"`
	Settings map[string]string `json:"settings"`
}

func (c *Console) handlePostSettings(w http.ResponseWriter, r *http.Request) {
	var p settingsPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	updates := map[string]string{}
	for k, v := range p.Settings {
		updates[k] = v
	}
	if p.Key != "" {
		updates[p.Key] = p.Value
	}
	if len(updates) == 0 {
		writeError(w, http.StatusBadRequest, "no settings provided")
		return
	}

	applied := make(map[string]string, len(updates))
	for k, v := range updates {
		// Never store the redaction placeholder: it means "leave the existing
		// secret unchanged", so the UI can re-submit a form without wiping secrets.
		if isSecretKey(k) && v == redacted {
			continue
		}
		if err := c.store.SetSetting(r.Context(), k, v, isSecretKey(k)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		applied[k] = v
	}
	if c.onSettingsApply != nil && len(applied) > 0 {
		if err := c.onSettingsApply(r.Context(), applied); err != nil {
			writeError(w, http.StatusInternalServerError, "settings saved but reload failed: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": len(applied)})
}

// authFile is the minimal shape we validate before persisting auth.json. The
// presence of tokens.refresh_token is what authfile-mode codex login requires.
type authFile struct {
	Tokens struct {
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
}

func (c *Console) handlePostAuthFile(w http.ResponseWriter, r *http.Request) {
	raw, err := readAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	// Must be valid JSON.
	var af authFile
	if err := json.Unmarshal(raw, &af); err != nil {
		writeError(w, http.StatusBadRequest, "auth.json is not valid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(af.Tokens.RefreshToken) == "" {
		writeError(w, http.StatusBadRequest, "auth.json missing tokens.refresh_token")
		return
	}

	codexHome := c.codexHome
	if cfg := c.currentConfig(); cfg != nil && strings.TrimSpace(cfg.CodexHome) != "" {
		codexHome = cfg.CodexHome
	}
	if codexHome == "" {
		writeError(w, http.StatusInternalServerError, "codex home not configured")
		return
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "create codex home: "+err.Error())
		return
	}
	dest := filepath.Join(codexHome, "auth.json")
	if err := os.WriteFile(dest, raw, 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, "write auth.json: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": dest})
}

func (c *Console) handleStatus(w http.ResponseWriter, r *http.Request) {
	statusFns := c.currentStatusFns()
	if len(statusFns) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"status": "status check not configured",
		})
		return
	}
	statuses := map[string]any{}
	allOK := true
	var lines []string
	for name, fn := range statusFns {
		status, err := fn(r.Context())
		ok := err == nil
		if !ok {
			allOK = false
			if strings.TrimSpace(status) == "" {
				status = err.Error()
			} else {
				status = strings.TrimSpace(status) + "\n\nerror: " + err.Error()
			}
		}
		statuses[name] = map[string]any{"ok": ok, "status": status}
		lines = append(lines, name+": "+status)
	}
	statusText := strings.Join(lines, "\n\n")
	if len(statusFns) == 1 {
		for _, v := range statuses {
			if item, ok := v.(map[string]any); ok {
				statusText, _ = item["status"].(string)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": allOK, "status": statusText, "reviewers": statuses})
}

func (c *Console) handleEffectiveConfig(w http.ResponseWriter, r *http.Request) {
	cfg := c.currentConfig()
	if cfg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "config not available"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                          true,
		"gitea_url":                   cfg.GiteaURL,
		"gitea_token_set":             strings.TrimSpace(cfg.GiteaToken) != "",
		"webhook_secret_set":          strings.TrimSpace(cfg.WebhookSecret) != "",
		"gitea_timeout":               cfg.GiteaTimeout.String(),
		"model":                       cfg.Model,
		"codex_reasoning_effort":      cfg.CodexReasoningEffort,
		"codex_base_url":              cfg.CodexBaseURL,
		"codex_auth_mode":             cfg.CodexAuthMode,
		"codex_home_effective":        effectiveCodexHome(cfg),
		"codex_ccswitch_home":         effectiveCCSwitchHome(cfg),
		"codex_api_key_set":           strings.TrimSpace(cfg.CodexAPIKey) != "",
		"codex_api_key_fingerprint":   secretFingerprint(cfg.CodexAPIKey),
		"codex_cc_switch_provider_id": cfg.CodexCCSwitchProvider,
		"codex_sandbox_mode":          cfg.CodexSandbox,
		"claude_enabled":              cfg.ClaudeEnabled,
		"claude_model":                cfg.ClaudeModel,
		"claude_api_key_set":          strings.TrimSpace(cfg.ClaudeAPIKey) != "",
		"claude_base_url":             cfg.ClaudeBaseURL,
		"claude_home":                 cfg.ClaudeHome,
		"cc_switch_config_dir":        cfg.CCSwitchConfigDir,
		"cc_switch_provider_id":       cfg.CCSwitchProvider,
		"claude_max_budget_usd":       cfg.ClaudeMaxBudgetUSD,
		"minimax_enabled":             cfg.MiniMaxEnabled,
		"minimax_model":               cfg.MiniMaxModel,
		"minimax_provider_id":         cfg.MiniMaxProvider,
		"minimax_api_key_set":         strings.TrimSpace(cfg.MiniMaxAPIKey) != "",
		"minimax_base_url":            cfg.MiniMaxBaseURL,
		"minimax_max_budget_usd":      cfg.MiniMaxMaxBudgetUSD,
		"concurrency":                 cfg.Concurrency,
		"trigger_keywords":            strings.Join(cfg.TriggerKeywords, ","),
		"repo_allowlist":              strings.Join(cfg.RepoAllowlist, ","),
		"config_source":               os.Getenv("CONFIG_SOURCE"),
		"runtime_reload_note":         "保存设置会写入数据库；后续 review 会热生效 Gitea token/timeout、Webhook 密钥、触发关键词、仓库白名单和 reviewer 配置；监听地址和 worker 并发仍需重启服务。",
	})
}

func effectiveCodexHome(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if cfg.CodexAuthMode == config.AuthModeCCSwitch && strings.TrimSpace(cfg.CodexHome) != "" {
		return filepath.Join(cfg.CodexHome, "ccswitch-runtime")
	}
	return cfg.CodexHome
}

func effectiveCCSwitchHome(cfg *config.Config) string {
	if cfg == nil || cfg.CodexAuthMode != config.AuthModeCCSwitch || strings.TrimSpace(cfg.CodexHome) == "" {
		return ""
	}
	return filepath.Join(cfg.CodexHome, "ccswitch-home")
}

func secretFingerprint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= 8 {
		return fmt.Sprintf("len=%d", len(runes))
	}
	return fmt.Sprintf("len=%d %s...%s", len(runes), string(runes[:4]), string(runes[len(runes)-4:]))
}

func normalizeOpenAIBaseURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1"
		return strings.TrimRight(u.String(), "/")
	}
	return raw
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (c *Console) handleChatProbe(w http.ResponseWriter, r *http.Request) {
	if c.chatProbe == nil {
		writeError(w, http.StatusServiceUnavailable, "chat probe is not configured")
		return
	}
	var in ChatProbeInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	in.Reviewer = strings.ToLower(strings.TrimSpace(in.Reviewer))
	if in.Reviewer == "" {
		in.Reviewer = "codex"
	}
	switch in.Reviewer {
	case "codex", "claude", "minimax":
	default:
		writeError(w, http.StatusBadRequest, "unsupported reviewer: "+in.Reviewer)
		return
	}
	in.Prompt = strings.TrimSpace(in.Prompt)
	if in.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	c.resolveChatProbeProvider(r.Context(), &in)
	start := time.Now()
	out, err := c.chatProbe(r.Context(), in)
	elapsed := time.Since(start)
	debug := c.chatProbeDebug(in)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"reviewer":   in.Reviewer,
			"error":      err.Error(),
			"elapsed_ms": elapsed.Milliseconds(),
			"debug":      debug,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"reviewer":   in.Reviewer,
		"output":     out,
		"elapsed_ms": elapsed.Milliseconds(),
		"debug":      debug,
	})
}

func (c *Console) resolveChatProbeProvider(ctx context.Context, in *ChatProbeInput) {
	if in == nil || in.Reviewer != "codex" || strings.TrimSpace(in.ProviderID) != "" {
		return
	}
	cfg := c.currentConfig()
	if cfg == nil || cfg.CodexAuthMode != config.AuthModeCCSwitch {
		return
	}
	targetBaseURL := normalizeOpenAIBaseURL(firstNonEmpty(in.BaseURL, cfg.CodexBaseURL))
	if targetBaseURL == "" {
		return
	}
	options := c.buildCCSwitchCodexOptions(ctx)
	for _, provider := range options.Providers {
		if normalizeOpenAIBaseURL(provider.BaseURL) == targetBaseURL {
			in.ProviderID = provider.ID
			return
		}
	}
}

func (c *Console) chatProbeDebug(in ChatProbeInput) map[string]any {
	cfg := c.currentConfig()
	debug := map[string]any{
		"base_url":            strings.TrimSpace(in.BaseURL),
		"provider_id":         strings.TrimSpace(in.ProviderID),
		"model":               strings.TrimSpace(in.Model),
		"reasoning_effort":    strings.TrimSpace(in.ReasoningEffort),
		"configured_base_url": "",
		"auth_route":          in.Reviewer,
		"api_key_set":         false,
		"api_key_fingerprint": "",
	}
	if cfg == nil {
		return debug
	}
	switch in.Reviewer {
	case "codex":
		debug["configured_base_url"] = cfg.CodexBaseURL
		debug["auth_route"] = cfg.CodexAuthMode
		debug["api_key_set"] = strings.TrimSpace(cfg.CodexAPIKey) != ""
		debug["api_key_fingerprint"] = secretFingerprint(cfg.CodexAPIKey)
		if cfg.CodexAuthMode == config.AuthModeCCSwitch {
			debug["direct_base_url_used"] = false
		} else if debug["base_url"] == "" {
			debug["base_url"] = cfg.CodexBaseURL
		}
	case "claude":
		debug["configured_base_url"] = cfg.ClaudeBaseURL
		debug["api_key_set"] = strings.TrimSpace(cfg.ClaudeAPIKey) != ""
		debug["api_key_fingerprint"] = secretFingerprint(cfg.ClaudeAPIKey)
		if debug["base_url"] == "" {
			debug["base_url"] = cfg.ClaudeBaseURL
		}
	case "minimax":
		debug["configured_base_url"] = cfg.MiniMaxBaseURL
		debug["api_key_set"] = strings.TrimSpace(cfg.MiniMaxAPIKey) != ""
		debug["api_key_fingerprint"] = secretFingerprint(cfg.MiniMaxAPIKey)
		if debug["base_url"] == "" {
			debug["base_url"] = cfg.MiniMaxBaseURL
		}
	}
	return debug
}

type ccSwitchCodexProviderView struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Current         bool   `json:"current"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	BaseURL         string `json:"base_url,omitempty"`
}

type ccSwitchModelView struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
}

type ccSwitchCodexOptionsResponse struct {
	OK               bool                        `json:"ok"`
	Path             string                      `json:"path"`
	Providers        []ccSwitchCodexProviderView `json:"providers"`
	Models           []ccSwitchModelView         `json:"models"`
	ReasoningEfforts []string                    `json:"reasoning_efforts"`
	Error            string                      `json:"error,omitempty"`
}

func (c *Console) handleCCSwitchCodexOptions(w http.ResponseWriter, r *http.Request) {
	resp := c.buildCCSwitchCodexOptions(r.Context())
	writeJSON(w, http.StatusOK, resp)
}

type ccSwitchFetchModelsPayload struct {
	ProviderID string `json:"provider_id"`
	BaseURL    string `json:"base_url"`
}

func (c *Console) handleFetchCCSwitchCodexModels(w http.ResponseWriter, r *http.Request) {
	var p ccSwitchFetchModelsPayload
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p)
	}

	cfg := c.currentConfig()
	ccDir := config.DefaultCCSwitchDir
	if cfg != nil && strings.TrimSpace(cfg.CCSwitchConfigDir) != "" {
		ccDir = strings.TrimSpace(cfg.CCSwitchConfigDir)
	}
	options := c.buildCCSwitchCodexOptions(r.Context())
	providerID := strings.TrimSpace(p.ProviderID)
	if providerID == "" && cfg != nil {
		providerID = strings.TrimSpace(cfg.CodexCCSwitchProvider)
	}
	if providerID == "" {
		for _, provider := range options.Providers {
			if provider.Current {
				providerID = provider.ID
				break
			}
		}
	}
	baseURL := strings.TrimSpace(p.BaseURL)
	if baseURL == "" && cfg != nil {
		baseURL = strings.TrimSpace(cfg.CodexBaseURL)
	}
	if baseURL == "" {
		for _, provider := range options.Providers {
			if provider.ID == providerID || (providerID == "" && provider.Current) {
				baseURL = strings.TrimSpace(provider.BaseURL)
				break
			}
		}
	}
	if providerID == "" {
		writeError(w, http.StatusBadRequest, "cc-switch fetch-models requires a Codex provider id; select a provider or set the current Codex provider in cc-switch")
		return
	}
	if baseURL == "" {
		writeError(w, http.StatusBadRequest, "cc-switch fetch-models requires a Codex base URL")
		return
	}
	baseURL = normalizeOpenAIBaseURL(baseURL)

	args := []string{"--app", "codex", "provider", "fetch-models", "--base-url", baseURL, providerID}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "cc-switch", args...)
	cmd.Env = append(os.Environ(), "CC_SWITCH_CONFIG_DIR="+ccDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if ctx.Err() == context.DeadlineExceeded {
			writeError(w, http.StatusGatewayTimeout, "cc-switch fetch-models timed out")
			return
		}
		if msg != "" {
			writeError(w, http.StatusBadGateway, "cc-switch fetch-models failed: "+msg)
			return
		}
		writeError(w, http.StatusBadGateway, "cc-switch fetch-models failed: "+err.Error())
		return
	}

	resp := c.buildCCSwitchCodexOptions(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"fetched":           true,
		"cc_switch_output":  strings.TrimSpace(string(out)),
		"path":              resp.Path,
		"providers":         resp.Providers,
		"models":            resp.Models,
		"reasoning_efforts": resp.ReasoningEfforts,
		"error":             resp.Error,
	})
}

func (c *Console) buildCCSwitchCodexOptions(ctx context.Context) ccSwitchCodexOptionsResponse {
	cfg := c.currentConfig()
	ccDir := config.DefaultCCSwitchDir
	if cfg != nil && strings.TrimSpace(cfg.CCSwitchConfigDir) != "" {
		ccDir = strings.TrimSpace(cfg.CCSwitchConfigDir)
	}
	dbPath := filepath.Join(ccDir, "cc-switch.db")
	resp := ccSwitchCodexOptionsResponse{
		OK:               true,
		Path:             dbPath,
		ReasoningEfforts: defaultReasoningEfforts(),
	}
	if cfg != nil {
		resp.Models = appendModelOption(resp.Models, cfg.Model, "")
		resp.ReasoningEfforts = appendUnique(resp.ReasoningEfforts, cfg.CodexReasoningEffort)
	}
	if _, err := os.Stat(dbPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			resp.Error = err.Error()
		}
		return resp
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		resp.Error = err.Error()
		return resp
	}
	defer db.Close()

	providers, err := readCCSwitchCodexProviders(ctx, db)
	if err != nil {
		resp.Error = err.Error()
		return resp
	}
	resp.Providers = providers
	for _, provider := range providers {
		resp.Models = appendModelOption(resp.Models, provider.Model, "")
		resp.ReasoningEfforts = appendUnique(resp.ReasoningEfforts, provider.ReasoningEffort)
	}

	models, err := readCCSwitchCodexModels(ctx, db)
	if err == nil {
		for _, model := range models {
			resp.Models = appendModelOption(resp.Models, model.ID, model.DisplayName)
		}
	} else if resp.Error == "" && !errors.Is(err, sql.ErrNoRows) {
		resp.Error = err.Error()
	}
	return resp
}

func readCCSwitchCodexProviders(ctx context.Context, db *sql.DB) ([]ccSwitchCodexProviderView, error) {
	rows, err := db.QueryContext(ctx, `
SELECT id, name, settings_config, is_current
FROM providers
WHERE app_type = 'codex'
ORDER BY is_current DESC, name ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var providers []ccSwitchCodexProviderView
	for rows.Next() {
		var id, name, settingsConfig string
		var isCurrent bool
		if err := rows.Scan(&id, &name, &settingsConfig, &isCurrent); err != nil {
			return nil, err
		}
		providerConfig := parseCCSwitchCodexConfig(settingsConfig)
		providers = append(providers, ccSwitchCodexProviderView{
			ID:              id,
			Name:            name,
			Current:         isCurrent,
			Model:           providerConfig.Model,
			ReasoningEffort: providerConfig.ReasoningEffort,
			BaseURL:         providerConfig.BaseURL,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return providers, nil
}

func readCCSwitchCodexModels(ctx context.Context, db *sql.DB) ([]ccSwitchModelView, error) {
	rows, err := db.QueryContext(ctx, `SELECT model_id, display_name FROM model_pricing ORDER BY model_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var models []ccSwitchModelView
	for rows.Next() {
		var id, display string
		if err := rows.Scan(&id, &display); err != nil {
			return nil, err
		}
		if !isCodexModelID(id) {
			continue
		}
		models = append(models, ccSwitchModelView{ID: id, DisplayName: display})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return models, nil
}

type ccSwitchParsedCodexConfig struct {
	Model           string
	ReasoningEffort string
	BaseURL         string
}

func parseCCSwitchCodexConfig(settingsConfig string) ccSwitchParsedCodexConfig {
	var out ccSwitchParsedCodexConfig
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settingsConfig), &raw); err != nil {
		return out
	}
	for _, key := range []string{"base_url", "baseUrl", "baseURL"} {
		if v, ok := raw[key]; ok {
			_ = json.Unmarshal(v, &out.BaseURL)
			if strings.TrimSpace(out.BaseURL) != "" {
				out.BaseURL = strings.TrimSpace(out.BaseURL)
				break
			}
		}
	}
	var configText string
	if v, ok := raw["config"]; ok {
		_ = json.Unmarshal(v, &configText)
	}
	if configText == "" {
		return out
	}
	model, effort, baseURL := parseCodexTOMLModelConfig(configText)
	out.Model = model
	out.ReasoningEffort = effort
	if out.BaseURL == "" {
		out.BaseURL = baseURL
	}
	return out
}

func parseCodexTOMLModelConfig(configText string) (string, string, string) {
	var modelID, reasoning, baseURL string
	for _, line := range strings.Split(configText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		switch key {
		case "model":
			modelID = strings.TrimSpace(value)
		case "model_reasoning_effort":
			reasoning = strings.TrimSpace(value)
		case "base_url":
			baseURL = strings.TrimSpace(value)
		}
	}
	return modelID, reasoning, baseURL
}

func defaultReasoningEfforts() []string {
	return []string{"minimal", "low", "medium", "high", "xhigh"}
}

func appendUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func appendModelOption(models []ccSwitchModelView, id, display string) []ccSwitchModelView {
	id = strings.TrimSpace(id)
	if id == "" {
		return models
	}
	for _, existing := range models {
		if existing.ID == id {
			return models
		}
	}
	return append(models, ccSwitchModelView{ID: id, DisplayName: strings.TrimSpace(display)})
}

func isCodexModelID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" || strings.HasPrefix(id, "claude-") {
		return false
	}
	return strings.HasPrefix(id, "gpt-") ||
		strings.HasPrefix(id, "o1") ||
		strings.HasPrefix(id, "o3") ||
		strings.HasPrefix(id, "o4") ||
		strings.HasPrefix(id, "chatgpt-")
}

// jobView is the JSON shape returned to the console, with PRRef flattened.
type jobView struct {
	ID            int64        `json:"id"`
	Owner         string       `json:"owner"`
	Repo          string       `json:"repo"`
	Number        int          `json:"number"`
	Event         string       `json:"event"`
	Action        string       `json:"action"`
	Status        string       `json:"status"`
	Attempts      int          `json:"attempts"`
	Error         string       `json:"error"`
	ErrorType     string       `json:"error_type"`
	Retryable     bool         `json:"retryable"`
	CreatedAt     string       `json:"created_at"`
	StartedAt     string       `json:"started_at"`
	FinishedAt    string       `json:"finished_at"`
	NextAttemptAt string       `json:"next_attempt_at"`
	SessionID     string       `json:"session_id"`
	LogCount      int          `json:"log_count"`
	Logs          []jobLogView `json:"logs"`
}

type jobLogView struct {
	Stage     string `json:"stage"`
	Message   string `json:"message"`
	CreatedAt string `json:"created_at"`
}

type jobsResponse struct {
	Jobs       []jobView `json:"jobs"`
	Page       int       `json:"page"`
	PageSize   int       `json:"page_size"`
	Total      int       `json:"total"`
	TotalPages int       `json:"total_pages"`
	HasMore    bool      `json:"has_more"`
}

type jobStatsView struct {
	TotalJobs        int     `json:"total_jobs"`
	ReviewJobs       int     `json:"review_jobs"`
	ReviewedJobs     int     `json:"reviewed_jobs"`
	Done             int     `json:"done"`
	Failed           int     `json:"failed"`
	Running          int     `json:"running"`
	Pending          int     `json:"pending"`
	Superseded       int     `json:"superseded"`
	Canceled         int     `json:"canceled"`
	RetryablePending int     `json:"retryable_pending"`
	SuccessRate      float64 `json:"success_rate"`
}

func (c *Console) handleJobs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	pageSize := parsePositiveInt(r.URL.Query().Get("page_size"), 20)
	if pageSize > 100 {
		pageSize = 100
	}
	offset := (page - 1) * pageSize
	filter, err := parseJobFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	jobs, err := c.store.ListJobsFiltered(ctx, filter, pageSize+1, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hasMore := len(jobs) > pageSize
	if hasMore {
		jobs = jobs[:pageSize]
	}
	out := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toJobView(j))
	}
	writeJSON(w, http.StatusOK, jobsResponse{
		Jobs:     out,
		Page:     page,
		PageSize: pageSize,
		HasMore:  hasMore,
	})
}

func (c *Console) handleJobStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	filter, err := parseJobFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	stats, err := c.store.JobStatsFiltered(ctx, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toJobStatsView(stats))
}

func (c *Console) handleJobDetail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, err := c.store.GetJobView(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toJobView(*job))
}

func (c *Console) handleJobRerun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, err := c.store.RerunJob(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": toJobView(model.JobView{
		ID: job.ID, PR: job.PR, Event: job.Event, Action: job.Action, Status: job.Status,
		Attempts: job.Attempts, Error: job.Error, ErrorType: job.ErrorType, Retryable: job.Retryable,
		CreatedAt: job.CreatedAt, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt,
		NextAttemptAt: job.NextAttemptAt,
	})})
}

func (c *Console) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, err := c.store.CancelPendingJob(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		if errors.Is(err, model.ErrJobNotPending) {
			writeError(w, http.StatusConflict, "job is not pending")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": toJobView(model.JobView{
		ID: job.ID, PR: job.PR, Event: job.Event, Action: job.Action, Status: job.Status,
		Attempts: job.Attempts, Error: job.Error, ErrorType: job.ErrorType, Retryable: job.Retryable,
		CreatedAt: job.CreatedAt, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt,
		NextAttemptAt: job.NextAttemptAt,
	})})
}

func toJobView(j model.JobView) jobView {
	jv := jobView{
		ID:        j.ID,
		Owner:     j.PR.Owner,
		Repo:      j.PR.Repo,
		Number:    j.PR.Number,
		Event:     string(j.Event),
		Action:    j.Action,
		Status:    string(j.Status),
		Attempts:  j.Attempts,
		Error:     j.Error,
		ErrorType: string(j.ErrorType),
		Retryable: j.Retryable,
		SessionID: j.SessionID,
		LogCount:  j.LogCount,
	}
	if !j.CreatedAt.IsZero() {
		jv.CreatedAt = j.CreatedAt.Format(timeLayout)
	}
	if j.StartedAt != nil && !j.StartedAt.IsZero() {
		jv.StartedAt = j.StartedAt.Format(timeLayout)
	}
	if j.FinishedAt != nil && !j.FinishedAt.IsZero() {
		jv.FinishedAt = j.FinishedAt.Format(timeLayout)
	}
	if j.NextAttemptAt != nil && !j.NextAttemptAt.IsZero() {
		jv.NextAttemptAt = j.NextAttemptAt.Format(timeLayout)
	}
	if len(j.Logs) > 0 {
		jv.Logs = make([]jobLogView, 0, len(j.Logs))
		for _, l := range j.Logs {
			lv := jobLogView{Stage: l.Stage, Message: l.Message}
			if !l.CreatedAt.IsZero() {
				lv.CreatedAt = l.CreatedAt.Format(timeLayout)
			}
			jv.Logs = append(jv.Logs, lv)
		}
		if jv.LogCount == 0 {
			jv.LogCount = len(j.Logs)
		}
	}
	return jv
}

func toJobStatsView(stats model.JobStats) jobStatsView {
	completed := stats.Done + stats.Failed
	successRate := 0.0
	if completed > 0 {
		successRate = float64(stats.Done) / float64(completed)
	}
	return jobStatsView{
		TotalJobs:        stats.TotalJobs,
		ReviewJobs:       stats.ReviewJobs,
		ReviewedJobs:     stats.ReviewedJobs,
		Done:             stats.Done,
		Failed:           stats.Failed,
		Running:          stats.Running,
		Pending:          stats.Pending,
		Superseded:       stats.Superseded,
		Canceled:         stats.Canceled,
		RetryablePending: stats.RetryablePending,
		SuccessRate:      successRate,
	}
}

type analysisReportView struct {
	ID        int64                 `json:"id"`
	CreatedAt string                `json:"created_at"`
	GiteaURL  string                `json:"gitea_url"`
	Summary   model.AnalysisSummary `json:"summary"`
}

type analysisTrendPointView struct {
	Bucket               string  `json:"bucket"`
	Interval             string  `json:"interval"`
	Day                  string  `json:"day"`
	FinishedAt           string  `json:"finished_at"`
	TotalReviewRuns      int     `json:"total_review_runs"`
	SuccessfulReviewRuns int     `json:"successful_review_runs"`
	FailedReviewRuns     int     `json:"failed_review_runs"`
	SuccessRate          float64 `json:"success_rate"`
	TotalFindings        int     `json:"total_findings"`
	OpenFindings         int     `json:"open_findings"`
	HighCriticalOpen     int     `json:"high_critical_open"`
}

func (c *Console) handleCreateAnalysisReport(w http.ResponseWriter, r *http.Request) {
	summary, err := c.store.BuildAnalysisSummary(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	report, err := c.store.CreateAnalysisReport(r.Context(), summary)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": c.toAnalysisReportView(*report)})
}

func (c *Console) handleLatestAnalysisReport(w http.ResponseWriter, r *http.Request) {
	report, err := c.store.LatestAnalysisReport(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if report == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": c.toAnalysisReportView(*report)})
}

func (c *Console) handleListAnalysisReports(w http.ResponseWriter, r *http.Request) {
	limit := parsePositiveInt(r.URL.Query().Get("limit"), 20)
	reports, err := c.store.ListAnalysisReports(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]analysisReportView, 0, len(reports))
	for _, report := range reports {
		out = append(out, c.toAnalysisReportView(report))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reports": out})
}

func (c *Console) handleAnalysisTrend(w http.ResponseWriter, r *http.Request) {
	limit := parsePositiveInt(r.URL.Query().Get("limit"), 12)
	interval := r.URL.Query().Get("interval")
	points, err := c.store.BuildAnalysisTrend(r.Context(), limit, interval)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]analysisTrendPointView, 0, len(points))
	for _, point := range points {
		item := analysisTrendPointView{
			Bucket: point.Bucket, Interval: point.Interval,
			Day: point.Day, TotalReviewRuns: point.TotalReviewRuns, SuccessfulReviewRuns: point.SuccessfulReviewRuns,
			FailedReviewRuns: point.FailedReviewRuns, SuccessRate: point.SuccessRate,
			TotalFindings: point.TotalFindings, OpenFindings: point.OpenFindings,
			HighCriticalOpen: point.HighCriticalOpen,
		}
		if !point.FinishedAt.IsZero() {
			item.FinishedAt = point.FinishedAt.Format(timeLayout)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "points": out})
}

type projectSkillSummaryView struct {
	Owner            string `json:"owner"`
	Repo             string `json:"repo"`
	Slug             string `json:"slug"`
	PullRequests     int    `json:"pull_requests"`
	ReviewRuns       int    `json:"review_runs"`
	Findings         int    `json:"findings"`
	OpenFindings     int    `json:"open_findings"`
	HighCriticalOpen int    `json:"high_critical_open"`
	SkillVersion     int    `json:"skill_version"`
	SkillUpdatedAt   string `json:"skill_updated_at"`
}

type projectSkillView struct {
	Owner              string                    `json:"owner"`
	Repo               string                    `json:"repo"`
	Slug               string                    `json:"slug"`
	Title              string                    `json:"title"`
	Content            string                    `json:"content"`
	Version            int                       `json:"version"`
	SourceFindingCount int                       `json:"source_finding_count"`
	CreatedAt          string                    `json:"created_at"`
	UpdatedAt          string                    `json:"updated_at"`
	Context            model.ProjectSkillContext `json:"context"`
}

type skillGenerationTask struct {
	ID         string
	Owner      string
	Repo       string
	Status     string
	Error      string
	Skill      *projectSkillView
	CreatedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt *time.Time
}

type skillGenerationTaskView struct {
	ID         string            `json:"id"`
	Owner      string            `json:"owner"`
	Repo       string            `json:"repo"`
	Status     string            `json:"status"`
	Error      string            `json:"error,omitempty"`
	Skill      *projectSkillView `json:"skill,omitempty"`
	CreatedAt  string            `json:"created_at"`
	UpdatedAt  string            `json:"updated_at"`
	FinishedAt string            `json:"finished_at,omitempty"`
}

func (c *Console) handleSkillProjects(w http.ResponseWriter, r *http.Request) {
	items, err := c.store.ListProjectSkillSummaries(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]projectSkillSummaryView, 0, len(items))
	for _, item := range items {
		out = append(out, toProjectSkillSummaryView(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "projects": out})
}

func (c *Console) handleSkillDetail(w http.ResponseWriter, r *http.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	ctx, err := c.store.BuildProjectSkillContext(r.Context(), owner, repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	skill, err := c.store.GetProjectSkill(r.Context(), owner, repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skill": toProjectSkillView(skill, ctx)})
}

func (c *Console) handleSkillGenerate(w http.ResponseWriter, r *http.Request) {
	if c.skillGenerator == nil {
		writeError(w, http.StatusServiceUnavailable, "codex skill generator is not configured")
		return
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")

	if task := c.runningSkillTask(owner, repo); task != nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "task": toSkillGenerationTaskView(task)})
		return
	}

	ctx, err := c.store.BuildProjectSkillContext(r.Context(), owner, repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ctx.Findings == 0 {
		writeError(w, http.StatusBadRequest, "no findings for project")
		return
	}
	existing, err := c.store.GetProjectSkill(r.Context(), owner, repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	task := c.createSkillTask(owner, repo)
	go c.runSkillGenerationTask(task.ID, ctx, existing)

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "task": toSkillGenerationTaskView(task)})
}

func (c *Console) handleSkillGenerateStatus(w http.ResponseWriter, r *http.Request) {
	owner, repo, taskID := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("taskID")
	task := c.getSkillTask(taskID)
	if task == nil || task.Owner != owner || task.Repo != repo {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": toSkillGenerationTaskView(task)})
}

func (c *Console) createSkillTask(owner, repo string) *skillGenerationTask {
	c.skillTasksMu.Lock()
	defer c.skillTasksMu.Unlock()
	now := time.Now()
	c.pruneSkillTasksLocked(now)
	task := &skillGenerationTask{ID: newTaskID(), Owner: owner, Repo: repo, Status: "running", CreatedAt: now, UpdatedAt: now}
	c.skillTasks[task.ID] = task
	return cloneSkillGenerationTask(task)
}

func (c *Console) runningSkillTask(owner, repo string) *skillGenerationTask {
	c.skillTasksMu.Lock()
	defer c.skillTasksMu.Unlock()
	for _, task := range c.skillTasks {
		if task.Owner == owner && task.Repo == repo && task.Status == "running" {
			return cloneSkillGenerationTask(task)
		}
	}
	return nil
}

func (c *Console) getSkillTask(id string) *skillGenerationTask {
	c.skillTasksMu.Lock()
	defer c.skillTasksMu.Unlock()
	task := c.skillTasks[id]
	return cloneSkillGenerationTask(task)
}

func (c *Console) updateSkillTask(id string, update func(*skillGenerationTask)) {
	c.skillTasksMu.Lock()
	defer c.skillTasksMu.Unlock()
	if task := c.skillTasks[id]; task != nil {
		update(task)
		task.UpdatedAt = time.Now()
	}
}

func (c *Console) runSkillGenerationTask(taskID string, ctx model.ProjectSkillContext, existing *model.ProjectSkill) {
	runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	content, err := c.skillGenerator(runCtx, model.SkillGenerationInput{Context: ctx, Existing: existing})
	if err == nil {
		content = normalizeSkillContent(content)
		skill := &model.ProjectSkill{
			Owner: ctx.Owner, Repo: ctx.Repo, Slug: ctx.Slug, Title: skillTitle(ctx.Owner, ctx.Repo, content),
			Content: content, SourceFindingCount: ctx.Findings,
		}
		if upsertErr := c.store.UpsertProjectSkill(runCtx, skill); upsertErr != nil {
			err = upsertErr
		}
	}
	var skillView *projectSkillView
	if err == nil {
		saved, getErr := c.store.GetProjectSkill(runCtx, ctx.Owner, ctx.Repo)
		if getErr != nil {
			err = getErr
		} else {
			view := toProjectSkillView(saved, ctx)
			skillView = &view
		}
	}
	finished := time.Now()
	c.updateSkillTask(taskID, func(task *skillGenerationTask) {
		task.FinishedAt = &finished
		if err != nil {
			task.Status = "failed"
			task.Error = err.Error()
			return
		}
		task.Status = "done"
		task.Skill = skillView
	})
}

func (c *Console) pruneSkillTasksLocked(now time.Time) {
	for id, task := range c.skillTasks {
		if task.Status != "running" && now.Sub(task.UpdatedAt) > 24*time.Hour {
			delete(c.skillTasks, id)
		}
	}
}

func cloneSkillGenerationTask(task *skillGenerationTask) *skillGenerationTask {
	if task == nil {
		return nil
	}
	out := *task
	return &out
}

func toSkillGenerationTaskView(task *skillGenerationTask) skillGenerationTaskView {
	out := skillGenerationTaskView{
		ID: task.ID, Owner: task.Owner, Repo: task.Repo, Status: task.Status, Error: task.Error, Skill: task.Skill,
	}
	if !task.CreatedAt.IsZero() {
		out.CreatedAt = task.CreatedAt.Format(timeLayout)
	}
	if !task.UpdatedAt.IsZero() {
		out.UpdatedAt = task.UpdatedAt.Format(timeLayout)
	}
	if task.FinishedAt != nil && !task.FinishedAt.IsZero() {
		out.FinishedAt = task.FinishedAt.Format(timeLayout)
	}
	return out
}

func newTaskID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func (c *Console) handlePublicSkillDownload(w http.ResponseWriter, r *http.Request) {
	skill, err := c.store.GetProjectSkill(r.Context(), r.PathValue("owner"), r.PathValue("repo"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if skill == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="SKILL.md"`)
	_, _ = w.Write([]byte(skill.Content))
}

func toProjectSkillSummaryView(item model.ProjectSkillSummary) projectSkillSummaryView {
	out := projectSkillSummaryView{
		Owner: item.Owner, Repo: item.Repo, Slug: item.Slug,
		PullRequests: item.PullRequests, ReviewRuns: item.ReviewRuns, Findings: item.Findings,
		OpenFindings: item.OpenFindings, HighCriticalOpen: item.HighCriticalOpen,
		SkillVersion: item.SkillVersion,
	}
	if !item.SkillUpdatedAt.IsZero() {
		out.SkillUpdatedAt = item.SkillUpdatedAt.Format(timeLayout)
	}
	return out
}

func toProjectSkillView(skill *model.ProjectSkill, ctx model.ProjectSkillContext) projectSkillView {
	out := projectSkillView{Owner: ctx.Owner, Repo: ctx.Repo, Slug: ctx.Slug, Context: ctx}
	if skill == nil {
		return out
	}
	out.Owner, out.Repo, out.Slug, out.Title = skill.Owner, skill.Repo, skill.Slug, skill.Title
	out.Content, out.Version, out.SourceFindingCount = skill.Content, skill.Version, skill.SourceFindingCount
	if !skill.CreatedAt.IsZero() {
		out.CreatedAt = skill.CreatedAt.Format(timeLayout)
	}
	if !skill.UpdatedAt.IsZero() {
		out.UpdatedAt = skill.UpdatedAt.Format(timeLayout)
	}
	return out
}

func normalizeSkillContent(content string) string {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```markdown")
	content = strings.TrimPrefix(content, "```md")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "---") {
		content = "---\nname: project-defect-skill\ndescription: Use this skill when working on this project to avoid recurring defects found by review agents.\n---\n\n" + content
	}
	return content + "\n"
}

func skillTitle(owner, repo, content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return owner + "/" + repo + " defect-prevention skill"
}

func (c *Console) toAnalysisReportView(report model.AnalysisReport) analysisReportView {
	out := analysisReportView{ID: report.ID, Summary: report.Summary}
	if cfg := c.currentConfig(); cfg != nil {
		out.GiteaURL = strings.TrimRight(strings.TrimSpace(cfg.GiteaURL), "/")
	}
	if !report.CreatedAt.IsZero() {
		out.CreatedAt = report.CreatedAt.Format(timeLayout)
	}
	return out
}

func parsePositiveInt(raw string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func parseJobFilter(r *http.Request) (model.JobFilter, error) {
	var filter model.JobFilter
	from, err := parseOptionalTime(r.URL.Query().Get("created_from"), false)
	if err != nil {
		return filter, err
	}
	to, err := parseOptionalTime(r.URL.Query().Get("created_to"), true)
	if err != nil {
		return filter, err
	}
	if from != nil && to != nil && from.After(*to) {
		return filter, fmt.Errorf("created_from must be before created_to")
	}
	filter.CreatedFrom = from
	filter.CreatedTo = to
	return filter, nil
}

func parseOptionalTime(raw string, endOfDay bool) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", raw, time.Local); err == nil {
		if endOfDay {
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		return &t, nil
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("invalid time %q", raw)
}
