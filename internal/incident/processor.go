package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/turning4th/codex-gitea/internal/model"
)

type RevisionCache interface {
	PrepareRevision(context.Context, string, string, int64) (string, string, error)
	CleanupRevision(string, int64) error
}

type AnalyzerFunc func(context.Context, string, string, model.AnalysisConfig) (string, error)

type TaskProcessor interface {
	Process(context.Context, *model.AnalysisTask) (string, error)
	NotifyTerminal(context.Context, *model.AnalysisTask, string)
}

type Processor struct {
	Store    model.AnalysisStore
	Logs     SLSFetcher
	Cache    RevisionCache
	Analyze  AnalyzerFunc
	Notifier Notifier
}

func TestRepository(ctx context.Context, cache RevisionCache, cfg model.AnalysisConfig) error {
	if cache == nil {
		return errors.New("repository cache is not configured")
	}
	const testTaskID int64 = 0
	_, _, err := cache.PrepareRevision(ctx, cfg.RepositoryURL, cfg.RepositoryRef, testTaskID)
	if err != nil {
		return err
	}
	return cache.CleanupRevision(cfg.RepositoryURL, testTaskID)
}

func (p *Processor) Process(ctx context.Context, task *model.AnalysisTask) (string, error) {
	if task == nil || task.ConfigID == nil {
		return "", errors.New("analysis task configuration was deleted")
	}
	cfg, err := p.Store.GetAnalysisConfig(ctx, *task.ConfigID)
	if err != nil {
		return "", err
	}
	p.phase(ctx, *cfg, task, "starting", "开始分析告警", nil)

	if err := p.setPhase(ctx, task.ID, "fetching_logs", "正在查询 SLS 原始日志"); err != nil {
		return "", err
	}
	logs, err := p.Logs.Fetch(ctx, *cfg, task.Alert)
	if err != nil {
		return "", fmt.Errorf("fetch alert logs: %w", err)
	}
	logData, _ := json.Marshal(map[string]any{"count": len(logs)})
	p.phase(ctx, *cfg, task, "logs_ready", fmt.Sprintf("已获取 %d 条原始日志", len(logs)), logData)

	revision := strings.TrimSpace(task.Alert.DeploymentSHA)
	if revision == "" {
		revision = cfg.RepositoryRef
	}
	if err := p.setPhase(ctx, task.ID, "preparing_repository", "正在准备只读代码版本 "+revision); err != nil {
		return "", err
	}
	worktree, resolvedSHA, err := p.Cache.PrepareRevision(ctx, cfg.RepositoryURL, revision, task.ID)
	if err != nil {
		return "", fmt.Errorf("prepare analysis repository: %w", err)
	}
	defer p.Cache.CleanupRevision(cfg.RepositoryURL, task.ID)
	repoData, _ := json.Marshal(map[string]any{"revision": revision, "resolved_sha": resolvedSHA})
	p.phase(ctx, *cfg, task, "repository_ready", "代码版本已准备："+shortSHA(resolvedSHA), repoData)

	gitFacts, err := collectGitFacts(ctx, worktree)
	if err != nil {
		return "", err
	}
	if err := p.setPhase(ctx, task.ID, "analyzing", "正在定位接口、代码和相关提交"); err != nil {
		return "", err
	}
	p.phase(ctx, *cfg, task, "analyzing", "Codex 正在综合日志与 Git 证据", nil)
	prompt := BuildPrompt(task.Alert, logs, gitFacts, resolvedSHA, task.Alert.DeploymentSHA != "", cfg.Prompt)
	if p.Analyze == nil {
		return "", errors.New("incident analyzer is not configured")
	}
	raw, err := p.Analyze(ctx, worktree, prompt, *cfg)
	if err != nil {
		return "", fmt.Errorf("run incident analyzer: %w", err)
	}
	result, err := parseAnalysisResult(raw)
	if err != nil {
		return "", err
	}
	if task.Alert.DeploymentSHA == "" {
		result.EvidenceGaps = appendUnique(result.EvidenceGaps, "未提供 deployment_sha；提交判断基于配置分支当前版本，未确认与告警发生时线上版本完全一致")
	}
	if len(logs) == 0 {
		result.EvidenceGaps = appendUnique(result.EvidenceGaps, "SLS 查询未返回匹配的原始日志")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	p.phase(ctx, *cfg, task, "succeeded", "分析完成", encoded)
	return string(encoded), nil
}

func (p *Processor) setPhase(ctx context.Context, taskID int64, phase, message string) error {
	if err := p.Store.SetAnalysisTaskPhase(ctx, taskID, phase); err != nil {
		return err
	}
	return p.Store.AppendAnalysisTaskEvent(ctx, taskID, phase, "info", message, nil)
}

func (p *Processor) phase(ctx context.Context, cfg model.AnalysisConfig, task *model.AnalysisTask, phase, message string, data json.RawMessage) {
	_ = p.Store.SetAnalysisTaskPhase(ctx, task.ID, phase)
	_ = p.Store.AppendAnalysisTaskEvent(ctx, task.ID, phase, "info", message, data)
	task.Phase = phase
	if p.Notifier == nil || !shouldNotifyPhase(cfg, phase) {
		return
	}
	var result *model.AnalysisResult
	if phase == "succeeded" && len(data) > 0 {
		var parsed model.AnalysisResult
		if json.Unmarshal(data, &parsed) == nil {
			result = &parsed
		}
	}
	messageID, err := p.Notifier.SendPhase(ctx, cfg, *task, phase, result)
	if err != nil {
		_ = p.Store.SetAnalysisNotification(context.Background(), task.ID, "failed", err.Error(), "")
		_ = p.Store.AppendAnalysisTaskEvent(context.Background(), task.ID, "notification", "warning", "飞书通知失败："+err.Error(), nil)
		return
	}
	if messageID != "" {
		task.FeishuMessageID = messageID
	}
	_ = p.Store.SetAnalysisNotification(context.Background(), task.ID, "sent", "", messageID)
}

func (p *Processor) NotifyTerminal(ctx context.Context, task *model.AnalysisTask, phase string) {
	if task == nil || task.ConfigID == nil || p.Notifier == nil {
		return
	}
	if phase == "suppressed" || phase == "throttled" {
		return
	}
	cfg, err := p.Store.GetAnalysisConfig(ctx, *task.ConfigID)
	if err != nil || !hasFeishuDestination(*cfg) {
		return
	}
	task.Phase = phase
	messageID, err := p.Notifier.SendPhase(ctx, *cfg, *task, phase, nil)
	if err != nil {
		_ = p.Store.SetAnalysisNotification(context.Background(), task.ID, "failed", err.Error(), "")
		return
	}
	if messageID != "" {
		task.FeishuMessageID = messageID
	}
	_ = p.Store.SetAnalysisNotification(context.Background(), task.ID, "sent", "", messageID)
}

func shouldNotifyPhase(cfg model.AnalysisConfig, phase string) bool {
	if !hasFeishuDestination(cfg) || phase == "suppressed" || phase == "throttled" {
		return false
	}
	if cfg.FeishuMode == "app" {
		return true
	}
	return phase == "succeeded" || phase == "failed" || phase == "canceled"
}

func hasFeishuDestination(cfg model.AnalysisConfig) bool {
	if cfg.FeishuMode == "app" {
		return strings.TrimSpace(cfg.FeishuAppID) != "" && strings.TrimSpace(cfg.FeishuAppSecret) != "" && strings.TrimSpace(cfg.FeishuChatID) != ""
	}
	return strings.TrimSpace(cfg.FeishuWebhook) != ""
}

func collectGitFacts(ctx context.Context, worktree string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "log", "-n", "40", "--date=iso-strict", "--pretty=format:%H%x09%an%x09%ae%x09%ad%x09%s")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("collect git history: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func parseAnalysisResult(raw string) (*model.AnalysisResult, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		if len(lines) >= 3 {
			lines = lines[1 : len(lines)-1]
			raw = strings.Join(lines, "\n")
		}
	}
	var result model.AnalysisResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("decode incident analysis result: %w", err)
	}
	if strings.TrimSpace(result.Summary) == "" {
		return nil, errors.New("incident analysis result has no summary")
	}
	if strings.TrimSpace(result.Confidence) == "" {
		result.Confidence = "low"
	}
	severity, validSeverity := normalizeAssessedSeverity(result.AssessedSeverity)
	result.AssessedSeverity = severity
	if strings.TrimSpace(result.SeverityReason) == "" {
		result.SeverityReason = "现有证据不足，无法可靠判断严重程度"
	}
	if len(result.ImpactScope) == 0 {
		result.ImpactScope = []string{"现有日志与告警信息不足，影响面尚未确认"}
	}
	if !validSeverity {
		result.EvidenceGaps = appendUnique(result.EvidenceGaps, "AI 未返回有效的严重程度，已保守标记为 low")
	}
	return &result, nil
}

func normalizeAssessedSeverity(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "critical", "high", "medium", "low":
		return value, true
	default:
		return "low", false
	}
}

func appendUnique(values []string, value string) []string {
	for _, item := range values {
		if item == value {
			return values
		}
	}
	return append(values, value)
}
