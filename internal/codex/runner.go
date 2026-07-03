package codex

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
	_ "modernc.org/sqlite"
)

// defaultTimeout bounds a single codex invocation when none is configured.
const defaultTimeout = 30 * time.Minute

// secretEnvKeys are environment variables that must never reach the codex
// subprocess (and thus the worktree it inspects). The orchestrator's Gitea and
// OpenAI service tokens live here; codex authenticates via CODEX_HOME or its
// own CODEX_API_KEY, never the service tokens.
var secretEnvKeys = map[string]bool{
	"GITEA_TOKEN":         true,
	"GITEA_API_TOKEN":     true,
	"GITEA_PASSWORD":      true,
	"OPENAI_API_KEY":      true,
	"OPENAI_TOKEN":        true,
	"OPENAI_ORG_ID":       true,
	"OPENAI_ORGANIZATION": true,
	"ANTHROPIC_API_KEY":   true,
	// We always set CODEX_HOME / CODEX_API_KEY explicitly below, so drop any
	// inherited values to avoid surprises.
	"CODEX_HOME":    true,
	"CODEX_API_KEY": true,
}

// Options configures a Runner.
type Options struct {
	// Bin is the codex executable. Defaults to "codex"; the CODEX_BIN env var
	// overrides it (used by tests to point at a stub).
	Bin string
	// CodexHome sets CODEX_HOME for the codex process (auth/config dir).
	CodexHome string
	// Model optionally overrides the codex model (--model).
	Model string
	// ReasoningEffort optionally overrides Codex reasoning effort.
	ReasoningEffort string
	// BaseURL optionally points Codex at an OpenAI-compatible relay/provider.
	BaseURL string
	// APIKey, when set, runs codex in api-key mode (CODEX_API_KEY).
	APIKey string
	// CCSwitchBin is the cc-switch executable. Defaults to "cc-switch".
	CCSwitchBin string
	// CCSwitchConfigDir sets CC_SWITCH_CONFIG_DIR for cc-switch calls.
	CCSwitchConfigDir string
	// CCSwitchBaseURL optionally selects or builds a Codex runtime provider in ccswitch mode.
	CCSwitchBaseURL string
	// UseCCSwitch reports status through cc-switch even without a forced provider id.
	UseCCSwitch bool
	// CCSwitchProviderID, when set, is switched before each codex invocation.
	CCSwitchProviderID string
	// SandboxMode is passed to codex as sandbox_mode. Defaults to read-only.
	SandboxMode string
	// Timeout bounds a single invocation. Defaults to 30m.
	Timeout time.Duration
}

// Runner invokes the codex CLI in headless mode to perform structured reviews.
type Runner struct {
	bin         string
	codexHome   string
	model       string
	reasoning   string
	baseURL     string
	apiKey      string
	ccBin       string
	ccDir       string
	ccBaseURL   string
	useCCSwitch bool
	ccProvider  string
	sandbox     string
	timeout     time.Duration
}

var _ model.CodexRunner = (*Runner)(nil)

func (r *Runner) Name() string { return "codex" }

// New builds a Runner from Options. CODEX_BIN overrides Options.Bin.
func New(opts Options) *Runner {
	bin := opts.Bin
	if env := os.Getenv("CODEX_BIN"); env != "" {
		bin = env
	}
	if bin == "" {
		bin = "codex"
	}
	ccBin := opts.CCSwitchBin
	if ccBin == "" {
		ccBin = "cc-switch"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	sandbox := opts.SandboxMode
	if strings.TrimSpace(sandbox) == "" {
		sandbox = "read-only"
	}
	return &Runner{
		bin:         bin,
		codexHome:   opts.CodexHome,
		model:       strings.TrimSpace(opts.Model),
		reasoning:   strings.TrimSpace(opts.ReasoningEffort),
		baseURL:     normalizeOpenAIBaseURL(opts.BaseURL),
		apiKey:      opts.APIKey,
		ccBin:       ccBin,
		ccDir:       opts.CCSwitchConfigDir,
		ccBaseURL:   normalizeOpenAIBaseURL(opts.CCSwitchBaseURL),
		useCCSwitch: opts.UseCCSwitch,
		ccProvider:  strings.TrimSpace(opts.CCSwitchProviderID),
		sandbox:     sandbox,
		timeout:     timeout,
	}
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

// env builds the codex/cc-switch process environment: the parent environment
// minus any secret-bearing variables, plus explicit runtime config.
func (r *Runner) env() []string {
	out := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := kv[:eq]
		if secretEnvKeys[key] || (r.useCCSwitch && key == "HOME") {
			continue
		}
		out = append(out, kv)
	}
	if r.codexHome != "" {
		out = append(out, "CODEX_HOME="+r.effectiveCodexHome())
	}
	if r.useCCSwitch {
		if home := r.ccSwitchHome(); home != "" {
			out = append(out, "HOME="+home)
		}
	}
	if r.apiKey != "" {
		out = append(out, "CODEX_API_KEY="+r.apiKey)
	}
	if r.ccDir != "" {
		out = append(out, "CC_SWITCH_CONFIG_DIR="+r.ccDir)
	}
	return out
}

func (r *Runner) effectiveCodexHome() string {
	if !r.useCCSwitch {
		return r.codexHome
	}
	if strings.TrimSpace(r.codexHome) == "" {
		return ""
	}
	return filepath.Join(r.codexHome, "ccswitch-runtime")
}

func (r *Runner) ccSwitchHome() string {
	if !r.useCCSwitch || strings.TrimSpace(r.codexHome) == "" {
		return ""
	}
	return filepath.Join(r.codexHome, "ccswitch-home")
}

func (r *Runner) prepareCodexHome() error {
	home := strings.TrimSpace(r.effectiveCodexHome())
	if home == "" {
		return nil
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("prepare codex home: %w", err)
	}
	if r.useCCSwitch {
		if err := os.Remove(filepath.Join(home, "auth.json")); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove ccswitch auth.json: %w", err)
		}
		ccHome := r.ccSwitchHome()
		if ccHome == "" {
			return nil
		}
		if err := os.MkdirAll(ccHome, 0o700); err != nil {
			return fmt.Errorf("prepare cc-switch home: %w", err)
		}
		codexLink := filepath.Join(ccHome, ".codex")
		if target, err := os.Readlink(codexLink); err == nil && target == home {
			return nil
		}
		if err := os.RemoveAll(codexLink); err != nil {
			return fmt.Errorf("reset cc-switch codex home link: %w", err)
		}
		if err := os.Symlink(home, codexLink); err != nil {
			return fmt.Errorf("link cc-switch codex home: %w", err)
		}
	}
	return nil
}

// reviewBaseArgs returns the flags shared by new and resume reviews.
func (r *Runner) reviewBaseArgs(schemaPath, outPath string) []string {
	args := []string{
		"--json",
		"--output-schema", schemaPath,
		"-o", outPath,
		"-c", "approval_policy=never",
		"-c", "sandbox_mode=" + r.sandbox,
		"--skip-git-repo-check",
	}
	args = r.appendModelConfig(args)
	return args
}

func (r *Runner) appendModelConfig(args []string) []string {
	if r.baseURL != "" {
		args = append(args,
			"-c", "model_provider=\"codex_gitea\"",
			"-c", "model_providers.codex_gitea.name=\"codex-gitea\"",
			"-c", fmt.Sprintf("model_providers.codex_gitea.base_url=%q", r.baseURL),
		)
		if r.apiKey != "" {
			args = append(args, "-c", "model_providers.codex_gitea.env_key=\"CODEX_API_KEY\"")
		}
	}
	if r.reasoning != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", r.reasoning))
	}
	if r.model != "" {
		args = append(args, "--model", r.model)
	}
	return args
}

// Review runs a structured review (new when in.SessionID is empty, otherwise a
// resume of that thread) and returns findings plus the codex session id.
func (r *Runner) Review(ctx context.Context, in model.CodexInput) (*model.ReviewResult, error) {
	if in.Worktree == "" {
		return nil, fmt.Errorf("codex review: empty worktree")
	}

	schemaPath, cleanupSchema, err := writeSchemaTemp()
	if err != nil {
		return nil, err
	}
	defer cleanupSchema()

	outFile, err := os.CreateTemp("", "codex-findings-*.json")
	if err != nil {
		return nil, fmt.Errorf("codex review: create out file: %w", err)
	}
	outPath := outFile.Name()
	outFile.Close()
	defer os.Remove(outPath)

	var args []string
	var prompt string
	if in.SessionID == "" {
		prompt = buildReviewPrompt(in.BaseRef, in.Note)
		args = append([]string{"exec", "-"}, r.reviewBaseArgs(schemaPath, outPath)...)
	} else {
		prompt = buildResumePrompt(in.BaseRef, in.Note)
		args = append([]string{"exec", "resume", in.SessionID, "-"}, r.reviewBaseArgs(schemaPath, outPath)...)
	}
	prompt = validUTF8Prompt(prompt)

	stream, err := r.runWithProvider(ctx, in.Worktree, args, prompt)
	if err != nil {
		return nil, err
	}

	sr, err := parseStream(bytes.NewReader(stream))
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("codex review: read output file: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("codex review: empty output file (no structured result produced)")
	}

	var result model.ReviewResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("codex review: parse output file: %w", err)
	}
	normalizeReviewResult(&result)

	result.SessionID = sr.ThreadID
	if result.SessionID == "" {
		// Fall back to the input session id on resume if the stream omitted it.
		result.SessionID = in.SessionID
	}
	return &result, nil
}

// Ask resumes a session with a free-form question and returns the agent's text.
func (r *Runner) Ask(ctx context.Context, sessionID, worktree, question string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("codex ask: empty session id")
	}
	prompt := validUTF8Prompt(buildAskPrompt(question))
	args := []string{
		"exec", "resume", sessionID, "-",
		"--json",
		"-c", "approval_policy=never",
		"-c", "sandbox_mode=" + r.sandbox,
		"--skip-git-repo-check",
	}
	args = r.appendModelConfig(args)

	stream, err := r.runWithProvider(ctx, worktree, args, prompt)
	if err != nil {
		return "", err
	}
	sr, err := parseStream(bytes.NewReader(stream))
	if err != nil {
		return "", err
	}
	if sr.LastAgentMessage == "" {
		return "", fmt.Errorf("codex ask: no agent message in response")
	}
	return humanizeStructuredAnswer(sr.LastAgentMessage), nil
}

// GenerateText runs a one-shot prompt and returns the final agent message.
func (r *Runner) GenerateText(ctx context.Context, worktree, prompt string) (string, error) {
	if strings.TrimSpace(worktree) == "" {
		worktree = os.TempDir()
	}
	args := []string{
		"exec", "-",
		"--json",
		"-c", "approval_policy=never",
		"-c", "sandbox_mode=" + r.sandbox,
		"--skip-git-repo-check",
	}
	args = r.appendModelConfig(args)
	stream, err := r.runWithProvider(ctx, worktree, args, validUTF8Prompt(prompt))
	if err != nil {
		return "", err
	}
	sr, err := parseStream(bytes.NewReader(stream))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(sr.LastAgentMessage) == "" {
		return "", fmt.Errorf("codex generate text: no agent message in response")
	}
	return strings.TrimSpace(sr.LastAgentMessage), nil
}

func validUTF8Prompt(prompt string) string {
	return strings.ToValidUTF8(prompt, "\uFFFD")
}

// Status reports codex auth state by running `codex login status`.
func (r *Runner) Status(ctx context.Context) (string, error) {
	if r.useCCSwitch || strings.TrimSpace(r.ccProvider) != "" {
		return r.ccSwitchStatus(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.bin, "login", "status")
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		text = r.withConfiguredProviderStatus(text)
		if text != "" {
			return text, fmt.Errorf("codex login status: %w: %s", err, text)
		}
		return "", fmt.Errorf("codex login status: %w", err)
	}
	return r.withConfiguredProviderStatus(text), nil
}

func (r *Runner) withConfiguredProviderStatus(status string) string {
	configured := r.configuredProviderStatus()
	if configured == "" {
		return status
	}
	status = strings.TrimSpace(status)
	if status == "" {
		return configured
	}
	return configured + "\n\n" + status
}

func (r *Runner) configuredProviderStatus() string {
	if strings.TrimSpace(r.baseURL) == "" {
		return ""
	}
	apiKeyStatus := "empty"
	if r.apiKey != "" {
		apiKeyStatus = "set"
	}
	parts := []string{
		"codex configured provider:",
		"Base URL: " + r.baseURL,
		"API key: " + apiKeyStatus,
	}
	if r.model != "" {
		parts = append(parts, "Model: "+r.model)
	}
	if r.reasoning != "" {
		parts = append(parts, "Reasoning effort: "+r.reasoning)
	}
	return strings.Join(parts, "\n")
}

func (r *Runner) ccSwitchStatus(ctx context.Context) (string, error) {
	parts := []string{}
	failures := []string{}
	if current, err := r.runCommand(ctx, "", r.ccBin, []string{"--app", "codex", "provider", "current"}, ""); err == nil {
		parts = append(parts, "cc-switch current:\n"+strings.TrimSpace(string(current)))
	} else {
		parts = append(parts, "cc-switch current failed: "+err.Error())
		failures = append(failures, "cc-switch current: "+err.Error())
	}
	if envCheck, err := r.runCommand(ctx, "", r.ccBin, []string{"--app", "codex", "env", "check"}, ""); err == nil {
		parts = append(parts, "cc-switch env:\n"+strings.TrimSpace(string(envCheck)))
	} else {
		parts = append(parts, "cc-switch env failed: "+err.Error())
		failures = append(failures, "cc-switch env: "+err.Error())
	}
	status := strings.Join(parts, "\n\n")
	if len(failures) > 0 {
		return status, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return status, nil
}

func (r *Runner) applyProvider(ctx context.Context) (string, error) {
	providerID := strings.TrimSpace(r.ccProvider)
	if providerID == "" && r.useCCSwitch && strings.TrimSpace(r.ccBaseURL) != "" {
		resolvedID, err := r.resolveCCSwitchProviderByBaseURL(ctx, r.ccBaseURL)
		if err != nil {
			return "", err
		}
		providerID = resolvedID
		if providerID == "" {
			if err := r.syncCCSwitchDirectCodexConfig(); err != nil {
				return "", err
			}
			return "", nil
		}
	}
	if providerID == "" {
		if !r.useCCSwitch {
			return "", nil
		}
		current, err := r.runCommand(ctx, "", r.ccBin, []string{"--app", "codex", "provider", "current"}, "")
		if err != nil {
			return "", fmt.Errorf("cc-switch codex provider current: %w", err)
		}
		providerID = parseCCSwitchCurrentProviderID(string(current))
		if providerID == "" {
			return "", fmt.Errorf("cc-switch codex provider current: could not parse provider id")
		}
	}
	_, err := r.runCommand(ctx, "", r.ccBin, []string{"--app", "codex", "provider", "switch", providerID}, "")
	if err != nil {
		return "", fmt.Errorf("cc-switch codex provider switch: %w", err)
	}
	if err := r.syncCCSwitchCodexConfig(ctx, providerID); err != nil {
		return "", err
	}
	return providerID, nil
}

func (r *Runner) resolveCCSwitchProviderByBaseURL(ctx context.Context, baseURL string) (string, error) {
	baseURL = normalizeOpenAIBaseURL(baseURL)
	if baseURL == "" {
		return "", nil
	}
	dbPath := filepath.Join(firstNonEmpty(r.ccDir, "/cc-switch"), "cc-switch.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return "", fmt.Errorf("cc-switch codex config: open db: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `SELECT id, settings_config FROM providers WHERE app_type='codex'`)
	if err != nil {
		return "", fmt.Errorf("cc-switch codex config: read providers from %s: %w", dbPath, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, settingsConfig string
		if err := rows.Scan(&id, &settingsConfig); err != nil {
			return "", fmt.Errorf("cc-switch codex config: scan provider: %w", err)
		}
		if normalizeOpenAIBaseURL(baseURLFromCCSwitchProviderConfig(settingsConfig)) == baseURL {
			return strings.TrimSpace(id), nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("cc-switch codex config: read providers: %w", err)
	}
	return "", nil
}

func parseCCSwitchCurrentProviderID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if id, ok := strings.CutPrefix(line, "ID:"); ok {
			return strings.TrimSpace(id)
		}
		if id, ok := strings.CutPrefix(line, "id:"); ok {
			return strings.TrimSpace(id)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "current provider:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "current provider:"))
		}
	}
	return ""
}

func (r *Runner) runWithProvider(ctx context.Context, dir string, args []string, stdin string) ([]byte, error) {
	if !r.useCCSwitch && strings.TrimSpace(r.ccProvider) == "" {
		return r.runCommand(ctx, dir, r.bin, args, stdin)
	}
	providerMu.Lock()
	defer providerMu.Unlock()
	if _, err := r.applyProvider(ctx); err != nil {
		return nil, err
	}
	return r.runCommand(ctx, dir, r.bin, args, stdin)
}

func (r *Runner) syncCCSwitchCodexConfig(ctx context.Context, providerID string) error {
	if !r.useCCSwitch {
		return nil
	}
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return fmt.Errorf("cc-switch codex config: empty provider id")
	}
	dbPath := filepath.Join(firstNonEmpty(r.ccDir, "/cc-switch"), "cc-switch.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("cc-switch codex config: open db: %w", err)
	}
	defer db.Close()

	var settingsConfig string
	var name string
	err = db.QueryRowContext(ctx, `SELECT settings_config, name FROM providers WHERE app_type='codex' AND id=?`, providerID).Scan(&settingsConfig, &name)
	if err != nil {
		return fmt.Errorf("cc-switch codex config: provider %q not found in %s: %w", providerID, dbPath, err)
	}
	configText, err := codexConfigFromCCSwitchProvider(providerID, name, settingsConfig)
	if err != nil {
		return err
	}
	home := r.effectiveCodexHome()
	if home == "" {
		return fmt.Errorf("cc-switch codex config: CODEX_HOME is empty")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("cc-switch codex config: prepare home: %w", err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		return fmt.Errorf("cc-switch codex config: write config.toml: %w", err)
	}
	if !strings.Contains(configText, "base_url") {
		return fmt.Errorf("cc-switch codex config: provider %q config has no base_url", providerID)
	}
	return nil
}

func (r *Runner) syncCCSwitchDirectCodexConfig() error {
	baseURL := strings.TrimSpace(r.ccBaseURL)
	if baseURL == "" {
		return fmt.Errorf("cc-switch codex config: base_url is required when provider is empty")
	}
	configText := codexConfigFromBaseURL("console", "Console", baseURL)
	home := r.effectiveCodexHome()
	if home == "" {
		return fmt.Errorf("cc-switch codex config: CODEX_HOME is empty")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("cc-switch codex config: prepare home: %w", err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		return fmt.Errorf("cc-switch codex config: write config.toml: %w", err)
	}
	return nil
}

func codexConfigFromCCSwitchProvider(providerID, name, settingsConfig string) (string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settingsConfig), &raw); err != nil {
		return "", fmt.Errorf("cc-switch codex config: parse provider %q settings: %w", providerID, err)
	}
	var configText string
	if v, ok := raw["config"]; ok {
		_ = json.Unmarshal(v, &configText)
	}
	configText = strings.TrimSpace(configText)
	if configText != "" {
		return configText + "\n", nil
	}
	var baseURL string
	for _, key := range []string{"base_url", "baseUrl", "baseURL"} {
		if v, ok := raw[key]; ok {
			_ = json.Unmarshal(v, &baseURL)
			baseURL = strings.TrimSpace(baseURL)
			if baseURL != "" {
				break
			}
		}
	}
	if baseURL == "" {
		return "", fmt.Errorf("cc-switch codex config: provider %q has neither config nor base_url", providerID)
	}
	return codexConfigFromBaseURL(firstNonEmpty(providerID, name, "ccswitch"), firstNonEmpty(name, providerID, "cc-switch"), baseURL), nil
}

func baseURLFromCCSwitchProviderConfig(settingsConfig string) string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settingsConfig), &raw); err != nil {
		return ""
	}
	for _, key := range []string{"base_url", "baseUrl", "baseURL"} {
		if v, ok := raw[key]; ok {
			var baseURL string
			_ = json.Unmarshal(v, &baseURL)
			if strings.TrimSpace(baseURL) != "" {
				return strings.TrimSpace(baseURL)
			}
		}
	}
	var configText string
	if v, ok := raw["config"]; ok {
		_ = json.Unmarshal(v, &configText)
	}
	_, _, baseURL := parseCodexTOMLModelConfig(configText)
	return baseURL
}

func parseCodexTOMLModelConfig(configText string) (string, string, string) {
	var modelID, reasoning, baseURL string
	for _, line := range strings.Split(configText, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "model = "); ok {
			modelID = strings.Trim(strings.TrimSpace(value), `"`)
		}
		if value, ok := strings.CutPrefix(line, "model_reasoning_effort = "); ok {
			reasoning = strings.Trim(strings.TrimSpace(value), `"`)
		}
		if value, ok := strings.CutPrefix(line, "base_url = "); ok {
			baseURL = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return modelID, reasoning, baseURL
}

func codexConfigFromBaseURL(providerID, name, baseURL string) string {
	providerName := sanitizeProviderID(firstNonEmpty(providerID, name, "ccswitch"))
	displayName := strings.ReplaceAll(firstNonEmpty(name, providerID, "cc-switch"), `"`, `\"`)
	return fmt.Sprintf("model_provider = %q\n[model_providers.%s]\nname = %q\nbase_url = %q\n", providerName, providerName, displayName, normalizeOpenAIBaseURL(baseURL))
}

func sanitizeProviderID(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "ccswitch"
	}
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

var providerMu sync.Mutex

func (r *Runner) runCommand(ctx context.Context, dir, bin string, args []string, stdin string) ([]byte, error) {
	if err := r.prepareCodexHome(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = r.env()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%s timed out after %s", bin, r.timeout)
		}
		msg := strings.TrimSpace(strings.Join([]string{stderr.String(), stdout.String()}, "\n"))
		if msg != "" {
			return nil, fmt.Errorf("%s failed: %w: %s", bin, err, msg)
		}
		return nil, fmt.Errorf("%s failed: %w", bin, err)
	}
	return stdout.Bytes(), nil
}

func formatCommandFailure(stderr, stdout string) string {
	stderr = strings.TrimSpace(stderr)
	stdout = strings.TrimSpace(stdout)
	switch {
	case stderr != "" && stdout != "":
		return "stderr:\n" + stderr + "\n\nstdout:\n" + stdout
	case stderr != "":
		return stderr
	case stdout != "":
		return stdout
	default:
		return ""
	}
}

func normalizeReviewResult(result *model.ReviewResult) {
	if result == nil {
		return
	}
	trimmed := strings.TrimSpace(result.Summary)
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	var nested model.ReviewResult
	if err := json.Unmarshal([]byte(trimmed), &nested); err != nil {
		return
	}
	if strings.TrimSpace(nested.Summary) == "" && len(nested.Findings) == 0 && nested.OverallSeverity == "" {
		return
	}
	sessionID := result.SessionID
	*result = nested
	result.SessionID = sessionID
}

func humanizeStructuredAnswer(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") {
		return text
	}
	var result model.ReviewResult
	if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
		return text
	}
	if strings.TrimSpace(result.Summary) == "" && len(result.Findings) == 0 && result.OverallSeverity == "" {
		return text
	}
	var b strings.Builder
	b.WriteString(result.Summary)
	if result.OverallSeverity != "" {
		fmt.Fprintf(&b, "\n\n整体严重程度：**%s**", result.OverallSeverity)
	}
	if len(result.Findings) > 0 {
		b.WriteString("\n\n需要关注的问题：\n")
		for _, f := range result.Findings {
			fmt.Fprintf(&b, "- **[%s] %s** (`%s:%d`): %s\n",
				strings.ToUpper(string(f.Severity)), f.Title, f.Path, f.Line, f.Body)
		}
	}
	return strings.TrimSpace(b.String())
}
