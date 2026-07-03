package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

func writeClaudeStub(t *testing.T, logPath string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "claude-stub.sh")
	body := `#!/bin/sh
if [ -n "$STUB_SLEEP" ]; then
  sleep "$STUB_SLEEP"
fi
{
  echo "CWD=$(pwd)"
  echo "HOME=${HOME}"
  echo "CC_SWITCH_CONFIG_DIR=${CC_SWITCH_CONFIG_DIR}"
  echo "ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY}"
  echo "ANTHROPIC_BASE_URL=${ANTHROPIC_BASE_URL}"
  echo "GITEA_TOKEN=${GITEA_TOKEN}"
  i=0
  for a in "$@"; do
    echo "ARG[$i]=$a"
    i=$((i+1))
  done
} >> "$STUB_LOG"
if [ "$1" = "auth" ]; then
  echo "Claude auth ok"
  exit 0
fi
if [ -n "$STUB_ERROR_JSON" ]; then
  printf '%s\n' "$STUB_ERROR_JSON"
  exit 1
fi
case "$*" in
  *"Return exactly OK. Do not use tools."*)
    cat <<'JSON'
{"type":"result","session_id":"claude-smoke-session","result":"OK"}
JSON
    exit 0
    ;;
esac
cat <<'JSON'
{"type":"result","session_id":"claude-session-1","result":"{\"summary\":\"claude summary\",\"overall_severity\":\"high\",\"findings\":[{\"path\":\"calc.go\",\"line\":19,\"side\":\"NEW\",\"severity\":\"high\",\"title\":\"bad math\",\"body\":\"boom\",\"tags\":[\"runtime\"]}],\"resolved_fingerprints\":[]}"}
JSON
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("STUB_LOG", logPath)
	return script
}

func TestRunnerReview(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	bin := writeClaudeStub(t, logPath)
	t.Setenv("GITEA_TOKEN", "secret")

	worktree := t.TempDir()
	home := t.TempDir()
	ccDir := t.TempDir()
	r := New(Options{
		Bin: bin, ClaudeHome: home, CCSwitchConfigDir: ccDir, Model: "sonnet",
		APIKey: "ak-claude", BaseURL: "https://relay.example", MaxBudgetUSD: 0.3,
	})
	res, err := r.Review(context.Background(), model.CodexInput{Worktree: worktree, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.SessionID != "claude-session-1" || res.Summary != "claude summary" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(res.Findings) != 1 || res.Findings[0].Tags[0] != "runtime" {
		t.Fatalf("finding tags not parsed: %+v", res.Findings)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(raw)
	for _, want := range []string{
		"ARG[0]=--print",
		"--output-format",
		"--json-schema",
		"--permission-mode",
		"--allowedTools",
		"Bash(git diff:*)",
		"--max-budget-usd",
		"0.3",
		"not a top-N review",
		"--model",
		"sonnet",
		"git diff --name-only main...HEAD",
		"git diff main...HEAD -- <path>",
		"Do NOT use shell redirection",
		"HOME=" + home,
		"CC_SWITCH_CONFIG_DIR=" + ccDir,
		"ANTHROPIC_API_KEY=ak-claude",
		"ANTHROPIC_BASE_URL=https://relay.example",
		"GITEA_TOKEN=\n",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "Bash(*)") {
		t.Fatalf("review should not allow unrestricted shell tools:\n%s", log)
	}
	if !strings.Contains(log, "Read,Grep,Glob,Bash(git diff:*),Bash(git show:*),Bash(git status:*),Bash(git ls-files:*)") {
		t.Fatalf("review should allow only read-only inspection tools:\n%s", log)
	}
}

func TestRunnerReviewOmitsBudgetWhenDisabled(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	bin := writeClaudeStub(t, logPath)

	r := New(Options{Bin: bin, Model: "sonnet", MaxBudgetUSD: 0})
	if _, err := r.Review(context.Background(), model.CodexInput{Worktree: t.TempDir(), BaseRef: "main"}); err != nil {
		t.Fatalf("Review: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(raw), "--max-budget-usd") {
		t.Fatalf("budget arg should be omitted when disabled:\n%s", raw)
	}
}

func TestRunnerReviewCanUseNamedCCSwitchProvider(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	bin := writeClaudeStub(t, logPath)

	r := New(Options{
		Name:             "minimax",
		Bin:              bin,
		CCSwitchBin:      bin,
		CCSwitchProvider: "minimaxreview",
		Model:            "minimax-m3",
		APIKey:           "ak-minimax",
		BaseURL:          "https://minimax-relay.example.com",
	})
	res, err := r.Review(context.Background(), model.CodexInput{Worktree: t.TempDir(), BaseRef: "main"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if r.Name() != "minimax" {
		t.Fatalf("Name = %q, want minimax", r.Name())
	}
	if res.SessionID != "claude-session-1" {
		t.Fatalf("SessionID = %q, want claude-session-1", res.SessionID)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(raw)
	for _, want := range []string{
		"ARG[0]=--app",
		"ARG[1]=claude",
		"ARG[2]=provider",
		"ARG[3]=switch",
		"ARG[4]=minimaxreview",
		"--model",
		"minimax-m3",
		"ANTHROPIC_API_KEY=ak-minimax",
		"ANTHROPIC_BASE_URL=https://minimax-relay.example.com",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q:\n%s", want, log)
		}
	}
}

func TestRunnerReviewCanUseDirectBaseURLWithoutProviderSwitch(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	bin := writeClaudeStub(t, logPath)

	r := New(Options{
		Name:    "minimax",
		Bin:     bin,
		Model:   "minimax-m3",
		APIKey:  "ak-minimax",
		BaseURL: "https://minimax-relay.example.com",
	})
	if _, err := r.Review(context.Background(), model.CodexInput{Worktree: t.TempDir(), BaseRef: "main"}); err != nil {
		t.Fatalf("Review: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(raw)
	if strings.Contains(log, "ARG[0]=--app") || strings.Contains(log, "ARG[2]=provider") {
		t.Fatalf("direct Base URL mode should not switch cc-switch provider:\n%s", log)
	}
	for _, want := range []string{
		"ANTHROPIC_API_KEY=ak-minimax",
		"ANTHROPIC_BASE_URL=https://minimax-relay.example.com",
		"--model",
		"minimax-m3",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q:\n%s", want, log)
		}
	}
}

func TestRunnerReviewFallsBackToClaudeStructuredOutputLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude-stub.sh")
	body := `#!/bin/sh
{
  echo "CWD=$(pwd)"
  i=0
  for a in "$@"; do
    echo "ARG[$i]=$a"
    i=$((i+1))
  done
} >> "$STUB_LOG"
cat <<'JSON'
{"type":"result","session_id":"session-from-stdout","result":"## Markdown review\n\nStructured output was provided separately."}
JSON
`
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("STUB_LOG", logPath)

	claudeHome := t.TempDir()
	projectDir := filepath.Join(claudeHome, "projects", "some-project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	transcript := `{"sessionId":"session-from-stdout","attachment":{"type":"structured_output","data":{"summary":"from structured output","overall_severity":"medium","findings":[{"path":"src/app.go","line":12,"side":"NEW","severity":"medium","title":"real finding","body":"details","tags":["runtime"]}],"resolved_fingerprints":[]}}}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, "session.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	r := New(Options{Name: "minimax", Bin: bin, ClaudeHome: claudeHome})
	res, err := r.Review(context.Background(), model.CodexInput{Worktree: t.TempDir(), BaseRef: "main"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.SessionID != "session-from-stdout" || res.Summary != "from structured output" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(res.Findings) != 1 || res.Findings[0].Title != "real finding" {
		t.Fatalf("findings not loaded from structured output: %+v", res.Findings)
	}
}

func TestRunnerReviewTimeout(t *testing.T) {
	bin := writeClaudeStub(t, filepath.Join(t.TempDir(), "claude.log"))
	t.Setenv("STUB_SLEEP", "5")
	r := New(Options{Bin: bin, Timeout: 100 * time.Millisecond})
	_, err := r.Review(context.Background(), model.CodexInput{Worktree: t.TempDir(), BaseRef: "main"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
}

func TestParseReviewOutputReportsClaudeAPIError(t *testing.T) {
	_, _, err := parseReviewOutput([]byte(`{"type":"result","is_error":true,"api_error_status":503,"result":"API Error: no available channel"}`))
	if err == nil || !strings.Contains(err.Error(), "claude api error 503: API Error: no available channel") {
		t.Fatalf("err = %v, want claude api error", err)
	}
}

func TestRunnerReviewReportsStructuredCLIAPIError(t *testing.T) {
	bin := writeClaudeStub(t, filepath.Join(t.TempDir(), "claude.log"))
	t.Setenv("STUB_ERROR_JSON", `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Invalid API key · Fix external API key","session_id":"s1"}`)
	r := New(Options{Name: "minimax", Bin: bin})
	_, err := r.Review(context.Background(), model.CodexInput{Worktree: t.TempDir(), BaseRef: "main"})
	if err == nil {
		t.Fatal("Review err = nil, want structured API error")
	}
	if got := err.Error(); !strings.Contains(got, "minimax api error 401: Invalid API key") || strings.Contains(got, `"type":"result"`) {
		t.Fatalf("err = %q, want concise minimax API error without raw JSON", got)
	}
}

func TestRunnerStatusReturnsErrorWhenChecksFail(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-claude")
	r := New(Options{Bin: missing, CCSwitchBin: missing, Timeout: 100 * time.Millisecond})
	status, err := r.Status(context.Background())
	if err == nil {
		t.Fatalf("Status err = nil, want failure")
	}
	if !strings.Contains(status, "cc-switch: skipped") || !strings.Contains(status, "claude auth failed") {
		t.Fatalf("status missing failure details:\n%s", status)
	}
}

func TestRunnerStatusDoesNotRequireCCSwitchWithoutProvider(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	bin := writeClaudeStub(t, logPath)
	t.Setenv("STUB_SLEEP", "")
	missing := filepath.Join(t.TempDir(), "missing-cc-switch")
	r := New(Options{Bin: bin, CCSwitchBin: missing, Timeout: time.Second})
	status, err := r.Status(context.Background())
	if err != nil {
		t.Fatalf("Status err = %v, want nil when cc-switch provider is not configured\n%s", err, status)
	}
	if !strings.Contains(status, "cc-switch: skipped") || !strings.Contains(status, "claude auth") {
		t.Fatalf("status missing optional cc-switch/auth details:\n%s", status)
	}
	if !strings.Contains(status, "claude api smoke test:\nOK") {
		t.Fatalf("status missing real API smoke test:\n%s", status)
	}
}

func TestRunnerStatusRequiresCCSwitchWhenProviderConfigured(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	bin := writeClaudeStub(t, logPath)
	t.Setenv("STUB_SLEEP", "")
	missing := filepath.Join(t.TempDir(), "missing-cc-switch")
	r := New(Options{Bin: bin, CCSwitchBin: missing, CCSwitchProvider: "relay", Timeout: time.Second})
	status, err := r.Status(context.Background())
	if err == nil {
		t.Fatalf("Status err = nil, want cc-switch failure when provider is configured\n%s", status)
	}
}

func TestRunnerStatusReportsSmokeAPIError(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	bin := writeClaudeStub(t, logPath)
	t.Setenv("STUB_ERROR_JSON", `{"type":"result","is_error":true,"api_error_status":401,"result":"Invalid API key · Fix external API key"}`)
	r := New(Options{Name: "minimax", Bin: bin, Timeout: time.Second})
	status, err := r.Status(context.Background())
	if err == nil {
		t.Fatalf("Status err = nil, want smoke failure\n%s", status)
	}
	if !strings.Contains(status, "minimax api smoke test failed: minimax api error 401: Invalid API key") {
		t.Fatalf("status missing concise smoke API error:\n%s", status)
	}
}

func TestRunnerGenerateTextDisablesTools(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	bin := writeClaudeStub(t, logPath)
	r := New(Options{Bin: bin, Model: "sonnet"})
	if _, err := r.GenerateText(context.Background(), t.TempDir(), "Return exactly OK."); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(raw)
	for _, want := range []string{
		"ARG[0]=--print",
		"ARG[5]=--tools",
		"ARG[6]=",
		"ARG[7]=--model",
		"ARG[8]=sonnet",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in log:\n%s", want, log)
		}
	}
}
