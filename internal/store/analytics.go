package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

func (s *Store) CreateAnalysisReport(ctx context.Context, summary model.AnalysisSummary) (*model.AnalysisReport, error) {
	data, err := json.Marshal(summary)
	if err != nil {
		return nil, fmt.Errorf("marshal analysis summary: %w", err)
	}
	now := nowRFC3339()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO analysis_reports(summary_json,created_at) VALUES(?,?)`, string(data), now)
	if err != nil {
		return nil, fmt.Errorf("insert analysis report: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("analysis report id: %w", err)
	}
	return &model.AnalysisReport{ID: id, CreatedAt: parseTime(now), Summary: summary}, nil
}

func (s *Store) LatestAnalysisReport(ctx context.Context) (*model.AnalysisReport, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, summary_json, created_at FROM analysis_reports ORDER BY id DESC LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("latest analysis report: %w", err)
	}
	defer rows.Close()
	reports, err := scanAnalysisReports(rows)
	if err != nil {
		return nil, err
	}
	if len(reports) == 0 {
		return nil, nil
	}
	return &reports[0], nil
}

func (s *Store) ListAnalysisReports(ctx context.Context, limit int) ([]model.AnalysisReport, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, summary_json, created_at FROM analysis_reports ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list analysis reports: %w", err)
	}
	defer rows.Close()
	return scanAnalysisReports(rows)
}

func (s *Store) BuildAnalysisTrend(ctx context.Context, limit int, interval string) ([]model.AnalysisTrendPoint, error) {
	interval = normalizeTrendInterval(interval)
	if limit <= 0 || limit > 100 {
		limit = defaultTrendLimit(interval)
	}
	reviewBucketExpr := trendBucketExpr("COALESCE(finished_at, started_at)", interval)
	alertBucketExpr := trendBucketExpr("COALESCE(finished_at, started_at, created_at)", interval)
	rows, err := s.db.QueryContext(ctx,
		`SELECT bucket FROM (
		   SELECT `+reviewBucketExpr+` AS bucket
		   FROM review_runs
		   WHERE status IN (?, ?) AND COALESCE(finished_at, started_at, '') <> ''
		   UNION
		   SELECT `+alertBucketExpr+` AS bucket
		   FROM analysis_tasks
		   WHERE COALESCE(finished_at, started_at, created_at, '') <> ''
		 ) ORDER BY bucket DESC LIMIT ?`, string(model.ReviewRunDone), string(model.ReviewRunFailed), limit)
	if err != nil {
		return nil, fmt.Errorf("analysis trend buckets: %w", err)
	}
	defer rows.Close()
	var buckets []string
	for rows.Next() {
		var bucket string
		if err := rows.Scan(&bucket); err != nil {
			return nil, fmt.Errorf("scan analysis trend bucket: %w", err)
		}
		buckets = append(buckets, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analysis trend buckets: %w", err)
	}
	sort.Strings(buckets)

	out := make([]model.AnalysisTrendPoint, 0, len(buckets))
	for _, bucket := range buckets {
		point, err := s.analysisTrendPoint(ctx, bucket, interval)
		if err != nil {
			return nil, err
		}
		out = append(out, point)
	}
	return out, nil
}

func (s *Store) analysisTrendPoint(ctx context.Context, bucket, interval string) (model.AnalysisTrendPoint, error) {
	var point model.AnalysisTrendPoint
	point.Bucket = bucket
	point.Interval = interval
	point.Day = bucket
	point.FinishedAt = parseTrendBucketTime(bucket, interval)
	bucketExpr := trendBucketExpr("COALESCE(finished_at, started_at)", interval)
	row := s.db.QueryRowContext(ctx,
		`SELECT
		   COUNT(*),
		   COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END), 0)
		 FROM review_runs
		 WHERE status IN (?, ?)
		   AND COALESCE(finished_at, started_at, '') <> ''
		   AND `+bucketExpr+` = ?`,
		string(model.ReviewRunDone), string(model.ReviewRunFailed),
		string(model.ReviewRunDone), string(model.ReviewRunFailed), bucket)
	if err := row.Scan(&point.TotalReviewRuns, &point.SuccessfulReviewRuns, &point.FailedReviewRuns); err != nil {
		return model.AnalysisTrendPoint{}, fmt.Errorf("scan analysis trend runs: %w", err)
	}
	completed := point.SuccessfulReviewRuns + point.FailedReviewRuns
	if completed > 0 {
		point.SuccessRate = float64(point.SuccessfulReviewRuns) / float64(completed)
	}

	findingBucketExpr := trendBucketExpr("COALESCE(rr.finished_at, rr.started_at)", interval)
	row = s.db.QueryRowContext(ctx,
		`SELECT
		   COUNT(f.id),
		   COALESCE(SUM(CASE WHEN COALESCE(f.status,'open')='open' THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN COALESCE(f.status,'open')='open'
		             AND COALESCE(f.severity,'info') IN (?, ?) THEN 1 ELSE 0 END), 0)
		 FROM findings f
		 JOIN review_runs rr ON rr.id=f.review_run_id
		 WHERE COALESCE(rr.finished_at, rr.started_at, '') <> ''
		   AND `+findingBucketExpr+` = ?`,
		string(model.SeverityHigh), string(model.SeverityCritical), bucket)
	if err := row.Scan(&point.TotalFindings, &point.OpenFindings, &point.HighCriticalOpen); err != nil {
		return model.AnalysisTrendPoint{}, fmt.Errorf("scan analysis trend findings: %w", err)
	}
	alertBucketExpr := trendBucketExpr("COALESCE(finished_at, started_at, created_at)", interval)
	row = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		   COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END), 0)
		 FROM analysis_tasks
		 WHERE COALESCE(finished_at, started_at, created_at, '') <> ''
		   AND `+alertBucketExpr+` = ?`,
		string(model.AnalysisTaskSucceeded), string(model.AnalysisTaskFailed), string(model.AnalysisTaskSuppressed), bucket)
	if err := row.Scan(&point.TotalAlerts, &point.AnalyzedAlerts, &point.FailedAlerts, &point.SuppressedAlerts); err != nil {
		return model.AnalysisTrendPoint{}, fmt.Errorf("scan alert analysis trend: %w", err)
	}
	return point, nil
}

func normalizeTrendInterval(interval string) string {
	switch strings.ToLower(strings.TrimSpace(interval)) {
	case "week", "weekly":
		return "week"
	case "month", "monthly":
		return "month"
	default:
		return "day"
	}
}

func defaultTrendLimit(interval string) int {
	switch normalizeTrendInterval(interval) {
	case "week":
		return 12
	case "month":
		return 12
	default:
		return 14
	}
}

func trendBucketExpr(timeExpr, interval string) string {
	switch normalizeTrendInterval(interval) {
	case "week":
		return "date(" + timeExpr + ", printf('-%d days', (CAST(strftime('%w', " + timeExpr + ") AS INTEGER) + 6) % 7))"
	case "month":
		return "substr(" + timeExpr + ", 1, 7)"
	default:
		return "substr(" + timeExpr + ", 1, 10)"
	}
}

func parseTrendBucketTime(bucket, interval string) time.Time {
	switch normalizeTrendInterval(interval) {
	case "month":
		return parseTime(bucket + "-01T00:00:00Z")
	default:
		return parseTime(bucket + "T00:00:00Z")
	}
}

func scanAnalysisReports(rows *sql.Rows) ([]model.AnalysisReport, error) {
	var out []model.AnalysisReport
	for rows.Next() {
		var (
			r       model.AnalysisReport
			payload string
			created string
		)
		if err := rows.Scan(&r.ID, &payload, &created); err != nil {
			return nil, fmt.Errorf("scan analysis report: %w", err)
		}
		if err := json.Unmarshal([]byte(payload), &r.Summary); err != nil {
			return nil, fmt.Errorf("parse analysis report: %w", err)
		}
		r.CreatedAt = parseTime(created)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analysis reports: %w", err)
	}
	return out, nil
}

func (s *Store) BuildAnalysisSummary(ctx context.Context) (model.AnalysisSummary, error) {
	summary := model.AnalysisSummary{
		ByAgent:    map[string]model.AgentSummary{},
		BySeverity: map[string]int{},
		ByStatus:   map[string]int{},
		Alerts: model.AlertAnalysisSummary{
			ByClassification: map[string]int{},
			BySeverity:       map[string]int{},
			ByConfidence:     map[string]int{},
			ByEnvironment:    map[string]int{},
		},
	}
	if err := s.fillReviewRunSummary(ctx, &summary); err != nil {
		return model.AnalysisSummary{}, err
	}
	if err := s.fillFindingSummary(ctx, &summary); err != nil {
		return model.AnalysisSummary{}, err
	}
	if err := s.fillDeveloperSummary(ctx, &summary); err != nil {
		return model.AnalysisSummary{}, err
	}
	if err := s.fillAlertAnalysisSummary(ctx, &summary.Alerts); err != nil {
		return model.AnalysisSummary{}, err
	}
	completed := summary.SuccessfulReviewRuns + summary.FailedReviewRuns
	if completed > 0 {
		summary.SuccessRate = float64(summary.SuccessfulReviewRuns) / float64(completed)
	}
	return summary, nil
}

func (s *Store) fillAlertAnalysisSummary(ctx context.Context, summary *model.AlertAnalysisSummary) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id,status,fingerprint,alert_payload,result_json,created_at
		FROM analysis_tasks ORDER BY id DESC`)
	if err != nil {
		return fmt.Errorf("alert analysis summary: %w", err)
	}
	defer rows.Close()

	services := map[string]int{}
	endpoints := map[string]int{}
	errorCodes := map[string]int{}
	type recurring struct {
		item model.RecurringAlertSummary
	}
	recurrences := map[string]*recurring{}
	var analyzed []alertInsightRecord

	for rows.Next() {
		var id int64
		var status, fingerprint, alertPayload, resultJSON, createdAt string
		if err := rows.Scan(&id, &status, &fingerprint, &alertPayload, &resultJSON, &createdAt); err != nil {
			return fmt.Errorf("scan alert analysis summary: %w", err)
		}
		summary.Total++
		switch model.AnalysisTaskStatus(status) {
		case model.AnalysisTaskSucceeded:
			summary.Analyzed++
		case model.AnalysisTaskFailed:
			summary.Failed++
		case model.AnalysisTaskSuppressed:
			summary.Suppressed++
		case model.AnalysisTaskCanceled:
			summary.Canceled++
		}
		var alert model.AlertEnvelope
		if err := json.Unmarshal([]byte(alertPayload), &alert); err != nil {
			continue // Keep lifecycle totals available if a legacy payload is malformed.
		}

		endpoint := strings.TrimSpace(strings.Join([]string{alert.Method, alert.Endpoint}, " "))
		incrementNormalized(summary.ByEnvironment, alert.Environment, "unknown")
		incrementNormalized(services, alert.Service, "未标注服务")
		incrementNormalized(endpoints, endpoint, "未标注接口")
		incrementNormalized(errorCodes, alert.ErrorCode, "未标注错误码")

		key := strings.TrimSpace(fingerprint)
		if key == "" {
			key = strings.Join([]string{alert.Service, alert.Method, alert.Endpoint, alert.ErrorCode, alert.ErrorMessage}, "|")
		}
		if recurrences[key] == nil {
			title := strings.TrimSpace(alert.Title)
			if title == "" {
				title = strings.TrimSpace(alert.Rule)
			}
			if title == "" {
				title = "未命名告警"
			}
			recurrences[key] = &recurring{item: model.RecurringAlertSummary{Title: title, Service: alert.Service, Endpoint: endpoint, ErrorCode: alert.ErrorCode}}
		}
		recurrences[key].item.Count++

		record := alertInsightRecord{
			ID:          id,
			Status:      model.AnalysisTaskStatus(status),
			Fingerprint: key,
			Alert:       alert,
			Endpoint:    endpoint,
			CreatedAt:   createdAt,
		}
		if strings.TrimSpace(resultJSON) != "" {
			var result model.AnalysisResult
			if json.Unmarshal([]byte(resultJSON), &result) == nil {
				record.Result = result
				record.HasResult = true
				classification := normalizedLabel(result.Classification, "unknown")
				severity := normalizedLabel(result.AssessedSeverity, "unknown")
				confidence := normalizedLabel(result.Confidence, "unknown")
				summary.ByClassification[classification]++
				summary.BySeverity[severity]++
				summary.ByConfidence[confidence]++
				if severity == "critical" || severity == "high" {
					summary.HighCritical++
					if len(summary.RecentSevere) < 10 {
						title := strings.TrimSpace(alert.Title)
						if title == "" {
							title = strings.TrimSpace(result.Summary)
						}
						summary.RecentSevere = append(summary.RecentSevere, model.SevereAlertSummary{
							TaskID: id, Title: title, Classification: classification, Severity: severity,
							Environment: alert.Environment, Service: alert.Service,
							Endpoint: endpoint, ErrorCode: alert.ErrorCode, Confidence: confidence, CreatedAt: createdAt,
						})
					}
				}
			}
		}
		analyzed = append(analyzed, record)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate alert analysis summary: %w", err)
	}

	completed := summary.Analyzed + summary.Failed
	if completed > 0 {
		summary.AnalysisSuccessRate = float64(summary.Analyzed) / float64(completed)
	}
	if summary.Total > 0 {
		summary.SuppressionRate = float64(summary.Suppressed) / float64(summary.Total)
	}
	summary.TopServices = topAlertDimensions(services, 8)
	summary.DistinctServices = len(services)
	summary.TopEndpoints = topAlertDimensions(endpoints, 8)
	summary.TopErrorCodes = topAlertDimensions(errorCodes, 8)
	for _, recurrence := range recurrences {
		if recurrence.item.Count > 1 {
			summary.RecurringAlerts = append(summary.RecurringAlerts, recurrence.item)
		}
	}
	sort.Slice(summary.RecurringAlerts, func(i, j int) bool {
		if summary.RecurringAlerts[i].Count != summary.RecurringAlerts[j].Count {
			return summary.RecurringAlerts[i].Count > summary.RecurringAlerts[j].Count
		}
		return summary.RecurringAlerts[i].Title < summary.RecurringAlerts[j].Title
	})
	if len(summary.RecurringAlerts) > 10 {
		summary.RecurringAlerts = summary.RecurringAlerts[:10]
	}
	synthesizeAlertInsights(summary, analyzed)
	return nil
}

type alertInsightCluster struct {
	key      string
	title    string
	count    int
	analyzed int
	records  []alertInsightRecord
}

type alertInsightRecord struct {
	ID          int64
	Status      model.AnalysisTaskStatus
	Fingerprint string
	Alert       model.AlertEnvelope
	Result      model.AnalysisResult
	HasResult   bool
	Endpoint    string
	CreatedAt   string
}

func synthesizeAlertInsights(summary *model.AlertAnalysisSummary, records []alertInsightRecord) {
	if len(records) == 0 {
		return
	}
	clusters := map[string]*alertInsightCluster{}
	actionCounts := map[string]int{}
	actionWhere := map[string]map[string]int{}
	gapCounts := map[string]int{}
	downgraded := 0
	compared := 0
	for _, rec := range records {
		cl := clusters[rec.Fingerprint]
		if cl == nil {
			title := strings.TrimSpace(rec.Alert.Title)
			if title == "" {
				title = strings.TrimSpace(rec.Alert.Rule)
			}
			if title == "" && rec.HasResult {
				title = strings.TrimSpace(rec.Result.Summary)
			}
			if title == "" {
				title = "未命名告警簇"
			}
			cl = &alertInsightCluster{key: rec.Fingerprint, title: title}
			clusters[rec.Fingerprint] = cl
		}
		cl.count++
		cl.records = append(cl.records, rec)
		if rec.HasResult {
			cl.analyzed++
			where := firstNonEmpty(rec.Alert.Service, rec.Endpoint, rec.Alert.ErrorCode)
			for _, action := range rec.Result.RecommendedActions {
				action = compactInsightText(action)
				if action == "" {
					continue
				}
				actionCounts[action]++
				if actionWhere[action] == nil {
					actionWhere[action] = map[string]int{}
				}
				if where != "" {
					actionWhere[action][where]++
				}
			}
			for _, gap := range rec.Result.EvidenceGaps {
				gap = compactInsightText(gap)
				if gap != "" {
					gapCounts[gap]++
				}
			}
			if src, assessed, ok := comparableSeverities(rec.Alert.Severity, rec.Result.AssessedSeverity); ok {
				compared++
				if assessed < src {
					downgraded++
				}
			}
		}
	}

	summary.FailureModes = buildAlertFailureModes(clusters)
	summary.Playbook = buildAlertPlaybook(actionCounts, actionWhere)
	summary.BlindSpots = buildAlertBlindSpots(gapCounts)
	summary.Lessons = buildAlertLessons(summary, clusters, actionCounts, gapCounts, compared, downgraded)
	summary.Briefing = buildAlertBriefing(summary)
}

func buildAlertFailureModes(clusters map[string]*alertInsightCluster) []model.AlertFailureMode {
	out := make([]model.AlertFailureMode, 0, len(clusters))
	for _, cl := range clusters {
		if cl.analyzed == 0 || cl.count < 2 {
			continue
		}
		best := pickCanonicalAnalysis(cl.records)
		services := uniqueInsightLabels(cl.records, func(rec alertInsightRecord) string { return rec.Alert.Service }, 3)
		endpoints := uniqueInsightLabels(cl.records, func(rec alertInsightRecord) string { return rec.Endpoint }, 3)
		mode := model.AlertFailureMode{
			Title:          cl.title,
			Count:          cl.count,
			Analyzed:       cl.analyzed,
			Services:       services,
			Endpoints:      endpoints,
			Classification: normalizedLabel(best.Result.Classification, "unknown"),
			Severity:       normalizedLabel(best.Result.AssessedSeverity, ""),
			Conclusion:     strings.TrimSpace(best.Result.Summary),
			WhyItRepeats:   firstInsightText(append(append([]string{}, best.Result.Hypotheses...), append(best.Result.Facts, best.Result.SeverityReason)...)),
			WhatToDo:       firstInsightText(best.Result.RecommendedActions),
		}
		if mode.Conclusion == "" {
			mode.Conclusion = "该告警簇被重复触发，但缺少可复用的中文结论。"
		}
		out = append(out, mode)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Analyzed != out[j].Analyzed {
			return out[i].Analyzed > out[j].Analyzed
		}
		return out[i].Title < out[j].Title
	})
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

func buildAlertPlaybook(actionCounts map[string]int, actionWhere map[string]map[string]int) []model.AlertPlaybookItem {
	out := make([]model.AlertPlaybookItem, 0, len(actionCounts))
	for action, count := range actionCounts {
		if count < 2 {
			continue
		}
		item := model.AlertPlaybookItem{Action: action, Count: count, Where: topMapLabels(actionWhere[action], 3)}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Action < out[j].Action
	})
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

func buildAlertBlindSpots(gapCounts map[string]int) []model.AlertBlindSpot {
	out := make([]model.AlertBlindSpot, 0, len(gapCounts))
	for gap, count := range gapCounts {
		if count < 2 {
			continue
		}
		out = append(out, model.AlertBlindSpot{Gap: gap, Count: count, Implication: implicationForGap(gap)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Gap < out[j].Gap
	})
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

func buildAlertLessons(summary *model.AlertAnalysisSummary, clusters map[string]*alertInsightCluster, actionCounts, gapCounts map[string]int, compared, downgraded int) []model.AlertLesson {
	var lessons []model.AlertLesson
	if summary.Total > 0 && summary.SuppressionRate >= 0.3 {
		lessons = append(lessons, model.AlertLesson{
			Kind:  "noise",
			Count: summary.Suppressed,
			Title: "重复告警已经在讲同一件事",
			Body:  fmt.Sprintf("全部 %d 条里有 %d 条被指纹抑制（%.0f%%）。看历史时应该按告警簇而不是按条数，否则会把同一个未修复问题数成一波新事故。", summary.Total, summary.Suppressed, summary.SuppressionRate*100),
		})
	}
	if summary.Analyzed > 0 {
		class, count := topMapEntry(summary.ByClassification)
		share := float64(count) / float64(summary.Analyzed)
		if count >= 2 && share >= 0.4 {
			lessons = append(lessons, model.AlertLesson{
				Kind:           "classification",
				Classification: class,
				Count:          count,
				Title:          classificationLessonTitle(class),
				Body:           classificationLessonBody(class, count, summary.Analyzed, share),
			})
		}
	}
	hot := hottestAnalyzedCluster(clusters)
	if hot != nil && hot.analyzed >= 2 {
		best := pickCanonicalAnalysis(hot.records)
		body := fmt.Sprintf("“%s”已被分析 %d 次、累计触发 %d 次。", hot.title, hot.analyzed, hot.count)
		if summary := strings.TrimSpace(best.Result.Summary); summary != "" {
			body += "最近高置信结论是：" + summary
			if !strings.HasSuffix(body, "。") {
				body += "。"
			}
		}
		if action := firstInsightText(best.Result.RecommendedActions); action != "" {
			body += "反复建议的下一步是" + action + "。"
		}
		lessons = append(lessons, model.AlertLesson{
			Kind:           "hotspot",
			Classification: normalizedLabel(best.Result.Classification, ""),
			Count:          hot.count,
			Title:          "同一热点在反复复发",
			Body:           body,
		})
	}
	if compared >= 3 && float64(downgraded)/float64(compared) >= 0.5 {
		lessons = append(lessons, model.AlertLesson{
			Kind:  "severity",
			Count: downgraded,
			Title: "来源告警等级经常高于真实影响",
			Body:  fmt.Sprintf("在能对比的 %d 次分析里，有 %d 次模型把严重度下调了。处理队列时不要直接照抄原告警等级，应以评估严重度和影响面为准。", compared, downgraded),
		})
	}
	if gap, count := topMapEntry(gapCounts); count >= 2 {
		lessons = append(lessons, model.AlertLesson{
			Kind:  "evidence",
			Count: count,
			Title: "证据缺口在重复出现",
			Body:  fmt.Sprintf("“%s”出现了 %d 次。%s", gap, count, implicationForGap(gap)),
		})
	}
	if action, count := topMapEntry(actionCounts); count >= 2 {
		lessons = append(lessons, model.AlertLesson{
			Kind:  "playbook",
			Count: count,
			Title: "已经形成可复用的处置习惯",
			Body:  fmt.Sprintf("历史结论里最常出现的下一步是“%s”（%d 次）。这更像一条应该沉淀下来的运行手册，而不是单次告警的临时建议。", action, count),
		})
	}
	completed := summary.Analyzed + summary.Failed
	if completed >= 4 && summary.Failed > 0 && float64(summary.Failed)/float64(completed) >= 0.25 {
		lessons = append(lessons, model.AlertLesson{
			Kind:  "pipeline",
			Count: summary.Failed,
			Title: "分析管道本身也在丢结论",
			Body:  fmt.Sprintf("完成态任务里有 %d / %d 次失败。页面上的类型分布只覆盖成功写出 JSON 的分析；失败任务不能当成“没有问题”。", summary.Failed, completed),
		})
	}
	if len(lessons) > 6 {
		lessons = lessons[:6]
	}
	return lessons
}

func buildAlertBriefing(summary *model.AlertAnalysisSummary) string {
	if summary.Total == 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("历史共 %d 条告警记录，完成分析 %d 条。", summary.Total, summary.Analyzed)}
	if summary.Suppressed > 0 {
		parts = append(parts, fmt.Sprintf("另有 %d 条因指纹重复被抑制，说明相当一部分流量是同一问题在回放。", summary.Suppressed))
	}
	if class, count := topMapEntry(summary.ByClassification); count > 0 {
		parts = append(parts, fmt.Sprintf("已有结论里最常见的判断是%s（%d 次）。", alertClassPhrase(class), count))
	}
	if summary.HighCritical > 0 {
		parts = append(parts, fmt.Sprintf("模型独立评估为 high/critical 的有 %d 条，这和原告警等级不是一回事。", summary.HighCritical))
	}
	if len(summary.FailureModes) > 0 {
		mode := summary.FailureModes[0]
		parts = append(parts, fmt.Sprintf("最顽固的复发簇是“%s”，已触发 %d 次。", mode.Title, mode.Count))
	}
	if len(summary.Lessons) > 0 {
		parts = append(parts, "下面把这些历史分析收成可复用经验，而不是再列一遍计数。")
	}
	return strings.Join(parts, "")
}

func hottestAnalyzedCluster(clusters map[string]*alertInsightCluster) *alertInsightCluster {
	var best *alertInsightCluster
	for _, cl := range clusters {
		if cl.analyzed == 0 {
			continue
		}
		if best == nil || cl.analyzed > best.analyzed || (cl.analyzed == best.analyzed && cl.count > best.count) {
			best = cl
		}
	}
	return best
}

func pickCanonicalAnalysis(records []alertInsightRecord) alertInsightRecord {
	var best alertInsightRecord
	bestScore := -1
	for _, rec := range records {
		if !rec.HasResult || strings.TrimSpace(rec.Result.Summary) == "" {
			continue
		}
		score := confidenceScore(rec.Result.Confidence)*10 + severityScore(rec.Result.AssessedSeverity)
		if rec.ID > best.ID {
			score++
		}
		if score >= bestScore {
			bestScore = score
			best = rec
		}
	}
	if bestScore >= 0 {
		return best
	}
	for _, rec := range records {
		if rec.HasResult {
			return rec
		}
	}
	return alertInsightRecord{}
}

func classificationLessonTitle(class string) string {
	switch class {
	case "expected_business":
		return "很大一部分告警其实是预期业务行为"
	case "code_regression":
		return "历史结论更偏向代码回归，而不是偶发噪音"
	case "data":
		return "数据问题在告警结论里反复出现"
	case "infra":
		return "基础设施抖动被当成了业务事故"
	default:
		return "仍有不少告警停在“待确认”"
	}
}

func classificationLessonBody(class string, count, analyzed int, share float64) string {
	pct := int(share * 100)
	switch class {
	case "expected_business":
		return fmt.Sprintf("%d / %d 次成功分析（%d%%）把它判成预期业务行为。这类告警优先考虑收敛规则、忽略码和产品预期，而不是每次都当缺陷排查。", count, analyzed, pct)
	case "code_regression":
		return fmt.Sprintf("%d / %d 次成功分析（%d%%）指向代码回归。经验上应沿同一接口的最近改动、调用链和部署版本核对，而不是把每次 paging 当成新的未知故障。", count, analyzed, pct)
	case "data":
		return fmt.Sprintf("%d / %d 次成功分析（%d%%）落在数据问题上。后续分析应先核对输入数据、缓存和上游状态，避免一上来就在当前服务里找回归。", count, analyzed, pct)
	case "infra":
		return fmt.Sprintf("%d / %d 次成功分析（%d%%）更像基础设施或依赖抖动。对这类告警，依赖健康、超时和容量往往比业务代码更值得先看。", count, analyzed, pct)
	default:
		return fmt.Sprintf("%d / %d 次成功分析（%d%%）仍是 unknown。说明现有日志、版本或代码证据经常不够支撑定性，应该先补证据而不是强行给根因。", count, analyzed, pct)
	}
}

func implicationForGap(gap string) string {
	lower := strings.ToLower(gap)
	switch {
	case strings.Contains(lower, "deployment_sha") || strings.Contains(gap, "部署版本") || strings.Contains(gap, "deployment"):
		return "没有线上版本锚点时，git blame / 最近提交只能当线索，不能写成生产根因。"
	case strings.Contains(lower, "sls") || strings.Contains(gap, "原始日志") || strings.Contains(gap, "日志"):
		return "缺原始日志时，结论容易停在假设层；应优先修日志检索窗口和匹配字段。"
	case strings.Contains(gap, "置信") || strings.Contains(lower, "confidence"):
		return "低置信结论不适合直接派给修复，需要先补证据或人工复核。"
	default:
		return "同一缺口反复出现，说明分析质量被系统性地限制，而不是某一次偶发信息不全。"
	}
}

func alertClassPhrase(class string) string {
	switch class {
	case "expected_business":
		return "预期业务行为"
	case "code_regression":
		return "代码回归"
	case "data":
		return "数据问题"
	case "infra":
		return "基础设施问题"
	default:
		return "待确认"
	}
}

func comparableSeverities(source, assessed string) (int, int, bool) {
	src, ok1 := severityScoreOK(source)
	got, ok2 := severityScoreOK(assessed)
	return src, got, ok1 && ok2
}

func severityScoreOK(value string) (int, bool) {
	score := severityScore(value)
	return score, score > 0
}

func severityScore(value string) int {
	switch normalizedLabel(value, "") {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium", "warning", "warn":
		return 2
	case "low", "info":
		return 1
	default:
		return 0
	}
}

func confidenceScore(value string) int {
	switch normalizedLabel(value, "") {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func compactInsightText(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) > 120 {
		return string(runes[:120])
	}
	return value
}

func firstInsightText(groups ...[]string) string {
	for _, group := range groups {
		for _, item := range group {
			item = strings.TrimSpace(item)
			if item != "" {
				return item
			}
		}
	}
	return ""
}

func firstInsightTextFrom(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	return firstInsightTextFrom(values...)
}

func uniqueInsightLabels(records []alertInsightRecord, getter func(alertInsightRecord) string, limit int) []string {
	seen := map[string]bool{}
	var out []string
	for _, rec := range records {
		label := strings.TrimSpace(getter(rec))
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func topMapEntry(counts map[string]int) (string, int) {
	best := ""
	bestCount := 0
	for key, count := range counts {
		if count > bestCount || (count == bestCount && (best == "" || key < best)) {
			best = key
			bestCount = count
		}
	}
	return best, bestCount
}

func topMapLabels(counts map[string]int, limit int) []string {
	items := topAlertDimensions(counts, limit)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item.Label != "" {
			out = append(out, item.Label)
		}
	}
	return out
}

func normalizedLabel(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback
	}
	return value
}

func incrementNormalized(counts map[string]int, value, fallback string) {
	counts[normalizedLabel(value, fallback)]++
}

func topAlertDimensions(counts map[string]int, limit int) []model.AlertDimensionSummary {
	items := make([]model.AlertDimensionSummary, 0, len(counts))
	for label, count := range counts {
		items = append(items, model.AlertDimensionSummary{Label: label, Count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		return items[i].Label < items[j].Label
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (s *Store) fillReviewRunSummary(ctx context.Context, summary *model.AnalysisSummary) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(agent,'codex'), status, COUNT(*)
		 FROM review_runs GROUP BY COALESCE(agent,'codex'), status`)
	if err != nil {
		return fmt.Errorf("review run summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var agent, status string
		var n int
		if err := rows.Scan(&agent, &status, &n); err != nil {
			return fmt.Errorf("scan review run summary: %w", err)
		}
		as := summary.ByAgent[agent]
		as.ReviewRuns += n
		switch model.ReviewRunStatus(status) {
		case model.ReviewRunDone:
			as.Succeeded += n
			summary.SuccessfulReviewRuns += n
		case model.ReviewRunFailed:
			as.Failed += n
			summary.FailedReviewRuns += n
		}
		summary.TotalReviewRuns += n
		summary.ByAgent[agent] = as
	}
	return rows.Err()
}

func (s *Store) fillFindingSummary(ctx context.Context, summary *model.AnalysisSummary) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(f.agent,'codex'), COALESCE(f.fingerprint,''), COALESCE(f.path,''), COALESCE(f.line,0),
		        COALESCE(f.severity,'info'), COALESCE(f.title,''),
		        COALESCE(f.status,'open'), COALESCE(f.last_seen_sha,''), COALESCE(f.tags,''),
		        COALESCE(r.owner,''), COALESCE(r.name,''), COALESCE(p.number,0), COALESCE(f.pull_id,0)
		 FROM findings f
		 LEFT JOIN pulls p ON p.id=f.pull_id
		 LEFT JOIN repos r ON r.id=p.repo_id
		 ORDER BY f.id DESC`)
	if err != nil {
		return fmt.Errorf("finding summary: %w", err)
	}
	defer rows.Close()

	tagCounts := map[string]int{}
	titleCounts := map[string]int{}
	type overlapKey struct {
		pullID      int64
		fingerprint string
	}
	overlap := map[overlapKey]*model.AgentOverlapSummary{}
	overlapAgents := map[overlapKey]map[string]bool{}

	for rows.Next() {
		var agent, fp, path, severity, title, status, lastSeen, tagsRaw, owner, repo string
		var line, pullNumber int
		var pullID int64
		if err := rows.Scan(&agent, &fp, &path, &line, &severity, &title, &status, &lastSeen, &tagsRaw, &owner, &repo, &pullNumber, &pullID); err != nil {
			return fmt.Errorf("scan finding summary: %w", err)
		}
		summary.TotalFindings++
		summary.BySeverity[severity]++
		summary.ByStatus[status]++
		as := summary.ByAgent[agent]
		as.Findings++
		if status == "open" {
			as.Open++
			summary.OpenFindings++
			if severity == string(model.SeverityHigh) || severity == string(model.SeverityCritical) {
				summary.HighCriticalOpen++
			}
		} else if status == "fixed" {
			summary.FixedFindings++
		}
		summary.ByAgent[agent] = as

		if title != "" {
			titleCounts[title]++
		}
		var tags []string
		_ = json.Unmarshal([]byte(tagsRaw), &tags)
		for _, tag := range normalizeTags(tags) {
			tagCounts[tag]++
		}
		if (severity == string(model.SeverityHigh) || severity == string(model.SeverityCritical)) && len(summary.RecentSevere) < 10 {
			summary.RecentSevere = append(summary.RecentSevere, model.SevereFindingSummary{
				Agent: agent, Owner: owner, Repo: repo, PullNumber: pullNumber,
				Severity: model.Severity(severity), Title: title,
				Path: path, Line: line, Status: status, LastSeenSHA: lastSeen,
			})
		}
		baseFP := strings.TrimSpace(strings.TrimPrefix(fp, agent+":"))
		if baseFP == "" || pullID == 0 {
			continue
		}
		key := overlapKey{pullID: pullID, fingerprint: baseFP}
		if overlap[key] == nil {
			overlap[key] = &model.AgentOverlapSummary{
				Fingerprint: baseFP, Owner: owner, Repo: repo, PullNumber: pullNumber,
				Title: title, Path: path, Line: line, LastSeenSHA: lastSeen,
			}
			overlapAgents[key] = map[string]bool{}
		}
		overlapAgents[key][agent] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate finding summary: %w", err)
	}
	summary.TopTags = topTagSummaries(tagCounts, 12)
	summary.RepeatedTitles = topTitleSummaries(titleCounts, 12)
	for key, item := range overlap {
		if len(overlapAgents[key]) < 2 {
			continue
		}
		for agent := range overlapAgents[key] {
			item.Agents = append(item.Agents, agent)
		}
		sort.Strings(item.Agents)
		item.AgentCount = len(item.Agents)
		summary.AgentOverlap = append(summary.AgentOverlap, *item)
	}
	sort.Slice(summary.AgentOverlap, func(i, j int) bool {
		a, b := summary.AgentOverlap[i], summary.AgentOverlap[j]
		if a.AgentCount != b.AgentCount {
			return a.AgentCount > b.AgentCount
		}
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.PullNumber != b.PullNumber {
			return a.PullNumber > b.PullNumber
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Title < b.Title
	})
	if len(summary.AgentOverlap) > 20 {
		summary.AgentOverlap = summary.AgentOverlap[:20]
	}
	return nil
}

func (s *Store) fillDeveloperSummary(ctx context.Context, summary *model.AnalysisSummary) error {
	items := map[string]*model.DeveloperSummary{}
	get := func(name string) *model.DeveloperSummary {
		name = normalizeDeveloper(name)
		if items[name] == nil {
			items[name] = &model.DeveloperSummary{Developer: name}
		}
		return items[name]
	}
	fallbackAuthors, err := s.jobAuthorFallbacks(ctx)
	if err != nil {
		return err
	}
	developerForPull := func(owner, repo string, number int, author string) string {
		author = strings.TrimSpace(author)
		if author != "" {
			return author
		}
		return fallbackAuthors[pullAuthorKey(owner, repo, number)]
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(r.owner,''), COALESCE(r.name,''), p.number, COALESCE(p.author,'')
		 FROM pulls p
		 JOIN repos r ON r.id=p.repo_id`)
	if err != nil {
		return fmt.Errorf("developer pulls summary: %w", err)
	}
	for rows.Next() {
		var owner, repo, author string
		var number int
		if err := rows.Scan(&owner, &repo, &number, &author); err != nil {
			rows.Close()
			return fmt.Errorf("scan developer pulls summary: %w", err)
		}
		get(developerForPull(owner, repo, number, author)).PullRequests++
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close developer pulls summary: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate developer pulls summary: %w", err)
	}

	rows, err = s.db.QueryContext(ctx,
		`SELECT COALESCE(r.owner,''), COALESCE(r.name,''), p.number, COALESCE(p.author,''), rr.status, COUNT(*)
		 FROM review_runs rr
		 JOIN pulls p ON p.id=rr.pull_id
		 JOIN repos r ON r.id=p.repo_id
		 GROUP BY r.owner, r.name, p.number, p.author, rr.status`)
	if err != nil {
		return fmt.Errorf("developer review runs summary: %w", err)
	}
	for rows.Next() {
		var owner, repo, author, status string
		var number int
		var n int
		if err := rows.Scan(&owner, &repo, &number, &author, &status, &n); err != nil {
			rows.Close()
			return fmt.Errorf("scan developer review runs summary: %w", err)
		}
		item := get(developerForPull(owner, repo, number, author))
		item.ReviewRuns += n
		switch model.ReviewRunStatus(status) {
		case model.ReviewRunDone:
			item.SuccessfulReviewRuns += n
		case model.ReviewRunFailed:
			item.FailedReviewRuns += n
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close developer review runs summary: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate developer review runs summary: %w", err)
	}

	rows, err = s.db.QueryContext(ctx,
		`SELECT COALESCE(r.owner,''), COALESCE(r.name,''), p.number, COALESCE(p.author,''),
		        COALESCE(f.status,'open'), COALESCE(f.severity,'info'), COUNT(*)
		 FROM findings f
		 JOIN pulls p ON p.id=f.pull_id
		 JOIN repos r ON r.id=p.repo_id
		 GROUP BY r.owner, r.name, p.number, p.author, COALESCE(f.status,'open'), COALESCE(f.severity,'info')`)
	if err != nil {
		return fmt.Errorf("developer findings summary: %w", err)
	}
	for rows.Next() {
		var owner, repo, author, status, severity string
		var number int
		var n int
		if err := rows.Scan(&owner, &repo, &number, &author, &status, &severity, &n); err != nil {
			rows.Close()
			return fmt.Errorf("scan developer findings summary: %w", err)
		}
		item := get(developerForPull(owner, repo, number, author))
		item.Findings += n
		if status == "open" {
			item.OpenFindings += n
			if severity == string(model.SeverityHigh) || severity == string(model.SeverityCritical) {
				item.HighCriticalOpen += n
			}
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close developer findings summary: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate developer findings summary: %w", err)
	}

	summary.ByDeveloper = make([]model.DeveloperSummary, 0, len(items))
	for _, item := range items {
		summary.ByDeveloper = append(summary.ByDeveloper, *item)
	}
	sort.Slice(summary.ByDeveloper, func(i, j int) bool {
		a, b := summary.ByDeveloper[i], summary.ByDeveloper[j]
		if a.Findings != b.Findings {
			return a.Findings > b.Findings
		}
		if a.OpenFindings != b.OpenFindings {
			return a.OpenFindings > b.OpenFindings
		}
		if a.FailedReviewRuns != b.FailedReviewRuns {
			return a.FailedReviewRuns > b.FailedReviewRuns
		}
		if a.PullRequests != b.PullRequests {
			return a.PullRequests > b.PullRequests
		}
		return a.Developer < b.Developer
	})
	if len(summary.ByDeveloper) > 20 {
		summary.ByDeveloper = summary.ByDeveloper[:20]
	}
	return nil
}

func (s *Store) jobAuthorFallbacks(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM jobs ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("developer job author fallback: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("scan developer job author fallback: %w", err)
		}
		var ev model.WebhookEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue
		}
		key := pullAuthorKey(ev.PR.Owner, ev.PR.Repo, ev.PR.Number)
		if key == "" || out[key] != "" {
			continue
		}
		if author := authorFromStoredEvent(ev); author != "" {
			out[key] = author
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate developer job author fallback: %w", err)
	}
	return out, nil
}

func pullAuthorKey(owner, repo string, number int) string {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" || number == 0 {
		return ""
	}
	return strings.ToLower(owner) + "\x00" + strings.ToLower(repo) + "\x00" + fmt.Sprint(number)
}

type storedEventUser struct {
	Username string `json:"username"`
	Login    string `json:"login"`
	Name     string `json:"name"`
}

func (u storedEventUser) name() string {
	for _, value := range []string{u.Username, u.Login, u.Name} {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func authorFromStoredEvent(ev model.WebhookEvent) string {
	if author := strings.TrimSpace(ev.Author); author != "" {
		return author
	}
	if len(ev.Raw) == 0 {
		return ""
	}
	var raw struct {
		PullRequest struct {
			User   storedEventUser `json:"user"`
			Poster storedEventUser `json:"poster"`
		} `json:"pull_request"`
		Issue struct {
			User storedEventUser `json:"user"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(ev.Raw, &raw); err != nil {
		return ""
	}
	for _, author := range []string{
		raw.PullRequest.User.name(),
		raw.PullRequest.Poster.name(),
		raw.Issue.User.name(),
	} {
		if author != "" {
			return author
		}
	}
	return ""
}

func normalizeDeveloper(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unknown"
	}
	return name
}

func topTagSummaries(counts map[string]int, limit int) []model.TagSummary {
	items := make([]model.TagSummary, 0, len(counts))
	for tag, count := range counts {
		items = append(items, model.TagSummary{Tag: tag, Count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count == items[j].Count {
			return items[i].Tag < items[j].Tag
		}
		return items[i].Count > items[j].Count
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func topTitleSummaries(counts map[string]int, limit int) []model.TitleSummary {
	items := make([]model.TitleSummary, 0, len(counts))
	for title, count := range counts {
		if count < 2 {
			continue
		}
		items = append(items, model.TitleSummary{Title: title, Count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count == items[j].Count {
			return items[i].Title < items[j].Title
		}
		return items[i].Count > items[j].Count
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}
