package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
	"github.com/turning4th/codex-gitea/internal/secretbox"
)

const secretRedacted = "***set***"

func normalizeAnalysisConfig(c *model.AnalysisConfig) {
	c.Name = strings.TrimSpace(c.Name)
	c.RepositoryURL = strings.TrimSpace(c.RepositoryURL)
	c.RepositoryRef = strings.TrimSpace(c.RepositoryRef)
	c.SLSEndpoint = strings.TrimSpace(c.SLSEndpoint)
	c.SLSProject = strings.TrimSpace(c.SLSProject)
	c.SLSLogstore = strings.TrimSpace(c.SLSLogstore)
	c.FeishuMode = strings.ToLower(strings.TrimSpace(c.FeishuMode))
	c.FeishuWebhook = strings.TrimSpace(c.FeishuWebhook)
	c.FeishuAppID = strings.TrimSpace(c.FeishuAppID)
	c.FeishuChatID = strings.TrimSpace(c.FeishuChatID)
	c.FeishuMentionMapping = strings.TrimSpace(c.FeishuMentionMapping)
	c.IgnoredErrorCodes = strings.TrimSpace(c.IgnoredErrorCodes)
	c.Model = strings.TrimSpace(c.Model)
	c.ReasoningEffort = strings.TrimSpace(c.ReasoningEffort)
	if c.FeishuMode == "" {
		if c.FeishuAppID != "" || strings.TrimSpace(c.FeishuAppSecret) != "" || c.FeishuChatID != "" {
			c.FeishuMode = "app"
		} else {
			c.FeishuMode = "webhook"
		}
	}
	if c.RepositoryRef == "" {
		c.RepositoryRef = "main"
	}
	if c.Concurrency <= 0 {
		c.Concurrency = model.DefaultAnalysisConcurrency
	}
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = 1800
	}
	if c.LogWindowSeconds <= 0 {
		c.LogWindowSeconds = 180
	}
	if c.ThrottleThreshold <= 0 {
		c.ThrottleThreshold = 1
	}
	if c.ThrottleCooldownSecs < 0 {
		c.ThrottleCooldownSecs = 0
	}
	if strings.TrimSpace(c.ThrottleFields) == "" {
		c.ThrottleFields = "method,endpoint,error_code,error_message"
	}
}

func validateAnalysisConfig(c model.AnalysisConfig) error {
	for field, value := range map[string]string{
		"name": c.Name, "repository_url": c.RepositoryURL,
		"sls_endpoint": c.SLSEndpoint, "sls_project": c.SLSProject,
		"sls_logstore": c.SLSLogstore,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", field)
		}
	}
	if strings.TrimSpace(c.SLSAccessKeyID) == "" || strings.TrimSpace(c.SLSAccessKeySecret) == "" {
		return errors.New("SLS access key id and secret are required")
	}
	if c.Concurrency < 1 || c.Concurrency > model.MaxAnalysisConcurrency {
		return fmt.Errorf("analysis concurrency must be between 1 and %d", model.MaxAnalysisConcurrency)
	}
	switch c.FeishuMode {
	case "webhook":
	case "app":
		if c.FeishuAppID == "" || strings.TrimSpace(c.FeishuAppSecret) == "" || c.FeishuChatID == "" {
			return errors.New("Feishu app id, app secret and chat id are required in app mode")
		}
	default:
		return errors.New("Feishu mode must be webhook or app")
	}
	return nil
}

func (s *Store) encryptCredential(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	if s.secretBox == nil {
		return "", secretbox.ErrKeyRequired
	}
	return s.secretBox.Seal(value)
}

func (s *Store) decryptCredential(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if s.secretBox == nil {
		return "", secretbox.ErrKeyRequired
	}
	return s.secretBox.Open(value)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) CreateAnalysisConfig(ctx context.Context, c *model.AnalysisConfig) (*model.AnalysisConfig, error) {
	if c == nil {
		return nil, errors.New("analysis config is nil")
	}
	normalizeAnalysisConfig(c)
	if err := validateAnalysisConfig(*c); err != nil {
		return nil, err
	}
	akID, err := s.encryptCredential(c.SLSAccessKeyID)
	if err != nil {
		return nil, err
	}
	akSecret, err := s.encryptCredential(c.SLSAccessKeySecret)
	if err != nil {
		return nil, err
	}
	feishu, err := s.encryptCredential(c.FeishuWebhook)
	if err != nil {
		return nil, err
	}
	feishuAppSecret, err := s.encryptCredential(c.FeishuAppSecret)
	if err != nil {
		return nil, err
	}
	now := nowRFC3339()
	res, err := s.db.ExecContext(ctx, `INSERT INTO alert_analysis_configs(
		name,enabled,version,repository_url,repository_ref,sls_endpoint,sls_project,sls_logstore,
		sls_access_key_id,sls_access_key_secret,feishu_mode,feishu_webhook,feishu_app_id,feishu_app_secret,feishu_chat_id,feishu_mention_mapping,ignored_error_codes,
		model,reasoning_effort,concurrency,timeout_seconds,
		log_window_seconds,prompt,throttle_enabled,throttle_threshold,throttle_cooldown_seconds,
		throttle_fields,ingest_token_hash,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.Name, boolInt(c.Enabled), 1, c.RepositoryURL, c.RepositoryRef, c.SLSEndpoint,
		c.SLSProject, c.SLSLogstore, akID, akSecret, c.FeishuMode, feishu, c.FeishuAppID, feishuAppSecret, c.FeishuChatID, c.FeishuMentionMapping, c.IgnoredErrorCodes,
		c.Model, c.ReasoningEffort, c.Concurrency,
		c.TimeoutSeconds, c.LogWindowSeconds, c.Prompt, boolInt(c.ThrottleEnabled),
		c.ThrottleThreshold, c.ThrottleCooldownSecs, c.ThrottleFields, c.IngestTokenHash, now, now)
	if err != nil {
		return nil, fmt.Errorf("create analysis config: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("analysis config id: %w", err)
	}
	return s.GetAnalysisConfig(ctx, id)
}

func (s *Store) UpdateAnalysisConfig(ctx context.Context, c *model.AnalysisConfig) (*model.AnalysisConfig, error) {
	if c == nil || c.ID <= 0 {
		return nil, model.ErrAnalysisConfigNotFound
	}
	normalizeAnalysisConfig(c)
	if err := validateAnalysisConfig(*c); err != nil {
		return nil, err
	}
	akID, err := s.encryptCredential(c.SLSAccessKeyID)
	if err != nil {
		return nil, err
	}
	akSecret, err := s.encryptCredential(c.SLSAccessKeySecret)
	if err != nil {
		return nil, err
	}
	feishu, err := s.encryptCredential(c.FeishuWebhook)
	if err != nil {
		return nil, err
	}
	feishuAppSecret, err := s.encryptCredential(c.FeishuAppSecret)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE alert_analysis_configs SET
		name=?,enabled=?,version=version+1,repository_url=?,repository_ref=?,sls_endpoint=?,
		sls_project=?,sls_logstore=?,sls_access_key_id=?,sls_access_key_secret=?,feishu_mode=?,feishu_webhook=?,
		feishu_app_id=?,feishu_app_secret=?,feishu_chat_id=?,feishu_mention_mapping=?,ignored_error_codes=?,
		model=?,reasoning_effort=?,concurrency=?,timeout_seconds=?,log_window_seconds=?,prompt=?,throttle_enabled=?,
		throttle_threshold=?,throttle_cooldown_seconds=?,throttle_fields=?,updated_at=? WHERE id=?`,
		c.Name, boolInt(c.Enabled), c.RepositoryURL, c.RepositoryRef, c.SLSEndpoint, c.SLSProject,
		c.SLSLogstore, akID, akSecret, c.FeishuMode, feishu, c.FeishuAppID, feishuAppSecret, c.FeishuChatID, c.FeishuMentionMapping, c.IgnoredErrorCodes,
		c.Model, c.ReasoningEffort, c.Concurrency, c.TimeoutSeconds,
		c.LogWindowSeconds, c.Prompt, boolInt(c.ThrottleEnabled), c.ThrottleThreshold,
		c.ThrottleCooldownSecs, c.ThrottleFields, nowRFC3339(), c.ID)
	if err != nil {
		return nil, fmt.Errorf("update analysis config: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, model.ErrAnalysisConfigNotFound
	}
	return s.GetAnalysisConfig(ctx, c.ID)
}

func (s *Store) ListAnalysisConfigs(ctx context.Context) ([]model.AnalysisConfig, error) {
	rows, err := s.db.QueryContext(ctx, analysisConfigSelect+` ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list analysis configs: %w", err)
	}
	defer rows.Close()
	var out []model.AnalysisConfig
	for rows.Next() {
		c, err := s.scanAnalysisConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *Store) GetAnalysisConfig(ctx context.Context, id int64) (*model.AnalysisConfig, error) {
	row := s.db.QueryRowContext(ctx, analysisConfigSelect+` WHERE id=?`, id)
	c, err := s.scanAnalysisConfig(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, model.ErrAnalysisConfigNotFound
	}
	return c, err
}

const analysisConfigSelect = `SELECT id,name,enabled,version,repository_url,repository_ref,
	sls_endpoint,sls_project,sls_logstore,sls_access_key_id,sls_access_key_secret,feishu_mode,feishu_webhook,
	feishu_app_id,feishu_app_secret,feishu_chat_id,feishu_mention_mapping,ignored_error_codes,
	model,reasoning_effort,concurrency,timeout_seconds,log_window_seconds,prompt,throttle_enabled,
	throttle_threshold,throttle_cooldown_seconds,throttle_fields,ingest_token_hash,created_at,updated_at
	FROM alert_analysis_configs`

type rowScanner interface{ Scan(...any) error }

func (s *Store) scanAnalysisConfig(row rowScanner) (*model.AnalysisConfig, error) {
	var c model.AnalysisConfig
	var enabled, throttle int
	var akID, akSecret, feishu, feishuAppSecret, created, updated string
	if err := row.Scan(&c.ID, &c.Name, &enabled, &c.Version, &c.RepositoryURL, &c.RepositoryRef,
		&c.SLSEndpoint, &c.SLSProject, &c.SLSLogstore, &akID, &akSecret, &c.FeishuMode, &feishu,
		&c.FeishuAppID, &feishuAppSecret, &c.FeishuChatID, &c.FeishuMentionMapping, &c.IgnoredErrorCodes,
		&c.Model, &c.ReasoningEffort, &c.Concurrency, &c.TimeoutSeconds, &c.LogWindowSeconds, &c.Prompt,
		&throttle, &c.ThrottleThreshold, &c.ThrottleCooldownSecs, &c.ThrottleFields,
		&c.IngestTokenHash, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	if c.SLSAccessKeyID, err = s.decryptCredential(akID); err != nil {
		return nil, fmt.Errorf("decrypt SLS access key id: %w", err)
	}
	if c.SLSAccessKeySecret, err = s.decryptCredential(akSecret); err != nil {
		return nil, fmt.Errorf("decrypt SLS access key secret: %w", err)
	}
	if c.FeishuWebhook, err = s.decryptCredential(feishu); err != nil {
		return nil, fmt.Errorf("decrypt Feishu webhook: %w", err)
	}
	if c.FeishuAppSecret, err = s.decryptCredential(feishuAppSecret); err != nil {
		return nil, fmt.Errorf("decrypt Feishu app secret: %w", err)
	}
	c.Enabled = enabled != 0
	c.ThrottleEnabled = throttle != 0
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(updated)
	return &c, nil
}

func (s *Store) DeleteAnalysisConfig(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM alert_analysis_configs WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete analysis config: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.ErrAnalysisConfigNotFound
	}
	return nil
}

func (s *Store) SetAnalysisConfigEnabled(ctx context.Context, id int64, enabled bool) (*model.AnalysisConfig, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE alert_analysis_configs SET enabled=?,version=version+1,updated_at=? WHERE id=?`, boolInt(enabled), nowRFC3339(), id)
	if err != nil {
		return nil, fmt.Errorf("set analysis config enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, model.ErrAnalysisConfigNotFound
	}
	return s.GetAnalysisConfig(ctx, id)
}

func (s *Store) RotateAnalysisConfigToken(ctx context.Context, id int64, token string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE alert_analysis_configs SET ingest_token_hash=?,version=version+1,updated_at=? WHERE id=?`, hashToken(token), nowRFC3339(), id)
	if err != nil {
		return fmt.Errorf("rotate analysis token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.ErrAnalysisConfigNotFound
	}
	return nil
}

func (s *Store) VerifyAnalysisConfigToken(ctx context.Context, id int64, token string) (*model.AnalysisConfig, error) {
	c, err := s.GetAnalysisConfig(ctx, id)
	if err != nil {
		return nil, err
	}
	want, err := hex.DecodeString(c.IngestTokenHash)
	if err != nil || !hmac.Equal(want, mustDecodeHex(hashToken(token))) {
		return nil, model.ErrAnalysisConfigNotFound
	}
	return c, nil
}

func mustDecodeHex(value string) []byte {
	b, _ := hex.DecodeString(value)
	return b
}

func (s *Store) EnqueueAnalysisTask(ctx context.Context, cfg model.AnalysisConfig, alert model.AlertEnvelope, deliveryID, fingerprint string) (*model.AnalysisTask, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var existingID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM analysis_tasks WHERE config_id=? AND delivery_id=?`, cfg.ID, deliveryID).Scan(&existingID); err == nil {
		task, err := scanAnalysisTask(ctx, tx, existingID)
		return task, false, err
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	now := time.Now().UTC()
	status := model.AnalysisTaskQueued
	phase := "received"
	message := "告警已接收，等待分析"
	var duplicateOfTaskID *int64
	if cfg.ThrottleEnabled {
		suppressed, reason, err := applyAnalysisThrottle(ctx, tx, cfg, fingerprint, now)
		if err != nil {
			return nil, false, err
		}
		if suppressed {
			status = model.AnalysisTaskSuppressed
			phase = "throttled"
			if id, findErr := latestAnalyzedTaskID(ctx, tx, cfg.ID, fingerprint); findErr != nil {
				return nil, false, findErr
			} else if id > 0 {
				duplicateOfTaskID = &id
				message = fmt.Sprintf("重复报错，已有分析任务 #%d，不再重复运行", id)
			} else {
				message = reason
			}
		}
	}
	alertJSON, err := json.Marshal(alert)
	if err != nil {
		return nil, false, err
	}
	snapshotJSON, err := json.Marshal(cfg.Snapshot())
	if err != nil {
		return nil, false, err
	}
	finished := any(nil)
	if status == model.AnalysisTaskSuppressed {
		finished = now.Format(time.RFC3339)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO analysis_tasks(
		config_id,config_version,config_name,delivery_id,duplicate_of_task_id,fingerprint,alert_payload,config_snapshot,
		status,phase,attempts,cancel_requested,created_at,finished_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, cfg.ID, cfg.Version, cfg.Name, deliveryID, duplicateOfTaskID, fingerprint,
		string(alertJSON), string(snapshotJSON), string(status), phase, 0, 0, now.Format(time.RFC3339), finished)
	if err != nil {
		return nil, false, fmt.Errorf("insert analysis task: %w", err)
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx, `INSERT INTO analysis_task_events(task_id,sequence,phase,level,message,created_at) VALUES(?,?,?,?,?,?)`, id, 1, phase, "info", message, now.Format(time.RFC3339)); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	task, err := s.GetAnalysisTask(ctx, id)
	return task, true, err
}

func latestAnalyzedTaskID(ctx context.Context, tx *sql.Tx, configID int64, fingerprint string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM analysis_tasks
		WHERE config_id=? AND fingerprint=? AND status<>?
		ORDER BY CASE status WHEN 'succeeded' THEN 0 WHEN 'running' THEN 1 WHEN 'queued' THEN 2 ELSE 3 END, id DESC
		LIMIT 1`, configID, fingerprint, string(model.AnalysisTaskSuppressed)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func applyAnalysisThrottle(ctx context.Context, tx *sql.Tx, cfg model.AnalysisConfig, fingerprint string, now time.Time) (bool, string, error) {
	var last string
	var count int
	var until sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT last_fingerprint,consecutive_count,suppressed_until FROM analysis_throttle_states WHERE config_id=?`, cfg.ID).Scan(&last, &count, &until)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO analysis_throttle_states(config_id,last_fingerprint,consecutive_count,suppressed_until,updated_at) VALUES(?,?,?,?,?)`, cfg.ID, fingerprint, 1, nil, now.Format(time.RFC3339))
		return false, "", err
	}
	if err != nil {
		return false, "", err
	}
	if last != fingerprint {
		_, err = tx.ExecContext(ctx, `UPDATE analysis_throttle_states SET last_fingerprint=?,consecutive_count=1,suppressed_until=NULL,updated_at=? WHERE config_id=?`, fingerprint, now.Format(time.RFC3339), cfg.ID)
		return false, "", err
	}
	if until.Valid && until.String != "" {
		if parsed := parseTime(until.String); parsed.After(now) {
			_, err = tx.ExecContext(ctx, `UPDATE analysis_throttle_states SET consecutive_count=consecutive_count+1,updated_at=? WHERE config_id=?`, now.Format(time.RFC3339), cfg.ID)
			return true, fmt.Sprintf("重复报错，已触发过分析；重新分析冷却至 %s", parsed.Format(time.RFC3339)), err
		}
		_, err = tx.ExecContext(ctx, `UPDATE analysis_throttle_states SET consecutive_count=1,suppressed_until=NULL,updated_at=? WHERE config_id=?`, now.Format(time.RFC3339), cfg.ID)
		return false, "", err
	}
	if count >= cfg.ThrottleThreshold {
		if cfg.ThrottleCooldownSecs == 0 {
			_, err = tx.ExecContext(ctx, `UPDATE analysis_throttle_states SET consecutive_count=consecutive_count+1,suppressed_until=NULL,updated_at=? WHERE config_id=?`, now.Format(time.RFC3339), cfg.ID)
			return true, "重复报错，已触发过分析，不再重复运行", err
		}
		suppressedUntil := now.Add(time.Duration(cfg.ThrottleCooldownSecs) * time.Second)
		_, err = tx.ExecContext(ctx, `UPDATE analysis_throttle_states SET consecutive_count=consecutive_count+1,suppressed_until=?,updated_at=? WHERE config_id=?`, suppressedUntil.Format(time.RFC3339), now.Format(time.RFC3339), cfg.ID)
		return true, fmt.Sprintf("重复报错，已触发过分析；暂停重新分析至 %s", suppressedUntil.Format(time.RFC3339)), err
	}
	_, err = tx.ExecContext(ctx, `UPDATE analysis_throttle_states SET consecutive_count=consecutive_count+1,updated_at=? WHERE config_id=?`, now.Format(time.RFC3339), cfg.ID)
	return false, "", err
}

func (s *Store) ClaimAnalysisTask(ctx context.Context) (*model.AnalysisTask, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `UPDATE analysis_tasks SET status=?,phase=?,attempts=attempts+1,started_at=?,finished_at=NULL,error='',error_type=''
		WHERE id=(
			SELECT queued.id
			FROM analysis_tasks queued
			LEFT JOIN alert_analysis_configs config ON config.id=queued.config_id
			WHERE queued.status=? AND (
				queued.config_id IS NULL OR config.id IS NULL OR
				(SELECT COUNT(*) FROM analysis_tasks active
				 WHERE active.config_id=queued.config_id AND active.status IN (?,?)) < config.concurrency
			)
			ORDER BY queued.id
			LIMIT 1
		) RETURNING id`,
		string(model.AnalysisTaskRunning), "starting", nowRFC3339(), string(model.AnalysisTaskQueued),
		string(model.AnalysisTaskRunning), string(model.AnalysisTaskCancelRequested)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim analysis task: %w", err)
	}
	_ = s.AppendAnalysisTaskEvent(ctx, id, "starting", "info", "分析 Worker 已启动", nil)
	return s.GetAnalysisTask(ctx, id)
}

func (s *Store) GetAnalysisTask(ctx context.Context, id int64) (*model.AnalysisTask, error) {
	t, err := scanAnalysisTask(ctx, s.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, model.ErrAnalysisTaskNotFound
	}
	return t, err
}

func (s *Store) ListAnalysisTasks(ctx context.Context, filter model.AnalysisTaskFilter) ([]model.AnalysisTask, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	clauses := []string{"1=1"}
	args := []any{}
	if filter.Status != "" {
		clauses = append(clauses, "status=?")
		args = append(args, string(filter.Status))
	}
	if filter.ConfigID > 0 {
		clauses = append(clauses, "config_id=?")
		args = append(args, filter.ConfigID)
	}
	args = append(args, limit, filter.Offset)
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM analysis_tasks WHERE `+strings.Join(clauses, " AND ")+` ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	var out []model.AnalysisTask
	for _, id := range ids {
		t, err := s.GetAnalysisTask(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func scanAnalysisTask(ctx context.Context, q querier, id int64) (*model.AnalysisTask, error) {
	var t model.AnalysisTask
	var configID sql.NullInt64
	var retryID, duplicateID sql.NullInt64
	var alertJSON, snapshotJSON, status, created string
	var started, finished sql.NullString
	var cancelRequested int
	err := q.QueryRowContext(ctx, `SELECT id,config_id,config_version,config_name,delivery_id,retry_of_task_id,duplicate_of_task_id,
		fingerprint,alert_payload,config_snapshot,status,phase,attempts,cancel_requested,error_type,error,
		result_json,notification_status,notification_error,feishu_message_id,created_at,started_at,finished_at
		FROM analysis_tasks WHERE id=?`, id).Scan(&t.ID, &configID, &t.ConfigVersion, &t.ConfigName,
		&t.DeliveryID, &retryID, &duplicateID, &t.Fingerprint, &alertJSON, &snapshotJSON, &status, &t.Phase,
		&t.Attempts, &cancelRequested, &t.ErrorType, &t.Error, &t.ResultJSON,
		&t.NotificationStatus, &t.NotificationError, &t.FeishuMessageID, &created, &started, &finished)
	if err != nil {
		return nil, err
	}
	if configID.Valid {
		v := configID.Int64
		t.ConfigID = &v
	}
	if retryID.Valid {
		v := retryID.Int64
		t.RetryOfTaskID = &v
	}
	if duplicateID.Valid {
		v := duplicateID.Int64
		t.DuplicateOfTaskID = &v
	}
	if err := json.Unmarshal([]byte(alertJSON), &t.Alert); err != nil {
		return nil, fmt.Errorf("decode analysis alert: %w", err)
	}
	if err := json.Unmarshal([]byte(snapshotJSON), &t.ConfigSnapshot); err != nil {
		return nil, fmt.Errorf("decode analysis config snapshot: %w", err)
	}
	t.Status = model.AnalysisTaskStatus(status)
	t.CancelRequested = cancelRequested != 0
	t.CreatedAt = parseTime(created)
	t.StartedAt = nullableTime(started)
	t.FinishedAt = nullableTime(finished)
	return &t, nil
}

func (s *Store) AppendAnalysisTaskEvent(ctx context.Context, taskID int64, phase, level, message string, data json.RawMessage) error {
	if level == "" {
		level = "info"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO analysis_task_events(task_id,sequence,phase,level,message,data_json,created_at)
		SELECT ?,COALESCE(MAX(sequence),0)+1,?,?,?,?,? FROM analysis_task_events WHERE task_id=?`,
		taskID, phase, level, message, string(data), nowRFC3339(), taskID)
	return err
}

func (s *Store) ListAnalysisTaskEvents(ctx context.Context, taskID, afterID int64) ([]model.AnalysisTaskEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,sequence,phase,level,message,data_json,created_at FROM analysis_task_events WHERE task_id=? AND id>? ORDER BY id`, taskID, afterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AnalysisTaskEvent
	for rows.Next() {
		var ev model.AnalysisTaskEvent
		var data, created string
		if err := rows.Scan(&ev.ID, &ev.TaskID, &ev.Sequence, &ev.Phase, &ev.Level, &ev.Message, &data, &created); err != nil {
			return nil, err
		}
		if data != "" {
			ev.Data = json.RawMessage(data)
		}
		ev.CreatedAt = parseTime(created)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) FinishAnalysisTask(ctx context.Context, id int64, status model.AnalysisTaskStatus, resultJSON, errorType, errorMessage string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE analysis_tasks SET status=?,phase=?,result_json=?,error_type=?,error=?,finished_at=?
		WHERE id=? AND status IN(?,?)`, string(status), string(status), resultJSON, errorType, errorMessage,
		nowRFC3339(), id, string(model.AnalysisTaskRunning), string(model.AnalysisTaskCancelRequested))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.ErrAnalysisTaskTerminal
	}
	return nil
}

func (s *Store) SetAnalysisTaskPhase(ctx context.Context, id int64, phase string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE analysis_tasks SET phase=? WHERE id=? AND status IN(?,?)`, phase, id, string(model.AnalysisTaskRunning), string(model.AnalysisTaskCancelRequested))
	return err
}

func (s *Store) SetAnalysisNotification(ctx context.Context, id int64, status, errorMessage, messageID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE analysis_tasks SET notification_status=?,notification_error=?,
		feishu_message_id=CASE WHEN ?<>'' THEN ? ELSE feishu_message_id END WHERE id=?`, status, errorMessage, messageID, messageID, id)
	return err
}

func (s *Store) RequestAnalysisTaskCancel(ctx context.Context, id int64) (*model.AnalysisTask, error) {
	t, err := s.GetAnalysisTask(ctx, id)
	if err != nil {
		return nil, err
	}
	now := nowRFC3339()
	switch t.Status {
	case model.AnalysisTaskQueued:
		_, err = s.db.ExecContext(ctx, `UPDATE analysis_tasks SET status=?,phase=?,cancel_requested=1,finished_at=? WHERE id=? AND status=?`, string(model.AnalysisTaskCanceled), "canceled", now, id, string(model.AnalysisTaskQueued))
	case model.AnalysisTaskRunning:
		_, err = s.db.ExecContext(ctx, `UPDATE analysis_tasks SET status=?,phase=?,cancel_requested=1 WHERE id=? AND status=?`, string(model.AnalysisTaskCancelRequested), "cancel_requested", id, string(model.AnalysisTaskRunning))
	case model.AnalysisTaskCancelRequested:
		return t, nil
	default:
		return nil, model.ErrAnalysisTaskTerminal
	}
	if err != nil {
		return nil, err
	}
	_ = s.AppendAnalysisTaskEvent(ctx, id, "cancel_requested", "warning", "管理员请求取消分析", nil)
	return s.GetAnalysisTask(ctx, id)
}

func (s *Store) RetryAnalysisTask(ctx context.Context, id int64, snapshot model.AnalysisConfigSnapshot) (*model.AnalysisTask, error) {
	original, err := s.GetAnalysisTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if original.Status == model.AnalysisTaskQueued || original.Status == model.AnalysisTaskRunning || original.Status == model.AnalysisTaskCancelRequested {
		return nil, model.ErrAnalysisTaskTerminal
	}
	alertJSON, _ := json.Marshal(original.Alert)
	snapshotJSON, _ := json.Marshal(snapshot)
	deliveryID := fmt.Sprintf("%s:retry:%d", original.DeliveryID, time.Now().UnixNano())
	now := nowRFC3339()
	res, err := s.db.ExecContext(ctx, `INSERT INTO analysis_tasks(config_id,config_version,config_name,delivery_id,retry_of_task_id,fingerprint,alert_payload,config_snapshot,status,phase,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, snapshot.ConfigID, snapshot.ConfigVersion, snapshot.Name, deliveryID,
		original.ID, original.Fingerprint, string(alertJSON), string(snapshotJSON), string(model.AnalysisTaskQueued), "received", now)
	if err != nil {
		return nil, err
	}
	newID, _ := res.LastInsertId()
	_ = s.AppendAnalysisTaskEvent(ctx, newID, "received", "info", fmt.Sprintf("从任务 #%d 重新分析", original.ID), nil)
	return s.GetAnalysisTask(ctx, newID)
}

func (s *Store) RecoverAnalysisTasks(ctx context.Context) error {
	now := nowRFC3339()
	if _, err := s.db.ExecContext(ctx, `UPDATE analysis_tasks SET status=?,phase=?,finished_at=? WHERE status=?`, string(model.AnalysisTaskCanceled), "canceled", now, string(model.AnalysisTaskCancelRequested)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE analysis_tasks SET status=?,phase=?,started_at=NULL WHERE status=?`, string(model.AnalysisTaskQueued), "recovered", string(model.AnalysisTaskRunning))
	return err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

var _ model.AnalysisStore = (*Store)(nil)

// Keep the redaction sentinel close to persistence so callers can safely merge
// edits without ever trying to encrypt the placeholder itself.
func IsRedactedAnalysisSecret(value string) bool { return value == secretRedacted }
