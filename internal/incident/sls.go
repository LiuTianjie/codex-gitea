package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	sls "github.com/aliyun/aliyun-log-go-sdk"
	"github.com/turning4th/codex-gitea/internal/model"
)

type SLSFetcher interface {
	Fetch(context.Context, model.AnalysisConfig, model.AlertEnvelope) ([]string, error)
	Test(context.Context, model.AnalysisConfig) error
}

type AliyunSLSFetcher struct{}

func (AliyunSLSFetcher) Fetch(ctx context.Context, cfg model.AnalysisConfig, alert model.AlertEnvelope) ([]string, error) {
	center := parseAlertTime(alert.AlertTime)
	window := time.Duration(cfg.LogWindowSeconds) * time.Second
	if window <= 0 {
		window = 3 * time.Minute
	}
	query := buildSLSQuery(alert)
	client := sls.CreateNormalInterface(cfg.SLSEndpoint, cfg.SLSAccessKeyID, cfg.SLSAccessKeySecret, "")
	if concrete, ok := client.(*sls.Client); ok {
		concrete.SetHTTPClient(&http.Client{Timeout: 30 * time.Second})
		defer concrete.Close()
	}
	type response struct {
		logs []map[string]string
		err  error
	}
	ch := make(chan response, 1)
	go func() {
		result, err := client.GetLogs(cfg.SLSProject, cfg.SLSLogstore, "", center.Add(-window).Unix(), center.Add(window).Unix()+1, query, 100, 0, false)
		if err != nil {
			ch <- response{err: err}
			return
		}
		ch <- response{logs: result.Logs}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-ch:
		if result.err != nil {
			return nil, fmt.Errorf("query SLS %s/%s: %w", cfg.SLSProject, cfg.SLSLogstore, result.err)
		}
		return formatSLSLogs(result.logs), nil
	}
}

func (AliyunSLSFetcher) Test(ctx context.Context, cfg model.AnalysisConfig) error {
	client := sls.CreateNormalInterface(cfg.SLSEndpoint, cfg.SLSAccessKeyID, cfg.SLSAccessKeySecret, "")
	if concrete, ok := client.(*sls.Client); ok {
		concrete.SetHTTPClient(&http.Client{Timeout: 15 * time.Second})
		defer concrete.Close()
	}
	type response struct {
		ok  bool
		err error
	}
	ch := make(chan response, 1)
	go func() {
		ok, err := client.CheckLogstoreExist(cfg.SLSProject, cfg.SLSLogstore)
		ch <- response{ok: ok, err: err}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case result := <-ch:
		if result.err != nil {
			return result.err
		}
		if !result.ok {
			return fmt.Errorf("SLS Logstore %s/%s does not exist", cfg.SLSProject, cfg.SLSLogstore)
		}
		return nil
	}
}

func parseAlertTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Now().UTC()
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	if number, err := strconv.ParseInt(value, 10, 64); err == nil {
		if number > 1_000_000_000_000 {
			number /= 1000
		}
		return time.Unix(number, 0).UTC()
	}
	return time.Now().UTC()
}

func buildSLSQuery(alert model.AlertEnvelope) string {
	var clauses []string
	if alert.EventID != "" {
		clauses = append(clauses, `event_id: "`+escapeSLSValue(alert.EventID)+`"`)
	}
	if alert.TraceID != "" {
		clauses = append(clauses, `trace_id: "`+escapeSLSValue(alert.TraceID)+`"`)
	}
	if len(clauses) == 0 && alert.Endpoint != "" {
		clauses = append(clauses, `endpoint: "`+escapeSLSValue(alert.Endpoint)+`"`)
	}
	if len(clauses) == 0 {
		return "*"
	}
	return strings.Join(clauses, " OR ")
}

func escapeSLSValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}

func formatSLSLogs(logs []map[string]string) []string {
	const maxBytes = 256 * 1024
	var out []string
	total := 0
	for _, entry := range logs {
		clean := make(map[string]string, len(entry))
		keys := make([]string, 0, len(entry))
		for key := range entry {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "authorization") {
				clean[key] = "***redacted***"
			} else {
				clean[key] = entry[key]
			}
		}
		encoded, _ := json.Marshal(clean)
		if total+len(encoded) > maxBytes {
			break
		}
		out = append(out, string(encoded))
		total += len(encoded)
	}
	return out
}
