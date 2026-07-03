package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
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
		if secretEnvKeys[kv[:eq]] {
			continue
		}
		out = append(out, kv)
	}
	if r.codexHome != "" {
		out = append(out, "CODEX_HOME="+r.codexHome)
	}
	if r.apiKey != "" {
		out = append(out, "CODEX_API_KEY="+r.apiKey)
	}
	if r.ccDir != "" {
		out = append(out, "CC_SWITCH_CONFIG_DIR="+r.ccDir)
	}
	return out
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
		status, err := r.ccSwitchStatus(ctx)
		return r.withConfiguredProviderStatus(status), err
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

func (r *Runner) applyProvider(ctx context.Context) error {
	if strings.TrimSpace(r.ccProvider) == "" {
		return nil
	}
	_, err := r.runCommand(ctx, "", r.ccBin, []string{"--app", "codex", "provider", "switch", r.ccProvider}, "")
	if err != nil {
		return fmt.Errorf("cc-switch codex provider switch: %w", err)
	}
	return nil
}

func (r *Runner) runWithProvider(ctx context.Context, dir string, args []string, stdin string) ([]byte, error) {
	if strings.TrimSpace(r.ccProvider) == "" {
		return r.runCommand(ctx, dir, r.bin, args, stdin)
	}
	providerMu.Lock()
	defer providerMu.Unlock()
	if err := r.applyProvider(ctx); err != nil {
		return nil, err
	}
	return r.runCommand(ctx, dir, r.bin, args, stdin)
}

var providerMu sync.Mutex

func (r *Runner) runCommand(ctx context.Context, dir, bin string, args []string, stdin string) ([]byte, error) {
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
