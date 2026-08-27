package incident

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

type Notifier interface {
	SendPhase(context.Context, model.AnalysisConfig, model.AnalysisTask, string, *model.AnalysisResult) error
	Test(context.Context, model.AnalysisConfig) error
}

type FeishuWebhookNotifier struct {
	HTTPClient     *http.Client
	ConsoleBaseURL string
}

func (n FeishuWebhookNotifier) SendPhase(ctx context.Context, cfg model.AnalysisConfig, task model.AnalysisTask, phase string, result *model.AnalysisResult) error {
	if strings.TrimSpace(cfg.FeishuWebhook) == "" {
		return nil
	}
	card := n.buildCard(task, phase, result)
	payload, err := json.Marshal(map[string]any{"msg_type": "interactive", "card": card})
	if err != nil {
		return err
	}
	client := n.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.FeishuWebhook, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send Feishu analysis card: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Feishu webhook status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		Code       *int   `json:"code"`
		StatusCode *int   `json:"StatusCode"`
		Msg        string `json:"msg"`
		Message    string `json:"StatusMessage"`
	}
	if len(bytes.TrimSpace(body)) > 0 && json.Unmarshal(body, &envelope) == nil {
		code := 0
		if envelope.Code != nil {
			code = *envelope.Code
		} else if envelope.StatusCode != nil {
			code = *envelope.StatusCode
		}
		if code != 0 {
			return fmt.Errorf("Feishu webhook rejected card: %d %s%s", code, envelope.Msg, envelope.Message)
		}
	}
	return nil
}

func (n FeishuWebhookNotifier) Test(ctx context.Context, cfg model.AnalysisConfig) error {
	if strings.TrimSpace(cfg.FeishuWebhook) == "" {
		return fmt.Errorf("Feishu webhook is not configured")
	}
	task := model.AnalysisTask{ID: 0, ConfigName: cfg.Name, Status: model.AnalysisTaskQueued, Phase: "test", Alert: model.AlertEnvelope{Title: "告警分析配置测试", Environment: "TEST", Endpoint: "/connection-test"}}
	return n.SendPhase(ctx, cfg, task, "test", nil)
}

func (n FeishuWebhookNotifier) buildCard(task model.AnalysisTask, phase string, result *model.AnalysisResult) map[string]any {
	template, tagColor, statusText := phaseStyle(phase)
	endpoint := firstNonEmpty(task.Alert.Endpoint, task.Alert.Service, "未知目标")
	alertTitle := firstNonEmpty(task.Alert.Title, task.Alert.Rule, "告警分析")
	detail := fmt.Sprintf("**告警**\n%s\n\n**接口 / 服务**\n`%s`\n\n**Trace ID**\n`%s`", safeCardText(alertTitle, 300), safeCardText(endpoint, 300), safeCardText(firstNonEmpty(task.Alert.TraceID, "无"), 200))
	statusBlock := map[string]any{
		"tag": "column_set", "flex_mode": "none", "background_style": template + "-50", "horizontal_spacing": "8px", "margin": "0px 0px 12px 0px",
		"columns": []any{map[string]any{
			"tag": "column", "width": "weighted", "weight": 1, "padding": "12px", "vertical_spacing": "4px",
			"elements": []any{
				map[string]any{"tag": "markdown", "content": "**当前状态**"},
				map[string]any{"tag": "markdown", "content": safeCardText(statusText, 200)},
			},
		}},
	}
	elements := []any{statusBlock, map[string]any{"tag": "markdown", "content": detail, "margin": "0px 0px 12px 0px"}}
	if phase == "suppressed" {
		duplicate := "相同接口与错误信息已有分析任务，本次不再重复运行。"
		if task.DuplicateOfTaskID != nil {
			duplicate = fmt.Sprintf("相同接口与错误信息已由任务 #%d 分析，本次不再重复运行。", *task.DuplicateOfTaskID)
			if link := n.taskURL(*task.DuplicateOfTaskID); link != "" {
				duplicate += fmt.Sprintf("\n\n[查看已有分析任务 #%d](%s)", *task.DuplicateOfTaskID, link)
			}
		}
		elements = append(elements, map[string]any{"tag": "markdown", "content": "**重复报错，已分析**\n" + duplicate})
	}
	if link := safeHTTPURL(task.Alert.DetailURL); link != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": fmt.Sprintf("[查看原告警详情](%s)", link)})
	}
	if result != nil {
		impact := "影响面尚不明确"
		if len(result.ImpactScope) > 0 {
			impact = "- " + strings.Join(result.ImpactScope, "\n- ")
		}
		commit := "未锁定具体提交"
		if len(result.SuspectCommits) > 0 {
			item := result.SuspectCommits[0]
			commit = fmt.Sprintf("`%s` %s\n相关作者：%s", safeCardText(shortSHA(item.SHA), 20), safeCardText(item.Title, 300), safeCardText(firstNonEmpty(item.Author, "未知"), 100))
		}
		resultContent := fmt.Sprintf("**分析结论**\n%s\n\n**AI 评估严重程度**\n%s\n%s\n\n**影响面**\n%s\n\n**分析置信度**\n%s\n\n**最相关提交**\n%s",
			safeCardText(result.Summary, 1200), analysisEnumLabel(result.AssessedSeverity),
			safeCardText(result.SeverityReason, 500), safeCardText(impact, 800), analysisEnumLabel(result.Confidence), commit)
		elements = append(elements, map[string]any{
			"tag": "column_set", "flex_mode": "none", "background_style": template + "-50", "columns": []any{map[string]any{
				"tag": "column", "width": "weighted", "weight": 1, "padding": "12px", "elements": []any{map[string]any{"tag": "markdown", "content": resultContent}},
			}},
		})
	}
	if link := n.taskURL(task.ID); link != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": fmt.Sprintf("[在 Codex-Gitea 控制台查看任务 #%d](%s)", task.ID, link)})
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true, "width_mode": "default", "summary": map[string]any{"content": fmt.Sprintf("告警分析 #%d · %s", task.ID, statusText)}},
		"header": map[string]any{
			"title":         map[string]any{"tag": "plain_text", "content": fmt.Sprintf("告警分析 #%d", task.ID)},
			"subtitle":      map[string]any{"tag": "plain_text", "content": firstNonEmpty(task.ConfigName, task.Alert.Environment, "Codex-Gitea")},
			"template":      template,
			"icon":          map[string]any{"tag": "standard_icon", "token": "ai-common_colorful"},
			"text_tag_list": []any{map[string]any{"tag": "text_tag", "text": map[string]any{"tag": "plain_text", "content": statusText}, "color": tagColor}},
		},
		"body": map[string]any{"direction": "vertical", "padding": "12px 12px 20px 12px", "vertical_spacing": "12px", "elements": elements},
	}
}

func analysisEnumLabel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "critical":
		return "严重"
	case "high":
		return "高"
	case "medium":
		return "中"
	case "low":
		return "低"
	default:
		return safeCardText(value, 50)
	}
}

func phaseStyle(phase string) (template, tagColor, label string) {
	switch phase {
	case "succeeded":
		return "green", "green", "分析完成"
	case "failed":
		return "red", "red", "分析失败"
	case "canceled", "cancel_requested":
		return "grey", "neutral", "分析已取消"
	case "suppressed", "throttled":
		return "orange", "orange", "重复报错，已分析"
	case "logs_ready":
		return "blue", "blue", "原始日志已获取"
	case "repository_ready":
		return "blue", "blue", "代码版本已准备"
	case "analyzing":
		return "violet", "violet", "正在分析代码和提交"
	case "test":
		return "blue", "blue", "配置连接测试"
	default:
		return "blue", "blue", "分析已开始"
	}
}

func (n FeishuWebhookNotifier) taskURL(id int64) string {
	base := strings.TrimRight(strings.TrimSpace(n.ConsoleBaseURL), "/")
	if base == "" || id <= 0 {
		return ""
	}
	u, err := url.Parse(base + "/admin/")
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("analysis_task", strconv.FormatInt(id, 10))
	u.RawQuery = q.Encode()
	return u.String()
}

func safeCardText(value string, max int) string {
	value = strings.ReplaceAll(value, "<at", "&#60;at")
	value = strings.ReplaceAll(value, "<person", "&#60;person")
	value = strings.TrimSpace(value)
	if max > 0 && len([]rune(value)) > max {
		return string([]rune(value)[:max]) + "…"
	}
	return value
}

func safeHTTPURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	return parsed.String()
}

func shortSHA(value string) string {
	if len(value) > 10 {
		return value[:10]
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
