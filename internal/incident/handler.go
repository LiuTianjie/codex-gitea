package incident

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/turning4th/codex-gitea/internal/model"
)

type WakeFunc func()

// Handler receives config-specific alert webhooks and performs only the fast,
// durable handoff. Analysis always happens asynchronously.
type Handler struct {
	Store model.AnalysisStore
	Wake  WakeFunc
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/hooks/alert-analysis/"), "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	configID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || configID <= 0 || strings.TrimSpace(parts[1]) == "" {
		http.NotFound(w, r)
		return
	}
	cfg, err := h.Store.VerifyAnalysisConfigToken(r.Context(), configID, parts[1])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !cfg.Enabled {
		writeJSON(w, http.StatusGone, map[string]any{"ok": false, "error": "analysis config is disabled"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid alert body"})
		return
	}
	alert, err := parseAlertEnvelope(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	deliveryID := strings.TrimSpace(alert.DeliveryID)
	if deliveryID == "" {
		deliveryID = strings.TrimSpace(r.Header.Get("X-Alert-Delivery-ID"))
	}
	if deliveryID == "" {
		deliveryID = strings.TrimSpace(alert.AlertID)
	}
	if deliveryID == "" {
		sum := sha256.Sum256(body)
		deliveryID = "payload:" + hex.EncodeToString(sum[:])
	}
	fingerprint := alertFingerprint(*cfg, alert)
	task, created, err := h.Store.EnqueueAnalysisTask(r.Context(), *cfg, alert, deliveryID, fingerprint)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "enqueue alert analysis failed"})
		return
	}
	if created && task.Status == model.AnalysisTaskQueued && h.Wake != nil {
		h.Wake()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "created": created, "task_id": task.ID,
		"status": task.Status, "phase": task.Phase,
	})
}

func parseAlertEnvelope(body []byte) (model.AlertEnvelope, error) {
	var raw map[string]any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return model.AlertEnvelope{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	if len(raw) == 0 {
		return model.AlertEnvelope{}, errors.New("alert body is empty")
	}
	raw = redactMap(raw)
	get := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := raw[key]; ok && value != nil {
				if s := strings.TrimSpace(fmt.Sprint(value)); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return model.AlertEnvelope{
		DeliveryID:    get("delivery_id", "upstream_delivery_id"),
		AlertID:       get("alert_id", "alertId", "id"),
		AlertTime:     get("alert_time", "alertTime", "occurred_at", "time", "timestamp"),
		Environment:   get("environment", "env"),
		Service:       get("service", "source"),
		Rule:          get("rule", "alert_name", "alertName"),
		Title:         get("title", "alert_title"),
		Severity:      get("severity", "level"),
		Method:        strings.ToUpper(get("method", "http_method")),
		Endpoint:      get("endpoint", "path", "request_path"),
		EventID:       get("event_id", "eventId"),
		TraceID:       get("trace_id", "traceId"),
		ErrorCode:     get("error_code", "response_code", "code"),
		ErrorMessage:  get("error_message", "message", "response_message"),
		DeploymentSHA: get("deployment_sha", "commit_sha", "revision"),
		DetailURL:     get("detail_url", "url"),
		Raw:           raw,
	}, nil
}

func redactMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") ||
			strings.Contains(lower, "password") || strings.Contains(lower, "access_key") ||
			strings.Contains(lower, "authorization") {
			out[key] = "***redacted***"
			continue
		}
		switch typed := value.(type) {
		case map[string]any:
			out[key] = redactMap(typed)
		case []any:
			items := make([]any, 0, len(typed))
			for _, item := range typed {
				if nested, ok := item.(map[string]any); ok {
					items = append(items, redactMap(nested))
				} else {
					items = append(items, item)
				}
			}
			out[key] = items
		default:
			out[key] = value
		}
	}
	return out
}

func alertFingerprint(cfg model.AnalysisConfig, alert model.AlertEnvelope) string {
	values := map[string]string{
		"environment": alert.Environment, "service": alert.Service, "rule": alert.Rule,
		"title": alert.Title, "severity": alert.Severity, "method": alert.Method,
		"endpoint": alert.Endpoint, "error_code": alert.ErrorCode,
		"error_message": alert.ErrorMessage,
	}
	fields := strings.Split(cfg.ThrottleFields, ",")
	var normalized []string
	for _, field := range fields {
		field = strings.TrimSpace(strings.ToLower(field))
		if field == "" {
			continue
		}
		normalized = append(normalized, field+"="+strings.TrimSpace(strings.ToLower(values[field])))
	}
	if len(normalized) == 0 {
		normalized = append(normalized, strings.ToLower(alert.Method), strings.ToLower(alert.Endpoint), strings.ToLower(alert.ErrorMessage))
	}
	sum := sha256.Sum256([]byte(strings.Join(normalized, "\n")))
	return hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
