package console

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/turning4th/codex-gitea/internal/config"
	"github.com/turning4th/codex-gitea/internal/store"
)

func TestAnalysisConfigAppSecretIsRedactedAndPreservedOnUpdate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "console-analysis.db"), store.WithSecretKey("console-analysis-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	c := New(st, &config.Config{AdminPassword: testPassword}, t.TempDir(), AnalysisDependencies{Store: st})
	h := c.Routes()
	createBody := `{
		"name":"serverx prod","enabled":true,
		"repository_url":"https://gitea.example.com/serverx.git","repository_ref":"main",
		"sls_endpoint":"cn-beijing.log.aliyuncs.com","sls_project":"project","sls_logstore":"raw",
		"sls_access_key_id":"ak-id","sls_access_key_secret":"ak-secret",
		"feishu_mode":"app","feishu_app_id":"cli_test","feishu_app_secret":"app-secret","feishu_chat_id":"oc_old",
		"feishu_mention_mapping":"znc,Starslayerx | 张宁池 | ou_123",
		"ignored_error_codes":"4290,5001",
		"concurrency":4
	}`
	w := do(t, h, http.MethodPost, "/admin/api/alert-analysis/configs", createBody, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Config["feishu_app_secret"] != redacted {
		t.Fatalf("app secret response=%v, want redacted", created.Config["feishu_app_secret"])
	}
	if created.Config["concurrency"] != float64(4) {
		t.Fatalf("concurrency response=%v, want 4", created.Config["concurrency"])
	}
	if created.Config["feishu_mention_mapping"] != "znc,Starslayerx | 张宁池 | ou_123" {
		t.Fatalf("mention mapping response=%v", created.Config["feishu_mention_mapping"])
	}
	if created.Config["ignored_error_codes"] != "4290,5001" {
		t.Fatalf("ignored error codes response=%v", created.Config["ignored_error_codes"])
	}
	id, ok := created.Config["id"].(float64)
	if !ok || id <= 0 {
		t.Fatalf("created config id=%v", created.Config["id"])
	}

	updateBody := `{
		"name":"serverx prod","enabled":true,
		"repository_url":"https://gitea.example.com/serverx.git","repository_ref":"main",
		"sls_endpoint":"cn-beijing.log.aliyuncs.com","sls_project":"project","sls_logstore":"raw",
		"sls_access_key_id":"***set***","sls_access_key_secret":"***set***",
		"feishu_mode":"app","feishu_app_id":"cli_test","feishu_app_secret":"***set***","feishu_chat_id":"oc_new",
		"feishu_mention_mapping":"Lin | 陈惠琳 | ou_456",
		"ignored_error_codes":"4290",
		"concurrency":3
	}`
	w = do(t, h, http.MethodPut, "/admin/api/alert-analysis/configs/1", updateBody, true)
	if w.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", w.Code, w.Body.String())
	}
	stored, err := st.GetAnalysisConfig(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if stored.FeishuAppSecret != "app-secret" || stored.FeishuChatID != "oc_new" || stored.Concurrency != 3 || stored.FeishuMentionMapping != "Lin | 陈惠琳 | ou_456" || stored.IgnoredErrorCodes != "4290" {
		t.Fatalf("stored app config secret=%q chat=%q concurrency=%d mentions=%q ignored=%q", stored.FeishuAppSecret, stored.FeishuChatID, stored.Concurrency, stored.FeishuMentionMapping, stored.IgnoredErrorCodes)
	}
}
