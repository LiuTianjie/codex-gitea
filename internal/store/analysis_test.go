package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/turning4th/codex-gitea/internal/model"
)

func openAnalysisTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "analysis.db"), WithSecretKey("test-secret-key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func analysisTestConfig(token string) model.AnalysisConfig {
	sum := sha256.Sum256([]byte(token))
	return model.AnalysisConfig{
		Name: "serverx prod", Enabled: true, RepositoryURL: "https://gitea.example.com/serverx.git",
		RepositoryRef: "main", SLSEndpoint: "cn-beijing.log.aliyuncs.com",
		SLSProject: "project", SLSLogstore: "raw", SLSAccessKeyID: "ak-id",
		SLSAccessKeySecret: "ak-secret", FeishuWebhook: "https://open.feishu.cn/hook/secret",
		FeishuMentionMapping: "znc,Starslayerx | 张宁池 | ou_123",
		ThrottleEnabled:      true, ThrottleThreshold: 1, ThrottleCooldownSecs: 0,
		ThrottleFields:  "method,endpoint,error_code,error_message",
		IngestTokenHash: hex.EncodeToString(sum[:]),
	}
}

func TestAnalysisConfigSecretsAreEncryptedAndRedeliveriesAreIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openAnalysisTestStore(t)
	cfg, err := s.CreateAnalysisConfig(ctx, ptr(analysisTestConfig("token")))
	if err != nil {
		t.Fatal(err)
	}
	var storedID, storedSecret, storedWebhook string
	if err := s.db.QueryRow(`SELECT sls_access_key_id,sls_access_key_secret,feishu_webhook FROM alert_analysis_configs WHERE id=?`, cfg.ID).Scan(&storedID, &storedSecret, &storedWebhook); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"id": storedID, "secret": storedSecret, "webhook": storedWebhook} {
		if value == "ak-id" || value == "ak-secret" || value == "https://open.feishu.cn/hook/secret" {
			t.Fatalf("%s stored in plaintext", name)
		}
	}
	verified, err := s.VerifyAnalysisConfigToken(ctx, cfg.ID, "token")
	if err != nil || verified.SLSAccessKeySecret != "ak-secret" {
		t.Fatalf("verify/decrypt = %+v, %v", verified, err)
	}
	alert := model.AlertEnvelope{Environment: "PROD", Method: "POST", Endpoint: "/api/test", ErrorCode: "500", ErrorMessage: "boom"}
	task, created, err := s.EnqueueAnalysisTask(ctx, *cfg, alert, "delivery-1", "same")
	if err != nil || !created {
		t.Fatalf("enqueue = %+v created=%v err=%v", task, created, err)
	}
	again, created, err := s.EnqueueAnalysisTask(ctx, *cfg, alert, "delivery-1", "same")
	if err != nil || created || again.ID != task.ID {
		t.Fatalf("redelivery = %+v created=%v err=%v", again, created, err)
	}
	if err := s.SetAnalysisNotification(ctx, task.ID, "sent", "", "om_message"); err != nil {
		t.Fatal(err)
	}
	storedTask, err := s.GetAnalysisTask(ctx, task.ID)
	if err != nil || storedTask.FeishuMessageID != "om_message" {
		t.Fatalf("stored message id task=%+v err=%v", storedTask, err)
	}
	if storedTask.ConfigSnapshot.FeishuMentionMapping != cfg.FeishuMentionMapping {
		t.Fatalf("task mention mapping=%q, want %q", storedTask.ConfigSnapshot.FeishuMentionMapping, cfg.FeishuMentionMapping)
	}
}

func TestAnalysisConfigEncryptsFeishuAppSecret(t *testing.T) {
	ctx := context.Background()
	s := openAnalysisTestStore(t)
	cfg := analysisTestConfig("app-token")
	cfg.FeishuMode = "app"
	cfg.FeishuWebhook = ""
	cfg.FeishuAppID = "cli_test"
	cfg.FeishuAppSecret = "app-secret"
	cfg.FeishuChatID = "oc_test"
	created, err := s.CreateAnalysisConfig(ctx, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	var encrypted string
	if err := s.db.QueryRow(`SELECT feishu_app_secret FROM alert_analysis_configs WHERE id=?`, created.ID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if encrypted == "app-secret" || encrypted == "" {
		t.Fatalf("Feishu app secret was not encrypted: %q", encrypted)
	}
	if created.FeishuAppSecret != "app-secret" || created.FeishuMode != "app" || created.FeishuChatID != "oc_test" {
		t.Fatalf("created config=%+v", created)
	}
}

func TestAnalysisTaskClaimHonorsConcurrencyPerConfig(t *testing.T) {
	ctx := context.Background()
	s := openAnalysisTestStore(t)

	one := analysisTestConfig("one-token")
	one.Name = "one"
	one.Concurrency = 1
	one.ThrottleEnabled = false
	oneCreated, err := s.CreateAnalysisConfig(ctx, &one)
	if err != nil {
		t.Fatal(err)
	}
	two := analysisTestConfig("two-token")
	two.Name = "two"
	two.Concurrency = 2
	two.ThrottleEnabled = false
	twoCreated, err := s.CreateAnalysisConfig(ctx, &two)
	if err != nil {
		t.Fatal(err)
	}

	enqueue := func(cfg model.AnalysisConfig, delivery string) {
		t.Helper()
		if _, created, err := s.EnqueueAnalysisTask(ctx, cfg, model.AlertEnvelope{Endpoint: "/" + delivery}, delivery, delivery); err != nil || !created {
			t.Fatalf("enqueue %s: created=%v err=%v", delivery, created, err)
		}
	}
	enqueue(*oneCreated, "one-1")
	enqueue(*oneCreated, "one-2")
	enqueue(*twoCreated, "two-1")
	enqueue(*twoCreated, "two-2")

	first, err := s.ClaimAnalysisTask(ctx)
	if err != nil || first == nil || first.ConfigID == nil || *first.ConfigID != oneCreated.ID {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	second, err := s.ClaimAnalysisTask(ctx)
	if err != nil || second == nil || second.ConfigID == nil || *second.ConfigID != twoCreated.ID {
		t.Fatalf("second claim=%+v err=%v", second, err)
	}
	third, err := s.ClaimAnalysisTask(ctx)
	if err != nil || third == nil || third.ConfigID == nil || *third.ConfigID != twoCreated.ID {
		t.Fatalf("third claim=%+v err=%v", third, err)
	}
	blocked, err := s.ClaimAnalysisTask(ctx)
	if err != nil || blocked != nil {
		t.Fatalf("claim over per-config limits=%+v err=%v", blocked, err)
	}

	if err := s.FinishAnalysisTask(ctx, first.ID, model.AnalysisTaskSucceeded, `{}`, "", ""); err != nil {
		t.Fatal(err)
	}
	next, err := s.ClaimAnalysisTask(ctx)
	if err != nil || next == nil || next.ConfigID == nil || *next.ConfigID != oneCreated.ID {
		t.Fatalf("claim after slot released=%+v err=%v", next, err)
	}
}

func TestAnalysisConfigRejectsConcurrencyAboveLimit(t *testing.T) {
	s := openAnalysisTestStore(t)
	cfg := analysisTestConfig("too-many")
	cfg.Concurrency = model.MaxAnalysisConcurrency + 1
	if _, err := s.CreateAnalysisConfig(context.Background(), &cfg); err == nil {
		t.Fatal("expected concurrency validation error")
	}
}

func TestAnalysisThrottleAnalyzesOnceThenSuppressesDuplicates(t *testing.T) {
	ctx := context.Background()
	s := openAnalysisTestStore(t)
	cfg, err := s.CreateAnalysisConfig(ctx, ptr(analysisTestConfig("token")))
	if err != nil {
		t.Fatal(err)
	}
	alert := model.AlertEnvelope{Environment: "PROD", Endpoint: "/same", ErrorMessage: "same"}
	var firstID int64
	for i := 1; i <= 4; i++ {
		task, created, err := s.EnqueueAnalysisTask(ctx, *cfg, alert, fmt.Sprintf("delivery-%d", i), "same-fingerprint")
		if err != nil || !created {
			t.Fatalf("enqueue %d: created=%v err=%v", i, created, err)
		}
		want := model.AnalysisTaskQueued
		if i > 1 {
			want = model.AnalysisTaskSuppressed
		}
		if task.Status != want {
			t.Fatalf("task %d status=%s want=%s", i, task.Status, want)
		}
		if i == 1 {
			firstID = task.ID
		} else if task.DuplicateOfTaskID == nil || *task.DuplicateOfTaskID != firstID {
			t.Fatalf("task %d duplicate_of=%v want=%d", i, task.DuplicateOfTaskID, firstID)
		}
	}
}

func TestAnalysisThrottleResetsWhenFingerprintChanges(t *testing.T) {
	ctx := context.Background()
	s := openAnalysisTestStore(t)
	cfg, err := s.CreateAnalysisConfig(ctx, ptr(analysisTestConfig("token")))
	if err != nil {
		t.Fatal(err)
	}
	alert := model.AlertEnvelope{Environment: "PROD", Endpoint: "/same", ErrorMessage: "same"}
	for i := 1; i <= 3; i++ {
		if _, _, err := s.EnqueueAnalysisTask(ctx, *cfg, alert, fmt.Sprintf("same-%d", i), "same-fingerprint"); err != nil {
			t.Fatalf("enqueue same %d: %v", i, err)
		}
	}
	changed, _, err := s.EnqueueAnalysisTask(ctx, *cfg, alert, "changed", "different-fingerprint")
	if err != nil {
		t.Fatal(err)
	}
	if changed.Status != model.AnalysisTaskQueued {
		t.Fatalf("changed fingerprint status=%s, want queued", changed.Status)
	}
	again, _, err := s.EnqueueAnalysisTask(ctx, *cfg, alert, "same-again", "same-fingerprint")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != model.AnalysisTaskQueued {
		t.Fatalf("sequence after changed fingerprint status=%s, want queued", again.Status)
	}
}

func TestDeleteAnalysisConfigPreservesTaskSnapshot(t *testing.T) {
	ctx := context.Background()
	s := openAnalysisTestStore(t)
	cfg, err := s.CreateAnalysisConfig(ctx, ptr(analysisTestConfig("token")))
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := s.EnqueueAnalysisTask(ctx, *cfg, model.AlertEnvelope{AlertID: "a"}, "delivery", "fingerprint")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAnalysisConfig(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	preserved, err := s.GetAnalysisTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.ConfigID != nil || preserved.ConfigName != "serverx prod" || preserved.ConfigSnapshot.RepositoryRef != "main" {
		t.Fatalf("preserved task = %+v", preserved)
	}
}

func ptr[T any](value T) *T { return &value }
