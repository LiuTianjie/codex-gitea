package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/turning4th/codex-gitea/internal/gitcache"
	"github.com/turning4th/codex-gitea/internal/model"
	"os/exec"
	"strings"
	"testing"
)

func TestParseAnalysisResultKeepsAIAssessment(t *testing.T) {
	result, err := parseAnalysisResult(`{
		"summary":"核心接口回归",
		"confidence":"high",
		"assessed_severity":"HIGH",
		"severity_reason":"影响生产学习主链路",
		"impact_scope":["PROD 学生用户","练习提交接口"]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.AssessedSeverity != "high" || result.SeverityReason == "" || len(result.ImpactScope) != 2 {
		t.Fatalf("assessment = %+v", result)
	}
}

func TestParseAnalysisResultFallsBackWhenAssessmentMissing(t *testing.T) {
	result, err := parseAnalysisResult(`{"summary":"证据不足","confidence":"low"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.AssessedSeverity != "low" || result.SeverityReason == "" || len(result.ImpactScope) != 1 || len(result.EvidenceGaps) != 1 {
		t.Fatalf("fallback assessment = %+v", result)
	}
}

// Exercise the real cache across two analyses to prove that main is re-fetched,
// even when both the configuration and alert request a different revision.
func TestProcessorFetchesLatestMain(t *testing.T) {
	ctx := context.Background()
	s, cfg, _ := createIncidentTestStore(t)
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial")
	oldSHA := git("rev-parse", "HEAD")
	git("branch", "dev")
	cfg.RepositoryURL = repo
	cfg.RepositoryRef = "dev"
	if _, err := s.UpdateAnalysisConfig(ctx, &cfg); err != nil {
		t.Fatal(err)
	}
	policy := gitcache.DefaultIncidentCachePolicy()
	policy.MinFreeBytes = 0
	cache := gitcache.New(t.TempDir(), t.TempDir(), gitcache.WithIncidentCachePolicyFunc(func() gitcache.IncidentCachePolicy { return policy }))
	for i, deploymentSHA := range []string{"", oldSHA} {
		git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", fmt.Sprintf("advance main %d", i))
		wantSHA := git("rev-parse", "HEAD")
		task, _, err := s.EnqueueAnalysisTask(ctx, cfg, model.AlertEnvelope{DeploymentSHA: deploymentSHA}, fmt.Sprintf("delivery-%d", i), fmt.Sprintf("fingerprint-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		p := Processor{Store: s, Logs: processorTestLogs{}, Cache: cache, Analyze: func(ctx context.Context, worktree, prompt string, _ model.AnalysisConfig) (string, error) {
			out, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(out)); got != wantSHA {
				t.Fatalf("HEAD=%s want latest main %s", got, wantSHA)
			}
			if !strings.Contains(prompt, "当前检出 SHA："+wantSHA) {
				t.Fatal("prompt missing resolved SHA")
			}
			return `{"summary":"已分析","assessed_severity":"low","severity_reason":"单次异常","evidence_gaps":[]}`, nil
		}}
		resultJSON, err := p.Process(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		var result model.AnalysisResult
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.EvidenceGaps) != 0 {
			t.Fatalf("unexpected gaps: %v", result.EvidenceGaps)
		}
		events, err := s.ListAnalysisTaskEvents(ctx, task.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range events {
			if event.Phase == "repository_ready" {
				var data map[string]string
				if err := json.Unmarshal(event.Data, &data); err != nil {
					t.Fatal(err)
				}
				if data["revision"] != AnalysisRevision || data["resolved_sha"] != wantSHA {
					t.Fatalf("repository event=%v", data)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("missing repository evidence")
		}
	}
	// A stale/nonexistent legacy ref must not affect the connection test either.
	cfg.RepositoryRef = "nonexistent-legacy-ref"
	if err := TestRepository(ctx, cache, cfg); err != nil {
		t.Fatal(err)
	}
}

type processorTestLogs struct{}

func (processorTestLogs) Fetch(context.Context, model.AnalysisConfig, model.AlertEnvelope) ([]string, error) {
	return []string{"example log"}, nil
}
func (processorTestLogs) Test(context.Context, model.AnalysisConfig) error { return nil }
