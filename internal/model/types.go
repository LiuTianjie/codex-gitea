// Package model defines shared domain types and module interfaces (ports).
// Concrete implementations live in sibling packages and depend only on this.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// ---------- Identifiers ----------

// PRRef identifies a pull request within a Gitea instance.
type PRRef struct {
	Owner  string
	Repo   string
	Number int
}

// Key returns a stable filesystem-safe key for caches and locks.
func (p PRRef) Key() string { return p.Owner + "__" + p.Repo }

// ---------- Enums ----------

// Severity ranks the importance of a finding.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Blocking reports whether a severity should trigger REQUEST_CHANGES.
func (s Severity) Blocking() bool { return s == SeverityHigh || s == SeverityCritical }

// Side indicates whether a finding/line refers to the new or old file.
type Side string

const (
	SideNew Side = "NEW"
	SideOld Side = "OLD"
)

// EventType is the Gitea webhook event class (X-Gitea-Event header).
type EventType string

const (
	EventPullRequest     EventType = "pull_request"
	EventPullRequestSync EventType = "pull_request_sync"
	EventIssueComment    EventType = "issue_comment"
)

// JobStatus tracks a job through the queue.
type JobStatus string

const (
	JobPending    JobStatus = "pending"
	JobRunning    JobStatus = "running"
	JobDone       JobStatus = "done"
	JobFailed     JobStatus = "failed"
	JobSuperseded JobStatus = "superseded"
	JobCanceled   JobStatus = "canceled"
)

var ErrJobNotPending = errors.New("job is not pending")

// ErrorType classifies failed work so the console can distinguish retryable
// provider/network problems from configuration or auth issues.
type ErrorType string

const (
	ErrorTypeUnknown  ErrorType = "unknown"
	ErrorTypeGit      ErrorType = "git"
	ErrorTypeGitea    ErrorType = "gitea"
	ErrorTypeAuth     ErrorType = "auth"
	ErrorTypeReviewer ErrorType = "reviewer"
	ErrorTypeUpstream ErrorType = "upstream"
	ErrorTypeTimeout  ErrorType = "timeout"
	ErrorTypeConfig   ErrorType = "config"
	ErrorTypeQueue    ErrorType = "queue"
)

// ReviewEventType is the Gitea review verdict.
type ReviewEventType string

const (
	ReviewEventComment        ReviewEventType = "COMMENT"
	ReviewEventRequestChanges ReviewEventType = "REQUEST_CHANGES"
	ReviewEventApproved       ReviewEventType = "APPROVED"
)

// PullRequestStatus is the live state of a pull request as reported by Gitea.
type PullRequestStatus struct {
	State  string
	Merged bool
	Author string
}

// Open reports whether the PR can still accept reviews.
func (s PullRequestStatus) Open() bool {
	return !s.Merged && (s.State == "" || s.State == "open")
}

// PullComment is a regular issue-thread comment attached to a pull request.
// It is used as review context, not as a finding source by itself.
type PullComment struct {
	ID        int64
	User      string
	Body      string
	CreatedAt time.Time
}

// ---------- Webhook / Job ----------

// WebhookEvent is the normalized result of parsing a Gitea webhook payload.
type WebhookEvent struct {
	DeliveryID string
	Event      EventType
	Action     string // opened|synchronized|edited|closed|reopened (PR); created|edited|deleted (comment)
	PR         PRRef
	Author     string
	BaseRef    string
	HeadRef    string
	HeadSHA    string
	CloneURL   string

	// issue_comment only
	IsPR        bool // issue.pull_request != null
	CommentBody string
	CommentUser string

	Raw []byte // raw verified body, persisted for replay/debug
}

// Job is a queued unit of work derived from a WebhookEvent.
type Job struct {
	ID            int64
	DeliveryID    string
	PR            PRRef
	Event         EventType
	Action        string
	Payload       []byte // serialized WebhookEvent
	Status        JobStatus
	Attempts      int
	Error         string
	ErrorType     ErrorType
	Retryable     bool
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
	NextAttemptAt *time.Time
}

// JobFinish carries terminal state, retry scheduling, and error metadata.
type JobFinish struct {
	Status        JobStatus
	Error         string
	ErrorType     ErrorType
	Retryable     bool
	NextAttemptAt *time.Time
}

// ---------- Codex / Review ----------

// Finding is one issue reported by codex, before diff-position mapping.
type Finding struct {
	Path     string   `json:"path"`
	Line     int      `json:"line"`
	Side     Side     `json:"side"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Tags     []string `json:"tags,omitempty"`
}

// Fingerprint is a stable hash key for dedup across re-reviews.
// Computed by the store layer; defined here so producers agree on inputs.
func (f Finding) FingerprintInput() string {
	return f.Path + "|" + f.Title + "|" + string(f.Severity)
}

// Fingerprint is a stable sha256 key for deduplicating findings across runs.
func (f Finding) Fingerprint() string {
	sum := sha256.Sum256([]byte(f.FingerprintInput()))
	return hex.EncodeToString(sum[:])
}

// ReviewResult is the structured output schema codex returns.
type ReviewResult struct {
	Summary              string    `json:"summary"`
	OverallSeverity      Severity  `json:"overall_severity"`
	Findings             []Finding `json:"findings"`
	ResolvedFingerprints []string  `json:"resolved_fingerprints"`

	// SessionID is the codex thread_id (from thread.started), filled by the runner.
	SessionID string `json:"-"`
}

// PullState is the persisted per-PR state (session continuity, last review).
type PullState struct {
	ID           int64
	PR           PRRef
	Author       string
	SessionID    string
	HeadSHA      string
	BaseRef      string
	LastReviewID int64
	UpdatedAt    time.Time
}

// PullReviewerState is the persisted per-agent state for a PR.
type PullReviewerState struct {
	ID           int64
	PullID       int64
	PR           PRRef
	Agent        string
	SessionID    string
	HeadSHA      string
	BaseRef      string
	LastReviewID int64
	UpdatedAt    time.Time
}

// ReviewRunStatus tracks one reviewer invocation.
type ReviewRunStatus string

const (
	ReviewRunRunning ReviewRunStatus = "running"
	ReviewRunDone    ReviewRunStatus = "done"
	ReviewRunFailed  ReviewRunStatus = "failed"
	ReviewRunSkipped ReviewRunStatus = "skipped"
)

// ReviewRun records one agent's attempt to review a PR head.
type ReviewRun struct {
	ID           int64
	PullID       int64
	JobID        int64
	Agent        string
	HeadSHA      string
	Status       ReviewRunStatus
	Error        string
	FindingCount int
	StartedAt    time.Time
	FinishedAt   *time.Time
}

// StoredFinding is a Finding with persistence metadata.
type StoredFinding struct {
	Finding
	ID             int64
	PullID         int64
	ReviewRunID    int64
	Agent          string
	Fingerprint    string
	GiteaCommentID int64
	ReviewID       int64
	FirstSeenSHA   string
	LastSeenSHA    string
	MappedInline   bool
	Status         string // open|fixed
}

// FindingSaveOptions carries metadata attached to a batch of findings.
type FindingSaveOptions struct {
	Agent              string
	ReviewRunID        int64
	MappedFingerprints map[string]bool
}

// ---------- Diff mapping ----------

// FileDiff records which line numbers in a file fall inside the PR diff hunks.
type FileDiff struct {
	NewLines map[int]bool
	OldLines map[int]bool
}

// DiffMap maps changed lines per file; built by the gitea diff parser and
// consumed by the orchestrator to decide which findings can be inline comments.
type DiffMap struct {
	Files map[string]FileDiff
}

// Position resolves a finding's (path,line,side) to Gitea review-comment
// positions. ok=false means the line is outside the diff (fold into summary).
func (d DiffMap) Position(path string, line int, side Side) (newPos, oldPos int64, ok bool) {
	fd, exists := d.Files[path]
	if !exists {
		return 0, 0, false
	}
	if side == SideOld {
		if fd.OldLines[line] {
			return 0, int64(line), true
		}
		return 0, 0, false
	}
	if fd.NewLines[line] {
		return int64(line), 0, true
	}
	return 0, 0, false
}

// ReviewComment is one inline comment in a Gitea review submission.
type ReviewComment struct {
	Path        string `json:"path"`
	Body        string `json:"body"`
	NewPosition int64  `json:"new_position"`
	OldPosition int64  `json:"old_position"`
}

// GiteaReview is a summary of an existing review (for listing/dismissing).
type GiteaReview struct {
	ID    int64
	State string // APPROVED|REQUEST_CHANGES|COMMENT|PENDING|DISMISSED
	Body  string
	Stale bool
}

// ---------- Console views ----------

// JobView is a read model for the console job list.
type JobView struct {
	ID            int64
	PR            PRRef
	Event         EventType
	Action        string
	Status        JobStatus
	Attempts      int
	Error         string
	ErrorType     ErrorType
	Retryable     bool
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
	NextAttemptAt *time.Time
	SessionID     string
	LogCount      int
	Logs          []JobLog
}

// JobLog is a timestamped progress entry for a queued job.
type JobLog struct {
	ID        int64
	Stage     string
	Message   string
	CreatedAt time.Time
}

// JobStats summarizes queue/review health for the console dashboard.
type JobStats struct {
	TotalJobs        int
	ReviewJobs       int
	ReviewedJobs     int
	Done             int
	Failed           int
	Running          int
	Pending          int
	Superseded       int
	Canceled         int
	RetryablePending int
}

// JobFilter constrains console job history queries.
type JobFilter struct {
	CreatedFrom *time.Time
	CreatedTo   *time.Time
}

// AnalysisReport is a saved snapshot of full-history review/finding analytics.
type AnalysisReport struct {
	ID        int64
	CreatedAt time.Time
	Summary   AnalysisSummary
}

type AnalysisTrendPoint struct {
	Bucket               string    `json:"bucket"`
	Interval             string    `json:"interval"`
	Day                  string    `json:"day"`
	FinishedAt           time.Time `json:"finished_at"`
	TotalReviewRuns      int       `json:"total_review_runs"`
	SuccessfulReviewRuns int       `json:"successful_review_runs"`
	FailedReviewRuns     int       `json:"failed_review_runs"`
	SuccessRate          float64   `json:"success_rate"`
	TotalFindings        int       `json:"total_findings"`
	OpenFindings         int       `json:"open_findings"`
	HighCriticalOpen     int       `json:"high_critical_open"`
	TotalAlerts          int       `json:"total_alerts"`
	AnalyzedAlerts       int       `json:"analyzed_alerts"`
	FailedAlerts         int       `json:"failed_alerts"`
	SuppressedAlerts     int       `json:"suppressed_alerts"`
}

type AnalysisSummary struct {
	TotalReviewRuns      int                     `json:"total_review_runs"`
	SuccessfulReviewRuns int                     `json:"successful_review_runs"`
	FailedReviewRuns     int                     `json:"failed_review_runs"`
	SuccessRate          float64                 `json:"success_rate"`
	TotalFindings        int                     `json:"total_findings"`
	OpenFindings         int                     `json:"open_findings"`
	FixedFindings        int                     `json:"fixed_findings"`
	HighCriticalOpen     int                     `json:"high_critical_open"`
	ByAgent              map[string]AgentSummary `json:"by_agent"`
	ByDeveloper          []DeveloperSummary      `json:"by_developer"`
	BySeverity           map[string]int          `json:"by_severity"`
	ByStatus             map[string]int          `json:"by_status"`
	TopTags              []TagSummary            `json:"top_tags"`
	RepeatedTitles       []TitleSummary          `json:"repeated_titles"`
	RecentSevere         []SevereFindingSummary  `json:"recent_severe"`
	AgentOverlap         []AgentOverlapSummary   `json:"agent_overlap"`
	Alerts               AlertAnalysisSummary    `json:"alerts"`
}

// AlertAnalysisSummary aggregates durable alert-analysis tasks and their
// structured model results. Raw alert fields and assessed conclusions remain
// separate so the dashboard does not mistake source severity for analysis.
type AlertAnalysisSummary struct {
	Total               int                     `json:"total"`
	Analyzed            int                     `json:"analyzed"`
	Failed              int                     `json:"failed"`
	Suppressed          int                     `json:"suppressed"`
	Canceled            int                     `json:"canceled"`
	AnalysisSuccessRate float64                 `json:"analysis_success_rate"`
	SuppressionRate     float64                 `json:"suppression_rate"`
	HighCritical        int                     `json:"high_critical"`
	DistinctServices    int                     `json:"distinct_services"`
	ByClassification    map[string]int          `json:"by_classification"`
	BySeverity          map[string]int          `json:"by_severity"`
	ByConfidence        map[string]int          `json:"by_confidence"`
	ByEnvironment       map[string]int          `json:"by_environment"`
	TopServices         []AlertDimensionSummary `json:"top_services"`
	TopEndpoints        []AlertDimensionSummary `json:"top_endpoints"`
	TopErrorCodes       []AlertDimensionSummary `json:"top_error_codes"`
	RecurringAlerts     []RecurringAlertSummary `json:"recurring_alerts"`
	RecentSevere        []SevereAlertSummary    `json:"recent_severe"`
}

type AlertDimensionSummary struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

type RecurringAlertSummary struct {
	Title     string `json:"title"`
	Service   string `json:"service"`
	Endpoint  string `json:"endpoint"`
	ErrorCode string `json:"error_code"`
	Count     int    `json:"count"`
}

type SevereAlertSummary struct {
	TaskID         int64  `json:"task_id"`
	Title          string `json:"title"`
	Classification string `json:"classification"`
	Severity       string `json:"severity"`
	Environment    string `json:"environment"`
	Service        string `json:"service"`
	Endpoint       string `json:"endpoint"`
	ErrorCode      string `json:"error_code"`
	Confidence     string `json:"confidence"`
	CreatedAt      string `json:"created_at"`
}

type AgentSummary struct {
	ReviewRuns int `json:"review_runs"`
	Succeeded  int `json:"succeeded"`
	Failed     int `json:"failed"`
	Findings   int `json:"findings"`
	Open       int `json:"open"`
}

type DeveloperSummary struct {
	Developer            string `json:"developer"`
	PullRequests         int    `json:"pull_requests"`
	ReviewRuns           int    `json:"review_runs"`
	SuccessfulReviewRuns int    `json:"successful_review_runs"`
	FailedReviewRuns     int    `json:"failed_review_runs"`
	Findings             int    `json:"findings"`
	OpenFindings         int    `json:"open_findings"`
	HighCriticalOpen     int    `json:"high_critical_open"`
}

type TagSummary struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

type TitleSummary struct {
	Title string `json:"title"`
	Count int    `json:"count"`
}

type SevereFindingSummary struct {
	Agent       string   `json:"agent"`
	Owner       string   `json:"owner"`
	Repo        string   `json:"repo"`
	PullNumber  int      `json:"pull_number"`
	Severity    Severity `json:"severity"`
	Title       string   `json:"title"`
	Path        string   `json:"path"`
	Line        int      `json:"line"`
	Status      string   `json:"status"`
	LastSeenSHA string   `json:"last_seen_sha"`
}

type AgentOverlapSummary struct {
	Fingerprint string   `json:"fingerprint"`
	AgentCount  int      `json:"agent_count"`
	Owner       string   `json:"owner"`
	Repo        string   `json:"repo"`
	PullNumber  int      `json:"pull_number"`
	Title       string   `json:"title"`
	Path        string   `json:"path"`
	Line        int      `json:"line"`
	LastSeenSHA string   `json:"last_seen_sha"`
	Agents      []string `json:"agents"`
}

type ProjectSkillSummary struct {
	Owner            string    `json:"owner"`
	Repo             string    `json:"repo"`
	Slug             string    `json:"slug"`
	PullRequests     int       `json:"pull_requests"`
	ReviewRuns       int       `json:"review_runs"`
	Findings         int       `json:"findings"`
	OpenFindings     int       `json:"open_findings"`
	HighCriticalOpen int       `json:"high_critical_open"`
	SkillVersion     int       `json:"skill_version"`
	SkillUpdatedAt   time.Time `json:"skill_updated_at"`
}

type ProjectSkill struct {
	ID                 int64
	Owner              string
	Repo               string
	Slug               string
	Title              string
	Content            string
	Version            int
	SourceFindingCount int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type ProjectSkillPattern struct {
	Title      string   `json:"title"`
	Severity   Severity `json:"severity"`
	Status     string   `json:"status"`
	Count      int      `json:"count"`
	OpenCount  int      `json:"open_count"`
	SamplePath string   `json:"sample_path"`
	SampleLine int      `json:"sample_line"`
	Agents     []string `json:"agents"`
	Tags       []string `json:"tags"`
}

type ProjectSkillContext struct {
	Owner            string
	Repo             string
	Slug             string
	PullRequests     int
	ReviewRuns       int
	Findings         int
	OpenFindings     int
	HighCriticalOpen int
	TopTags          []TagSummary
	Patterns         []ProjectSkillPattern
}

type SkillGenerationInput struct {
	Context  ProjectSkillContext
	Existing *ProjectSkill
}

// ---------- Ports (interfaces) ----------

// Store persists jobs, PR state, findings, and console-editable settings.
type Store interface {
	// Jobs
	EnqueueJob(ctx context.Context, ev *WebhookEvent) (job *Job, created bool, err error)
	SupersedePending(ctx context.Context, pr PRRef) error
	ClaimJob(ctx context.Context) (*Job, error) // next pending -> running; nil if none
	FinishJob(ctx context.Context, id int64, status JobStatus, errMsg string) error
	FinishJobDetailed(ctx context.Context, id int64, finish JobFinish) error
	FinishRunningJobDetailed(ctx context.Context, id int64, finish JobFinish) (bool, error)
	RerunJob(ctx context.Context, id int64) (*Job, error)
	CancelPendingJob(ctx context.Context, id int64) (*Job, error)
	RecoverRunning(ctx context.Context) error // running -> pending on boot
	AppendJobLog(ctx context.Context, jobID int64, stage, message string) error
	ListJobs(ctx context.Context, limit, offset int) ([]JobView, error)
	ListJobsFiltered(ctx context.Context, filter JobFilter, limit, offset int) ([]JobView, error)
	CountJobs(ctx context.Context) (int, error)
	CountJobsFiltered(ctx context.Context, filter JobFilter) (int, error)
	JobStats(ctx context.Context) (JobStats, error)
	JobStatsFiltered(ctx context.Context, filter JobFilter) (JobStats, error)
	GetJobView(ctx context.Context, id int64) (*JobView, error)
	GetJob(ctx context.Context, id int64) (*Job, error)

	// Pulls / sessions
	GetPull(ctx context.Context, pr PRRef) (*PullState, error)
	UpsertPull(ctx context.Context, st *PullState) error
	SetSession(ctx context.Context, pr PRRef, sessionID string) error
	SetLastReview(ctx context.Context, pr PRRef, reviewID int64) error
	GetReviewerState(ctx context.Context, pr PRRef, agent string) (*PullReviewerState, error)
	UpsertReviewerState(ctx context.Context, st *PullReviewerState) error

	// Findings
	CreateReviewRun(ctx context.Context, run *ReviewRun) error
	FinishReviewRun(ctx context.Context, runID int64, status ReviewRunStatus, errMsg string, findingCount int) error
	SaveFindings(ctx context.Context, pullID int64, headSHA string, fs []Finding, opts FindingSaveOptions) error
	MarkFindingsFixed(ctx context.Context, pullID int64, fingerprints []string) error
	ListFindings(ctx context.Context, pullID int64) ([]StoredFinding, error)
	MarkFindingPosted(ctx context.Context, findingID, commentID, reviewID int64) error

	// Analytics
	CreateAnalysisReport(ctx context.Context, summary AnalysisSummary) (*AnalysisReport, error)
	LatestAnalysisReport(ctx context.Context) (*AnalysisReport, error)
	ListAnalysisReports(ctx context.Context, limit int) ([]AnalysisReport, error)
	BuildAnalysisSummary(ctx context.Context) (AnalysisSummary, error)
	BuildAnalysisTrend(ctx context.Context, limit int, interval string) ([]AnalysisTrendPoint, error)
	ListProjectSkillSummaries(ctx context.Context) ([]ProjectSkillSummary, error)
	GetProjectSkill(ctx context.Context, owner, repo string) (*ProjectSkill, error)
	UpsertProjectSkill(ctx context.Context, skill *ProjectSkill) error
	BuildProjectSkillContext(ctx context.Context, owner, repo string) (ProjectSkillContext, error)

	// Settings (console-editable runtime config)
	GetSetting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string, isSecret bool) error
	AllSettings(ctx context.Context) (map[string]string, error)

	Close() error
}

// GitCache manages per-repo bare mirrors and throwaway worktrees.
type GitCache interface {
	// Prepare ensures the mirror exists, fetches base+PR refs, and checks out
	// a deterministic worktree at headSHA. Returns the worktree path.
	Prepare(ctx context.Context, pr PRRef, cloneURL, baseRef, headRef, headSHA string) (worktree string, err error)
	// Cleanup removes the worktree but keeps the mirror.
	Cleanup(pr PRRef) error
}

// CodexInput parameterizes a codex review run.
type CodexInput struct {
	Worktree  string
	BaseRef   string
	SessionID string // empty = new review; set = resume an existing thread
	Note      string // optional extra context (e.g. "head moved A->B, note what's fixed")
}

// Reviewer invokes a code-review agent in headless mode.
type Reviewer interface {
	Name() string
	// Review runs a structured review (new or resume) and returns findings + session id.
	Review(ctx context.Context, in CodexInput) (*ReviewResult, error)
	// Ask resumes a session with a free-form question and returns the text answer.
	Ask(ctx context.Context, sessionID, worktree, question string) (string, error)
	// Status reports whether codex auth is currently valid (codex login status).
	Status(ctx context.Context) (string, error)
}

// CodexRunner is kept as a compatibility alias for the default reviewer.
type CodexRunner interface {
	Reviewer
}

// GiteaClient talks to the Gitea REST API.
type GiteaClient interface {
	GetPullRequestStatus(ctx context.Context, pr PRRef) (PullRequestStatus, error)
	GetDiff(ctx context.Context, pr PRRef) (DiffMap, error)
	ListIssueComments(ctx context.Context, pr PRRef) ([]PullComment, error)
	PostReview(ctx context.Context, pr PRRef, commitID string, event ReviewEventType, body string, comments []ReviewComment) (reviewID int64, err error)
	PostComment(ctx context.Context, pr PRRef, body string) (commentID int64, err error)
	ListReviews(ctx context.Context, pr PRRef) ([]GiteaReview, error)
	DismissReview(ctx context.Context, pr PRRef, reviewID int64, msg string) error
}
