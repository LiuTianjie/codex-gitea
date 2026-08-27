// Command codex-gitea runs the Gitea PR auto-review service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/turning4th/codex-gitea/internal/claude"
	"github.com/turning4th/codex-gitea/internal/codex"
	"github.com/turning4th/codex-gitea/internal/config"
	"github.com/turning4th/codex-gitea/internal/console"
	"github.com/turning4th/codex-gitea/internal/gitcache"
	"github.com/turning4th/codex-gitea/internal/gitea"
	"github.com/turning4th/codex-gitea/internal/incident"
	"github.com/turning4th/codex-gitea/internal/model"
	"github.com/turning4th/codex-gitea/internal/queue"
	"github.com/turning4th/codex-gitea/internal/review"
	"github.com/turning4th/codex-gitea/internal/store"
	"github.com/turning4th/codex-gitea/internal/webhook"
)

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmsgprefix)

	cfg := config.LoadEnv()

	st, err := store.Open(cfg.DBPath, store.WithSecretKey(cfg.SecretKey))
	if err != nil {
		logger.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// DB settings normally override env defaults. CONFIG_SOURCE=env makes
	// container env the source of truth for deployments that should not depend
	// on the mutable admin-console settings table.
	envConfigSource := strings.EqualFold(os.Getenv("CONFIG_SOURCE"), "env")
	if envConfigSource {
		logger.Printf("config source: env (database settings ignored)")
	} else {
		if settings, err := st.AllSettings(context.Background()); err == nil {
			cfg.ApplyOverrides(settings)
		} else {
			logger.Printf("load settings: %v", err)
		}
	}
	for _, w := range cfg.Warnings() {
		logger.Printf("config warning: %s", w)
	}
	if err := cfg.Validate(); err != nil {
		logger.Fatalf("invalid config: %v", err)
	}
	var cfgMu sync.RWMutex
	runtimeCfg := cfg.Clone()
	configSnapshot := func() *config.Config {
		cfgMu.RLock()
		defer cfgMu.RUnlock()
		return runtimeCfg.Clone()
	}
	applySettings := func(_ context.Context, updates map[string]string) error {
		if envConfigSource {
			return nil
		}
		cfgMu.Lock()
		defer cfgMu.Unlock()
		next := runtimeCfg.Clone()
		next.ApplyOverrides(updates)
		if err := next.Validate(); err != nil {
			return err
		}
		runtimeCfg = next
		return nil
	}

	// Build modules.
	giteaClient := gitea.NewDynamic(func() gitea.Config {
		snap := configSnapshot()
		return gitea.Config{
			BaseURL: snap.GiteaURL,
			Token:   snap.GiteaToken,
			Timeout: snap.GiteaTimeout,
		}
	})
	cache := gitcache.New(cfg.CacheDir, cfg.WorkDir,
		gitcache.WithTokenFunc(func() (string, error) {
			return configSnapshot().GiteaToken, nil
		}),
		gitcache.WithIncidentCachePolicyFunc(func() gitcache.IncidentCachePolicy {
			snap := configSnapshot()
			return gitcache.IncidentCachePolicy{
				FetchDepth:      snap.AnalysisGitFetchDepth,
				MaxRepositories: snap.AnalysisCacheMaxRepositories,
				MaxBytes:        snap.AnalysisCacheMaxMB << 20,
				MaxIdle:         snap.AnalysisCacheMaxIdle,
				WorktreeTTL:     snap.AnalysisWorktreeTTL,
				CleanupInterval: snap.AnalysisCacheCleanupInterval,
				MinFreeBytes:    snap.AnalysisMinFreeMB << 20,
			}
		}),
	)

	orch := &review.Orchestrator{
		Store:         st,
		Cache:         cache,
		ReviewersFunc: func() []model.Reviewer { return buildReviewers(configSnapshot()) },
		Gitea:         giteaClient,
		TriggerKeywordsFunc: func() []string {
			return configSnapshot().TriggerKeywords
		},
		RepoAllowlistFunc: func() []string {
			return configSnapshot().RepoAllowlist
		},
		Logger: logger,
	}

	q := queue.New(st, orch, cfg.Concurrency, logger)
	incidentLogs := incident.AliyunSLSFetcher{}
	incidentNotifier := &incident.FeishuNotifier{ConsoleBaseURL: os.Getenv("PUBLIC_URL")}
	incidentProcessor := &incident.Processor{
		Store: st,
		Logs:  incidentLogs,
		Cache: cache,
		Analyze: func(ctx context.Context, worktree, prompt string, analysisCfg model.AnalysisConfig) (string, error) {
			snap := configSnapshot()
			timeout := snap.Timeout
			if analysisCfg.TimeoutSeconds > 0 {
				timeout = time.Duration(analysisCfg.TimeoutSeconds) * time.Second
			}
			modelName := analysisCfg.Model
			if strings.TrimSpace(modelName) == "" {
				modelName = snap.Model
			}
			reasoning := analysisCfg.ReasoningEffort
			if strings.TrimSpace(reasoning) == "" {
				reasoning = snap.CodexReasoningEffort
			}
			runner := codex.New(codex.Options{
				CodexHome: snap.CodexHome, Model: modelName, ReasoningEffort: reasoning,
				BaseURL: codexBaseURLForDirectMode(snap), APIKey: codexAPIKeyForMode(snap),
				CCSwitchConfigDir: snap.CCSwitchConfigDir, CCSwitchBaseURL: snap.CodexBaseURL,
				UseCCSwitch:        snap.CodexAuthMode == config.AuthModeCCSwitch,
				CCSwitchProviderID: codexProviderForMode(snap), SandboxMode: snap.CodexSandbox,
				Timeout: timeout,
			})
			return runner.GenerateText(ctx, worktree, prompt)
		},
		Notifier: incidentNotifier,
	}
	incidentQ := incident.NewQueue(st, incidentProcessor, 2, logger)
	incidentHandler := &incident.Handler{Store: st, Wake: incidentQ.Notify}

	// Webhook -> enqueue (fast, then 200).
	onEvent := func(ctx context.Context, ev *model.WebhookEvent) error {
		if ev.Event == model.EventPullRequest && ev.Action == "synchronized" {
			if err := st.SupersedePending(ctx, ev.PR); err != nil {
				logger.Printf("supersede %s#%d: %v", ev.PR.Repo, ev.PR.Number, err)
			}
			q.CancelInFlight(ev.PR)
		}
		_, created, err := st.EnqueueJob(ctx, ev)
		if err != nil {
			return err
		}
		if created {
			q.Notify()
		}
		return nil
	}
	wh := webhook.NewHandlerWithSecretFunc(func() string {
		return configSnapshot().WebhookSecret
	}, onEvent)

	statusProvider := func() map[string]console.StatusFunc {
		return buildStatusFns(configSnapshot())
	}
	cons := console.New(st, cfg, cfg.CodexHome,
		console.ConfigProvider(configSnapshot),
		console.StatusProvider(statusProvider),
		console.SettingsApplyFunc(applySettings),
		console.SkillGeneratorFunc(func(ctx context.Context, in model.SkillGenerationInput) (string, error) {
			snap := configSnapshot()
			runner := codex.New(codex.Options{
				CodexHome:          snap.CodexHome,
				Model:              snap.Model,
				ReasoningEffort:    snap.CodexReasoningEffort,
				BaseURL:            codexBaseURLForDirectMode(snap),
				APIKey:             codexAPIKeyForMode(snap),
				CCSwitchConfigDir:  snap.CCSwitchConfigDir,
				CCSwitchBaseURL:    snap.CodexBaseURL,
				UseCCSwitch:        snap.CodexAuthMode == config.AuthModeCCSwitch,
				CCSwitchProviderID: codexProviderForMode(snap),
				SandboxMode:        snap.CodexSandbox,
				Timeout:            snap.Timeout,
			})
			return runner.GenerateText(ctx, os.TempDir(), console.BuildProjectSkillPrompt(in))
		}),
		console.ChatProbeFunc(func(ctx context.Context, in console.ChatProbeInput) (string, error) {
			return runChatProbe(ctx, configSnapshot(), in)
		}),
		console.AnalysisDependencies{
			Store: st, Control: incidentQ,
			Tests: console.AnalysisTestFuncs{
				SLS:    incidentLogs.Test,
				Feishu: incidentNotifier.Test,
				Repo: func(ctx context.Context, cfg model.AnalysisConfig) error {
					return incident.TestRepository(ctx, cache, cfg)
				},
			},
		},
	)

	root := http.NewServeMux()
	root.Handle("/webhook", wh)
	root.Handle("/hooks/alert-analysis/", incidentHandler)
	root.Handle("/healthz", wh)
	root.HandleFunc("/readyz", readyzHandler(st, configSnapshot))
	root.Handle("/admin/", cons.Routes())
	root.Handle("/skills/", cons.PublicRoutes())

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: root}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Printf("worker pool starting (concurrency=%d)", cfg.Concurrency)
		if err := q.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("queue stopped: %v", err)
		}
	}()

	go func() {
		logger.Printf("incident worker pool starting (concurrency=2)")
		if err := incidentQ.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("incident queue stopped: %v", err)
		}
	}()

	go func() {
		logger.Printf("incident git cache janitor starting")
		if err := cache.RunIncidentJanitor(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("incident git cache janitor stopped: %v", err)
		}
	}()

	go func() {
		logger.Printf("listening on %s (webhook=/webhook, alert-analysis=/hooks/alert-analysis/, console=/admin/)", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Printf("shutting down...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

func buildReviewers(cfg *config.Config) []model.Reviewer {
	if cfg == nil {
		return nil
	}
	codexOpts := codex.Options{
		CodexHome:          cfg.CodexHome,
		Model:              cfg.Model,
		ReasoningEffort:    cfg.CodexReasoningEffort,
		BaseURL:            codexBaseURLForDirectMode(cfg),
		APIKey:             codexAPIKeyForMode(cfg),
		CCSwitchConfigDir:  cfg.CCSwitchConfigDir,
		CCSwitchBaseURL:    cfg.CodexBaseURL,
		UseCCSwitch:        cfg.CodexAuthMode == config.AuthModeCCSwitch,
		CCSwitchProviderID: codexProviderForMode(cfg),
		SandboxMode:        cfg.CodexSandbox,
		Timeout:            cfg.Timeout,
	}
	reviewers := []model.Reviewer{codex.New(codexOpts)}
	if cfg.ClaudeEnabled {
		reviewers = append(reviewers, claude.New(claude.Options{
			Model:             cfg.ClaudeModel,
			APIKey:            cfg.ClaudeAPIKey,
			BaseURL:           cfg.ClaudeBaseURL,
			ClaudeHome:        cfg.ClaudeHome,
			CCSwitchConfigDir: cfg.CCSwitchConfigDir,
			CCSwitchProvider:  cfg.CCSwitchProvider,
			Timeout:           cfg.Timeout,
			MaxBudgetUSD:      cfg.ClaudeMaxBudgetUSD,
		}))
	}
	if cfg.MiniMaxEnabled {
		reviewers = append(reviewers, claude.New(claude.Options{
			Name:              "minimax",
			Model:             cfg.MiniMaxModel,
			APIKey:            cfg.MiniMaxAPIKey,
			BaseURL:           cfg.MiniMaxBaseURL,
			ClaudeHome:        cfg.ClaudeHome,
			CCSwitchConfigDir: cfg.CCSwitchConfigDir,
			CCSwitchProvider:  cfg.MiniMaxProvider,
			Timeout:           cfg.Timeout,
			MaxBudgetUSD:      cfg.MiniMaxMaxBudgetUSD,
		}))
	}
	return reviewers
}

func runChatProbe(ctx context.Context, cfg *config.Config, in console.ChatProbeInput) (string, error) {
	if cfg == nil {
		return "", errors.New("config is unavailable")
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return "", errors.New("prompt is required")
	}
	switch strings.ToLower(strings.TrimSpace(in.Reviewer)) {
	case "", "codex":
		modelName := firstNonEmpty(in.Model, cfg.Model)
		reasoning := firstNonEmpty(in.ReasoningEffort, cfg.CodexReasoningEffort)
		baseURL := ""
		if cfg.CodexAuthMode != config.AuthModeCCSwitch {
			baseURL = firstNonEmpty(in.BaseURL, cfg.CodexBaseURL)
		}
		providerID := firstNonEmpty(in.ProviderID, codexProviderForMode(cfg))
		runner := codex.New(codex.Options{
			CodexHome:          cfg.CodexHome,
			Model:              modelName,
			ReasoningEffort:    reasoning,
			BaseURL:            baseURL,
			APIKey:             codexAPIKeyForMode(cfg),
			CCSwitchConfigDir:  cfg.CCSwitchConfigDir,
			CCSwitchBaseURL:    firstNonEmpty(in.BaseURL, cfg.CodexBaseURL),
			UseCCSwitch:        cfg.CodexAuthMode == config.AuthModeCCSwitch,
			CCSwitchProviderID: providerID,
			SandboxMode:        config.SandboxReadOnly,
			Timeout:            cfg.Timeout,
		})
		return runner.GenerateText(ctx, os.TempDir(), prompt)
	case "claude":
		runner := claude.New(claude.Options{
			Model:             firstNonEmpty(in.Model, cfg.ClaudeModel),
			APIKey:            cfg.ClaudeAPIKey,
			BaseURL:           firstNonEmpty(in.BaseURL, cfg.ClaudeBaseURL),
			ClaudeHome:        cfg.ClaudeHome,
			CCSwitchConfigDir: cfg.CCSwitchConfigDir,
			CCSwitchProvider:  firstNonEmpty(in.ProviderID, cfg.CCSwitchProvider),
			Timeout:           cfg.Timeout,
			MaxBudgetUSD:      cfg.ClaudeMaxBudgetUSD,
		})
		return runner.GenerateText(ctx, os.TempDir(), prompt)
	case "minimax":
		runner := claude.New(claude.Options{
			Name:              "minimax",
			Model:             firstNonEmpty(in.Model, cfg.MiniMaxModel),
			APIKey:            cfg.MiniMaxAPIKey,
			BaseURL:           firstNonEmpty(in.BaseURL, cfg.MiniMaxBaseURL),
			ClaudeHome:        cfg.ClaudeHome,
			CCSwitchConfigDir: cfg.CCSwitchConfigDir,
			CCSwitchProvider:  firstNonEmpty(in.ProviderID, cfg.MiniMaxProvider),
			Timeout:           cfg.Timeout,
			MaxBudgetUSD:      cfg.MiniMaxMaxBudgetUSD,
		})
		return runner.GenerateText(ctx, os.TempDir(), prompt)
	default:
		return "", fmt.Errorf("unsupported reviewer: %s", in.Reviewer)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func codexAPIKeyForMode(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if cfg.CodexAuthMode == config.AuthModeAPIKey || cfg.CodexAuthMode == config.AuthModeCCSwitch {
		return cfg.CodexAPIKey
	}
	return ""
}

func codexBaseURLForDirectMode(cfg *config.Config) string {
	if cfg == nil || cfg.CodexAuthMode == config.AuthModeCCSwitch {
		return ""
	}
	return cfg.CodexBaseURL
}

func codexProviderForMode(cfg *config.Config) string {
	if cfg != nil && cfg.CodexAuthMode == config.AuthModeCCSwitch {
		return cfg.CodexCCSwitchProvider
	}
	return ""
}

func buildStatusFns(cfg *config.Config) map[string]console.StatusFunc {
	statusFns := map[string]console.StatusFunc{}
	for _, reviewer := range buildReviewers(cfg) {
		r := reviewer
		statusFns[r.Name()] = func(ctx context.Context) (string, error) { return r.Status(ctx) }
	}
	return statusFns
}

type readyzJobStats struct {
	Total            int `json:"total"`
	Running          int `json:"running"`
	Pending          int `json:"pending"`
	Failed           int `json:"failed"`
	Superseded       int `json:"superseded"`
	Canceled         int `json:"canceled"`
	RetryablePending int `json:"retryable_pending"`
}

type readyzFailure struct {
	ID         int64  `json:"id"`
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	Number     int    `json:"number"`
	ErrorType  string `json:"error_type"`
	Error      string `json:"error"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at"`
}

func readyzHandler(st *store.Store, configSnapshot func() *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		cfg := configSnapshot()
		warnings := []string(nil)
		if cfg == nil {
			warnings = append(warnings, "config is unavailable")
		} else {
			warnings = cfg.Warnings()
		}

		dbOK := true
		var stats model.JobStats
		var dbErr string
		if st == nil {
			dbOK = false
			dbErr = "store is unavailable"
		} else {
			var err error
			stats, err = st.JobStats(ctx)
			if err != nil {
				dbOK = false
				dbErr = err.Error()
			}
		}

		var latestFailure *readyzFailure
		if dbOK {
			if failed, err := st.LatestFailedJobView(ctx); err != nil {
				dbOK = false
				dbErr = err.Error()
			} else if failed != nil {
				latestFailure = &readyzFailure{
					ID: failed.ID, Owner: failed.PR.Owner, Repo: failed.PR.Repo, Number: failed.PR.Number,
					ErrorType: string(failed.ErrorType), Error: failed.Error,
					CreatedAt: failed.CreatedAt.Format(time.RFC3339),
				}
				if failed.FinishedAt != nil && !failed.FinishedAt.IsZero() {
					latestFailure.FinishedAt = failed.FinishedAt.Format(time.RFC3339)
				}
			}
		}

		ok := dbOK && len(warnings) == 0
		status := http.StatusOK
		if !ok {
			status = http.StatusServiceUnavailable
		}
		resp := map[string]any{
			"ok":              ok,
			"db_ok":           dbOK,
			"config_warnings": warnings,
			"jobs": readyzJobStats{
				Total: stats.TotalJobs, Running: stats.Running, Pending: stats.Pending,
				Failed: stats.Failed, Superseded: stats.Superseded, Canceled: stats.Canceled,
				RetryablePending: stats.RetryablePending,
			},
			"latest_failure": latestFailure,
		}
		if dbErr != "" {
			resp["db_error"] = dbErr
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
