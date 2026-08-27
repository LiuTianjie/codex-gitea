package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

type analysisConfigPayload struct {
	Name                 string `json:"name"`
	Enabled              bool   `json:"enabled"`
	RepositoryURL        string `json:"repository_url"`
	RepositoryRef        string `json:"repository_ref"`
	SLSEndpoint          string `json:"sls_endpoint"`
	SLSProject           string `json:"sls_project"`
	SLSLogstore          string `json:"sls_logstore"`
	SLSAccessKeyID       string `json:"sls_access_key_id"`
	SLSAccessKeySecret   string `json:"sls_access_key_secret"`
	FeishuMode           string `json:"feishu_mode"`
	FeishuWebhook        string `json:"feishu_webhook"`
	FeishuAppID          string `json:"feishu_app_id"`
	FeishuAppSecret      string `json:"feishu_app_secret"`
	FeishuChatID         string `json:"feishu_chat_id"`
	FeishuMentionMapping string `json:"feishu_mention_mapping"`
	IgnoredErrorCodes    string `json:"ignored_error_codes"`
	Model                string `json:"model"`
	ReasoningEffort      string `json:"reasoning_effort"`
	Concurrency          int    `json:"concurrency"`
	TimeoutSeconds       int    `json:"timeout_seconds"`
	LogWindowSeconds     int    `json:"log_window_seconds"`
	Prompt               string `json:"prompt"`
	ThrottleEnabled      bool   `json:"throttle_enabled"`
	ThrottleThreshold    int    `json:"throttle_threshold"`
	ThrottleCooldownSecs int    `json:"throttle_cooldown_seconds"`
	ThrottleFields       string `json:"throttle_fields"`
}

func (p analysisConfigPayload) model() model.AnalysisConfig {
	return model.AnalysisConfig{
		Name: p.Name, Enabled: p.Enabled, RepositoryURL: p.RepositoryURL,
		RepositoryRef: p.RepositoryRef, SLSEndpoint: p.SLSEndpoint,
		SLSProject: p.SLSProject, SLSLogstore: p.SLSLogstore,
		SLSAccessKeyID: p.SLSAccessKeyID, SLSAccessKeySecret: p.SLSAccessKeySecret,
		FeishuMode: p.FeishuMode, FeishuWebhook: p.FeishuWebhook,
		FeishuAppID: p.FeishuAppID, FeishuAppSecret: p.FeishuAppSecret, FeishuChatID: p.FeishuChatID,
		FeishuMentionMapping: p.FeishuMentionMapping,
		IgnoredErrorCodes:    p.IgnoredErrorCodes,
		Model:                p.Model, ReasoningEffort: p.ReasoningEffort, Concurrency: p.Concurrency,
		TimeoutSeconds: p.TimeoutSeconds, LogWindowSeconds: p.LogWindowSeconds,
		Prompt: p.Prompt, ThrottleEnabled: p.ThrottleEnabled,
		ThrottleThreshold: p.ThrottleThreshold, ThrottleCooldownSecs: p.ThrottleCooldownSecs,
		ThrottleFields: p.ThrottleFields,
	}
}

func (c *Console) handleAnalysisConfigs(w http.ResponseWriter, r *http.Request) {
	if !c.requireAnalysis(w) {
		return
	}
	configs, err := c.analysisStore.ListAnalysisConfigs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(configs))
	for _, cfg := range configs {
		out = append(out, analysisConfigView(cfg))
	}
	writeJSON(w, http.StatusOK, map[string]any{"configs": out})
}

func (c *Console) handleCreateAnalysisConfig(w http.ResponseWriter, r *http.Request) {
	if !c.requireAnalysis(w) {
		return
	}
	var payload analysisConfigPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	cfg := payload.model()
	if strings.TrimSpace(cfg.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	token, hash, err := newAnalysisToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg.IngestTokenHash = hash
	created, err := c.analysisStore.CreateAnalysisConfig(r.Context(), &cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	view := analysisConfigView(*created)
	view["ingest_url"] = analysisIngestURL(r, created.ID, token)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "config": view})
}

func (c *Console) handleUpdateAnalysisConfig(w http.ResponseWriter, r *http.Request) {
	if !c.requireAnalysis(w) {
		return
	}
	id, ok := parseAnalysisID(w, r.PathValue("id"))
	if !ok {
		return
	}
	existing, err := c.analysisStore.GetAnalysisConfig(r.Context(), id)
	if err != nil {
		writeAnalysisError(w, err)
		return
	}
	var payload analysisConfigPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	updated := payload.model()
	updated.ID = id
	if payload.SLSAccessKeyID == "" || payload.SLSAccessKeyID == redacted {
		updated.SLSAccessKeyID = existing.SLSAccessKeyID
	}
	if payload.SLSAccessKeySecret == "" || payload.SLSAccessKeySecret == redacted {
		updated.SLSAccessKeySecret = existing.SLSAccessKeySecret
	}
	if payload.FeishuWebhook == redacted {
		updated.FeishuWebhook = existing.FeishuWebhook
	}
	if payload.FeishuAppSecret == "" || payload.FeishuAppSecret == redacted {
		updated.FeishuAppSecret = existing.FeishuAppSecret
	}
	result, err := c.analysisStore.UpdateAnalysisConfig(r.Context(), &updated)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": analysisConfigView(*result)})
}

func (c *Console) handleDeleteAnalysisConfig(w http.ResponseWriter, r *http.Request) {
	if !c.requireAnalysis(w) {
		return
	}
	id, ok := parseAnalysisID(w, r.PathValue("id"))
	if !ok {
		return
	}
	if err := c.analysisStore.DeleteAnalysisConfig(r.Context(), id); err != nil {
		writeAnalysisError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (c *Console) handleSetAnalysisConfigEnabled(w http.ResponseWriter, r *http.Request) {
	if !c.requireAnalysis(w) {
		return
	}
	id, ok := parseAnalysisID(w, r.PathValue("id"))
	if !ok {
		return
	}
	var payload struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	cfg, err := c.analysisStore.SetAnalysisConfigEnabled(r.Context(), id, payload.Enabled)
	if err != nil {
		writeAnalysisError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": analysisConfigView(*cfg)})
}

func (c *Console) handleRotateAnalysisConfigToken(w http.ResponseWriter, r *http.Request) {
	if !c.requireAnalysis(w) {
		return
	}
	id, ok := parseAnalysisID(w, r.PathValue("id"))
	if !ok {
		return
	}
	token, _, err := newAnalysisToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := c.analysisStore.RotateAnalysisConfigToken(r.Context(), id, token); err != nil {
		writeAnalysisError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ingest_url": analysisIngestURL(r, id, token)})
}

func (c *Console) handleTestAnalysisConfig(w http.ResponseWriter, r *http.Request) {
	if !c.requireAnalysis(w) {
		return
	}
	id, ok := parseAnalysisID(w, r.PathValue("id"))
	if !ok {
		return
	}
	cfg, err := c.analysisStore.GetAnalysisConfig(r.Context(), id)
	if err != nil {
		writeAnalysisError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var test func(context.Context, model.AnalysisConfig) error
	switch r.PathValue("kind") {
	case "sls":
		test = c.analysisTests.SLS
	case "feishu":
		test = c.analysisTests.Feishu
	case "repo":
		test = c.analysisTests.Repo
	default:
		writeError(w, http.StatusBadRequest, "unknown test kind")
		return
	}
	if test == nil {
		writeError(w, http.StatusNotImplemented, "connection test is not configured")
		return
	}
	if err := test(ctx, *cfg); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (c *Console) handleAnalysisTasks(w http.ResponseWriter, r *http.Request) {
	if !c.requireAnalysis(w) {
		return
	}
	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	pageSize := parsePositiveInt(r.URL.Query().Get("page_size"), 20)
	if pageSize > 100 {
		pageSize = 100
	}
	configID, _ := strconv.ParseInt(r.URL.Query().Get("config_id"), 10, 64)
	filter := model.AnalysisTaskFilter{Status: model.AnalysisTaskStatus(r.URL.Query().Get("status")), ConfigID: configID, Limit: pageSize + 1, Offset: (page - 1) * pageSize}
	tasks, err := c.analysisStore.ListAnalysisTasks(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hasMore := len(tasks) > pageSize
	if hasMore {
		tasks = tasks[:pageSize]
	}
	out := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, analysisTaskView(task, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out, "page": page, "page_size": pageSize, "has_more": hasMore})
}

func (c *Console) handleAnalysisTaskDetail(w http.ResponseWriter, r *http.Request) {
	if !c.requireAnalysis(w) {
		return
	}
	id, ok := parseAnalysisID(w, r.PathValue("id"))
	if !ok {
		return
	}
	task, err := c.analysisStore.GetAnalysisTask(r.Context(), id)
	if err != nil {
		writeAnalysisError(w, err)
		return
	}
	events, err := c.analysisStore.ListAnalysisTaskEvents(r.Context(), id, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	view := analysisTaskView(*task, true)
	view["events"] = events
	writeJSON(w, http.StatusOK, view)
}

func (c *Console) handleAnalysisTaskEvents(w http.ResponseWriter, r *http.Request) {
	if !c.requireAnalysis(w) {
		return
	}
	id, ok := parseAnalysisID(w, r.PathValue("id"))
	if !ok {
		return
	}
	afterID, _ := strconv.ParseInt(r.URL.Query().Get("after_id"), 10, 64)
	events, err := c.analysisStore.ListAnalysisTaskEvents(r.Context(), id, afterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (c *Console) handleAnalysisTaskCancel(w http.ResponseWriter, r *http.Request) {
	if c.analysisControl == nil {
		writeError(w, http.StatusServiceUnavailable, "analysis control is unavailable")
		return
	}
	id, ok := parseAnalysisID(w, r.PathValue("id"))
	if !ok {
		return
	}
	task, err := c.analysisControl.Cancel(r.Context(), id)
	if err != nil {
		writeAnalysisError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": analysisTaskView(*task, false)})
}

func (c *Console) handleAnalysisTaskRetry(w http.ResponseWriter, r *http.Request) {
	if c.analysisControl == nil {
		writeError(w, http.StatusServiceUnavailable, "analysis control is unavailable")
		return
	}
	id, ok := parseAnalysisID(w, r.PathValue("id"))
	if !ok {
		return
	}
	task, err := c.analysisControl.Retry(r.Context(), id)
	if err != nil {
		writeAnalysisError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "task": analysisTaskView(*task, false)})
}

func (c *Console) requireAnalysis(w http.ResponseWriter) bool {
	if c.analysisStore == nil {
		writeError(w, http.StatusServiceUnavailable, "alert analysis is not configured")
		return false
	}
	return true
}

func analysisConfigView(cfg model.AnalysisConfig) map[string]any {
	secret := func(value string) string {
		if strings.TrimSpace(value) != "" {
			return redacted
		}
		return ""
	}
	return map[string]any{
		"id": cfg.ID, "name": cfg.Name, "enabled": cfg.Enabled, "version": cfg.Version,
		"repository_url": cfg.RepositoryURL, "repository_ref": cfg.RepositoryRef,
		"sls_endpoint": cfg.SLSEndpoint, "sls_project": cfg.SLSProject, "sls_logstore": cfg.SLSLogstore,
		"sls_access_key_id": secret(cfg.SLSAccessKeyID), "sls_access_key_secret": secret(cfg.SLSAccessKeySecret),
		"feishu_mode": cfg.FeishuMode, "feishu_webhook": secret(cfg.FeishuWebhook),
		"feishu_app_id": cfg.FeishuAppID, "feishu_app_secret": secret(cfg.FeishuAppSecret), "feishu_chat_id": cfg.FeishuChatID,
		"feishu_mention_mapping": cfg.FeishuMentionMapping,
		"ignored_error_codes":    cfg.IgnoredErrorCodes,
		"model":                  cfg.Model, "reasoning_effort": cfg.ReasoningEffort, "concurrency": cfg.Concurrency,
		"timeout_seconds": cfg.TimeoutSeconds, "log_window_seconds": cfg.LogWindowSeconds, "prompt": cfg.Prompt,
		"throttle_enabled": cfg.ThrottleEnabled, "throttle_threshold": cfg.ThrottleThreshold,
		"throttle_cooldown_seconds": cfg.ThrottleCooldownSecs, "throttle_fields": cfg.ThrottleFields,
		"created_at": cfg.CreatedAt.Format(time.RFC3339), "updated_at": cfg.UpdatedAt.Format(time.RFC3339),
		"ingest_path": fmt.Sprintf("/hooks/alert-analysis/%d/{token}", cfg.ID),
	}
}

func analysisTaskView(task model.AnalysisTask, includePayload bool) map[string]any {
	view := map[string]any{
		"id": task.ID, "config_id": task.ConfigID, "config_name": task.ConfigName,
		"config_version": task.ConfigVersion, "delivery_id": task.DeliveryID,
		"retry_of_task_id": task.RetryOfTaskID, "duplicate_of_task_id": task.DuplicateOfTaskID, "status": task.Status, "phase": task.Phase,
		"attempts": task.Attempts, "cancel_requested": task.CancelRequested,
		"error_type": task.ErrorType, "error": task.Error,
		"notification_status": task.NotificationStatus, "notification_error": task.NotificationError,
		"feishu_message_id": task.FeishuMessageID,
		"created_at":        task.CreatedAt.Format(time.RFC3339), "started_at": formatOptionalTime(task.StartedAt),
		"finished_at": formatOptionalTime(task.FinishedAt),
		"alert":       map[string]any{"alert_id": task.Alert.AlertID, "title": task.Alert.Title, "environment": task.Alert.Environment, "service": task.Alert.Service, "method": task.Alert.Method, "endpoint": task.Alert.Endpoint, "trace_id": task.Alert.TraceID, "error_code": task.Alert.ErrorCode},
	}
	if includePayload {
		view["alert"] = task.Alert
		view["config_snapshot"] = task.ConfigSnapshot
		if strings.TrimSpace(task.ResultJSON) != "" {
			var result any
			if json.Unmarshal([]byte(task.ResultJSON), &result) == nil {
				view["result"] = result
			} else {
				view["result_raw"] = task.ResultJSON
			}
		}
	}
	return view
}

func newAnalysisToken() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token := hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

func analysisIngestURL(r *http.Request, id int64, token string) string {
	proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		proto = "http"
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return fmt.Sprintf("%s://%s/hooks/alert-analysis/%d/%s", proto, host, id, token)
}

func parseAnalysisID(w http.ResponseWriter, value string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func writeAnalysisError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrAnalysisConfigNotFound), errors.Is(err, model.ErrAnalysisTaskNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, model.ErrAnalysisTaskTerminal):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339)
}
