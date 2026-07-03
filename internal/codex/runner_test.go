package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/turning4th/codex-gitea/internal/model"
)

// writeStub creates an executable stub codex script in a temp dir and points
// CODEX_BIN at it. The stub:
//   - on `login status`: prints a fixed auth line.
//   - on `exec [resume ...]`: prints a thread.started + agent_message stream to
//     stdout, and (if a -o path is given) writes a valid findings JSON there.
//
// It logs its cwd, selected env vars, and argv to logPath for assertions.
// If STUB_SLEEP is set in the env, it sleeps that many seconds first (timeout test).
func writeStub(t *testing.T, logPath string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "codex-stub.sh")

	body := `#!/bin/sh
if [ -n "$STUB_SLEEP" ]; then
  sleep "$STUB_SLEEP"
fi

if [ "$1" = "login" ]; then
  echo "Logged in using ChatGPT account (stub)"
  exit 0
fi

# exec mode: log context for assertions
stdin_payload="$(cat)"
{
  echo "CWD=$(pwd)"
  echo "GITEA_TOKEN=${GITEA_TOKEN}"
  echo "OPENAI_API_KEY=${OPENAI_API_KEY}"
  echo "CODEX_HOME=${CODEX_HOME}"
  echo "CODEX_API_KEY=${CODEX_API_KEY}"
  i=0
  for a in "$@"; do
    echo "ARG[$i]=$a"
    i=$((i+1))
  done
  echo "STDIN=${stdin_payload}"
} >> "$STUB_LOG"

# locate -o <path>
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then
    out="$a"
  fi
  prev="$a"
done

printf '%s\n' '{"type":"thread.started","thread_id":"stub-thread-xyz"}'
printf '%s\n' '{"type":"turn.started"}'
printf '%s\n' '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"stub answer text"}}'
printf '%s\n' '{"type":"turn.completed"}'

if [ -n "$out" ]; then
  cat > "$out" <<'JSON'
{"summary":"stub summary","overall_severity":"high","findings":[{"path":"calc.go","line":19,"side":"NEW","severity":"high","title":"stub finding","body":"stub body"}]}
JSON
fi
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("CODEX_BIN", script)
	t.Setenv("STUB_LOG", logPath)
	return script
}

func writeCCSwitchStub(t *testing.T, logPath string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "cc-switch-stub.sh")
	body := `#!/bin/sh
{
  echo "CC_SWITCH_CONFIG_DIR=${CC_SWITCH_CONFIG_DIR}"
  i=0
  for a in "$@"; do
    echo "CCARG[$i]=$a"
    i=$((i+1))
  done
} >> "$STUB_LOG"

if [ "$1" = "--app" ] && [ "$2" = "codex" ] && [ "$3" = "provider" ] && [ "$4" = "current" ]; then
  echo "current provider: codex-relay"
  exit 0
fi
if [ "$1" = "--app" ] && [ "$2" = "codex" ] && [ "$3" = "env" ] && [ "$4" = "check" ]; then
  echo "env ok"
  exit 0
fi
if [ "$1" = "--app" ] && [ "$2" = "codex" ] && [ "$3" = "provider" ] && [ "$4" = "switch" ]; then
  echo "switched $5"
  exit 0
fi
echo "unexpected cc-switch args: $*" >&2
exit 1
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write cc-switch stub: %v", err)
	}
	return script
}

func TestRunner_ReviewNew(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stub.log")
	writeStub(t, logPath)

	// A secret that must NOT reach the codex subprocess.
	t.Setenv("GITEA_TOKEN", "super-secret-gitea")
	t.Setenv("OPENAI_API_KEY", "super-secret-openai")

	worktree := t.TempDir()
	codexHome := t.TempDir()

	r := New(Options{CodexHome: codexHome, APIKey: "ak-test"})

	res, err := r.Review(context.Background(), model.CodexInput{
		Worktree: worktree,
		BaseRef:  "main",
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	if res.SessionID != "stub-thread-xyz" {
		t.Errorf("SessionID = %q, want stub-thread-xyz", res.SessionID)
	}
	if res.Summary != "stub summary" {
		t.Errorf("Summary = %q", res.Summary)
	}
	if res.OverallSeverity != model.SeverityHigh {
		t.Errorf("OverallSeverity = %q, want high", res.OverallSeverity)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("Findings len = %d, want 1", len(res.Findings))
	}
	f := res.Findings[0]
	if f.Path != "calc.go" || f.Line != 19 || f.Side != model.SideNew || f.Severity != model.SeverityHigh {
		t.Errorf("finding = %+v", f)
	}

	logb, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stub log: %v", err)
	}
	log := string(logb)

	// cmd.Dir must be the worktree. Resolve symlinks because macOS /tmp -> /private/tmp,
	// and the subprocess's reported CWD is the resolved path.
	wantCWD := worktree
	if resolved, rerr := filepath.EvalSymlinks(worktree); rerr == nil {
		wantCWD = resolved
	}
	if !strings.Contains(log, "CWD="+wantCWD) && !strings.Contains(log, "CWD="+worktree) {
		t.Errorf("stub cwd not the worktree; log:\n%s", log)
	}
	// secrets must be stripped from the subprocess env.
	if !strings.Contains(log, "GITEA_TOKEN=\n") {
		t.Errorf("GITEA_TOKEN leaked into codex env; log:\n%s", log)
	}
	if !strings.Contains(log, "OPENAI_API_KEY=\n") {
		t.Errorf("OPENAI_API_KEY leaked into codex env; log:\n%s", log)
	}
	// CODEX_HOME and CODEX_API_KEY must be set.
	if !strings.Contains(log, "CODEX_HOME="+codexHome) {
		t.Errorf("CODEX_HOME not set; log:\n%s", log)
	}
	if !strings.Contains(log, "CODEX_API_KEY=ak-test") {
		t.Errorf("CODEX_API_KEY not set; log:\n%s", log)
	}
	// required flags / subcommand for a new review.
	for _, want := range []string{
		"ARG[0]=exec",
		"ARG[1]=-",
		"approval_policy=never",
		"sandbox_mode=read-only",
		"--output-schema",
		"--skip-git-repo-check",
		"-o",
		"Write all human-facing review output in Simplified Chinese",
		"git diff --name-only <base>...HEAD",
		".github/copilot-instructions.md",
		"not a top-N review",
		"concrete failure mode",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("expected arg %q in stub log:\n%s", want, log)
		}
	}
	// a new review must NOT be a resume.
	if strings.Contains(log, "ARG[1]=resume") {
		t.Errorf("new review incorrectly issued a resume; log:\n%s", log)
	}
}

func TestRunner_ReviewSwitchesCodexProvider(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stub.log")
	writeStub(t, logPath)
	ccSwitchBin := writeCCSwitchStub(t, logPath)
	ccSwitchDir := t.TempDir()
	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"refresh_token":"legacy"}}`), 0o600); err != nil {
		t.Fatalf("write legacy auth.json: %v", err)
	}

	r := New(Options{
		CodexHome:          codexHome,
		CCSwitchBin:        ccSwitchBin,
		CCSwitchConfigDir:  ccSwitchDir,
		UseCCSwitch:        true,
		CCSwitchProviderID: "codex-relay",
	})

	if _, err := r.Review(context.Background(), model.CodexInput{
		Worktree: t.TempDir(),
		BaseRef:  "main",
	}); err != nil {
		t.Fatalf("Review: %v", err)
	}

	logb, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stub log: %v", err)
	}
	log := string(logb)
	for _, want := range []string{
		"CC_SWITCH_CONFIG_DIR=" + ccSwitchDir,
		"CCARG[0]=--app",
		"CCARG[1]=codex",
		"CCARG[2]=provider",
		"CCARG[3]=switch",
		"CCARG[4]=codex-relay",
		"CODEX_HOME=" + filepath.Join(codexHome, "ccswitch-runtime"),
		"ARG[0]=exec",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in log:\n%s", want, log)
		}
	}
	if strings.Index(log, "CCARG[3]=switch") > strings.Index(log, "ARG[0]=exec") {
		t.Fatalf("cc-switch provider switch must run before codex exec:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); err != nil {
		t.Fatalf("legacy auth.json should be left in place for authfile mode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "ccswitch-runtime", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("ccswitch runtime auth.json should not exist, err=%v", err)
	}
}

func TestRunner_ReviewSanitizesInvalidUTF8Prompt(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stub.log")
	writeStub(t, logPath)

	invalidNote := "guidance: " + string([]byte{0xff, 0xfe}) + " done"
	if utf8.ValidString(invalidNote) {
		t.Fatal("test note unexpectedly valid UTF-8")
	}

	r := New(Options{CodexHome: t.TempDir()})
	if _, err := r.Review(context.Background(), model.CodexInput{
		Worktree: t.TempDir(),
		BaseRef:  "main",
		Note:     invalidNote,
	}); err != nil {
		t.Fatalf("Review: %v", err)
	}

	logb, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stub log: %v", err)
	}
	log := string(logb)
	if !utf8.ValidString(log) {
		t.Fatalf("stub received invalid UTF-8:\n%x", logb)
	}
	if !strings.Contains(log, "guidance: \uFFFD done") {
		t.Fatalf("prompt did not replace invalid UTF-8 bytes:\n%s", log)
	}
}

func TestRunner_ReviewResume(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stub.log")
	writeStub(t, logPath)

	r := New(Options{CodexHome: t.TempDir()})

	res, err := r.Review(context.Background(), model.CodexInput{
		Worktree:  t.TempDir(),
		BaseRef:   "main",
		SessionID: "prev-session-123",
		Note:      "head moved abc->def, note what's fixed",
	})
	if err != nil {
		t.Fatalf("Review(resume): %v", err)
	}
	if res.SessionID != "stub-thread-xyz" {
		t.Errorf("SessionID = %q, want stub-thread-xyz", res.SessionID)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("Findings len = %d, want 1", len(res.Findings))
	}

	logb, _ := os.ReadFile(logPath)
	log := string(logb)
	if !strings.Contains(log, "ARG[0]=exec") || !strings.Contains(log, "ARG[1]=resume") {
		t.Errorf("resume args missing; log:\n%s", log)
	}
	if !strings.Contains(log, "ARG[2]=prev-session-123") {
		t.Errorf("resume session id missing; log:\n%s", log)
	}
	if !strings.Contains(log, "ARG[3]=-") {
		t.Errorf("resume prompt should be read from stdin; log:\n%s", log)
	}
	// the note must be folded into the prompt.
	if !strings.Contains(log, "head moved abc->def") {
		t.Errorf("resume note not in prompt; log:\n%s", log)
	}
	if !strings.Contains(log, "Write all human-facing review output in Simplified Chinese") {
		t.Errorf("resume prompt missing language instruction; log:\n%s", log)
	}
	if !strings.Contains(log, "prioritize correctness regressions") &&
		!strings.Contains(log, "Prioritize correctness regressions") {
		t.Errorf("resume prompt missing review quality guidance; log:\n%s", log)
	}
}

func TestRunner_ReviewCustomSandbox(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stub.log")
	writeStub(t, logPath)

	r := New(Options{CodexHome: t.TempDir(), SandboxMode: "danger-full-access"})

	if _, err := r.Review(context.Background(), model.CodexInput{
		Worktree: t.TempDir(),
		BaseRef:  "main",
	}); err != nil {
		t.Fatalf("Review: %v", err)
	}

	logb, _ := os.ReadFile(logPath)
	log := string(logb)
	if !strings.Contains(log, "sandbox_mode=danger-full-access") {
		t.Errorf("custom sandbox missing from args; log:\n%s", log)
	}
}

func TestRunner_ReviewCustomModelAndReasoningEffort(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stub.log")
	writeStub(t, logPath)

	r := New(Options{
		CodexHome:       t.TempDir(),
		Model:           "gpt-5.5",
		ReasoningEffort: "xhigh",
		BaseURL:         "https://codex-relay.example.com/v1",
		APIKey:          "sk-codex",
	})

	if _, err := r.Review(context.Background(), model.CodexInput{
		Worktree: t.TempDir(),
		BaseRef:  "main",
	}); err != nil {
		t.Fatalf("Review: %v", err)
	}

	logb, _ := os.ReadFile(logPath)
	log := string(logb)
	for _, want := range []string{
		"--model",
		"gpt-5.5",
		"model_reasoning_effort=\"xhigh\"",
		"model_provider=\"codex_gitea\"",
		"model_providers.codex_gitea.name=\"codex-gitea\"",
		"model_providers.codex_gitea.base_url=\"https://codex-relay.example.com/v1\"",
		"model_providers.codex_gitea.env_key=\"CODEX_API_KEY\"",
		"CODEX_API_KEY=sk-codex",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in args:\n%s", want, log)
		}
	}
}

func TestRunner_Ask(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stub.log")
	writeStub(t, logPath)

	r := New(Options{})

	ans, err := r.Ask(context.Background(), "sess-1", t.TempDir(), "is this fixed?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if ans != "stub answer text" {
		t.Errorf("Ask = %q, want 'stub answer text'", ans)
	}

	logb, _ := os.ReadFile(logPath)
	log := string(logb)
	if !strings.Contains(log, "ARG[1]=resume") || !strings.Contains(log, "ARG[2]=sess-1") {
		t.Errorf("Ask did not resume the given session; log:\n%s", log)
	}
	if !strings.Contains(log, "ARG[3]=-") {
		t.Errorf("Ask prompt should be read from stdin; log:\n%s", log)
	}
	if !strings.Contains(log, "is this fixed?") {
		t.Errorf("Ask question missing from args; log:\n%s", log)
	}
	if !strings.Contains(log, "Write all human-facing review output in Simplified Chinese") {
		t.Errorf("Ask prompt missing language instruction; log:\n%s", log)
	}
}

func TestRunner_GenerateText(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stub.log")
	writeStub(t, logPath)

	r := New(Options{})
	ans, err := r.GenerateText(context.Background(), t.TempDir(), "write a skill")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if ans != "stub answer text" {
		t.Fatalf("GenerateText = %q", ans)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "ARG[0]=exec") || !strings.Contains(log, "STDIN=write a skill") {
		t.Fatalf("GenerateText did not call codex exec with prompt:\n%s", log)
	}
}

func TestRunner_AskEmptySession(t *testing.T) {
	r := New(Options{})
	if _, err := r.Ask(context.Background(), "", t.TempDir(), "q"); err == nil {
		t.Fatal("expected error for empty session id")
	}
}

func TestRunner_Status(t *testing.T) {
	writeStub(t, filepath.Join(t.TempDir(), "stub.log"))
	r := New(Options{})
	out, err := r.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(out, "Logged in using ChatGPT account") {
		t.Errorf("Status = %q", out)
	}
}

func TestRunner_StatusWithCCSwitchProvider(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stub.log")
	writeStub(t, logPath)
	ccSwitchBin := writeCCSwitchStub(t, logPath)

	r := New(Options{CCSwitchBin: ccSwitchBin, CCSwitchProviderID: "codex-relay"})
	out, err := r.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "cc-switch current:") || !strings.Contains(out, "current provider: codex-relay") {
		t.Fatalf("Status missing cc-switch current provider:\n%s", out)
	}
	if !strings.Contains(out, "cc-switch env:") || !strings.Contains(out, "env ok") {
		t.Fatalf("Status missing cc-switch env check:\n%s", out)
	}
}

func TestRunner_StatusWithCCSwitchIgnoresDirectProviderConfig(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stub.log")
	writeStub(t, logPath)
	ccSwitchBin := writeCCSwitchStub(t, logPath)

	r := New(Options{
		CCSwitchBin:     ccSwitchBin,
		UseCCSwitch:     true,
		Model:           "gpt-5.5",
		ReasoningEffort: "high",
		BaseURL:         "https://llm.1sir.cc",
		APIKey:          "sk-codex",
	})
	out, err := r.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v\n%s", err, out)
	}
	for _, want := range []string{
		"cc-switch current:",
		"cc-switch env:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Status missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "codex configured provider:") || strings.Contains(out, "Base URL: https://llm.1sir.cc") {
		t.Fatalf("ccswitch status should not report direct provider config:\n%s", out)
	}
}

func TestRunner_Timeout(t *testing.T) {
	writeStub(t, filepath.Join(t.TempDir(), "stub.log"))
	t.Setenv("STUB_SLEEP", "5")

	r := New(Options{Timeout: 150 * time.Millisecond})
	start := time.Now()
	_, err := r.Review(context.Background(), model.CodexInput{Worktree: t.TempDir(), BaseRef: "main"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want a timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Review took %s; timeout not enforced", elapsed)
	}
}

func TestRunner_ReviewEmptyWorktree(t *testing.T) {
	r := New(Options{})
	if _, err := r.Review(context.Background(), model.CodexInput{}); err == nil {
		t.Fatal("expected error for empty worktree")
	}
}

func TestNormalizeReviewResult_UnwrapsJSONSummary(t *testing.T) {
	r := &model.ReviewResult{
		Summary: `{"summary":"重新审查后还有 1 个问题。","overall_severity":"low","findings":[{"path":"a.ts","line":7,"side":"NEW","severity":"low","title":"边界问题","body":"说明"}]}`,
	}
	normalizeReviewResult(r)
	if r.Summary != "重新审查后还有 1 个问题。" {
		t.Fatalf("Summary = %q", r.Summary)
	}
	if r.OverallSeverity != model.SeverityLow {
		t.Fatalf("OverallSeverity = %q", r.OverallSeverity)
	}
	if len(r.Findings) != 1 || r.Findings[0].Title != "边界问题" {
		t.Fatalf("Findings = %+v", r.Findings)
	}
}

func TestHumanizeStructuredAnswer(t *testing.T) {
	raw := `{"summary":"重新审查后还有 1 个问题。","overall_severity":"low","findings":[{"path":"a.ts","line":7,"side":"NEW","severity":"low","title":"边界问题","body":"说明"}]}`
	got := humanizeStructuredAnswer(raw)
	if strings.Contains(got, `{"summary"`) {
		t.Fatalf("answer still contains raw JSON: %s", got)
	}
	if !strings.Contains(got, "重新审查后还有 1 个问题。") || !strings.Contains(got, "`a.ts:7`") {
		t.Fatalf("humanized answer missing expected content: %s", got)
	}
}

func TestNew_CodexBinOverride(t *testing.T) {
	t.Setenv("CODEX_BIN", "/custom/path/codex")
	r := New(Options{Bin: "codex"})
	if r.bin != "/custom/path/codex" {
		t.Errorf("bin = %q, want /custom/path/codex", r.bin)
	}
}
