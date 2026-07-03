// Package config loads runtime configuration from environment variables and,
// optionally, the database settings table. Precedence is: built-in defaults are
// overridden by environment variables (LoadEnv), which are in turn overridden by
// database settings (ApplyOverrides). This lets the service boot from env and be
// reconfigured live from the web console.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// CodexAuthMode selects how codex authenticates.
const (
	AuthModeCCSwitch = "ccswitch" // cc-switch manages Codex provider/config.
	AuthModeAuthFile = "authfile" // <CodexHome>/auth.json (refresh-token login)
	AuthModeAPIKey   = "apikey"   // CODEX_API_KEY env / setting
)

// Codex sandbox modes are passed through to `codex exec -c sandbox_mode=...`.
const (
	SandboxReadOnly         = "read-only"
	SandboxWorkspaceWrite   = "workspace-write"
	SandboxDangerFullAccess = "danger-full-access"
)

// Defaults applied when neither env nor DB settings provide a value.
const (
	DefaultListenAddr      = ":8080"
	DefaultDBPath          = "/data/codex-gitea.db"
	DefaultCacheDir        = "/cache"
	DefaultWorkDir         = "/work"
	DefaultCodexHome       = "/codex-home"
	DefaultClaudeHome      = "/claude-home"
	DefaultCCSwitchDir     = "/cc-switch"
	DefaultModel           = "gpt-5-codex"
	DefaultClaudeModel     = "sonnet"
	DefaultMiniMaxModel    = ""
	DefaultMiniMaxProvider = ""
	DefaultAuthMode        = AuthModeCCSwitch
	DefaultReasoningEffort = "high"
	DefaultSandboxMode     = SandboxReadOnly
	DefaultConcurrency     = 5
	DefaultTimeout         = 30 * time.Minute
	DefaultGiteaTimeout    = 90 * time.Second
	DefaultClaudeBudget    = 0.30
	DefaultMiniMaxBudget   = 0.30
)

// DefaultTriggerKeywords are the comment phrases that trigger a review.
var DefaultTriggerKeywords = []string{"/review", "@review"}

// Config holds all runtime configuration for the service.
type Config struct {
	// Server / paths
	ListenAddr string
	DBPath     string
	CacheDir   string
	WorkDir    string
	CodexHome  string

	// Gitea
	GiteaURL      string
	GiteaToken    string
	WebhookSecret string
	GiteaTimeout  time.Duration

	// Codex
	Model                 string
	CodexReasoningEffort  string
	CodexBaseURL          string
	CodexAuthMode         string
	CodexAPIKey           string
	CodexCCSwitchProvider string
	CodexSandbox          string

	// Claude
	ClaudeEnabled      bool
	ClaudeModel        string
	ClaudeAPIKey       string
	ClaudeBaseURL      string
	ClaudeHome         string
	CCSwitchConfigDir  string
	CCSwitchProvider   string
	ClaudeMaxBudgetUSD float64

	// MiniMax via Claude Code + cc-switch
	MiniMaxEnabled      bool
	MiniMaxModel        string
	MiniMaxProvider     string
	MiniMaxAPIKey       string
	MiniMaxBaseURL      string
	MiniMaxMaxBudgetUSD float64

	// Console
	AdminPassword string

	// Behavior
	TriggerKeywords []string
	Concurrency     int
	RepoAllowlist   []string
	Timeout         time.Duration

	// SecretKey optionally encrypts secret columns in the DB. Empty = no encryption.
	SecretKey string
}

// Clone returns an independent copy safe for sharing across goroutines.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.TriggerKeywords = append([]string(nil), c.TriggerKeywords...)
	out.RepoAllowlist = append([]string(nil), c.RepoAllowlist...)
	return &out
}

// LoadEnv builds a Config from environment variables, falling back to defaults
// for any variable that is unset or empty.
func LoadEnv() *Config {
	c := &Config{
		ListenAddr:            getEnv("LISTEN_ADDR", DefaultListenAddr),
		DBPath:                getEnv("DB_PATH", DefaultDBPath),
		CacheDir:              getEnv("CACHE_DIR", DefaultCacheDir),
		WorkDir:               getEnv("WORK_DIR", DefaultWorkDir),
		CodexHome:             getEnv("CODEX_HOME", DefaultCodexHome),
		GiteaURL:              os.Getenv("GITEA_URL"),
		GiteaToken:            os.Getenv("GITEA_TOKEN"),
		WebhookSecret:         os.Getenv("WEBHOOK_SECRET"),
		GiteaTimeout:          parseDuration(os.Getenv("GITEA_TIMEOUT"), DefaultGiteaTimeout),
		Model:                 getEnv("MODEL", DefaultModel),
		CodexReasoningEffort:  DefaultReasoningEffort,
		CodexBaseURL:          strings.TrimSpace(os.Getenv("CODEX_BASE_URL")),
		CodexAuthMode:         normalizeAuthMode(getEnv("CODEX_AUTH_MODE", DefaultAuthMode)),
		CodexAPIKey:           os.Getenv("CODEX_API_KEY"),
		CodexCCSwitchProvider: os.Getenv("CODEX_CC_SWITCH_PROVIDER_ID"),
		CodexSandbox:          normalizeSandboxMode(getEnv("CODEX_SANDBOX_MODE", DefaultSandboxMode)),
		ClaudeEnabled:         parseBool(os.Getenv("CLAUDE_ENABLED"), false),
		ClaudeModel:           getEnv("CLAUDE_MODEL", DefaultClaudeModel),
		ClaudeAPIKey:          os.Getenv("CLAUDE_API_KEY"),
		ClaudeBaseURL:         os.Getenv("CLAUDE_BASE_URL"),
		ClaudeHome:            getEnv("CLAUDE_HOME", DefaultClaudeHome),
		CCSwitchConfigDir:     getEnv("CC_SWITCH_CONFIG_DIR", DefaultCCSwitchDir),
		CCSwitchProvider:      os.Getenv("CC_SWITCH_PROVIDER_ID"),
		ClaudeMaxBudgetUSD:    parseFloat(os.Getenv("CLAUDE_MAX_BUDGET_USD"), DefaultClaudeBudget),
		MiniMaxEnabled:        parseBool(os.Getenv("MINIMAX_ENABLED"), false),
		MiniMaxModel:          getEnv("MINIMAX_MODEL", DefaultMiniMaxModel),
		MiniMaxProvider:       getEnv("MINIMAX_PROVIDER_ID", DefaultMiniMaxProvider),
		MiniMaxAPIKey:         os.Getenv("MINIMAX_API_KEY"),
		MiniMaxBaseURL:        os.Getenv("MINIMAX_BASE_URL"),
		MiniMaxMaxBudgetUSD:   parseFloat(os.Getenv("MINIMAX_MAX_BUDGET_USD"), DefaultMiniMaxBudget),
		AdminPassword:         os.Getenv("ADMIN_PASSWORD"),
		TriggerKeywords:       parseList(os.Getenv("TRIGGER_KEYWORDS"), DefaultTriggerKeywords),
		Concurrency:           parseInt(os.Getenv("CONCURRENCY"), DefaultConcurrency),
		RepoAllowlist:         parseList(os.Getenv("REPO_ALLOWLIST"), nil),
		Timeout:               parseDuration(os.Getenv("TIMEOUT"), DefaultTimeout),
		SecretKey:             os.Getenv("SECRET_KEY"),
	}
	return c
}

// ApplyOverrides layers database settings on top of the current config. Only
// keys present in the map are touched, so unset settings leave env/defaults
// intact. Setting keys use lower_snake_case.
func (c *Config) ApplyOverrides(settings map[string]string) {
	if settings == nil {
		return
	}
	if v, ok := settings["gitea_url"]; ok {
		c.GiteaURL = v
	}
	if v, ok := settings["gitea_token"]; ok {
		c.GiteaToken = v
	}
	if v, ok := settings["webhook_secret"]; ok {
		c.WebhookSecret = v
	}
	if v, ok := settings["gitea_timeout"]; ok {
		c.GiteaTimeout = parseDuration(v, c.GiteaTimeout)
	}
	if v, ok := settings["model"]; ok {
		c.Model = strings.TrimSpace(v)
	}
	if v, ok := settings["codex_reasoning_effort"]; ok {
		c.CodexReasoningEffort = strings.TrimSpace(v)
	}
	if v, ok := settings["codex_base_url"]; ok {
		c.CodexBaseURL = strings.TrimSpace(v)
	}
	if v, ok := settings["codex_auth_mode"]; ok {
		c.CodexAuthMode = normalizeAuthMode(v)
	}
	if v, ok := settings["codex_api_key"]; ok {
		c.CodexAPIKey = v
	}
	if v, ok := settings["codex_cc_switch_provider_id"]; ok {
		c.CodexCCSwitchProvider = strings.TrimSpace(v)
	}
	if v, ok := settings["codex_sandbox_mode"]; ok {
		c.CodexSandbox = normalizeSandboxMode(v)
	}
	if v, ok := settings["claude_enabled"]; ok {
		c.ClaudeEnabled = parseBool(v, c.ClaudeEnabled)
	}
	if v, ok := settings["claude_model"]; ok {
		if strings.TrimSpace(v) == "" {
			c.ClaudeModel = DefaultClaudeModel
		} else {
			c.ClaudeModel = v
		}
	}
	if v, ok := settings["claude_api_key"]; ok {
		c.ClaudeAPIKey = v
	}
	if v, ok := settings["claude_base_url"]; ok {
		c.ClaudeBaseURL = v
	}
	if v, ok := settings["claude_home"]; ok {
		if strings.TrimSpace(v) == "" {
			c.ClaudeHome = DefaultClaudeHome
		} else {
			c.ClaudeHome = v
		}
	}
	if v, ok := settings["cc_switch_config_dir"]; ok {
		if strings.TrimSpace(v) == "" {
			c.CCSwitchConfigDir = DefaultCCSwitchDir
		} else {
			c.CCSwitchConfigDir = v
		}
	}
	if v, ok := settings["cc_switch_provider_id"]; ok {
		c.CCSwitchProvider = v
	}
	if v, ok := settings["claude_max_budget_usd"]; ok {
		c.ClaudeMaxBudgetUSD = parseFloat(v, c.ClaudeMaxBudgetUSD)
	}
	if v, ok := settings["minimax_enabled"]; ok {
		c.MiniMaxEnabled = parseBool(v, c.MiniMaxEnabled)
	}
	if v, ok := settings["minimax_model"]; ok {
		c.MiniMaxModel = strings.TrimSpace(v)
	}
	if v, ok := settings["minimax_provider_id"]; ok {
		c.MiniMaxProvider = strings.TrimSpace(v)
	}
	if v, ok := settings["minimax_api_key"]; ok {
		c.MiniMaxAPIKey = v
	}
	if v, ok := settings["minimax_base_url"]; ok {
		c.MiniMaxBaseURL = strings.TrimSpace(v)
	}
	if v, ok := settings["minimax_max_budget_usd"]; ok {
		c.MiniMaxMaxBudgetUSD = parseFloat(v, c.MiniMaxMaxBudgetUSD)
	}
	if v, ok := settings["admin_password"]; ok {
		c.AdminPassword = v
	}
	if v, ok := settings["trigger_keywords"]; ok {
		if kw := parseList(v, nil); len(kw) > 0 {
			c.TriggerKeywords = kw
		}
	}
	if v, ok := settings["concurrency"]; ok {
		c.Concurrency = parseInt(v, c.Concurrency)
	}
	if v, ok := settings["repo_allowlist"]; ok {
		c.RepoAllowlist = parseList(v, nil)
	}
	if v, ok := settings["timeout"]; ok {
		c.Timeout = parseDuration(v, c.Timeout)
	}
}

// Validate checks for fatal configuration problems. It returns an error only for
// conditions that make the service unable to function (e.g. apikey mode without
// a key). Soft problems (missing GiteaURL/WebhookSecret that can be filled from
// the console after boot) are reported via Warnings, not errors.
func (c *Config) Validate() error {
	switch c.CodexAuthMode {
	case AuthModeCCSwitch:
		// cc-switch may already have a current Codex provider; provider id is optional.
	case AuthModeAuthFile:
		// auth.json may be uploaded later from the console; nothing required here.
	case AuthModeAPIKey:
		if strings.TrimSpace(c.CodexAPIKey) == "" {
			return fmt.Errorf("codex_auth_mode=apikey requires CODEX_API_KEY")
		}
	default:
		return fmt.Errorf("invalid codex_auth_mode %q (want %q, %q, or %q)",
			c.CodexAuthMode, AuthModeCCSwitch, AuthModeAuthFile, AuthModeAPIKey)
	}
	if c.Concurrency < 1 {
		return fmt.Errorf("concurrency must be >= 1, got %d", c.Concurrency)
	}
	sandboxMode := normalizeSandboxMode(c.CodexSandbox)
	switch sandboxMode {
	case SandboxReadOnly, SandboxWorkspaceWrite, SandboxDangerFullAccess:
	default:
		return fmt.Errorf("invalid codex_sandbox_mode %q (want %q, %q, or %q)",
			c.CodexSandbox, SandboxReadOnly, SandboxWorkspaceWrite, SandboxDangerFullAccess)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("timeout must be > 0, got %s", c.Timeout)
	}
	if c.GiteaTimeout <= 0 {
		return fmt.Errorf("gitea_timeout must be > 0, got %s", c.GiteaTimeout)
	}
	if c.ClaudeMaxBudgetUSD < 0 {
		return fmt.Errorf("claude_max_budget_usd must be >= 0, got %g", c.ClaudeMaxBudgetUSD)
	}
	if c.MiniMaxMaxBudgetUSD < 0 {
		return fmt.Errorf("minimax_max_budget_usd must be >= 0, got %g", c.MiniMaxMaxBudgetUSD)
	}
	return nil
}

// Warnings returns non-fatal configuration issues. The caller may log these and
// continue; the missing values can be supplied later via the console.
func (c *Config) Warnings() []string {
	var w []string
	if strings.TrimSpace(c.GiteaURL) == "" {
		w = append(w, "gitea_url is empty; set it via env GITEA_URL or the console before reviewing")
	}
	if strings.TrimSpace(c.WebhookSecret) == "" {
		w = append(w, "webhook_secret is empty; webhook signature verification will reject deliveries")
	}
	if strings.TrimSpace(c.GiteaToken) == "" {
		w = append(w, "gitea_token is empty; the service cannot post reviews until it is set")
	}
	if strings.TrimSpace(c.AdminPassword) == "" {
		w = append(w, "admin_password is empty; the web console is disabled until ADMIN_PASSWORD is set")
	}
	return w
}

// ---------- helpers ----------

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseList splits a comma-separated string, trimming whitespace and dropping
// empty entries. Returns def when the input has no usable entries.
func parseList(s string, def []string) []string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func parseInt(s string, def int) int {
	if strings.TrimSpace(s) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func parseFloat(s string, def float64) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return v
}

func parseBool(s string, def bool) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return def
	}
	switch s {
	case "1", "true", "yes", "y", "on", "enabled":
		return true
	case "0", "false", "no", "n", "off", "disabled":
		return false
	default:
		return def
	}
}

func parseDuration(s string, def time.Duration) time.Duration {
	if strings.TrimSpace(s) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return d
}

func normalizeAuthMode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == AuthModeCCSwitch || s == "cc-switch" {
		return AuthModeCCSwitch
	}
	if s == AuthModeAPIKey {
		return AuthModeAPIKey
	}
	if s == AuthModeAuthFile {
		return AuthModeAuthFile
	}
	// Unknown values are passed through so Validate can flag them.
	if s == "" {
		return DefaultAuthMode
	}
	return s
}

func normalizeSandboxMode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case SandboxReadOnly, SandboxWorkspaceWrite, SandboxDangerFullAccess:
		return s
	case "":
		return DefaultSandboxMode
	default:
		return s
	}
}
