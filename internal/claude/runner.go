package claude

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

const defaultTimeout = 30 * time.Minute
const readOnlyTools = "Read,Grep,Glob,Bash(git diff:*),Bash(git show:*),Bash(git status:*),Bash(git ls-files:*)"

//go:embed findings.schema.json
var findingsSchema []byte

type Options struct {
	Name              string
	Bin               string
	Model             string
	APIKey            string
	BaseURL           string
	ClaudeHome        string
	CCSwitchBin       string
	CCSwitchConfigDir string
	CCSwitchProvider  string
	Timeout           time.Duration
	MaxBudgetUSD      float64
}

type Runner struct {
	name              string
	bin               string
	model             string
	apiKey            string
	baseURL           string
	claudeHome        string
	ccSwitchBin       string
	ccSwitchConfigDir string
	ccSwitchProvider  string
	timeout           time.Duration
	maxBudgetUSD      float64
}

var _ model.Reviewer = (*Runner)(nil)

var providerMu sync.Mutex

func New(opts Options) *Runner {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = "claude"
	}
	bin := opts.Bin
	if env := os.Getenv("CLAUDE_BIN"); env != "" {
		bin = env
	}
	if bin == "" {
		bin = "claude"
	}
	ccSwitchBin := opts.CCSwitchBin
	if ccSwitchBin == "" {
		ccSwitchBin = "cc-switch"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Runner{
		name:              name,
		bin:               bin,
		model:             opts.Model,
		apiKey:            opts.APIKey,
		baseURL:           opts.BaseURL,
		claudeHome:        opts.ClaudeHome,
		ccSwitchBin:       ccSwitchBin,
		ccSwitchConfigDir: opts.CCSwitchConfigDir,
		ccSwitchProvider:  opts.CCSwitchProvider,
		timeout:           timeout,
		maxBudgetUSD:      opts.MaxBudgetUSD,
	}
}

func (r *Runner) Name() string { return r.name }

func (r *Runner) Review(ctx context.Context, in model.CodexInput) (*model.ReviewResult, error) {
	if in.Worktree == "" {
		return nil, fmt.Errorf("%s review: empty worktree", r.Name())
	}
	args := []string{
		"--print",
		"--output-format", "json",
		"--json-schema", string(findingsSchema),
		"--permission-mode", "dontAsk",
		"--allowedTools", readOnlyTools,
	}
	args = r.appendBudgetArg(args)
	if r.model != "" {
		args = append(args, "--model", r.model)
	}
	if in.SessionID != "" {
		args = append(args, "--resume", in.SessionID)
	}
	args = append(args, buildReviewPrompt(in.BaseRef, in.Note, in.SessionID != ""))
	out, err := r.runWithProvider(ctx, in.Worktree, r.bin, args, "")
	if err != nil {
		return nil, err
	}
	result, sessionID, err := parseReviewOutputForAgent(out, r.Name())
	if err != nil {
		if fallback, fallbackSessionID, fallbackErr := r.parseReviewResultFromSessionLog(out, in.Worktree); fallbackErr == nil {
			if fallbackSessionID != "" {
				fallback.SessionID = fallbackSessionID
			} else {
				fallback.SessionID = in.SessionID
			}
			return fallback, nil
		}
		return nil, err
	}
	if sessionID != "" {
		result.SessionID = sessionID
	} else {
		result.SessionID = in.SessionID
	}
	return result, nil
}

func (r *Runner) Ask(ctx context.Context, sessionID, worktree, question string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("%s ask: empty session id", r.Name())
	}
	args := []string{
		"--print",
		"--output-format", "json",
		"--permission-mode", "dontAsk",
		"--allowedTools", readOnlyTools,
	}
	args = r.appendBudgetArg(args)
	if r.model != "" {
		args = append(args, "--model", r.model)
	}
	args = append(args, "--resume", sessionID, buildAskPrompt(question))
	out, err := r.runWithProvider(ctx, worktree, r.bin, args, "")
	if err != nil {
		return "", err
	}
	return parseTextOutput(out)
}

func (r *Runner) GenerateText(ctx context.Context, worktree, prompt string) (string, error) {
	args := []string{
		"--print",
		"--output-format", "json",
		"--permission-mode", "dontAsk",
		"--tools", "",
	}
	args = r.appendBudgetArg(args)
	if r.model != "" {
		args = append(args, "--model", r.model)
	}
	args = append(args, prompt)
	out, err := r.runWithProvider(ctx, worktree, r.bin, args, "")
	if err != nil {
		return "", err
	}
	return parseTextOutput(out)
}

func (r *Runner) appendBudgetArg(args []string) []string {
	if r.maxBudgetUSD <= 0 {
		return args
	}
	return append(args, "--max-budget-usd", strconv.FormatFloat(r.maxBudgetUSD, 'f', -1, 64))
}

func (r *Runner) Status(ctx context.Context) (string, error) {
	parts := []string{}
	failures := []string{}
	requireProvider := strings.TrimSpace(r.ccSwitchProvider) != ""
	if requireProvider {
		if current, err := r.run(ctx, "", r.ccSwitchBin, []string{"--app", "claude", "provider", "current"}, ""); err == nil {
			parts = append(parts, "cc-switch current:\n"+strings.TrimSpace(string(current)))
		} else {
			parts = append(parts, "cc-switch current failed: "+err.Error())
			failures = append(failures, "cc-switch current: "+err.Error())
		}
		if envCheck, err := r.run(ctx, "", r.ccSwitchBin, []string{"--app", "claude", "env", "check"}, ""); err == nil {
			parts = append(parts, "cc-switch env:\n"+strings.TrimSpace(string(envCheck)))
		} else {
			parts = append(parts, "cc-switch env failed: "+err.Error())
			failures = append(failures, "cc-switch env: "+err.Error())
		}
	} else {
		parts = append(parts, "cc-switch: skipped (provider id not configured; using Claude API env)")
	}
	if auth, err := r.run(ctx, "", r.bin, []string{"auth", "status"}, ""); err == nil {
		parts = append(parts, "claude auth:\n"+strings.TrimSpace(string(auth)))
	} else {
		parts = append(parts, "claude auth failed: "+err.Error())
		failures = append(failures, "claude auth: "+err.Error())
	}
	if smoke, err := r.smokeTest(ctx); err == nil {
		parts = append(parts, r.Name()+" api smoke test:\n"+smoke)
	} else {
		parts = append(parts, r.Name()+" api smoke test failed: "+err.Error())
		failures = append(failures, r.Name()+" api smoke test: "+err.Error())
	}
	status := strings.Join(parts, "\n\n")
	if len(failures) > 0 {
		return status, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return status, nil
}

func (r *Runner) smokeTest(ctx context.Context) (string, error) {
	args := []string{
		"--print",
		"--output-format", "json",
		"--permission-mode", "dontAsk",
	}
	args = r.appendBudgetArg(args)
	if r.model != "" {
		args = append(args, "--model", r.model)
	}
	args = append(args, "Return exactly OK. Do not use tools.")
	out, err := r.runWithProvider(ctx, "", r.bin, args, "")
	if err != nil {
		return "", err
	}
	text, err := parseTextOutput(out)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "OK", nil
	}
	return trimForError(text), nil
}

func (r *Runner) applyProvider(ctx context.Context) error {
	if strings.TrimSpace(r.ccSwitchProvider) == "" {
		return nil
	}
	_, err := r.run(ctx, "", r.ccSwitchBin, []string{"--app", "claude", "provider", "switch", r.ccSwitchProvider}, "")
	if err != nil {
		return fmt.Errorf("cc-switch provider switch: %w", err)
	}
	return nil
}

func (r *Runner) runWithProvider(ctx context.Context, dir, bin string, args []string, stdin string) ([]byte, error) {
	if strings.TrimSpace(r.ccSwitchProvider) == "" {
		return r.run(ctx, dir, bin, args, stdin)
	}
	providerMu.Lock()
	defer providerMu.Unlock()
	if err := r.applyProvider(ctx); err != nil {
		return nil, err
	}
	return r.run(ctx, dir, bin, args, stdin)
}

func (r *Runner) env() []string {
	out := make([]string, 0, len(os.Environ())+4)
	drop := map[string]bool{
		"GITEA_TOKEN": true, "GITEA_API_TOKEN": true, "GITEA_PASSWORD": true,
		"OPENAI_API_KEY": true, "OPENAI_TOKEN": true,
	}
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 || drop[kv[:eq]] {
			continue
		}
		out = append(out, kv)
	}
	if r.claudeHome != "" {
		out = append(out, "HOME="+r.claudeHome)
		out = append(out, "CLAUDE_CONFIG_DIR="+r.claudeHome)
	}
	if r.ccSwitchConfigDir != "" {
		out = append(out, "CC_SWITCH_CONFIG_DIR="+r.ccSwitchConfigDir)
	}
	if r.apiKey != "" {
		out = append(out, "ANTHROPIC_API_KEY="+r.apiKey)
	}
	if r.baseURL != "" {
		out = append(out, "ANTHROPIC_BASE_URL="+r.baseURL)
	}
	return out
}

func (r *Runner) run(ctx context.Context, dir, bin string, args []string, stdin string) ([]byte, error) {
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
		if structuredErr := parseStructuredCLIError(stdout.Bytes(), r.Name()); structuredErr != nil {
			return nil, structuredErr
		}
		msg := formatCommandFailure(stderr.String(), stdout.String())
		if msg != "" {
			return nil, fmt.Errorf("%s failed: %w: %s", bin, err, msg)
		}
		return nil, fmt.Errorf("%s failed: %w", bin, err)
	}
	return stdout.Bytes(), nil
}

func parseReviewOutput(out []byte) (*model.ReviewResult, string, error) {
	return parseReviewOutputForAgent(out, "claude")
}

type cliResultEnvelope struct {
	Result         any    `json:"result"`
	SessionID      string `json:"session_id"`
	IsError        bool   `json:"is_error"`
	APIErrorStatus int    `json:"api_error_status"`
}

func parseReviewOutputForAgent(out []byte, agent string) (*model.ReviewResult, string, error) {
	var direct model.ReviewResult
	if err := json.Unmarshal(bytes.TrimSpace(out), &direct); err == nil && (direct.Summary != "" || len(direct.Findings) > 0 || direct.OverallSeverity != "") {
		return &direct, "", nil
	}
	var wrapped cliResultEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(out), &wrapped); err != nil {
		return nil, "", fmt.Errorf("%s review: parse output: %w", agent, err)
	}
	if wrapped.IsError {
		msg := resultMessage(wrapped.Result)
		if wrapped.APIErrorStatus != 0 {
			return nil, "", fmt.Errorf("%s api error %d: %s", agent, wrapped.APIErrorStatus, msg)
		}
		return nil, "", fmt.Errorf("%s api error: %s", agent, msg)
	}
	switch v := wrapped.Result.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, "", fmt.Errorf("%s review: empty result string", agent)
		}
		if err := json.Unmarshal([]byte(v), &direct); err != nil {
			return nil, "", fmt.Errorf("%s review: parse result string: %w", agent, err)
		}
	case map[string]any:
		data, _ := json.Marshal(v)
		if err := json.Unmarshal(data, &direct); err != nil {
			return nil, "", fmt.Errorf("%s review: parse result object: %w", agent, err)
		}
	default:
		return nil, "", fmt.Errorf("%s review: missing structured result", agent)
	}
	return &direct, wrapped.SessionID, nil
}

func (r *Runner) parseReviewResultFromSessionLog(out []byte, worktree string) (*model.ReviewResult, string, error) {
	sessionID := sessionIDFromCLIOutput(out)
	if sessionID == "" {
		return nil, "", fmt.Errorf("%s review: session id missing", r.Name())
	}
	if strings.TrimSpace(r.claudeHome) == "" {
		return nil, "", fmt.Errorf("%s review: claude home missing", r.Name())
	}
	result, err := findStructuredOutputInClaudeHome(r.claudeHome, worktree, sessionID)
	if err != nil {
		return nil, "", err
	}
	return result, sessionID, nil
}

func sessionIDFromCLIOutput(out []byte) string {
	var wrapped cliResultEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(out), &wrapped); err != nil {
		return ""
	}
	return strings.TrimSpace(wrapped.SessionID)
}

func findStructuredOutputInClaudeHome(claudeHome, worktree, sessionID string) (*model.ReviewResult, error) {
	projectsDir := filepath.Join(claudeHome, "projects")
	var candidates []string
	if worktree != "" {
		projectDir := filepath.Join(projectsDir, claudeProjectDirName(worktree))
		candidates = append(candidates, projectDir)
	}
	candidates = append(candidates, projectsDir)

	var lastErr error
	for _, root := range candidates {
		result, err := findStructuredOutputInDir(root, sessionID)
		if err == nil {
			return result, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("claude structured output not found for session %s", sessionID)
}

func claudeProjectDirName(worktree string) string {
	name := filepath.ToSlash(filepath.Clean(worktree))
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "__", "--")
	return name
}

func findStructuredOutputInDir(root, sessionID string) (*model.ReviewResult, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}

	var latest *model.ReviewResult
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		result, err := structuredOutputFromClaudeJSONL(path, sessionID)
		if err != nil {
			return nil
		}
		if result != nil {
			latest = result
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, fmt.Errorf("claude structured output not found for session %s", sessionID)
	}
	return latest, nil
}

func structuredOutputFromClaudeJSONL(path, sessionID string) (*model.ReviewResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var latest *model.ReviewResult
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var line struct {
			SessionID  string `json:"sessionId"`
			Attachment struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"data"`
			} `json:"attachment"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if strings.TrimSpace(line.SessionID) != sessionID || line.Attachment.Type != "structured_output" || len(line.Attachment.Data) == 0 {
			continue
		}
		var result model.ReviewResult
		if err := json.Unmarshal(line.Attachment.Data, &result); err != nil {
			continue
		}
		if result.Summary != "" || len(result.Findings) > 0 || result.OverallSeverity != "" {
			latest = &result
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return latest, nil
}

func parseStructuredCLIError(out []byte, agent string) error {
	_, _, err := parseReviewOutputForAgent(out, agent)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), " api error") {
		return err
	}
	return nil
}

func resultMessage(v any) string {
	switch x := v.(type) {
	case string:
		return trimForError(x)
	case map[string]any:
		data, _ := json.Marshal(x)
		return trimForError(string(data))
	default:
		return trimForError(fmt.Sprint(x))
	}
}

func trimForError(s string) string {
	s = strings.TrimSpace(s)
	const max = 600
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func parseTextOutput(out []byte) (string, error) {
	var wrapped struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &wrapped); err == nil && strings.TrimSpace(wrapped.Result) != "" {
		return strings.TrimSpace(wrapped.Result), nil
	}
	return strings.TrimSpace(string(out)), nil
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

func buildReviewPrompt(baseRef, note string, resume bool) string {
	if baseRef == "" {
		baseRef = "main"
	}
	verb := "performing a STATIC review"
	if resume {
		verb = "re-reviewing a pull request STATICALLY"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `You are a senior code reviewer %s.

Use the pull-request diff from the target branch to the current HEAD as the entrypoint.
Start with simple read-only commands such as `+"`git diff --name-only %s...HEAD`"+`, `+"`git diff --stat %s...HEAD`"+`, and `+"`git diff --numstat %s...HEAD`"+`.
Inspect concrete file diffs with `+"`git diff %s...HEAD -- <path>`"+` and related source with Read/Grep/Glob.
For large pull requests, review changed files in batches; do not print or save the whole diff at once.

Hard rules:
- Do NOT build, run, compile, or test the code.
- Do NOT modify any files.
- Only read-only inspection commands are allowed.
- Do NOT use shell redirection, pipes, `+"`&&`"+`, `+"`;`"+`, command substitution, or temp files. Run one allowed read-only command at a time.
- Allowed Bash commands are limited to simple git inspection commands like `+"`git diff ...`"+`, `+"`git show ...`"+`, `+"`git status ...`"+`, and `+"`git ls-files ...`"+`.
- Use the allowed read-only tools to inspect the diff and relevant surrounding code; do not rely on filenames alone.
- Report only risks introduced or exposed by this PR.
- If the additional context includes "Repository guidance context", read it before reviewing the diff and use it as the starting set of repository instructions.
- If the additional context includes "Current PR diff inventory", treat it as the authoritative coverage checklist for this review. Use it to plan file-by-file coverage before deciding findings.
- This is not a top-N review. Continue until every changed file and its relevant call paths have been checked.
- Report every concrete PR-caused risk you can substantiate, including low and medium severity issues. Do not stop after finding the first high-impact issue.
- If a changed file looks safe, still consider whether its callers, migrations, API consumers, or persisted data contracts make the change risky.
- Do not report pure style, readability, naming, formatting, or speculative maintainability feedback unless it creates a concrete bug risk.
- Write all human-facing output in Simplified Chinese by default.
- Summary, finding titles, and finding bodies MUST be Simplified Chinese. If you reasoned in English, translate the final structured output before returning.
- Inline comments are generated directly from finding titles and finding bodies, so every title/body field MUST be Simplified Chinese.
- Do not call the StructuredOutput tool, and do not return the final JSON, until summary, every finding title, and every finding body have been translated to Simplified Chinese.
- The summary must start by naming the concrete problem areas a developer should fix; do not leave the actionable problem list only in inline findings.
- For every finding include path, line, side, severity, title, body, and a tags array with zero to six short free-form tags.
- Include resolved_fingerprints for prior findings that are clearly fixed.

Return ONLY JSON conforming to the provided schema.`, verb, baseRef, baseRef, baseRef, baseRef)
	if strings.TrimSpace(note) != "" {
		fmt.Fprintf(&b, "\n\nAdditional context:\n%s", note)
	}
	return b.String()
}

func buildAskPrompt(question string) string {
	return "用简体中文回答这个 PR review follow-up。不要修改文件。\n\n问题：\n" + strings.TrimSpace(question)
}
