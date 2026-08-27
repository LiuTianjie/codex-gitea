package incident

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

type Notifier interface {
	SendPhase(context.Context, model.AnalysisConfig, model.AnalysisTask, string, *model.AnalysisResult) (string, error)
	Test(context.Context, model.AnalysisConfig) error
}

type cachedFeishuToken struct {
	value     string
	expiresAt time.Time
}

type feishuMentionEntry struct {
	Aliases     []string
	DisplayName string
	OpenID      string
}

var validFeishuOpenID = regexp.MustCompile(`^ou_[A-Za-z0-9_-]+$`)

type FeishuNotifier struct {
	HTTPClient       *http.Client
	ConsoleBaseURL   string
	OpenAPIBaseURL   string
	tokenMu          sync.Mutex
	tenantTokenCache map[string]cachedFeishuToken
}

func (n *FeishuNotifier) SendPhase(ctx context.Context, cfg model.AnalysisConfig, task model.AnalysisTask, phase string, result *model.AnalysisResult) (string, error) {
	card := n.buildCard(task, phase, result)
	if cfg.FeishuMode == "app" {
		return n.sendOrUpdateAppCard(ctx, cfg, task, card)
	}
	if strings.TrimSpace(cfg.FeishuWebhook) == "" {
		return "", nil
	}
	payload, err := json.Marshal(map[string]any{"msg_type": "interactive", "card": card})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.FeishuWebhook, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("send Feishu analysis card: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Feishu webhook status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
			return "", fmt.Errorf("Feishu webhook rejected card: %d %s%s", code, envelope.Msg, envelope.Message)
		}
	}
	return "", nil
}

func (n *FeishuNotifier) Test(ctx context.Context, cfg model.AnalysisConfig) error {
	if cfg.FeishuMode == "app" {
		if strings.TrimSpace(cfg.FeishuAppID) == "" || strings.TrimSpace(cfg.FeishuAppSecret) == "" || strings.TrimSpace(cfg.FeishuChatID) == "" {
			return fmt.Errorf("Feishu app bot is not fully configured")
		}
	} else if strings.TrimSpace(cfg.FeishuWebhook) == "" {
		return fmt.Errorf("Feishu webhook is not configured")
	}
	task := model.AnalysisTask{ID: 0, ConfigName: cfg.Name, Status: model.AnalysisTaskQueued, Phase: "test", Alert: model.AlertEnvelope{Title: "告警分析配置测试", Environment: "TEST", Endpoint: "/connection-test"}}
	_, err := n.SendPhase(ctx, cfg, task, "test", nil)
	return err
}

func (n *FeishuNotifier) sendOrUpdateAppCard(ctx context.Context, cfg model.AnalysisConfig, task model.AnalysisTask, card map[string]any) (string, error) {
	token, err := n.tenantAccessToken(ctx, cfg.FeishuAppID, cfg.FeishuAppSecret)
	if err != nil {
		return "", err
	}
	content, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(task.FeishuMessageID) != "" {
		payload, err := json.Marshal(map[string]string{"content": string(content)})
		if err != nil {
			return "", err
		}
		endpoint := n.apiBaseURL() + "/open-apis/im/v1/messages/" + url.PathEscape(task.FeishuMessageID)
		if err := n.doOpenAPI(ctx, http.MethodPatch, endpoint, token, payload, nil); err != nil {
			return "", fmt.Errorf("update Feishu analysis card: %w", err)
		}
		return task.FeishuMessageID, nil
	}
	body := map[string]any{
		"receive_id": cfg.FeishuChatID,
		"msg_type":   "interactive",
		"content":    string(content),
	}
	if task.ID > 0 {
		body["uuid"] = fmt.Sprintf("codex-alert-%d", task.ID)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	endpoint := n.apiBaseURL() + "/open-apis/im/v1/messages?receive_id_type=chat_id"
	var response struct {
		MessageID string `json:"message_id"`
	}
	if err := n.doOpenAPI(ctx, http.MethodPost, endpoint, token, payload, &response); err != nil {
		return "", fmt.Errorf("send Feishu app analysis card: %w", err)
	}
	if strings.TrimSpace(response.MessageID) == "" {
		return "", fmt.Errorf("send Feishu app analysis card: response has no message_id")
	}
	return response.MessageID, nil
}

func (n *FeishuNotifier) tenantAccessToken(ctx context.Context, appID, appSecret string) (string, error) {
	key := fmt.Sprintf("%s:%x", appID, sha256.Sum256([]byte(appSecret)))
	n.tokenMu.Lock()
	defer n.tokenMu.Unlock()
	if cached, ok := n.tenantTokenCache[key]; ok && time.Until(cached.expiresAt) > 5*time.Minute {
		return cached.value, nil
	}
	payload, err := json.Marshal(map[string]string{"app_id": appID, "app_secret": appSecret})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.apiBaseURL()+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := n.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("get Feishu tenant token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var envelope struct {
		Code        int    `json:"code"`
		Msg         string `json:"msg"`
		AccessToken string `json:"tenant_access_token"`
		Expire      int    `json:"expire"`
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || json.Unmarshal(body, &envelope) != nil || envelope.Code != 0 || envelope.AccessToken == "" {
		return "", fmt.Errorf("get Feishu tenant token rejected: status=%d code=%d msg=%s", resp.StatusCode, envelope.Code, envelope.Msg)
	}
	if envelope.Expire <= 0 {
		envelope.Expire = 7200
	}
	if n.tenantTokenCache == nil {
		n.tenantTokenCache = map[string]cachedFeishuToken{}
	}
	n.tenantTokenCache[key] = cachedFeishuToken{value: envelope.AccessToken, expiresAt: time.Now().Add(time.Duration(envelope.Expire) * time.Second)}
	return envelope.AccessToken, nil
}

func (n *FeishuNotifier) doOpenAPI(ctx context.Context, method, endpoint, token string, payload []byte, data any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := n.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("status=%d invalid JSON response", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || envelope.Code != 0 {
		return fmt.Errorf("status=%d code=%d msg=%s", resp.StatusCode, envelope.Code, envelope.Msg)
	}
	if data != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, data); err != nil {
			return fmt.Errorf("decode Feishu response data: %w", err)
		}
	}
	return nil
}

func (n *FeishuNotifier) client() *http.Client {
	if n.HTTPClient != nil {
		return n.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (n *FeishuNotifier) apiBaseURL() string {
	if base := strings.TrimRight(strings.TrimSpace(n.OpenAPIBaseURL), "/"); base != "" {
		return base
	}
	return "https://open.feishu.cn"
}

func (n *FeishuNotifier) buildCard(task model.AnalysisTask, phase string, result *model.AnalysisResult) map[string]any {
	template, tagColor, statusText := phaseStyle(phase)
	endpoint := firstNonEmpty(task.Alert.Endpoint, task.Alert.Service, "未知目标")
	alertTitle := firstNonEmpty(task.Alert.Title, task.Alert.Rule, "告警分析")
	traceID := firstNonEmpty(task.Alert.TraceID, "无")
	errorCode := firstNonEmpty(task.Alert.ErrorCode, "未提供")
	detailFields := []any{
		map[string]any{"is_short": false, "text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**告警**\n%s", safeCardText(alertTitle, 300))}},
		map[string]any{"is_short": false, "text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**接口 / 服务**\n`%s`", safeCardText(endpoint, 300))}},
		map[string]any{"is_short": true, "text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**Trace ID**\n`%s`", safeCardText(traceID, 160))}},
		map[string]any{"is_short": true, "text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**错误码**\n%s", safeCardText(errorCode, 100))}},
	}
	elements := []any{map[string]any{
		"tag": "column_set", "flex_mode": "none", "margin": "0px 0px 12px 0px",
		"columns": []any{map[string]any{
			"tag": "column", "width": "weighted", "weight": 1, "padding": "12px", "background_style": "grey-50",
			"elements": []any{map[string]any{"tag": "div", "fields": detailFields}},
		}},
	}}
	if link := safeHTTPURL(task.Alert.DetailURL); link != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": fmt.Sprintf("[查看原告警详情](%s)", link), "margin": "0px 0px 12px 0px"})
	}
	if result != nil {
		elements = append(elements, map[string]any{
			"tag": "column_set", "flex_mode": "bisect", "horizontal_spacing": "8px", "margin": "0px 0px 12px 0px",
			"columns": []any{
				map[string]any{"tag": "column", "padding": "10px", "background_style": "grey-50", "elements": []any{map[string]any{"tag": "markdown", "content": fmt.Sprintf("**严重程度**\n%s", analysisEnumLabel(result.AssessedSeverity))}}},
				map[string]any{"tag": "column", "padding": "10px", "background_style": "grey-50", "elements": []any{map[string]any{"tag": "markdown", "content": fmt.Sprintf("**分析置信度**\n%s", analysisEnumLabel(result.Confidence))}}},
			},
		})
		conclusion := fmt.Sprintf("**分析结论**\n%s", safeCardText(result.Summary, 1200))
		if strings.TrimSpace(result.SeverityReason) != "" {
			conclusion += "\n\n" + safeCardText(result.SeverityReason, 500)
		}
		elements = append(elements, map[string]any{
			"tag": "column_set", "flex_mode": "none", "margin": "0px 0px 12px 0px",
			"columns": []any{map[string]any{
				"tag": "column", "width": "weighted", "weight": 1, "padding": "12px", "background_style": template + "-50",
				"elements": []any{map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": conclusion, "lines": 9}}},
			}},
		})
		impact := bulletList(result.ImpactScope, 5, 900)
		actions := bulletList(result.RecommendedActions, 3, 600)
		if impact != "" || actions != "" {
			content := ""
			if impact != "" {
				content = "**影响面**\n" + impact
			}
			if actions != "" {
				if content != "" {
					content += "\n\n"
				}
				content += "**建议操作**\n" + actions
			}
			elements = append(elements, map[string]any{"tag": "markdown", "content": content, "margin": "0px 0px 12px 0px"})
		}
		if len(result.SuspectCommits) > 0 {
			item := result.SuspectCommits[0]
			fields := []any{
				map[string]any{"is_short": false, "text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**最相关提交**\n`%s` %s", safeCardText(shortSHA(item.SHA), 20), safeCardText(item.Title, 300))}},
				map[string]any{"is_short": true, "text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**相关作者**\n%s", safeCardText(firstNonEmpty(item.Author, "未知"), 100))}},
				map[string]any{"is_short": true, "text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**提交置信度**\n%s", analysisEnumLabel(item.Confidence))}},
			}
			elements = append(elements, map[string]any{
				"tag": "column_set", "flex_mode": "none", "margin": "0px 0px 12px 0px",
				"columns": []any{map[string]any{
					"tag": "column", "width": "weighted", "weight": 1, "padding": "12px", "background_style": "grey-50",
					"elements": []any{map[string]any{"tag": "div", "fields": fields}},
				}},
			})
		}
		if phase == "succeeded" {
			if mentions := resolveFeishuMentions(task.ConfigSnapshot.FeishuMentionMapping, result, 3); len(mentions) > 0 {
				items := make([]string, 0, len(mentions))
				for _, mention := range mentions {
					items = append(items, fmt.Sprintf("<at id=%s></at>", mention.OpenID))
				}
				elements = append(elements, map[string]any{
					"tag":     "markdown",
					"content": "**建议关注**\n" + strings.Join(items, "、"),
					"margin":  "0px 0px 12px 0px",
				})
			}
		}
	}
	if link := n.taskURL(task.ID); link != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": fmt.Sprintf("[在 Codex-Gitea 控制台查看任务 #%d](%s)", task.ID, link)})
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true, "width_mode": "default", "summary": map[string]any{"content": fmt.Sprintf("告警分析 #%d · %s", task.ID, statusText)}},
		"header": map[string]any{
			"title":         map[string]any{"tag": "plain_text", "content": fmt.Sprintf("告警分析 #%d", task.ID)},
			"subtitle":      map[string]any{"tag": "plain_text", "content": strings.Join(nonEmptyStrings(task.ConfigName, task.Alert.Environment), " · ")},
			"template":      template,
			"icon":          map[string]any{"tag": "standard_icon", "token": "ai-common_colorful"},
			"text_tag_list": []any{map[string]any{"tag": "text_tag", "text": map[string]any{"tag": "plain_text", "content": statusText}, "color": tagColor}},
		},
		"body": map[string]any{"direction": "vertical", "padding": "12px 12px 20px 12px", "vertical_spacing": "12px", "elements": elements},
	}
}

func parseFeishuMentionMapping(raw string) []feishuMentionEntry {
	entries := make([]feishuMentionEntry, 0)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		displayName := strings.TrimSpace(parts[1])
		openID := strings.TrimSpace(parts[2])
		if displayName == "" || !validFeishuOpenID.MatchString(openID) {
			continue
		}
		aliases := make([]string, 0)
		seen := map[string]struct{}{}
		for _, alias := range strings.Split(parts[0], ",") {
			alias = normalizeMentionKey(alias)
			if alias == "" {
				continue
			}
			if _, ok := seen[alias]; ok {
				continue
			}
			seen[alias] = struct{}{}
			aliases = append(aliases, alias)
		}
		if len(aliases) == 0 {
			continue
		}
		entries = append(entries, feishuMentionEntry{Aliases: aliases, DisplayName: displayName, OpenID: openID})
	}
	return entries
}

func resolveFeishuMentions(raw string, result *model.AnalysisResult, limit int) []feishuMentionEntry {
	if result == nil || limit <= 0 {
		return nil
	}
	entries := parseFeishuMentionMapping(raw)
	if len(entries) == 0 {
		return nil
	}
	mentions := make([]feishuMentionEntry, 0, min(limit, len(entries)))
	seenOpenIDs := map[string]struct{}{}
	appendMatches := func(candidates []string) bool {
		for _, candidate := range candidates {
			for _, entry := range entries {
				if _, ok := seenOpenIDs[entry.OpenID]; ok || !mentionEntryMatches(entry, []string{candidate}) {
					continue
				}
				seenOpenIDs[entry.OpenID] = struct{}{}
				mentions = append(mentions, entry)
				if len(mentions) >= limit {
					return true
				}
			}
		}
		return false
	}
	for _, commit := range result.SuspectCommits {
		candidates := []string{commit.Author}
		if email := strings.TrimSpace(commit.AuthorEmail); email != "" {
			if at := strings.IndexByte(email, '@'); at > 0 {
				email = email[:at]
			}
			candidates = append(candidates, email)
		}
		if appendMatches(candidates) {
			return mentions
		}
	}
	appendMatches(result.SuggestedContacts)
	return mentions
}

func mentionEntryMatches(entry feishuMentionEntry, candidates []string) bool {
	nameKey := normalizeMentionKey(entry.DisplayName)
	for _, candidate := range candidates {
		candidateKey := normalizeMentionKey(candidate)
		if candidateKey == "" {
			continue
		}
		if candidateKey == nameKey || (nameKey != "" && strings.Contains(candidateKey, nameKey)) {
			return true
		}
		tokens := mentionCandidateTokens(candidateKey)
		for _, alias := range entry.Aliases {
			if candidateKey == alias {
				return true
			}
			for _, token := range tokens {
				if token == alias {
					return true
				}
			}
		}
	}
	return false
}

func mentionCandidateTokens(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-')
	})
}

func normalizeMentionKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func bulletList(values []string, maxItems, maxRunes int) string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		value = safeCardText(value, maxRunes)
		if value == "" {
			continue
		}
		items = append(items, "- "+value)
		if maxItems > 0 && len(items) >= maxItems {
			break
		}
	}
	return strings.Join(items, "\n")
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return []string{"Codex-Gitea"}
	}
	return out
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

func (n *FeishuNotifier) taskURL(id int64) string {
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
