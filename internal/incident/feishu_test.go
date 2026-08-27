package incident

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

	notifier := FeishuWebhookNotifier{ConsoleBaseURL: "https://codex.example.com"}
	cfg := model.AnalysisConfig{Name: "serverx", FeishuWebhook: server.URL}
	task := model.AnalysisTask{
		ID: 12, ConfigName: "serverx", Alert: model.AlertEnvelope{
			Title: "API 响应异常", Endpoint: "/api/test", TraceID: "trace-1",
			DetailURL: "https://alerts.example.com/detail/1",
		},
	}
	result := &model.AnalysisResult{
		Summary: "定位到接口分支", Confidence: "medium", AssessedSeverity: "high",
		SeverityReason: "核心学习接口连续失败", ImpactScope: []string{"练习会话提交", "PROD 用户"},
		SuspectCommits: []model.AnalysisCommitEvidence{{SHA: "1234567890abcdef", Title: "change endpoint", Author: "Alice"}},
	}
	if err := notifier.SendPhase(context.Background(), cfg, task, "succeeded", result); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(payload)
	text := string(encoded)
	for _, want := range []string{"查看原告警详情", "https://alerts.example.com/detail/1", "AI 评估严重程度", "高", "核心学习接口连续失败", "练习会话提交", "PROD 用户", "1234567890", "Alice", "analysis_task=12"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card does not contain %q: %s", want, text)
		}
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

func TestFeishuSuppressedCardLinksExistingAnalysis(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&payload)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	originalID := int64(41)
	notifier := FeishuWebhookNotifier{ConsoleBaseURL: "https://codex.example.com"}
	task := model.AnalysisTask{
		ID: 42, DuplicateOfTaskID: &originalID, ConfigName: "serverx",
		Alert: model.AlertEnvelope{Endpoint: "/api/test", ErrorMessage: "same error"},
	}
	if err := notifier.SendPhase(context.Background(), model.AnalysisConfig{FeishuWebhook: server.URL}, task, "suppressed", nil); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(payload)
	text := string(encoded)
	for _, want := range []string{"重复报错，已分析", "任务 #41", "analysis_task=41"} {
		if !strings.Contains(text, want) {
			t.Fatalf("suppressed card does not contain %q: %s", want, text)
		}
	}
}
