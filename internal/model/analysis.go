package model

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// AnalysisTaskStatus is the durable lifecycle of one alert analysis.
type AnalysisTaskStatus string

const (
	AnalysisTaskQueued          AnalysisTaskStatus = "queued"
	AnalysisTaskRunning         AnalysisTaskStatus = "running"
	AnalysisTaskSucceeded       AnalysisTaskStatus = "succeeded"
	AnalysisTaskFailed          AnalysisTaskStatus = "failed"
	AnalysisTaskCancelRequested AnalysisTaskStatus = "cancel_requested"
	AnalysisTaskCanceled        AnalysisTaskStatus = "canceled"
	AnalysisTaskSuppressed      AnalysisTaskStatus = "suppressed"
)

var (
	ErrAnalysisConfigNotFound = errors.New("analysis config not found")
	ErrAnalysisTaskNotFound   = errors.New("analysis task not found")
	ErrAnalysisTaskTerminal   = errors.New("analysis task is already terminal")
)

const (
	DefaultAnalysisConcurrency = 2
	MaxAnalysisConcurrency     = 16
)

// AnalysisConfig is one console-managed alert source. Secret fields are
// plaintext only after the store decrypts them; HTTP responses must redact
// them before serialization.
type AnalysisConfig struct {
	ID                   int64
	Name                 string
	Enabled              bool
	Version              int
	RepositoryURL        string
	RepositoryRef        string
	SLSEndpoint          string
	SLSProject           string
	SLSLogstore          string
	SLSAccessKeyID       string
	SLSAccessKeySecret   string
	FeishuMode           string
	FeishuWebhook        string
	FeishuAppID          string
	FeishuAppSecret      string
	FeishuChatID         string
	Model                string
	ReasoningEffort      string
	Concurrency          int
	TimeoutSeconds       int
	LogWindowSeconds     int
	Prompt               string
	ThrottleEnabled      bool
	ThrottleThreshold    int
	ThrottleCooldownSecs int
	ThrottleFields       string
	IngestTokenHash      string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Snapshot returns the non-secret configuration persisted with every task.
func (c AnalysisConfig) Snapshot() AnalysisConfigSnapshot {
	return AnalysisConfigSnapshot{
		ConfigID:             c.ID,
		ConfigVersion:        c.Version,
		Name:                 c.Name,
		RepositoryURL:        c.RepositoryURL,
		RepositoryRef:        c.RepositoryRef,
		SLSEndpoint:          c.SLSEndpoint,
		SLSProject:           c.SLSProject,
		SLSLogstore:          c.SLSLogstore,
		Model:                c.Model,
		ReasoningEffort:      c.ReasoningEffort,
		Concurrency:          c.Concurrency,
		TimeoutSeconds:       c.TimeoutSeconds,
		LogWindowSeconds:     c.LogWindowSeconds,
		Prompt:               c.Prompt,
		ThrottleEnabled:      c.ThrottleEnabled,
		ThrottleThreshold:    c.ThrottleThreshold,
		ThrottleCooldownSecs: c.ThrottleCooldownSecs,
		ThrottleFields:       c.ThrottleFields,
	}
}

type AnalysisConfigSnapshot struct {
	ConfigID             int64  `json:"config_id"`
	ConfigVersion        int    `json:"config_version"`
	Name                 string `json:"name"`
	RepositoryURL        string `json:"repository_url"`
	RepositoryRef        string `json:"repository_ref"`
	SLSEndpoint          string `json:"sls_endpoint"`
	SLSProject           string `json:"sls_project"`
	SLSLogstore          string `json:"sls_logstore"`
	Model                string `json:"model"`
	ReasoningEffort      string `json:"reasoning_effort"`
	Concurrency          int    `json:"concurrency"`
	TimeoutSeconds       int    `json:"timeout_seconds"`
	LogWindowSeconds     int    `json:"log_window_seconds"`
	Prompt               string `json:"prompt,omitempty"`
	ThrottleEnabled      bool   `json:"throttle_enabled"`
	ThrottleThreshold    int    `json:"throttle_threshold"`
	ThrottleCooldownSecs int    `json:"throttle_cooldown_seconds"`
	ThrottleFields       string `json:"throttle_fields"`
}

// AlertEnvelope is the normalized machine payload sent by an alert source.
// Raw preserves additional configured fields for evidence and future sources.
type AlertEnvelope struct {
	DeliveryID    string         `json:"delivery_id"`
	AlertID       string         `json:"alert_id"`
	AlertTime     string         `json:"alert_time"`
	Environment   string         `json:"environment"`
	Service       string         `json:"service"`
	Rule          string         `json:"rule"`
	Title         string         `json:"title"`
	Severity      string         `json:"severity"`
	Method        string         `json:"method"`
	Endpoint      string         `json:"endpoint"`
	EventID       string         `json:"event_id"`
	TraceID       string         `json:"trace_id"`
	ErrorCode     string         `json:"error_code"`
	ErrorMessage  string         `json:"error_message"`
	DeploymentSHA string         `json:"deployment_sha"`
	DetailURL     string         `json:"detail_url"`
	Raw           map[string]any `json:"raw,omitempty"`
}

type AnalysisTask struct {
	ID                 int64
	ConfigID           *int64
	ConfigVersion      int
	ConfigName         string
	DeliveryID         string
	RetryOfTaskID      *int64
	DuplicateOfTaskID  *int64
	Fingerprint        string
	Alert              AlertEnvelope
	ConfigSnapshot     AnalysisConfigSnapshot
	Status             AnalysisTaskStatus
	Phase              string
	Attempts           int
	CancelRequested    bool
	ErrorType          string
	Error              string
	ResultJSON         string
	NotificationStatus string
	NotificationError  string
	FeishuMessageID    string
	CreatedAt          time.Time
	StartedAt          *time.Time
	FinishedAt         *time.Time
}

type AnalysisTaskEvent struct {
	ID        int64           `json:"id"`
	TaskID    int64           `json:"task_id"`
	Sequence  int             `json:"sequence"`
	Phase     string          `json:"phase"`
	Level     string          `json:"level"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type AnalysisCodeEvidence struct {
	Path     string `json:"path"`
	Line     int    `json:"line,omitempty"`
	Revision string `json:"revision,omitempty"`
	Reason   string `json:"reason"`
}

type AnalysisCommitEvidence struct {
	SHA         string `json:"sha"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	AuthorEmail string `json:"author_email,omitempty"`
	CommittedAt string `json:"committed_at,omitempty"`
	Reason      string `json:"reason"`
	Confidence  string `json:"confidence"`
}

type AnalysisResult struct {
	Classification           string                   `json:"classification"`
	Summary                  string                   `json:"summary"`
	Confidence               string                   `json:"confidence"`
	AssessedSeverity         string                   `json:"assessed_severity"`
	SeverityReason           string                   `json:"severity_reason"`
	ImpactScope              []string                 `json:"impact_scope"`
	Facts                    []string                 `json:"facts"`
	Hypotheses               []string                 `json:"hypotheses"`
	AffectedEndpointsOrTasks []string                 `json:"affected_endpoints_or_tasks"`
	CodeEvidence             []AnalysisCodeEvidence   `json:"code_evidence"`
	SuspectCommits           []AnalysisCommitEvidence `json:"suspect_commits"`
	SuggestedContacts        []string                 `json:"suggested_contacts"`
	EvidenceGaps             []string                 `json:"evidence_gaps"`
	RecommendedActions       []string                 `json:"recommended_actions"`
}

type AnalysisTaskFilter struct {
	Status   AnalysisTaskStatus
	ConfigID int64
	Limit    int
	Offset   int
}

// AnalysisStore is deliberately separate from the PR-review Store contract so
// the incident pipeline can evolve without making PR jobs generic prematurely.
type AnalysisStore interface {
	CreateAnalysisConfig(context.Context, *AnalysisConfig) (*AnalysisConfig, error)
	UpdateAnalysisConfig(context.Context, *AnalysisConfig) (*AnalysisConfig, error)
	ListAnalysisConfigs(context.Context) ([]AnalysisConfig, error)
	GetAnalysisConfig(context.Context, int64) (*AnalysisConfig, error)
	DeleteAnalysisConfig(context.Context, int64) error
	SetAnalysisConfigEnabled(context.Context, int64, bool) (*AnalysisConfig, error)
	RotateAnalysisConfigToken(context.Context, int64, string) error
	VerifyAnalysisConfigToken(context.Context, int64, string) (*AnalysisConfig, error)

	EnqueueAnalysisTask(context.Context, AnalysisConfig, AlertEnvelope, string, string) (*AnalysisTask, bool, error)
	ClaimAnalysisTask(context.Context) (*AnalysisTask, error)
	GetAnalysisTask(context.Context, int64) (*AnalysisTask, error)
	ListAnalysisTasks(context.Context, AnalysisTaskFilter) ([]AnalysisTask, error)
	AppendAnalysisTaskEvent(context.Context, int64, string, string, string, json.RawMessage) error
	ListAnalysisTaskEvents(context.Context, int64, int64) ([]AnalysisTaskEvent, error)
	FinishAnalysisTask(context.Context, int64, AnalysisTaskStatus, string, string, string) error
	SetAnalysisTaskPhase(context.Context, int64, string) error
	SetAnalysisNotification(context.Context, int64, string, string, string) error
	RequestAnalysisTaskCancel(context.Context, int64) (*AnalysisTask, error)
	RetryAnalysisTask(context.Context, int64, AnalysisConfigSnapshot) (*AnalysisTask, error)
	RecoverAnalysisTasks(context.Context) error
}
