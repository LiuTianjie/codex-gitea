package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/turning4th/codex-gitea/internal/config"
	"github.com/turning4th/codex-gitea/internal/model"
	"github.com/turning4th/codex-gitea/internal/store"
	_ "modernc.org/sqlite"
)

const testPassword = "s3cret-pw"

// newTestConsole builds a Console backed by a real temp-db store. statusFn is
// optional. It returns the console and its codex home dir.
func newTestConsole(t *testing.T, statusFn StatusFunc) (*Console, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	codexHome := filepath.Join(t.TempDir(), "codex-home")
	cfg := &config.Config{AdminPassword: testPassword, CodexHome: codexHome}

	var c *Console
	if statusFn != nil {
		c = New(st, cfg, codexHome, statusFn)
	} else {
		c = New(st, cfg, codexHome)
	}
	return c, codexHome
}

// do issues a request against the console's full handler tree, optionally with
// a logged-in console session.
func do(t *testing.T, h http.Handler, method, path, body string, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if withAuth {
		c := loginCookie(t, h)
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func loginCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	r := httptest.NewRequest("POST", "/admin/login", strings.NewReader("password="+testPassword))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login: status=%d body=%s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == consoleSessionCookie {
			return c
		}
	}
	t.Fatalf("login did not set %s cookie", consoleSessionCookie)
	return nil
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func TestNoAuthRedirectsToLogin(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "GET", "/admin/", "", false)
	if w.Code != http.StatusSeeOther {
		t.Errorf("no auth page: status = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/admin/login" {
		t.Errorf("no auth redirect = %q, want /admin/login", loc)
	}
}

func TestChatProbe(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{AdminPassword: testPassword}
	var got ChatProbeInput
	c := New(st, cfg, filepath.Join(t.TempDir(), "codex-home"), ChatProbeFunc(func(_ context.Context, in ChatProbeInput) (string, error) {
		got = in
		return "pong", nil
	}))

	w := do(t, c.Routes(), "POST", "/admin/api/chat-probe", `{"reviewer":"codex","prompt":"ping","model":"gpt-5.5","base_url":"https://relay.example.com/v1","provider_id":"codex-relay","reasoning_effort":"high"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		OK     bool   `json:"ok"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse body: %v\n%s", err, w.Body.String())
	}
	if !body.OK || body.Output != "pong" {
		t.Fatalf("body = %+v", body)
	}
	if got.Reviewer != "codex" || got.Prompt != "ping" || got.Model != "gpt-5.5" || got.BaseURL != "https://relay.example.com/v1" || got.ProviderID != "codex-relay" || got.ReasoningEffort != "high" {
		t.Fatalf("probe input = %+v", got)
	}
}

func TestCCSwitchCodexOptionsReadProvidersAndModels(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ccDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(ccDir, "cc-switch.db"))
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
CREATE TABLE model_pricing (
  model_id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL
);
INSERT INTO providers (id, app_type, name, settings_config, is_current)
VALUES
  ('codex-relay', 'codex', 'Relay', '{"config":"model = \"gpt-5.5\"\nmodel_reasoning_effort = \"xhigh\"\n[model_providers.relay]\nbase_url = \"https://codex-relay.example.com/v1\"\n"}', 1),
  ('claude-relay', 'claude', 'Claude Relay', '{"env":{}}', 0);
INSERT INTO model_pricing (model_id, display_name)
VALUES ('gpt-5.5', 'GPT-5.5'), ('gpt-5.5-mini', 'GPT-5.5 Mini'), ('claude-sonnet-4-6', 'Claude Sonnet');
`)
	if err != nil {
		t.Fatalf("seed cc-switch db: %v", err)
	}

	cfg := &config.Config{
		AdminPassword:         testPassword,
		CCSwitchConfigDir:     ccDir,
		Model:                 "gpt-5-codex",
		CodexReasoningEffort:  "high",
		CodexCCSwitchProvider: "codex-relay",
	}
	c := New(st, cfg, filepath.Join(t.TempDir(), "codex-home"))
	w := do(t, c.Routes(), "GET", "/admin/api/cc-switch/codex-options", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		OK        bool `json:"ok"`
		Providers []struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			Current         bool   `json:"current"`
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoning_effort"`
			BaseURL         string `json:"base_url"`
		} `json:"providers"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
		ReasoningEfforts []string `json:"reasoning_efforts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse body: %v\n%s", err, w.Body.String())
	}
	if !body.OK || len(body.Providers) != 1 {
		t.Fatalf("options body = %+v", body)
	}
	if p := body.Providers[0]; p.ID != "codex-relay" || !p.Current || p.Model != "gpt-5.5" || p.ReasoningEffort != "xhigh" || p.BaseURL != "https://codex-relay.example.com/v1" {
		t.Fatalf("provider = %+v", p)
	}
	var sawModel bool
	for _, m := range body.Models {
		if m.ID == "gpt-5.5" {
			sawModel = true
		}
		if strings.HasPrefix(m.ID, "claude-") {
			t.Fatalf("unexpected claude model in codex options: %+v", m)
		}
	}
	if !sawModel {
		t.Fatalf("models missing gpt-5.5: %+v", body.Models)
	}
	if !containsString(body.ReasoningEfforts, "xhigh") {
		t.Fatalf("reasoning efforts missing xhigh: %+v", body.ReasoningEfforts)
	}
}

func TestCCSwitchCodexFetchModelsUsesProviderAndConfigDir(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ccDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "cc-switch.log")
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "cc-switch")
	script := `#!/bin/sh
{
  echo "CC_SWITCH_CONFIG_DIR=${CC_SWITCH_CONFIG_DIR}"
  i=0
  for a in "$@"; do
    echo "ARG[$i]=$a"
    i=$((i+1))
  done
} >> "$CC_SWITCH_STUB_LOG"
echo "fetched"
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write cc-switch stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CC_SWITCH_STUB_LOG", logPath)

	cfg := &config.Config{AdminPassword: testPassword, CCSwitchConfigDir: ccDir}
	c := New(st, cfg, filepath.Join(t.TempDir(), "codex-home"))
	w := do(t, c.Routes(), "POST", "/admin/api/cc-switch/codex-options/fetch", `{"provider_id":"codex-relay","base_url":"https://codex-relay.example.com/v1"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		OK      bool   `json:"ok"`
		Fetched bool   `json:"fetched"`
		Output  string `json:"cc_switch_output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse body: %v\n%s", err, w.Body.String())
	}
	if !body.OK || !body.Fetched || body.Output != "fetched" {
		t.Fatalf("fetch body = %+v", body)
	}
	logb, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read cc-switch log: %v", err)
	}
	log := string(logb)
	for _, want := range []string{
		"CC_SWITCH_CONFIG_DIR=" + ccDir,
		"ARG[0]=--app",
		"ARG[1]=codex",
		"ARG[2]=provider",
		"ARG[3]=fetch-models",
		"ARG[4]=--base-url",
		"ARG[5]=https://codex-relay.example.com/v1",
		"ARG[6]=codex-relay",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in log:\n%s", want, log)
		}
	}
}

func TestCCSwitchCodexFetchModelsFallsBackToCurrentProvider(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ccDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(ccDir, "cc-switch.db"))
	if err != nil {
		t.Fatalf("open cc-switch db: %v", err)
	}
	_, err = db.Exec(`
CREATE TABLE providers (
  id TEXT NOT NULL,
  app_type TEXT NOT NULL,
  name TEXT NOT NULL,
  settings_config TEXT NOT NULL,
  is_current BOOLEAN NOT NULL DEFAULT 0,
  PRIMARY KEY (id, app_type)
);
CREATE TABLE model_pricing (
  model_id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL
);
INSERT INTO providers (id, app_type, name, settings_config, is_current)
VALUES ('codex-current', 'codex', 'Current Codex', '{"base_url":"https://current.example.com/v1","config":"model = \"gpt-5.5\""}', 1);
`)
	if err != nil {
		t.Fatalf("seed cc-switch db: %v", err)
	}
	db.Close()

	logPath := filepath.Join(t.TempDir(), "cc-switch.log")
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "cc-switch")
	script := `#!/bin/sh
{
  i=0
  for a in "$@"; do
    echo "ARG[$i]=$a"
    i=$((i+1))
  done
} >> "$CC_SWITCH_STUB_LOG"
echo "fetched current"
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write cc-switch stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CC_SWITCH_STUB_LOG", logPath)

	cfg := &config.Config{AdminPassword: testPassword, CCSwitchConfigDir: ccDir}
	c := New(st, cfg, filepath.Join(t.TempDir(), "codex-home"))
	w := do(t, c.Routes(), "POST", "/admin/api/cc-switch/codex-options/fetch", `{}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	logb, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read cc-switch log: %v", err)
	}
	log := string(logb)
	for _, want := range []string{
		"ARG[4]=--base-url",
		"ARG[5]=https://current.example.com/v1",
		"ARG[6]=codex-current",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in log:\n%s", want, log)
		}
	}
}

func TestNoAuthAPIRejectedWithoutBasicChallenge(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "GET", "/admin/api/settings", "", false)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no auth api: status = %d, want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("unexpected Basic Auth challenge: %q", got)
	}
	if !strings.Contains(w.Body.String(), "unauthorized") {
		t.Errorf("api unauthorized body = %q", w.Body.String())
	}
}

func TestLoginPageServed(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "GET", "/admin/login", "", false)
	if w.Code != http.StatusOK {
		t.Fatalf("login page: status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "登录控制台") {
		t.Errorf("login page missing styled form")
	}
	if strings.Contains(w.Body.String(), "WWW-Authenticate") {
		t.Errorf("login page should not include Basic Auth marker")
	}
}

func TestWrongPasswordRejected(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	r := httptest.NewRequest("POST", "/admin/login", strings.NewReader("password=wrong"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: status = %d, want 401", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == consoleSessionCookie && c.Value != "" {
			t.Fatalf("wrong password set session cookie: %+v", c)
		}
	}
	if !strings.Contains(w.Body.String(), "密码不正确") {
		t.Errorf("wrong password body missing message")
	}
}

func TestLoginThenGetSettings(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "GET", "/admin/api/settings", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("authorized by login cookie: status = %d, want 200", w.Code)
	}
}

func TestConsoleDisabledWhenNoPassword(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{AdminPassword: ""} // disabled
	c := New(st, cfg, t.TempDir())

	w := do(t, c.Routes(), "GET", "/admin/api/settings", "", false)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("disabled console: status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ADMIN_PASSWORD") {
		t.Errorf("disabled body = %q, want mention of ADMIN_PASSWORD", w.Body.String())
	}
}

func TestGetSettingsAuthorized(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "GET", "/admin/api/settings", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("authorized GET settings: status = %d, want 200", w.Code)
	}
	var m map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode settings: %v (body=%s)", err, w.Body.String())
	}
}

func TestPostSettingsThenGetRedacts(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	body := `{"settings":{"gitea_url":"https://git.example.com","gitea_token":"super-secret-token","model":"gpt-5-codex","minimax_api_key":"ak-minimax"}}`
	w := do(t, h, "POST", "/admin/api/settings", body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("POST settings: status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	w = do(t, h, "GET", "/admin/api/settings", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET settings: status = %d", w.Code)
	}
	var m map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["gitea_url"] != "https://git.example.com" {
		t.Errorf("gitea_url = %q, want plain value", m["gitea_url"])
	}
	if m["model"] != "gpt-5-codex" {
		t.Errorf("model = %q", m["model"])
	}
	// Secret must be redacted, never echoed.
	if m["gitea_token"] != redacted {
		t.Errorf("gitea_token = %q, want %q (redacted)", m["gitea_token"], redacted)
	}
	if m["minimax_api_key"] != redacted {
		t.Errorf("minimax_api_key = %q, want %q (redacted)", m["minimax_api_key"], redacted)
	}
	if strings.Contains(w.Body.String(), "super-secret-token") {
		t.Errorf("response leaked secret token value: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ak-minimax") {
		t.Errorf("response leaked minimax api key: %s", w.Body.String())
	}
}

func TestPostSettingsRedactedSentinelKeepsSecret(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	// Store a real secret first.
	do(t, h, "POST", "/admin/api/settings", `{"settings":{"gitea_token":"real-token"}}`, true)
	// Re-submit the redaction sentinel; the stored secret must be preserved.
	do(t, h, "POST", "/admin/api/settings", `{"settings":{"gitea_token":"`+redacted+`"}}`, true)

	val, ok, err := c.store.GetSetting(context.Background(), "gitea_token")
	if err != nil || !ok {
		t.Fatalf("GetSetting gitea_token: ok=%v err=%v", ok, err)
	}
	if val != "real-token" {
		t.Errorf("gitea_token overwritten by sentinel: %q, want real-token", val)
	}
}

func TestPostSettingsSingleKeyForm(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "POST", "/admin/api/settings", `{"key":"model","value":"gpt-5"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("single-key POST: status = %d (body=%s)", w.Code, w.Body.String())
	}
	val, ok, _ := c.store.GetSetting(context.Background(), "model")
	if !ok || val != "gpt-5" {
		t.Errorf("model = %q ok=%v, want gpt-5", val, ok)
	}
}

func TestPostSettingsAppliesRuntimeHook(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{AdminPassword: testPassword, CodexHome: filepath.Join(t.TempDir(), "codex-home")}
	var applied map[string]string
	c := New(st, cfg, cfg.CodexHome, SettingsApplyFunc(func(_ context.Context, updates map[string]string) error {
		applied = updates
		return nil
	}))
	h := c.Routes()

	w := do(t, h, "POST", "/admin/api/settings", `{"settings":{"claude_enabled":"true","claude_model":"sonnet"}}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("POST settings: status = %d (body=%s)", w.Code, w.Body.String())
	}
	if applied["claude_enabled"] != "true" || applied["claude_model"] != "sonnet" {
		t.Fatalf("runtime hook updates = %+v", applied)
	}
}

func TestPostSettingsEmptyRejected(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()
	w := do(t, h, "POST", "/admin/api/settings", `{}`, true)
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty settings POST: status = %d, want 400", w.Code)
	}
}

func TestAuthFileValidWritten0600(t *testing.T) {
	c, codexHome := newTestConsole(t, nil)
	h := c.Routes()

	authJSON := `{"tokens":{"refresh_token":"rt-abc","access_token":"at-xyz"},"OPENAI_API_KEY":null}`
	w := do(t, h, "POST", "/admin/api/authfile", authJSON, true)
	if w.Code != http.StatusOK {
		t.Fatalf("valid auth.json: status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	dest := filepath.Join(codexHome, "auth.json")
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("auth.json not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("auth.json perm = %o, want 600", perm)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if !strings.Contains(string(got), "rt-abc") {
		t.Errorf("auth.json content missing refresh token: %s", got)
	}
}

func TestAuthFileMissingRefreshToken(t *testing.T) {
	c, codexHome := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "POST", "/admin/api/authfile", `{"tokens":{"access_token":"only"}}`, true)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing refresh_token: status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); !os.IsNotExist(err) {
		t.Errorf("auth.json should not be written on validation failure")
	}
}

func TestAuthFileInvalidJSON(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()
	w := do(t, h, "POST", "/admin/api/authfile", `not json at all`, true)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: status = %d, want 400", w.Code)
	}
}

func TestStatusWithStub(t *testing.T) {
	c, _ := newTestConsole(t, func(ctx context.Context) (string, error) {
		return "Logged in using ChatGPT", nil
	})
	h := c.Routes()

	w := do(t, h, "GET", "/admin/api/status", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("status: code = %d", w.Code)
	}
	var resp struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !resp.OK || resp.Status != "Logged in using ChatGPT" {
		t.Errorf("status resp = %+v, want ok+message", resp)
	}
}

func TestStatusNotConfigured(t *testing.T) {
	c, _ := newTestConsole(t, nil) // no status func
	h := c.Routes()

	w := do(t, h, "GET", "/admin/api/status", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("status: code = %d", w.Code)
	}
	var resp struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.OK {
		t.Errorf("status ok = true, want false when unconfigured")
	}
}

func TestJobsEndpoint(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()
	job, _, err := c.store.EnqueueJob(context.Background(), &model.WebhookEvent{
		DeliveryID: "console-job",
		Event:      model.EventPullRequest,
		Action:     "opened",
		PR:         model.PRRef{Owner: "acme", Repo: "widgets", Number: 17},
		Raw:        []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	if err := c.store.AppendJobLog(context.Background(), job.ID, "codex", "review started"); err != nil {
		t.Fatalf("append job log: %v", err)
	}

	w := do(t, h, "GET", "/admin/api/jobs?page=1&page_size=20", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("jobs: code = %d", w.Code)
	}
	var resp struct {
		Jobs       []map[string]any `json:"jobs"`
		Page       int              `json:"page"`
		PageSize   int              `json:"page_size"`
		Total      int              `json:"total"`
		TotalPages int              `json:"total_pages"`
		HasMore    bool             `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode jobs: %v (body=%s)", err, w.Body.String())
	}
	if resp.Page != 1 || resp.PageSize != 20 {
		t.Fatalf("jobs pagination = page %d size %d, want 1/20", resp.Page, resp.PageSize)
	}
	if resp.Jobs == nil {
		t.Fatalf("jobs response missing jobs array")
	}
	if len(resp.Jobs) != 1 || resp.Jobs[0]["log_count"].(float64) != 1 {
		t.Fatalf("jobs log summary = %+v, want one row with log_count=1", resp.Jobs)
	}
	if logs, ok := resp.Jobs[0]["logs"].([]any); ok && len(logs) != 0 {
		t.Fatalf("jobs list should not include full logs: %+v", logs)
	}

	w = do(t, h, "GET", "/admin/api/jobs/stats", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("job stats: code = %d body=%s", w.Code, w.Body.String())
	}
	var stats map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode job stats: %v", err)
	}
	if stats["total_jobs"].(float64) != 1 {
		t.Fatalf("job stats = %+v, want total_jobs=1", stats)
	}

	w = do(t, h, "GET", "/admin/api/jobs/"+strconv.FormatInt(job.ID, 10), "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("job detail: code = %d body=%s", w.Code, w.Body.String())
	}
	var detail struct {
		LogCount int `json:"log_count"`
		Logs     []struct {
			Stage   string `json:"stage"`
			Message string `json:"message"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode job detail: %v", err)
	}
	if detail.LogCount != 1 || len(detail.Logs) != 1 || detail.Logs[0].Stage != "codex" || detail.Logs[0].Message != "review started" {
		t.Fatalf("job detail logs = %+v, want codex/review started", detail)
	}

	w = do(t, h, "POST", "/admin/api/jobs/"+strconv.FormatInt(job.ID, 10)+"/rerun", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("job rerun: code = %d body=%s", w.Code, w.Body.String())
	}
	var rerun struct {
		OK  bool `json:"ok"`
		Job struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rerun); err != nil {
		t.Fatalf("decode rerun: %v", err)
	}
	if !rerun.OK || rerun.Job.ID == job.ID || rerun.Job.Status != string(model.JobPending) {
		t.Fatalf("rerun response = %+v, original=%d", rerun, job.ID)
	}

	w = do(t, h, "POST", "/admin/api/jobs/"+strconv.FormatInt(job.ID, 10)+"/cancel", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("job cancel: code = %d body=%s", w.Code, w.Body.String())
	}
	var canceled struct {
		OK  bool `json:"ok"`
		Job struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &canceled); err != nil {
		t.Fatalf("decode cancel: %v", err)
	}
	if !canceled.OK || canceled.Job.ID != job.ID || canceled.Job.Status != string(model.JobCanceled) {
		t.Fatalf("cancel response = %+v, want canceled original job", canceled)
	}

	running, _, err := c.store.EnqueueJob(context.Background(), &model.WebhookEvent{
		DeliveryID: "console-job-running",
		Event:      model.EventPullRequest,
		Action:     "opened",
		PR:         model.PRRef{Owner: "acme", Repo: "widgets", Number: 18},
		Raw:        []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("enqueue running job: %v", err)
	}
	for i := 0; i < 3; i++ {
		claimed, err := c.store.ClaimJob(context.Background())
		if err != nil {
			t.Fatalf("claim running job: %v", err)
		}
		if claimed == nil {
			t.Fatalf("claim running job: got nil before job %d", running.ID)
		}
		if claimed.ID == running.ID {
			break
		}
		if i == 2 {
			t.Fatalf("claim running job: never claimed job %d", running.ID)
		}
	}
	w = do(t, h, "POST", "/admin/api/jobs/"+strconv.FormatInt(running.ID, 10)+"/cancel", "", true)
	if w.Code != http.StatusConflict {
		t.Fatalf("cancel running job: code = %d body=%s, want 409", w.Code, w.Body.String())
	}
}

func TestAnalyticsReportEndpoints(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "POST", "/admin/api/analytics/reports", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("create report: code = %d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		OK     bool           `json:"ok"`
		Report map[string]any `json:"report"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create report: %v", err)
	}
	if !created.OK || created.Report == nil {
		t.Fatalf("create report response mismatch: %+v", created)
	}

	w = do(t, h, "GET", "/admin/api/analytics/reports/latest", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("latest report: code = %d body=%s", w.Code, w.Body.String())
	}
	var latest struct {
		OK     bool           `json:"ok"`
		Report map[string]any `json:"report"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &latest); err != nil {
		t.Fatalf("decode latest report: %v", err)
	}
	if !latest.OK || latest.Report == nil {
		t.Fatalf("latest report response mismatch: %+v", latest)
	}
}

func TestSkillGenerateAsyncLifecycle(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	c.skillGenerator = func(ctx context.Context, in model.SkillGenerationInput) (string, error) {
		if in.Context.Owner != "acme" || in.Context.Repo != "widgets" {
			t.Fatalf("generator context = %s/%s", in.Context.Owner, in.Context.Repo)
		}
		return "# Generated Skill\n\nUse the existing defects.", nil
	}
	seedSkillProjectFindings(t, c, "acme", "widgets")
	h := c.Routes()

	w := do(t, h, "POST", "/admin/api/skills/acme/widgets/generate", "", true)
	if w.Code != http.StatusAccepted {
		t.Fatalf("generate: code=%d body=%s", w.Code, w.Body.String())
	}
	task := decodeSkillTaskResponse(t, w.Body.Bytes())
	if task.ID == "" || task.Status != "running" {
		t.Fatalf("initial task = %+v", task)
	}

	task = waitSkillTask(t, h, "acme", "widgets", task.ID)
	if task.Status != "done" || task.Skill == nil || !strings.Contains(task.Skill.Content, "Generated Skill") {
		t.Fatalf("done task = %+v", task)
	}
}

func TestSkillGenerateReturnsExistingRunningTask(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	c.skillGenerator = func(ctx context.Context, in model.SkillGenerationInput) (string, error) {
		close(started)
		<-release
		return "# Slow Skill\n\nGenerated after polling.", nil
	}
	seedSkillProjectFindings(t, c, "acme", "widgets")
	h := c.Routes()

	w := do(t, h, "POST", "/admin/api/skills/acme/widgets/generate", "", true)
	if w.Code != http.StatusAccepted {
		t.Fatalf("generate 1: code=%d body=%s", w.Code, w.Body.String())
	}
	first := decodeSkillTaskResponse(t, w.Body.Bytes())
	<-started

	w = do(t, h, "POST", "/admin/api/skills/acme/widgets/generate", "", true)
	if w.Code != http.StatusAccepted {
		t.Fatalf("generate 2: code=%d body=%s", w.Code, w.Body.String())
	}
	second := decodeSkillTaskResponse(t, w.Body.Bytes())
	if second.ID != first.ID {
		t.Fatalf("second task id=%q, want existing %q", second.ID, first.ID)
	}
	close(release)
	task := waitSkillTask(t, h, "acme", "widgets", first.ID)
	if task.Status != "done" {
		t.Fatalf("task status=%q, want done", task.Status)
	}
}

func TestSkillGenerateAsyncFailure(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	c.skillGenerator = func(ctx context.Context, in model.SkillGenerationInput) (string, error) {
		return "", context.DeadlineExceeded
	}
	seedSkillProjectFindings(t, c, "acme", "widgets")
	h := c.Routes()

	w := do(t, h, "POST", "/admin/api/skills/acme/widgets/generate", "", true)
	if w.Code != http.StatusAccepted {
		t.Fatalf("generate: code=%d body=%s", w.Code, w.Body.String())
	}
	task := decodeSkillTaskResponse(t, w.Body.Bytes())
	task = waitSkillTask(t, h, "acme", "widgets", task.ID)
	if task.Status != "failed" || task.Error == "" {
		t.Fatalf("failed task = %+v", task)
	}
}

func TestIndexServed(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "GET", "/admin/", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("index: code = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("index content-type = %q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "gitea-review-agent 控制台") {
		t.Errorf("index body missing title")
	}
}

func seedSkillProjectFindings(t *testing.T, c *Console, owner, repo string) {
	t.Helper()
	ctx := context.Background()
	pr := model.PRRef{Owner: owner, Repo: repo, Number: 12}
	if err := c.store.UpsertPull(ctx, &model.PullState{PR: pr, Author: "dev-a", HeadSHA: "sha0"}); err != nil {
		t.Fatalf("UpsertPull: %v", err)
	}
	pull, err := c.store.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	fs := []model.Finding{{Path: "src/app.tsx", Line: 42, Side: model.SideNew, Severity: model.SeverityHigh, Title: "Repeated bug", Body: "details", Tags: []string{"ui"}}}
	if err := c.store.SaveFindings(ctx, pull.ID, "sha1", fs, model.FindingSaveOptions{Agent: "codex"}); err != nil {
		t.Fatalf("SaveFindings: %v", err)
	}
}

func decodeSkillTaskResponse(t *testing.T, data []byte) skillGenerationTaskView {
	t.Helper()
	var resp struct {
		OK   bool                    `json:"ok"`
		Task skillGenerationTaskView `json:"task"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decode task: %v body=%s", err, string(data))
	}
	if !resp.OK {
		t.Fatalf("task response not ok: %s", string(data))
	}
	return resp.Task
}

func waitSkillTask(t *testing.T, h http.Handler, owner, repo, id string) skillGenerationTaskView {
	t.Helper()
	path := "/admin/api/skills/" + owner + "/" + repo + "/generate/" + id
	var task skillGenerationTaskView
	for i := 0; i < 50; i++ {
		w := do(t, h, "GET", path, "", true)
		if w.Code != http.StatusOK {
			t.Fatalf("poll task: code=%d body=%s", w.Code, w.Body.String())
		}
		task = decodeSkillTaskResponse(t, w.Body.Bytes())
		if task.Status != "running" {
			return task
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish, last=%+v", id, task)
	return task
}

func TestIsSecretKey(t *testing.T) {
	secret := []string{"gitea_token", "webhook_secret", "codex_api_key", "admin_password"}
	for _, k := range secret {
		if !isSecretKey(k) {
			t.Errorf("isSecretKey(%q) = false, want true", k)
		}
	}
	plain := []string{"gitea_url", "model", "concurrency", "trigger_keywords", "timeout"}
	for _, k := range plain {
		if isSecretKey(k) {
			t.Errorf("isSecretKey(%q) = true, want false", k)
		}
	}
}

func TestParseOptionalTimeDateOnly(t *testing.T) {
	from, err := parseOptionalTime("2026-06-05", false)
	if err != nil {
		t.Fatalf("parse from date: %v", err)
	}
	to, err := parseOptionalTime("2026-06-05", true)
	if err != nil {
		t.Fatalf("parse to date: %v", err)
	}
	if from.Hour() != 0 || from.Minute() != 0 || from.Second() != 0 || from.Nanosecond() != 0 {
		t.Fatalf("from = %s, want start of day", from.Format(time.RFC3339Nano))
	}
	if to.Hour() != 23 || to.Minute() != 59 || to.Second() != 59 || to.Nanosecond() != int(time.Second-time.Nanosecond) {
		t.Fatalf("to = %s, want end of day", to.Format(time.RFC3339Nano))
	}
}
