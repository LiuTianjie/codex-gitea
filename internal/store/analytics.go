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

		incrementNormalized(summary.ByEnvironment, alert.Environment, "unknown")
		incrementNormalized(services, alert.Service, "未标注服务")
		incrementNormalized(endpoints, strings.TrimSpace(strings.Join([]string{alert.Method, alert.Endpoint}, " ")), "未标注接口")
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
			recurrences[key] = &recurring{item: model.RecurringAlertSummary{Title: title, Service: alert.Service, Endpoint: strings.TrimSpace(strings.Join([]string{alert.Method, alert.Endpoint}, " ")), ErrorCode: alert.ErrorCode}}
		}
		recurrences[key].item.Count++

		if strings.TrimSpace(resultJSON) == "" {
			continue
		}
		var result model.AnalysisResult
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			continue // Preserve legacy/malformed task history without breaking reports.
		}
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
					Endpoint:  strings.TrimSpace(strings.Join([]string{alert.Method, alert.Endpoint}, " ")),
					ErrorCode: alert.ErrorCode, Confidence: confidence, CreatedAt: createdAt,
				})
			}
		}
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
	return nil
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
