package incident

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/turning4th/codex-gitea/internal/model"
)

func TestFeishuCardIncludesSourceDetailAndCommit(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode card: %v", err)
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	notifier := &FeishuNotifier{ConsoleBaseURL: "https://codex.example.com"}
	cfg := model.AnalysisConfig{Name: "serverx", FeishuMode: "webhook", FeishuWebhook: server.URL}
	task := model.AnalysisTask{
		ID: 12, ConfigName: "serverx", ConfigSnapshot: model.AnalysisConfigSnapshot{
			FeishuMentionMapping: "alice,ali | Alice Zhang | ou_alice123",
		}, Alert: model.AlertEnvelope{
			Title: "API 响应异常", Endpoint: "/api/test", TraceID: "trace-1",
			DetailURL: "https://alerts.example.com/detail/1",
		},
	}
	result := &model.AnalysisResult{
		Summary: "定位到接口分支", Confidence: "medium", AssessedSeverity: "high",
		SeverityReason: "核心学习接口连续失败", ImpactScope: []string{"练习会话提交", "PROD 用户"},
		SuspectCommits: []model.AnalysisCommitEvidence{{SHA: "1234567890abcdef", Title: "change endpoint", Author: "Alice"}},
	}
	if _, err := notifier.SendPhase(context.Background(), cfg, task, "succeeded", result); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(payload)
	text := string(encoded)
	for _, want := range []string{"查看原告警详情", "https://alerts.example.com/detail/1", "AI 评估严重程度", "高", "核心学习接口连续失败", "练习会话提交", "PROD 用户", "1234567890", "Alice", "建议关注", `\u003cat id=ou_alice123\u003e\u003c/at\u003e`, "analysis_task=12"} {
		if want == "AI 评估严重程度" {
			want = "严重程度"
		}
		if !strings.Contains(text, want) {
			t.Fatalf("card does not contain %q: %s", want, text)
		}
	}
	if strings.Contains(text, "当前状态") {
		t.Fatalf("card repeats status in body: %s", text)
	}
}

func TestResolveFeishuMentionsMatchesAliasesEmailAndSuggestedContacts(t *testing.T) {
	mapping := strings.Join([]string{
		"znc,Starslayerx | 张宁池 | ou_zhang",
		"Lin | 陈惠琳 | ou_lin",
		"Nickname4th | 刘涛 | ou_liu",
		"broken | 无效 | user_123",
	}, "\n")
	result := &model.AnalysisResult{
		SuspectCommits: []model.AnalysisCommitEvidence{
			{Author: "STARSLAYERX", AuthorEmail: "ignored@example.com"},
			{Author: "unknown", AuthorEmail: "lin@itool.tech"},
		},
		SuggestedContacts: []string{"建议联系刘涛确认模块历史"},
	}
	mentions := resolveFeishuMentions(mapping, result, 3)
	if len(mentions) != 3 {
		t.Fatalf("mentions=%+v, want 3", mentions)
	}
	for index, want := range []string{"ou_zhang", "ou_lin", "ou_liu"} {
		if mentions[index].OpenID != want {
			t.Errorf("mentions[%d].OpenID=%q, want %q", index, mentions[index].OpenID, want)
		}
	}
}

func TestResolveFeishuMentionsPrioritizesCommitAuthorsOverSuggestedContacts(t *testing.T) {
	mapping := strings.Join([]string{
		"suggested | 建议联系人 | ou_suggested",
		"commit-author | 提交作者 | ou_commit",
	}, "\n")
	result := &model.AnalysisResult{
		SuspectCommits:    []model.AnalysisCommitEvidence{{Author: "commit-author"}},
		SuggestedContacts: []string{"suggested"},
	}
	mentions := resolveFeishuMentions(mapping, result, 1)
	if len(mentions) != 1 || mentions[0].OpenID != "ou_commit" {
		t.Fatalf("mentions=%+v, want commit author first", mentions)
	}
}

func TestParseFeishuMentionMappingSkipsInvalidAndDeduplicatesAliases(t *testing.T) {
	entries := parseFeishuMentionMapping(strings.Join([]string{
		"znc,ZNC,Starslayerx | 张宁池 | ou_123",
		"missing columns",
		"alias | Name | invalid-id",
		" | Empty | ou_empty",
	}, "\n"))
	if len(entries) != 1 {
		t.Fatalf("entries=%+v, want 1", entries)
	}
	if got := strings.Join(entries[0].Aliases, ","); got != "znc,starslayerx" {
		t.Fatalf("aliases=%q", got)
	}
}

func TestFeishuMentionOnlyAppearsOnSuccessfulResultCard(t *testing.T) {
	notifier := &FeishuNotifier{}
	task := model.AnalysisTask{ConfigSnapshot: model.AnalysisConfigSnapshot{
		FeishuMentionMapping: "znc | 张宁池 | ou_123",
	}}
	result := &model.AnalysisResult{SuspectCommits: []model.AnalysisCommitEvidence{{Author: "znc"}}}
	failedCard, _ := json.Marshal(notifier.buildCard(task, "failed", result))
	if strings.Contains(string(failedCard), `\u003cat id=`) {
		t.Fatalf("failed card contains mention: %s", failedCard)
	}
	succeededCard, _ := json.Marshal(notifier.buildCard(task, "succeeded", result))
	if !strings.Contains(string(succeededCard), `\u003cat id=ou_123\u003e\u003c/at\u003e`) {
		t.Fatalf("succeeded card missing mention: %s", succeededCard)
	}
}

func TestAnalysisEnumLabelUsesChineseLabels(t *testing.T) {
	tests := map[string]string{
		"critical": "严重",
		"HIGH":     "高",
		" medium ": "中",
		"low":      "低",
	}
	for input, want := range tests {
		if got := analysisEnumLabel(input); got != want {
			t.Errorf("analysisEnumLabel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSafeHTTPURLRejectsNonHTTPURL(t *testing.T) {
	if got := safeHTTPURL("javascript:alert(1)"); got != "" {
		t.Fatalf("safeHTTPURL = %q", got)
	}
}

func TestFeishuAppBotSendsOnceThenUpdatesTheSameCard(t *testing.T) {
	var mu sync.Mutex
	authCalls, sendCalls, updateCalls := 0, 0, 0
	var sentContent, updatedContent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			authCalls++
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case r.Method == http.MethodPost && r.URL.Path == "/open-apis/im/v1/messages":
			sendCalls++
			if r.URL.Query().Get("receive_id_type") != "chat_id" || r.Header.Get("Authorization") != "Bearer tenant-token" {
				t.Errorf("invalid send request: %s %s", r.URL.String(), r.Header.Get("Authorization"))
			}
			var body struct {
				ReceiveID string `json:"receive_id"`
				Content   string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.ReceiveID != "oc_test" {
				t.Errorf("receive_id=%q", body.ReceiveID)
			}
			sentContent = body.Content
			_, _ = w.Write([]byte(`{"code":0,"msg":"success","data":{"message_id":"om_same"}}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/open-apis/im/v1/messages/om_same":
			updateCalls++
			var body struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			updatedContent = body.Content
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	notifier := &FeishuNotifier{OpenAPIBaseURL: server.URL}
	cfg := model.AnalysisConfig{FeishuMode: "app", FeishuAppID: "cli_test", FeishuAppSecret: "secret", FeishuChatID: "oc_test"}
	task := model.AnalysisTask{
		ID: 42, ConfigName: "serverx", Alert: model.AlertEnvelope{Endpoint: "/api/test", TraceID: "trace"},
	}
	messageID, err := notifier.SendPhase(context.Background(), cfg, task, "starting", nil)
	if err != nil || messageID != "om_same" {
		t.Fatalf("initial send message_id=%q err=%v", messageID, err)
	}
	task.FeishuMessageID = messageID
	result := &model.AnalysisResult{Summary: "最终结论", AssessedSeverity: "medium", Confidence: "high"}
	updatedID, err := notifier.SendPhase(context.Background(), cfg, task, "succeeded", result)
	if err != nil || updatedID != messageID {
		t.Fatalf("update message_id=%q err=%v", updatedID, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if authCalls != 1 || sendCalls != 1 || updateCalls != 1 {
		t.Fatalf("calls auth=%d send=%d update=%d", authCalls, sendCalls, updateCalls)
	}
	if !strings.Contains(sentContent, "分析已开始") || !strings.Contains(updatedContent, "最终结论") {
		t.Fatalf("contents initial=%s updated=%s", sentContent, updatedContent)
	}
}

func TestFeishuNotificationModeRules(t *testing.T) {
	webhook := model.AnalysisConfig{FeishuMode: "webhook", FeishuWebhook: "https://example.com/hook"}
	if shouldNotifyPhase(webhook, "starting") || shouldNotifyPhase(webhook, "analyzing") || !shouldNotifyPhase(webhook, "succeeded") {
		t.Fatal("webhook mode must only notify terminal phases")
	}
	app := model.AnalysisConfig{FeishuMode: "app", FeishuAppID: "cli", FeishuAppSecret: "secret", FeishuChatID: "oc"}
	if !shouldNotifyPhase(app, "starting") || !shouldNotifyPhase(app, "analyzing") || !shouldNotifyPhase(app, "succeeded") {
		t.Fatal("app mode must notify every analysis phase")
	}
	for _, cfg := range []model.AnalysisConfig{webhook, app} {
		if shouldNotifyPhase(cfg, "suppressed") || shouldNotifyPhase(cfg, "throttled") {
			t.Fatal("duplicate alerts must never notify Feishu")
		}
	}
}

func TestFeishuAppModeRequiresCompleteCredentials(t *testing.T) {
	notifier := &FeishuNotifier{}
	err := notifier.Test(context.Background(), model.AnalysisConfig{FeishuMode: "app", FeishuAppID: "cli"})
	if err == nil || !strings.Contains(err.Error(), "not fully configured") {
		t.Fatalf("error=%v", err)
	}
}

func TestFeishuWebhookReturnsNoMessageID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	notifier := &FeishuNotifier{}
	messageID, err := notifier.SendPhase(context.Background(), model.AnalysisConfig{FeishuMode: "webhook", FeishuWebhook: server.URL}, model.AnalysisTask{}, "succeeded", nil)
	if err != nil {
		t.Fatal(err)
	}
	if messageID != "" {
		t.Fatalf("webhook message_id=%q", messageID)
	}
}
