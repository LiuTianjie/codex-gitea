package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/turning4th/codex-gitea/internal/config"
	"github.com/turning4th/codex-gitea/internal/console"
	"github.com/turning4th/codex-gitea/internal/model"
	"github.com/turning4th/codex-gitea/internal/store"
	_ "modernc.org/sqlite"
)

func newReadyzTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedCodexProviderDB(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir cc-switch dir: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "cc-switch.db"))
	if err != nil {
		t.Fatalf("open cc-switch db: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(`
CREATE TABLE providers (
  id TEXT NOT NULL,
  app_type TEXT NOT NULL,
  name TEXT NOT NULL,
  settings_config TEXT NOT NULL,
  is_current BOOLEAN NOT NULL DEFAULT 0,
  PRIMARY KEY (id, app_type)
);
INSERT INTO providers (id, app_type, name, settings_config, is_current)
VALUES ('codex-relay', 'codex', 'Relay', '{"base_url":"https://llm.1sir.cc/v1"}', 1);
`)
	if err != nil {
		t.Fatalf("seed cc-switch db: %v", err)
	}
}

func readyzTestConfig() *config.Config {
	return &config.Config{
		GiteaURL:      "https://git.example.com",
		GiteaToken:    "token",
		WebhookSecret: "secret",
		AdminPassword: "admin",
		CodexAuthMode: config.AuthModeAuthFile,
		Concurrency:   1,
		Timeout:       time.Minute,
		GiteaTimeout:  time.Minute,
	}
}

func TestReadyzOK(t *testing.T) {
	st := newReadyzTestStore(t)
	ctx := context.Background()
	job, _, err := st.EnqueueJob(ctx, &model.WebhookEvent{
		DeliveryID: "readyz-failed",
		Event:      model.EventPullRequest,
		Action:     "opened",
		PR:         model.PRRef{Owner: "acme", Repo: "widgets", Number: 12},
		Raw:        []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.FinishJobDetailed(ctx, job.ID, model.JobFinish{
		Status: model.JobFailed, Error: "gitea timeout", ErrorType: model.ErrorTypeGitea, Retryable: true,
	}); err != nil {
		t.Fatalf("finish failed job: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	readyzHandler(st, readyzTestConfig).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK   bool `json:"ok"`
		DBOK bool `json:"db_ok"`
		Jobs struct {
			Total  int `json:"total"`
			Failed int `json:"failed"`
		} `json:"jobs"`
		LatestFailure *struct {
			ID        int64  `json:"id"`
			ErrorType string `json:"error_type"`
		} `json:"latest_failure"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	if !resp.OK || !resp.DBOK || resp.Jobs.Total != 1 || resp.Jobs.Failed != 1 {
		t.Fatalf("readyz response = %+v", resp)
	}
	if resp.LatestFailure == nil || resp.LatestFailure.ID != job.ID || resp.LatestFailure.ErrorType != string(model.ErrorTypeGitea) {
		t.Fatalf("latest failure = %+v, want failed job", resp.LatestFailure)
	}
}

func TestReadyzConfigWarnings(t *testing.T) {
	st := newReadyzTestStore(t)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	readyzHandler(st, func() *config.Config {
		return &config.Config{CodexAuthMode: config.AuthModeAuthFile, Concurrency: 1, Timeout: time.Minute, GiteaTimeout: time.Minute}
	}).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK             bool     `json:"ok"`
		DBOK           bool     `json:"db_ok"`
		ConfigWarnings []string `json:"config_warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	if resp.OK || !resp.DBOK || len(resp.ConfigWarnings) == 0 {
		t.Fatalf("readyz warnings response = %+v", resp)
	}
}

func TestRunChatProbeCodexForcesReadOnlySandbox(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "codex-stub.sh")
	script := `#!/bin/sh
stdin_payload="$(cat)"
{
  echo "CODEX_API_KEY=${CODEX_API_KEY}"
  i=0
  for a in "$@"; do
    echo "ARG[$i]=$a"
    i=$((i+1))
  done
  echo "STDIN=${stdin_payload}"
} >> "$CODEX_STUB_LOG"
printf '%s\n' '{"type":"thread.started","thread_id":"probe-thread"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"OK"}}'
printf '%s\n' '{"type":"turn.completed"}'
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write codex stub: %v", err)
	}
	ccSwitchPath := filepath.Join(binDir, "cc-switch")
	ccSwitchScript := `#!/bin/sh
if [ "$1" = "--app" ] && [ "$2" = "codex" ] && [ "$3" = "provider" ] && [ "$4" = "current" ]; then
  echo "Current Provider"
  echo "Basic Info"
  echo "  ID: codex-relay"
  exit 0
fi
if [ "$1" = "--app" ] && [ "$2" = "codex" ] && [ "$3" = "provider" ] && [ "$4" = "switch" ]; then
  echo "switched $5"
  exit 0
fi
echo "unexpected cc-switch args: $*" >&2
exit 1
`
	if err := os.WriteFile(ccSwitchPath, []byte(ccSwitchScript), 0o755); err != nil {
		t.Fatalf("write cc-switch stub: %v", err)
	}
	t.Setenv("CODEX_BIN", binPath)
	t.Setenv("CODEX_STUB_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ccSwitchDir := t.TempDir()
	seedCodexProviderDB(t, ccSwitchDir)

	cfg := &config.Config{
		CodexAuthMode:     config.AuthModeCCSwitch,
		CodexHome:         t.TempDir(),
		CodexSandbox:      config.SandboxDangerFullAccess,
		CodexBaseURL:      "https://llm.1sir.cc",
		CodexAPIKey:       "sk-from-settings",
		CCSwitchConfigDir: ccSwitchDir,
		Timeout:           time.Minute,
		GiteaTimeout:      time.Minute,
	}
	if _, err := runChatProbe(context.Background(), cfg, console.ChatProbeInput{Reviewer: "codex", Prompt: "Return OK"}); err != nil {
		t.Fatalf("runChatProbe: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read codex log: %v", err)
	}
	log := string(raw)
	if !strings.Contains(log, "sandbox_mode=read-only") {
		t.Fatalf("probe did not force read-only sandbox:\n%s", log)
	}
	if strings.Contains(log, "sandbox_mode=danger-full-access") {
		t.Fatalf("probe inherited danger-full-access sandbox:\n%s", log)
	}
	for _, want := range []string{
		"CODEX_API_KEY=sk-from-settings",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("probe should pass configured API key to Codex, missing %q:\n%s", want, log)
		}
	}
	for _, unwanted := range []string{
		"model_provider=\"codex_gitea\"",
		"model_providers.codex_gitea.base_url=",
		"model_providers.codex_gitea.env_key=",
	} {
		if strings.Contains(log, unwanted) {
			t.Fatalf("probe should not inject direct Codex provider config in ccswitch mode, found %q:\n%s", unwanted, log)
		}
	}
}

func TestRunChatProbeCodexUsesAPIKeyWithTemporaryBaseURLInAPIKeyMode(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex.log")
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "codex-stub.sh")
	script := `#!/bin/sh
stdin_payload="$(cat)"
{
  echo "CODEX_API_KEY=${CODEX_API_KEY}"
  i=0
  for a in "$@"; do
    echo "ARG[$i]=$a"
    i=$((i+1))
  done
  echo "STDIN=${stdin_payload}"
} >> "$CODEX_STUB_LOG"
printf '%s\n' '{"type":"thread.started","thread_id":"probe-thread"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"OK"}}'
printf '%s\n' '{"type":"turn.completed"}'
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write codex stub: %v", err)
	}
	t.Setenv("CODEX_BIN", binPath)
	t.Setenv("CODEX_STUB_LOG", logPath)

	cfg := &config.Config{
		CodexAuthMode: config.AuthModeAPIKey,
		CodexAPIKey:   "sk-from-settings",
		Timeout:       time.Minute,
		GiteaTimeout:  time.Minute,
	}
	if _, err := runChatProbe(context.Background(), cfg, console.ChatProbeInput{
		Reviewer: "codex",
		Prompt:   "Return OK",
		BaseURL:  "https://llm.1sir.cc/v1",
	}); err != nil {
		t.Fatalf("runChatProbe: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read codex log: %v", err)
	}
	log := string(raw)
	for _, want := range []string{
		"CODEX_API_KEY=sk-from-settings",
		"model_providers.codex_gitea.base_url=\"https://llm.1sir.cc/v1\"",
		"model_providers.codex_gitea.env_key=\"CODEX_API_KEY\"",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("probe missing temporary base URL auth value %q:\n%s", want, log)
		}
	}
}
