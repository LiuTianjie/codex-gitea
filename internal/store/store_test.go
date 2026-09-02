package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpenMigratesExistingFindingsTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open old sqlite: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE findings(
		id INTEGER PRIMARY KEY, pull_id INTEGER REFERENCES pulls(id),
		fingerprint TEXT, path TEXT, line INTEGER, side TEXT, severity TEXT,
		gitea_comment_id INTEGER, review_id INTEGER,
		first_seen_sha TEXT, status TEXT, UNIQUE(pull_id,fingerprint))`); err != nil {
		t.Fatalf("create old findings table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old sqlite: %v", err)
	}

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open migrated store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 12}
	if err := st.UpsertPull(ctx, &model.PullState{PR: pr, HeadSHA: "sha0"}); err != nil {
		t.Fatalf("UpsertPull after migration: %v", err)
	}
	pull, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if err := st.SaveFindings(ctx, pull.ID, "sha1", []model.Finding{{
		Path: "a.go", Line: 10, Side: model.SideNew, Severity: model.SeverityHigh, Title: "bug A", Body: "x",
	}}, model.FindingSaveOptions{Agent: "codex"}); err != nil {
		t.Fatalf("SaveFindings after migration: %v", err)
	}
}

func sampleEvent(deliveryID string) *model.WebhookEvent {
	return &model.WebhookEvent{
		DeliveryID: deliveryID,
		Event:      model.EventPullRequest,
		Action:     "opened",
		PR:         model.PRRef{Owner: "acme", Repo: "widgets", Number: 7},
		BaseRef:    "main",
		HeadRef:    "feature",
		HeadSHA:    "abc123",
		CloneURL:   "https://git.example/acme/widgets.git",
		Raw:        []byte(`{"raw":true}`),
	}
}

func TestEnqueueJobDedup(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	job, created, err := st.EnqueueJob(ctx, sampleEvent("delivery-1"))
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	if !created {
		t.Fatalf("first enqueue: created=false, want true")
	}
	if job.ID == 0 {
		t.Fatalf("first enqueue: got zero job id")
	}
	if job.Status != model.JobPending {
		t.Fatalf("first enqueue: status=%q, want pending", job.Status)
	}
	if job.PR.Owner != "acme" || job.PR.Repo != "widgets" || job.PR.Number != 7 {
		t.Fatalf("first enqueue: PR roundtrip mismatch: %+v", job.PR)
	}
	if len(job.Payload) == 0 {
		t.Fatalf("first enqueue: payload not persisted")
	}

	// Same delivery id => dedup, created=false, no error, same job id.
	dup, created, err := st.EnqueueJob(ctx, sampleEvent("delivery-1"))
	if err != nil {
		t.Fatalf("EnqueueJob dup: %v", err)
	}
	if created {
		t.Fatalf("dup enqueue: created=true, want false")
	}
	if dup.ID != job.ID {
		t.Fatalf("dup enqueue: id=%d, want %d", dup.ID, job.ID)
	}
}

func TestClaimAndFinishJob(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, _, err := st.EnqueueJob(ctx, sampleEvent("d-a")); err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	ev2 := sampleEvent("d-b")
	ev2.PR.Number = 8
	if _, _, err := st.EnqueueJob(ctx, ev2); err != nil {
		t.Fatalf("enqueue b: %v", err)
	}

	// Claims oldest first (PR 7).
	claimed, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if claimed == nil {
		t.Fatalf("ClaimJob: got nil, want a job")
	}
	if claimed.PR.Number != 7 {
		t.Fatalf("ClaimJob: claimed PR %d, want oldest (7)", claimed.PR.Number)
	}
	if claimed.Status != model.JobRunning {
		t.Fatalf("ClaimJob: status=%q, want running", claimed.Status)
	}
	if claimed.Attempts != 1 {
		t.Fatalf("ClaimJob: attempts=%d, want 1", claimed.Attempts)
	}
	if claimed.StartedAt == nil {
		t.Fatalf("ClaimJob: started_at not set")
	}

	// Finish it.
	if err := st.FinishJob(ctx, claimed.ID, model.JobDone, ""); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}
	got, err := st.GetJob(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != model.JobDone {
		t.Fatalf("after finish: status=%q, want done", got.Status)
	}
	if got.FinishedAt == nil {
		t.Fatalf("after finish: finished_at not set")
	}

	// Next claim returns the second job (PR 8).
	claimed2, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob 2: %v", err)
	}
	if claimed2 == nil || claimed2.PR.Number != 8 {
		t.Fatalf("ClaimJob 2: got %+v, want PR 8", claimed2)
	}

	// No more pending => nil, nil.
	none, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob empty: %v", err)
	}
	if none != nil {
		t.Fatalf("ClaimJob empty: got %+v, want nil", none)
	}
}

func TestFinishJobDetailedSchedulesRetry(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	job, _, err := st.EnqueueJob(ctx, sampleEvent("d-retry"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := st.ClaimJob(ctx)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob: %v / %v", claimed, err)
	}
	next := time.Now().UTC().Add(time.Hour)
	if err := st.FinishJobDetailed(ctx, claimed.ID, model.JobFinish{
		Status: model.JobPending, Error: "gitea: status 503", ErrorType: model.ErrorTypeGitea,
		Retryable: true, NextAttemptAt: &next,
	}); err != nil {
		t.Fatalf("FinishJobDetailed: %v", err)
	}
	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != model.JobPending || got.ErrorType != model.ErrorTypeGitea || !got.Retryable || got.NextAttemptAt == nil {
		t.Fatalf("scheduled retry job = %+v", got)
	}
	none, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob delayed: %v", err)
	}
	if none != nil {
		t.Fatalf("delayed retry claimed too early: %+v", none)
	}
	stats, err := st.JobStats(ctx)
	if err != nil {
		t.Fatalf("JobStats delayed retry: %v", err)
	}
	if stats.RetryablePending != 1 {
		t.Fatalf("RetryablePending = %d, want 1", stats.RetryablePending)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE jobs SET next_attempt_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), job.ID); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	due, err := st.ClaimJob(ctx)
	if err != nil || due == nil || due.ID != job.ID {
		t.Fatalf("due retry claim = %+v err=%v", due, err)
	}
}

func TestCancelPendingJob(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	job, _, err := st.EnqueueJob(ctx, sampleEvent("d-cancel"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	canceled, err := st.CancelPendingJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("CancelPendingJob: %v", err)
	}
	if canceled.Status != model.JobCanceled {
		t.Fatalf("canceled status=%q, want canceled", canceled.Status)
	}
	if canceled.FinishedAt == nil {
		t.Fatalf("canceled finished_at not set")
	}
	next, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob after cancel: %v", err)
	}
	if next != nil {
		t.Fatalf("ClaimJob after cancel = %+v, want nil", next)
	}
	detail, err := st.GetJobView(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJobView: %v", err)
	}
	if detail.LogCount != 1 || len(detail.Logs) != 1 || detail.Logs[0].Stage != "admin" {
		t.Fatalf("cancel logs = count %d logs %+v, want admin cancel log", detail.LogCount, detail.Logs)
	}
	stats, err := st.JobStats(ctx)
	if err != nil {
		t.Fatalf("JobStats: %v", err)
	}
	if stats.Pending != 0 || stats.Canceled != 1 {
		t.Fatalf("JobStats after cancel = %+v, want pending=0 canceled=1", stats)
	}
}

func TestCancelPendingJobRejectsRunning(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	job, _, err := st.EnqueueJob(ctx, sampleEvent("d-cancel-running"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if claimed.ID != job.ID {
		t.Fatalf("claimed id=%d, want %d", claimed.ID, job.ID)
	}
	if _, err := st.CancelPendingJob(ctx, job.ID); !errors.Is(err, model.ErrJobNotPending) {
		t.Fatalf("CancelPendingJob running err=%v, want ErrJobNotPending", err)
	}
	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != model.JobRunning {
		t.Fatalf("running job status=%q, want running", got.Status)
	}
}

func TestFinishRunningJobDetailedDoesNotOverwriteSuperseded(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	job, _, err := st.EnqueueJob(ctx, sampleEvent("d-finish-running-superseded"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if claimed.ID != job.ID {
		t.Fatalf("claimed id=%d, want %d", claimed.ID, job.ID)
	}
	if err := st.SupersedePending(ctx, job.PR); err != nil {
		t.Fatalf("SupersedePending: %v", err)
	}
	updated, err := st.FinishRunningJobDetailed(ctx, job.ID, model.JobFinish{Status: model.JobDone})
	if err != nil {
		t.Fatalf("FinishRunningJobDetailed: %v", err)
	}
	if updated {
		t.Fatalf("FinishRunningJobDetailed updated superseded job")
	}
	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != model.JobSuperseded {
		t.Fatalf("status=%q, want superseded", got.Status)
	}
}

func TestFinishRunningJobDetailedDoesNotOverwriteCanceled(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	job, _, err := st.EnqueueJob(ctx, sampleEvent("d-finish-running-canceled"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.FinishJob(ctx, job.ID, model.JobCanceled, ""); err != nil {
		t.Fatalf("mark canceled: %v", err)
	}
	updated, err := st.FinishRunningJobDetailed(ctx, job.ID, model.JobFinish{Status: model.JobDone})
	if err != nil {
		t.Fatalf("FinishRunningJobDetailed: %v", err)
	}
	if updated {
		t.Fatalf("FinishRunningJobDetailed updated canceled job")
	}
	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != model.JobCanceled {
		t.Fatalf("status=%q, want canceled", got.Status)
	}
}

func TestRerunJobCreatesFreshPendingJob(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	job, _, err := st.EnqueueJob(ctx, sampleEvent("d-rerun"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.FinishJobDetailed(ctx, job.ID, model.JobFinish{
		Status: model.JobFailed, Error: "codex review: 503 No available channel",
		ErrorType: model.ErrorTypeUpstream, Retryable: true,
	}); err != nil {
		t.Fatalf("finish failed job: %v", err)
	}
	rerun, err := st.RerunJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("RerunJob: %v", err)
	}
	if rerun.ID == job.ID || rerun.Status != model.JobPending || rerun.Attempts != 0 {
		t.Fatalf("rerun job = %+v, original=%d", rerun, job.ID)
	}
	if rerun.DeliveryID == job.DeliveryID || rerun.PR != job.PR || string(rerun.Payload) != string(job.Payload) {
		t.Fatalf("rerun did not copy job payload/PR with synthetic delivery: %+v original=%+v", rerun, job)
	}
}

func TestClaimJobSkipsPendingSamePRWhenRunning(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, _, err := st.EnqueueJob(ctx, sampleEvent("d-same-1")); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if _, _, err := st.EnqueueJob(ctx, sampleEvent("d-same-2")); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}

	claimed, err := st.ClaimJob(ctx)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob: %v / %v", claimed, err)
	}
	next, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob next: %v", err)
	}
	if next != nil {
		t.Fatalf("claimed same PR while first still running: %+v", next)
	}
}

func TestClaimJobConcurrentSamePRClaimsOnce(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 16; i++ {
		if _, _, err := st.EnqueueJob(ctx, sampleEvent(fmt.Sprintf("d-concurrent-%d", i))); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	results := make(chan *model.Job, 16)
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := st.ClaimJob(ctx)
			if err != nil {
				errs <- err
				return
			}
			results <- job
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("ClaimJob concurrent: %v", err)
	}
	claimed := map[int64]bool{}
	for job := range results {
		if job == nil {
			continue
		}
		claimed[job.ID] = true
		if job.Status != model.JobRunning {
			t.Fatalf("claimed status=%q, want running", job.Status)
		}
	}
	if len(claimed) != 1 {
		t.Fatalf("concurrent same-PR claims = %v, want exactly one claimed job", claimed)
	}
}

func TestRecoverRunning(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, _, err := st.EnqueueJob(ctx, sampleEvent("d-recover")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := st.ClaimJob(ctx)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob: %v / %v", claimed, err)
	}
	if claimed.Status != model.JobRunning {
		t.Fatalf("precondition: status=%q, want running", claimed.Status)
	}

	if err := st.RecoverRunning(ctx); err != nil {
		t.Fatalf("RecoverRunning: %v", err)
	}
	got, err := st.GetJob(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != model.JobPending {
		t.Fatalf("after recover: status=%q, want pending", got.Status)
	}
	if got.StartedAt != nil {
		t.Fatalf("after recover: started_at should be cleared")
	}

	// Recovered job is claimable again.
	reclaimed, err := st.ClaimJob(ctx)
	if err != nil || reclaimed == nil {
		t.Fatalf("reclaim: %v / %v", reclaimed, err)
	}
	if reclaimed.ID != claimed.ID {
		t.Fatalf("reclaim: id=%d, want %d", reclaimed.ID, claimed.ID)
	}
}

func TestSupersedePending(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 7}
	if _, _, err := st.EnqueueJob(ctx, sampleEvent("d-super")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.SupersedePending(ctx, pr); err != nil {
		t.Fatalf("SupersedePending: %v", err)
	}
	// No pending jobs should remain.
	none, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if none != nil {
		t.Fatalf("after supersede: claimed %+v, want nil", none)
	}

	// Superseding an unknown repo is a no-op, not an error.
	if err := st.SupersedePending(ctx, model.PRRef{Owner: "ghost", Repo: "none", Number: 1}); err != nil {
		t.Fatalf("SupersedePending unknown: %v", err)
	}
}

func TestListJobs(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	job1, _, err := st.EnqueueJob(ctx, sampleEvent("d-list-1"))
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	ev := sampleEvent("d-list-2")
	ev.PR.Number = 9
	job2, _, err := st.EnqueueJob(ctx, ev)
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if err := st.AppendJobLog(ctx, job2.ID, "codex", "review started"); err != nil {
		t.Fatalf("AppendJobLog: %v", err)
	}
	if err := st.FinishJob(ctx, job2.ID, model.JobDone, ""); err != nil {
		t.Fatalf("FinishJob done: %v", err)
	}
	if err := st.FinishJob(ctx, job1.ID, model.JobFailed, "boom"); err != nil {
		t.Fatalf("FinishJob failed: %v", err)
	}

	views, err := st.ListJobs(ctx, 10, 0)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("ListJobs: got %d, want 2", len(views))
	}
	// Newest first.
	if views[0].PR.Number != 9 {
		t.Fatalf("ListJobs order: first PR %d, want 9", views[0].PR.Number)
	}
	if views[0].PR.Owner != "acme" || views[0].PR.Repo != "widgets" {
		t.Fatalf("ListJobs: repo join mismatch: %+v", views[0].PR)
	}
	if views[0].LogCount != 1 || len(views[0].Logs) != 0 {
		t.Fatalf("ListJobs log summary = count %d logs %+v, want count-only summary", views[0].LogCount, views[0].Logs)
	}
	detail, err := st.GetJobView(ctx, job2.ID)
	if err != nil {
		t.Fatalf("GetJobView: %v", err)
	}
	if detail.LogCount != 1 || len(detail.Logs) != 1 || detail.Logs[0].Stage != "codex" || detail.Logs[0].Message != "review started" {
		t.Fatalf("GetJobView logs = count %d %+v, want codex/review started", detail.LogCount, detail.Logs)
	}
	paged, err := st.ListJobs(ctx, 1, 1)
	if err != nil {
		t.Fatalf("ListJobs page 2: %v", err)
	}
	if len(paged) != 1 || paged[0].PR.Number != 7 {
		t.Fatalf("ListJobs page 2 = %+v, want PR 7", paged)
	}
	total, err := st.CountJobs(ctx)
	if err != nil {
		t.Fatalf("CountJobs: %v", err)
	}
	if total != 2 {
		t.Fatalf("CountJobs = %d, want 2", total)
	}
	stats, err := st.JobStats(ctx)
	if err != nil {
		t.Fatalf("JobStats: %v", err)
	}
	if stats.TotalJobs != 2 || stats.ReviewJobs != 2 || stats.ReviewedJobs != 1 || stats.Done != 1 || stats.Failed != 1 {
		t.Fatalf("JobStats = %+v, want total/review/reviewed/done/failed = 2/2/1/1/1", stats)
	}

	oldTime := time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)
	if _, err := st.db.ExecContext(ctx, `UPDATE jobs SET created_at=? WHERE id=?`, oldTime.Format(time.RFC3339), job1.ID); err != nil {
		t.Fatalf("set old created_at: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE jobs SET created_at=? WHERE id=?`, newTime.Format(time.RFC3339), job2.ID); err != nil {
		t.Fatalf("set new created_at: %v", err)
	}
	filtered, err := st.ListJobsFiltered(ctx, model.JobFilter{CreatedFrom: &newTime}, 10, 0)
	if err != nil {
		t.Fatalf("ListJobsFiltered: %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != job2.ID {
		t.Fatalf("ListJobsFiltered = %+v, want only job2", filtered)
	}
	filteredTotal, err := st.CountJobsFiltered(ctx, model.JobFilter{CreatedFrom: &newTime})
	if err != nil {
		t.Fatalf("CountJobsFiltered: %v", err)
	}
	if filteredTotal != 1 {
		t.Fatalf("CountJobsFiltered = %d, want 1", filteredTotal)
	}
	filteredStats, err := st.JobStatsFiltered(ctx, model.JobFilter{CreatedFrom: &newTime})
	if err != nil {
		t.Fatalf("JobStatsFiltered: %v", err)
	}
	if filteredStats.TotalJobs != 1 || filteredStats.Done != 1 || filteredStats.Failed != 0 {
		t.Fatalf("JobStatsFiltered = %+v, want total/done/failed = 1/1/0", filteredStats)
	}
}

func TestListJobsTruncatesLargeErrorsButDetailKeepsFullError(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	job, _, err := st.EnqueueJob(ctx, sampleEvent("d-large-error"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	largeErr := strings.Repeat("x", 5000)
	if err := st.FinishJobDetailed(ctx, job.ID, model.JobFinish{
		Status: model.JobFailed, Error: largeErr, ErrorType: model.ErrorTypeReviewer,
	}); err != nil {
		t.Fatalf("finish job: %v", err)
	}
	list, err := st.ListJobs(ctx, 20, 0)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListJobs got %d rows, want 1", len(list))
	}
	if len(list[0].Error) >= len(largeErr) || !strings.Contains(list[0].Error, "truncated") {
		t.Fatalf("list error was not truncated: len=%d", len(list[0].Error))
	}
	detail, err := st.GetJobView(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJobView: %v", err)
	}
	if detail.Error != largeErr {
		t.Fatalf("detail error len=%d, want full len=%d", len(detail.Error), len(largeErr))
	}
}

func TestListJobsFilteredDateRangeBoundaries(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	before, _, err := st.EnqueueJob(ctx, sampleEvent("d-range-before"))
	if err != nil {
		t.Fatalf("enqueue before: %v", err)
	}
	insideMorningEvent := sampleEvent("d-range-morning")
	insideMorningEvent.PR.Number = 8
	insideMorning, _, err := st.EnqueueJob(ctx, insideMorningEvent)
	if err != nil {
		t.Fatalf("enqueue inside morning: %v", err)
	}
	insideNightEvent := sampleEvent("d-range-night")
	insideNightEvent.PR.Number = 9
	insideNight, _, err := st.EnqueueJob(ctx, insideNightEvent)
	if err != nil {
		t.Fatalf("enqueue inside night: %v", err)
	}
	afterEvent := sampleEvent("d-range-after")
	afterEvent.PR.Number = 10
	after, _, err := st.EnqueueJob(ctx, afterEvent)
	if err != nil {
		t.Fatalf("enqueue after: %v", err)
	}

	times := map[int64]time.Time{
		before.ID:        time.Date(2026, 6, 4, 23, 59, 59, 0, time.UTC),
		insideMorning.ID: time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC),
		insideNight.ID:   time.Date(2026, 6, 5, 23, 59, 59, 0, time.UTC),
		after.ID:         time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC),
	}
	for id, createdAt := range times {
		if _, err := st.db.ExecContext(ctx, `UPDATE jobs SET created_at=? WHERE id=?`, createdAt.Format(time.RFC3339), id); err != nil {
			t.Fatalf("set created_at for job %d: %v", id, err)
		}
	}

	from := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 5, 23, 59, 59, 0, time.UTC)
	filtered, err := st.ListJobsFiltered(ctx, model.JobFilter{CreatedFrom: &from, CreatedTo: &to}, 10, 0)
	if err != nil {
		t.Fatalf("ListJobsFiltered: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("ListJobsFiltered got %d jobs, want 2: %+v", len(filtered), filtered)
	}
	if filtered[0].ID != insideNight.ID || filtered[1].ID != insideMorning.ID {
		t.Fatalf("ListJobsFiltered order/contents = [%d,%d], want [%d,%d]", filtered[0].ID, filtered[1].ID, insideNight.ID, insideMorning.ID)
	}
	total, err := st.CountJobsFiltered(ctx, model.JobFilter{CreatedFrom: &from, CreatedTo: &to})
	if err != nil {
		t.Fatalf("CountJobsFiltered: %v", err)
	}
	if total != 2 {
		t.Fatalf("CountJobsFiltered = %d, want 2", total)
	}
	stats, err := st.JobStatsFiltered(ctx, model.JobFilter{CreatedFrom: &from, CreatedTo: &to})
	if err != nil {
		t.Fatalf("JobStatsFiltered: %v", err)
	}
	if stats.TotalJobs != 2 {
		t.Fatalf("JobStatsFiltered total = %d, want 2", stats.TotalJobs)
	}
}

func TestListJobsLargeHistorySummarizesCurrentPage(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	insertJobsForReadTests(t, st, 1200)

	views, err := st.ListJobs(ctx, 20, 0)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(views) != 20 {
		t.Fatalf("ListJobs got %d rows, want one page of 20", len(views))
	}
	if views[0].LogCount != 3 {
		t.Fatalf("latest job log_count = %d, want 3", views[0].LogCount)
	}
	for _, v := range views {
		if len(v.Logs) != 0 {
			t.Fatalf("list row %d included log bodies: %+v", v.ID, v.Logs)
		}
	}

	stats, err := st.JobStats(ctx)
	if err != nil {
		t.Fatalf("JobStats: %v", err)
	}
	if stats.TotalJobs != 1200 || stats.ReviewJobs != 1200 {
		t.Fatalf("JobStats totals = %+v, want 1200 review jobs", stats)
	}
}

func TestConsoleJobReadsDuringConcurrentLogWrites(t *testing.T) {
	st := newTestStore(t)
	insertJobsForReadTests(t, st, 200)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := st.AppendJobLog(ctx, int64(1+worker), "worker", fmt.Sprintf("log %d", j)); err != nil {
					t.Errorf("AppendJobLog: %v", err)
					return
				}
			}
		}(i)
	}

	for i := 0; i < 25; i++ {
		if _, err := st.ListJobs(ctx, 20, 0); err != nil {
			t.Fatalf("ListJobs while writing: %v", err)
		}
		if _, err := st.JobStats(ctx); err != nil {
			t.Fatalf("JobStats while writing: %v", err)
		}
	}
	wg.Wait()
}

func insertJobsForReadTests(t *testing.T, st *Store, count int) {
	t.Helper()
	ctx := context.Background()
	repoID, err := ensureRepo(ctx, st.db, "acme", "widgets")
	if err != nil {
		t.Fatalf("ensure repo: %v", err)
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	defer tx.Rollback()

	base := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		status := model.JobPending
		if i%3 == 0 {
			status = model.JobDone
		} else if i%5 == 0 {
			status = model.JobFailed
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO jobs(delivery_id,repo_id,pr_number,event,action,payload,status,attempts,created_at)
			 VALUES(?,?,?,?,?,?,?,0,?)`,
			fmt.Sprintf("seed-%d", i), repoID, i+1, string(model.EventPullRequest), "opened",
			[]byte(`{}`), string(status), base.Add(time.Duration(i)*time.Second).Format(time.RFC3339))
		if err != nil {
			t.Fatalf("insert job %d: %v", i, err)
		}
		if i >= count-20 {
			jobID, err := res.LastInsertId()
			if err != nil {
				t.Fatalf("job id %d: %v", i, err)
			}
			for n := 0; n < 3; n++ {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO job_logs(job_id,stage,message,created_at) VALUES(?,?,?,?)`,
					jobID, "seed", fmt.Sprintf("log %d", n), base.Add(time.Duration(i)*time.Second).Format(time.RFC3339)); err != nil {
					t.Fatalf("insert log %d/%d: %v", i, n, err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
}

func BenchmarkConsoleJobReadsLargeHistory(b *testing.B) {
	st := newBenchmarkStore(b)
	insertJobsForBenchmark(b, st, 5000)
	ctx := context.Background()

	b.Run("list_page", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := st.ListJobs(ctx, 20, 0); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("stats", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := st.JobStats(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func newBenchmarkStore(b *testing.B) *Store {
	b.Helper()
	st, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { st.Close() })
	return st
}

func insertJobsForBenchmark(b *testing.B, st *Store, count int) {
	b.Helper()
	ctx := context.Background()
	repoID, err := ensureRepo(ctx, st.db, "acme", "widgets")
	if err != nil {
		b.Fatalf("ensure repo: %v", err)
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatalf("begin seed tx: %v", err)
	}
	defer tx.Rollback()

	base := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		status := model.JobPending
		if i%3 == 0 {
			status = model.JobDone
		} else if i%5 == 0 {
			status = model.JobFailed
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO jobs(delivery_id,repo_id,pr_number,event,action,payload,status,attempts,created_at)
			 VALUES(?,?,?,?,?,?,?,0,?)`,
			fmt.Sprintf("bench-%d", i), repoID, i+1, string(model.EventPullRequest), "opened",
			[]byte(`{}`), string(status), base.Add(time.Duration(i)*time.Second).Format(time.RFC3339))
		if err != nil {
			b.Fatalf("insert job %d: %v", i, err)
		}
		if i >= count-20 {
			jobID, err := res.LastInsertId()
			if err != nil {
				b.Fatalf("job id %d: %v", i, err)
			}
			for n := 0; n < 3; n++ {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO job_logs(job_id,stage,message,created_at) VALUES(?,?,?,?)`,
					jobID, "seed", fmt.Sprintf("log %d", n), base.Add(time.Duration(i)*time.Second).Format(time.RFC3339)); err != nil {
					b.Fatalf("insert log %d/%d: %v", i, n, err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit seed tx: %v", err)
	}
}

func TestUpsertAndGetPull(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 42}

	// Missing pull => nil, nil.
	missing, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("GetPull missing: got %+v, want nil", missing)
	}

	in := &model.PullState{
		PR:           pr,
		Author:       "dev-a",
		SessionID:    "sess-1",
		HeadSHA:      "deadbeef",
		BaseRef:      "main",
		LastReviewID: 100,
	}
	if err := st.UpsertPull(ctx, in); err != nil {
		t.Fatalf("UpsertPull: %v", err)
	}

	got, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if got == nil {
		t.Fatalf("GetPull: got nil after upsert")
	}
	if got.Author != "dev-a" || got.SessionID != "sess-1" || got.HeadSHA != "deadbeef" ||
		got.BaseRef != "main" || got.LastReviewID != 100 {
		t.Fatalf("GetPull roundtrip mismatch: %+v", got)
	}
	if got.PR != pr {
		t.Fatalf("GetPull PR mismatch: %+v", got.PR)
	}
	if got.ID == 0 {
		t.Fatalf("GetPull: id not populated")
	}

	// Update overwrites fields.
	in.SessionID = "sess-2"
	in.Author = ""
	in.LastReviewID = 200
	if err := st.UpsertPull(ctx, in); err != nil {
		t.Fatalf("UpsertPull update: %v", err)
	}
	got2, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull 2: %v", err)
	}
	if got2.Author != "dev-a" || got2.SessionID != "sess-2" || got2.LastReviewID != 200 {
		t.Fatalf("GetPull after update mismatch: %+v", got2)
	}
	if got2.ID != got.ID {
		t.Fatalf("GetPull after update: id changed %d -> %d", got.ID, got2.ID)
	}
}

func TestSetSessionAndLastReview(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	pr := model.PRRef{Owner: "acme", Repo: "gizmos", Number: 3}

	// SetSession on a fresh PR auto-creates the pull row.
	if err := st.SetSession(ctx, pr, "thread-xyz"); err != nil {
		t.Fatalf("SetSession: %v", err)
	}
	got, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if got == nil || got.SessionID != "thread-xyz" {
		t.Fatalf("SetSession roundtrip: %+v", got)
	}

	if err := st.SetLastReview(ctx, pr, 555); err != nil {
		t.Fatalf("SetLastReview: %v", err)
	}
	got2, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull 2: %v", err)
	}
	if got2.LastReviewID != 555 {
		t.Fatalf("SetLastReview: last_review_id=%d, want 555", got2.LastReviewID)
	}
	// Session should survive setting the review.
	if got2.SessionID != "thread-xyz" {
		t.Fatalf("SetLastReview clobbered session: %q", got2.SessionID)
	}
}

func TestSaveAndListFindings(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 11}
	if err := st.UpsertPull(ctx, &model.PullState{PR: pr, Author: "dev-a", HeadSHA: "sha0"}); err != nil {
		t.Fatalf("UpsertPull: %v", err)
	}
	pull, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}

	fs := []model.Finding{
		{Path: "a.go", Line: 10, Side: model.SideNew, Severity: model.SeverityHigh, Title: "bug A", Body: "x"},
		{Path: "b.go", Line: 20, Side: model.SideNew, Severity: model.SeverityLow, Title: "nit B", Body: "y"},
	}
	if err := st.SaveFindings(ctx, pull.ID, "sha1", fs, model.FindingSaveOptions{Agent: "codex"}); err != nil {
		t.Fatalf("SaveFindings: %v", err)
	}

	got, err := st.ListFindings(ctx, pull.ID)
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListFindings: got %d, want 2", len(got))
	}
	for _, sf := range got {
		if sf.Status != "open" {
			t.Fatalf("finding %q status=%q, want open", sf.Path, sf.Status)
		}
		if sf.FirstSeenSHA != "sha1" {
			t.Fatalf("finding %q first_seen_sha=%q, want sha1", sf.Path, sf.FirstSeenSHA)
		}
		if sf.Fingerprint == "" {
			t.Fatalf("finding %q has empty fingerprint", sf.Path)
		}
		if sf.Title == "" || sf.Body == "" {
			t.Fatalf("finding %q did not persist title/body: %+v", sf.Path, sf)
		}
	}

	if err := st.MarkFindingsFixed(ctx, pull.ID, []string{got[0].Fingerprint}); err != nil {
		t.Fatalf("MarkFindingsFixed: %v", err)
	}
	fixed, err := st.ListFindings(ctx, pull.ID)
	if err != nil {
		t.Fatalf("ListFindings fixed: %v", err)
	}
	if fixed[0].Status != "fixed" {
		t.Fatalf("MarkFindingsFixed status=%q, want fixed", fixed[0].Status)
	}

	// Re-save the same findings with a new head SHA: upsert by fingerprint,
	// no duplicate rows, first_seen_sha preserved, and still-current findings reopen.
	if err := st.SaveFindings(ctx, pull.ID, "sha2", fs, model.FindingSaveOptions{Agent: "codex"}); err != nil {
		t.Fatalf("SaveFindings resave: %v", err)
	}
	got2, err := st.ListFindings(ctx, pull.ID)
	if err != nil {
		t.Fatalf("ListFindings 2: %v", err)
	}
	if len(got2) != 2 {
		t.Fatalf("ListFindings after resave: got %d, want 2 (dedup by fingerprint)", len(got2))
	}
	if got2[0].FirstSeenSHA != "sha1" {
		t.Fatalf("resave clobbered first_seen_sha: %q", got2[0].FirstSeenSHA)
	}
	if got2[0].Status != "open" {
		t.Fatalf("resave did not reopen current finding: %q", got2[0].Status)
	}

	// MarkFindingPosted records comment + review ids.
	if err := st.MarkFindingPosted(ctx, got2[0].ID, 999, 888); err != nil {
		t.Fatalf("MarkFindingPosted: %v", err)
	}
	got3, err := st.ListFindings(ctx, pull.ID)
	if err != nil {
		t.Fatalf("ListFindings 3: %v", err)
	}
	if got3[0].GiteaCommentID != 999 || got3[0].ReviewID != 888 {
		t.Fatalf("MarkFindingPosted roundtrip: comment=%d review=%d",
			got3[0].GiteaCommentID, got3[0].ReviewID)
	}
}

func TestAnalysisReports(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 21}
	if err := st.UpsertPull(ctx, &model.PullState{PR: pr, Author: "dev-a", HeadSHA: "sha0"}); err != nil {
		t.Fatalf("UpsertPull: %v", err)
	}
	pull, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	run := &model.ReviewRun{PullID: pull.ID, Agent: "codex", HeadSHA: "sha1"}
	if err := st.CreateReviewRun(ctx, run); err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}
	fs := []model.Finding{{
		Path: "a.go", Line: 10, Side: model.SideNew,
		Severity: model.SeverityHigh, Title: "bug A", Body: "x", Tags: []string{"runtime", "panic"},
	}}
	mapped := map[string]bool{"codex:" + fs[0].Fingerprint(): true}
	if err := st.SaveFindings(ctx, pull.ID, "sha1", fs, model.FindingSaveOptions{
		Agent: "codex", ReviewRunID: run.ID, MappedFingerprints: mapped,
	}); err != nil {
		t.Fatalf("SaveFindings: %v", err)
	}
	if err := st.FinishReviewRun(ctx, run.ID, model.ReviewRunDone, "", len(fs)); err != nil {
		t.Fatalf("FinishReviewRun: %v", err)
	}
	summary, err := st.BuildAnalysisSummary(ctx)
	if err != nil {
		t.Fatalf("BuildAnalysisSummary: %v", err)
	}
	if summary.TotalFindings != 1 || summary.HighCriticalOpen != 1 || summary.ByAgent["codex"].Findings != 1 {
		t.Fatalf("summary mismatch: %+v", summary)
	}
	if len(summary.RecentSevere) != 1 || summary.RecentSevere[0].Owner != "acme" ||
		summary.RecentSevere[0].Repo != "widgets" || summary.RecentSevere[0].PullNumber != 21 {
		t.Fatalf("recent severe repo metadata mismatch: %+v", summary.RecentSevere)
	}
	if len(summary.ByDeveloper) != 1 {
		t.Fatalf("ByDeveloper len = %d, want 1: %+v", len(summary.ByDeveloper), summary.ByDeveloper)
	}
	dev := summary.ByDeveloper[0]
	if dev.Developer != "dev-a" || dev.PullRequests != 1 || dev.ReviewRuns != 1 ||
		dev.SuccessfulReviewRuns != 1 || dev.Findings != 1 || dev.OpenFindings != 1 || dev.HighCriticalOpen != 1 {
		t.Fatalf("ByDeveloper mismatch: %+v", dev)
	}
	hasRuntime := false
	for _, tag := range summary.TopTags {
		if tag.Tag == "runtime" && tag.Count == 1 {
			hasRuntime = true
		}
	}
	if !hasRuntime {
		t.Fatalf("top tags mismatch: %+v", summary.TopTags)
	}
	report, err := st.CreateAnalysisReport(ctx, summary)
	if err != nil {
		t.Fatalf("CreateAnalysisReport: %v", err)
	}
	latest, err := st.LatestAnalysisReport(ctx)
	if err != nil {
		t.Fatalf("LatestAnalysisReport: %v", err)
	}
	if latest == nil || latest.ID != report.ID || latest.Summary.TotalFindings != 1 {
		t.Fatalf("latest mismatch: %+v", latest)
	}
	list, err := st.ListAnalysisReports(ctx, 10)
	if err != nil {
		t.Fatalf("ListAnalysisReports: %v", err)
	}
	if len(list) != 1 || list[0].ID != report.ID {
		t.Fatalf("list mismatch: %+v", list)
	}
}

func TestBuildAnalysisTrendBucketsReviewRunDataByDay(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 21}
	if err := st.UpsertPull(ctx, &model.PullState{PR: pr, Author: "dev-a", HeadSHA: "sha0"}); err != nil {
		t.Fatalf("UpsertPull: %v", err)
	}
	pull, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}

	run1 := &model.ReviewRun{PullID: pull.ID, Agent: "codex", HeadSHA: "sha1"}
	if err := st.CreateReviewRun(ctx, run1); err != nil {
		t.Fatalf("CreateReviewRun run1: %v", err)
	}
	finding := model.Finding{
		Path: "a.go", Line: 10, Side: model.SideNew,
		Severity: model.SeverityHigh, Title: "bug A", Body: "x",
	}
	if err := st.SaveFindings(ctx, pull.ID, "sha1", []model.Finding{finding}, model.FindingSaveOptions{
		Agent: "codex", ReviewRunID: run1.ID,
	}); err != nil {
		t.Fatalf("SaveFindings run1: %v", err)
	}
	if err := st.FinishReviewRun(ctx, run1.ID, model.ReviewRunDone, "", 1); err != nil {
		t.Fatalf("FinishReviewRun run1: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE review_runs SET finished_at='2026-06-17T01:00:00Z' WHERE id=?`, run1.ID); err != nil {
		t.Fatalf("backdate run1: %v", err)
	}

	run2 := &model.ReviewRun{PullID: pull.ID, Agent: "minimax", HeadSHA: "sha2"}
	if err := st.CreateReviewRun(ctx, run2); err != nil {
		t.Fatalf("CreateReviewRun run2: %v", err)
	}
	if err := st.FinishReviewRun(ctx, run2.ID, model.ReviewRunFailed, "timeout", 0); err != nil {
		t.Fatalf("FinishReviewRun run2: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE review_runs SET finished_at='2026-06-17T02:00:00Z' WHERE id=?`, run2.ID); err != nil {
		t.Fatalf("backdate run2: %v", err)
	}

	run3 := &model.ReviewRun{PullID: pull.ID, Agent: "codex", HeadSHA: "sha3"}
	if err := st.CreateReviewRun(ctx, run3); err != nil {
		t.Fatalf("CreateReviewRun run3: %v", err)
	}
	if err := st.FinishReviewRun(ctx, run3.ID, model.ReviewRunDone, "", 0); err != nil {
		t.Fatalf("FinishReviewRun run3: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE review_runs SET finished_at='2026-06-18T01:00:00Z' WHERE id=?`, run3.ID); err != nil {
		t.Fatalf("backdate run3: %v", err)
	}

	run4 := &model.ReviewRun{PullID: pull.ID, Agent: "codex", HeadSHA: "sha4"}
	if err := st.CreateReviewRun(ctx, run4); err != nil {
		t.Fatalf("CreateReviewRun run4: %v", err)
	}
	if err := st.FinishReviewRun(ctx, run4.ID, model.ReviewRunDone, "", 0); err != nil {
		t.Fatalf("FinishReviewRun run4: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE review_runs SET finished_at='2026-07-02T01:00:00Z' WHERE id=?`, run4.ID); err != nil {
		t.Fatalf("backdate run4: %v", err)
	}

	points, err := st.BuildAnalysisTrend(ctx, 12, "day")
	if err != nil {
		t.Fatalf("BuildAnalysisTrend: %v", err)
	}
	if len(points) != 3 {
		t.Fatalf("trend len = %d, want 3: %+v", len(points), points)
	}
	if points[0].Bucket != "2026-06-17" || points[0].Interval != "day" ||
		points[0].TotalReviewRuns != 2 || points[0].SuccessfulReviewRuns != 1 ||
		points[0].FailedReviewRuns != 1 || points[0].SuccessRate != 0.5 ||
		points[0].TotalFindings != 1 || points[0].OpenFindings != 1 || points[0].HighCriticalOpen != 1 {
		t.Fatalf("first daily trend point mismatch: %+v", points[0])
	}
	if points[1].Bucket != "2026-06-18" || points[1].TotalReviewRuns != 1 ||
		points[1].SuccessfulReviewRuns != 1 || points[1].TotalFindings != 0 {
		t.Fatalf("second daily trend point mismatch: %+v", points[1])
	}
	if points[2].Bucket != "2026-07-02" || points[2].TotalReviewRuns != 1 {
		t.Fatalf("third daily trend point mismatch: %+v", points[2])
	}

	weekly, err := st.BuildAnalysisTrend(ctx, 12, "week")
	if err != nil {
		t.Fatalf("BuildAnalysisTrend week: %v", err)
	}
	if len(weekly) != 2 {
		t.Fatalf("weekly len = %d, want 2: %+v", len(weekly), weekly)
	}
	if weekly[0].Bucket != "2026-06-15" || weekly[0].Interval != "week" ||
		weekly[0].TotalReviewRuns != 3 || weekly[0].SuccessfulReviewRuns != 2 ||
		weekly[0].FailedReviewRuns != 1 || weekly[0].TotalFindings != 1 {
		t.Fatalf("weekly trend point mismatch: %+v", weekly[0])
	}
	if weekly[1].Bucket != "2026-06-29" || weekly[1].TotalReviewRuns != 1 {
		t.Fatalf("second weekly trend point mismatch: %+v", weekly[1])
	}

	monthly, err := st.BuildAnalysisTrend(ctx, 12, "month")
	if err != nil {
		t.Fatalf("BuildAnalysisTrend month: %v", err)
	}
	if len(monthly) != 2 {
		t.Fatalf("monthly len = %d, want 2: %+v", len(monthly), monthly)
	}
	if monthly[0].Bucket != "2026-06" || monthly[0].Interval != "month" ||
		monthly[0].TotalReviewRuns != 3 || monthly[0].TotalFindings != 1 {
		t.Fatalf("monthly trend point mismatch: %+v", monthly[0])
	}
	if monthly[1].Bucket != "2026-07" || monthly[1].TotalReviewRuns != 1 {
		t.Fatalf("second monthly trend point mismatch: %+v", monthly[1])
	}
}

func TestBuildAnalysisSummaryIncludesHistoricalAlertInsights(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	insert := func(delivery, fingerprint, status, alert, result, created string) {
		t.Helper()
		_, err := st.db.ExecContext(ctx, `INSERT INTO analysis_tasks(
			config_id,config_version,config_name,delivery_id,fingerprint,alert_payload,config_snapshot,
			status,phase,result_json,created_at,finished_at)
			VALUES(NULL,1,'serverx prod',?,?,?,?,?,'done',?,?,?)`,
			delivery, fingerprint, alert, `{}`, status, result, created, created)
		if err != nil {
			t.Fatalf("insert alert task %s: %v", delivery, err)
		}
	}
	alert := `{"environment":"PROD","service":"serverx","title":"订单接口异常","method":"POST","endpoint":"/api/orders","error_code":"500","severity":"critical"}`
	result := `{"classification":"code_regression","confidence":"high","assessed_severity":"high","summary":"订单接口发生回归","hypotheses":["最近改动影响了下单校验"],"evidence_gaps":["未提供 deployment_sha"],"recommended_actions":["核对最近订单相关提交与部署版本"]}`
	insert("alert-1", "orders-500", "succeeded", alert, result, "2026-06-19T01:00:00Z")
	insert("alert-2", "orders-500", "suppressed", alert, ``, "2026-06-19T01:01:00Z")
	insert("alert-4", "orders-500", "succeeded", alert, result, "2026-06-19T01:02:00Z")
	insert("alert-3", "cache-timeout", "failed", `{"environment":"staging","service":"worker","method":"RUN","endpoint":"cache-sync","error_code":"TIMEOUT"}`, ``, "2026-06-20T01:00:00Z")

	summary, err := st.BuildAnalysisSummary(ctx)
	if err != nil {
		t.Fatalf("BuildAnalysisSummary: %v", err)
	}
	alerts := summary.Alerts
	if alerts.Total != 4 || alerts.Analyzed != 2 || alerts.Failed != 1 || alerts.Suppressed != 1 {
		t.Fatalf("alert lifecycle mismatch: %+v", alerts)
	}
	if alerts.AnalysisSuccessRate != float64(2)/3 || alerts.SuppressionRate != 0.25 || alerts.HighCritical != 2 {
		t.Fatalf("alert rates mismatch: %+v", alerts)
	}
	if alerts.ByClassification["code_regression"] != 2 || alerts.BySeverity["high"] != 2 || alerts.ByConfidence["high"] != 2 || alerts.ByEnvironment["prod"] != 3 {
		t.Fatalf("alert dimensions mismatch: %+v", alerts)
	}
	if len(alerts.TopServices) != 2 || alerts.TopServices[0].Label != "serverx" || alerts.TopServices[0].Count != 3 {
		t.Fatalf("top services mismatch: %+v", alerts.TopServices)
	}
	if len(alerts.RecurringAlerts) != 1 || alerts.RecurringAlerts[0].Count != 3 || alerts.RecurringAlerts[0].Title != "订单接口异常" {
		t.Fatalf("recurring alerts mismatch: %+v", alerts.RecurringAlerts)
	}
	if alerts.Briefing == "" || len(alerts.Lessons) == 0 || len(alerts.FailureModes) != 1 {
		t.Fatalf("alert insight briefing mismatch: briefing=%q lessons=%+v modes=%+v", alerts.Briefing, alerts.Lessons, alerts.FailureModes)
	}
	if alerts.FailureModes[0].Conclusion != "订单接口发生回归" || alerts.FailureModes[0].WhatToDo == "" {
		t.Fatalf("failure mode mismatch: %+v", alerts.FailureModes[0])
	}
	if len(alerts.Playbook) != 1 || alerts.Playbook[0].Count != 2 {
		t.Fatalf("playbook mismatch: %+v", alerts.Playbook)
	}
	if len(alerts.BlindSpots) != 1 || alerts.BlindSpots[0].Count != 2 {
		t.Fatalf("blind spots mismatch: %+v", alerts.BlindSpots)
	}
	if len(alerts.RecentSevere) != 2 || alerts.RecentSevere[0].Classification != "code_regression" {
		t.Fatalf("recent severe alerts mismatch: %+v", alerts.RecentSevere)
	}

	points, err := st.BuildAnalysisTrend(ctx, 14, "day")
	if err != nil {
		t.Fatalf("BuildAnalysisTrend: %v", err)
	}
	if len(points) != 2 || points[0].Bucket != "2026-06-19" || points[0].TotalAlerts != 3 || points[0].AnalyzedAlerts != 2 || points[0].SuppressedAlerts != 1 {
		t.Fatalf("first alert trend mismatch: %+v", points)
	}
	if points[1].Bucket != "2026-06-20" || points[1].TotalAlerts != 1 || points[1].FailedAlerts != 1 {
		t.Fatalf("second alert trend mismatch: %+v", points[1])
	}
}

func TestAnalysisAgentOverlapIsScopedToSamePull(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 21}
	if err := st.UpsertPull(ctx, &model.PullState{PR: pr, HeadSHA: "sha0"}); err != nil {
		t.Fatalf("UpsertPull same pull: %v", err)
	}
	pull, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull same pull: %v", err)
	}

	otherPR := model.PRRef{Owner: "acme", Repo: "widgets", Number: 22}
	if err := st.UpsertPull(ctx, &model.PullState{PR: otherPR, HeadSHA: "sha0"}); err != nil {
		t.Fatalf("UpsertPull other pull: %v", err)
	}
	otherPull, err := st.GetPull(ctx, otherPR)
	if err != nil {
		t.Fatalf("GetPull other pull: %v", err)
	}

	finding := model.Finding{
		Path: "a.go", Line: 10, Side: model.SideNew,
		Severity: model.SeverityHigh, Title: "shared bug", Body: "x",
	}
	for _, agent := range []string{"codex", "claude", "minimax"} {
		if err := st.SaveFindings(ctx, pull.ID, "sha1", []model.Finding{finding}, model.FindingSaveOptions{Agent: agent}); err != nil {
			t.Fatalf("SaveFindings %s: %v", agent, err)
		}
	}
	if err := st.SaveFindings(ctx, otherPull.ID, "sha2", []model.Finding{finding}, model.FindingSaveOptions{Agent: "codex"}); err != nil {
		t.Fatalf("SaveFindings other pull: %v", err)
	}

	summary, err := st.BuildAnalysisSummary(ctx)
	if err != nil {
		t.Fatalf("BuildAnalysisSummary: %v", err)
	}
	if len(summary.AgentOverlap) != 1 {
		t.Fatalf("AgentOverlap len = %d, want 1: %+v", len(summary.AgentOverlap), summary.AgentOverlap)
	}
	got := summary.AgentOverlap[0]
	if got.Owner != "acme" || got.Repo != "widgets" || got.PullNumber != 21 {
		t.Fatalf("AgentOverlap scoped to wrong pull: %+v", got)
	}
	if got.AgentCount != 3 || strings.Join(got.Agents, ",") != "claude,codex,minimax" {
		t.Fatalf("AgentOverlap agents = count %d list %+v, want claude,codex,minimax", got.AgentCount, got.Agents)
	}
	if got.Path != "a.go" || got.Line != 10 || got.LastSeenSHA != "sha1" {
		t.Fatalf("AgentOverlap location mismatch: %+v", got)
	}
}

func TestBuildAnalysisSummaryToleratesLegacyNullFindingFields(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 22}
	if err := st.UpsertPull(ctx, &model.PullState{PR: pr, HeadSHA: "sha0"}); err != nil {
		t.Fatalf("UpsertPull: %v", err)
	}
	pull, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO findings(pull_id,agent,fingerprint,path,line,severity,title,status,tags)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		pull.ID, "codex", "legacy:nulls", nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("insert legacy finding: %v", err)
	}
	summary, err := st.BuildAnalysisSummary(ctx)
	if err != nil {
		t.Fatalf("BuildAnalysisSummary: %v", err)
	}
	if summary.TotalFindings != 1 || summary.BySeverity["info"] != 1 || summary.ByStatus["open"] != 1 {
		t.Fatalf("summary mismatch: %+v", summary)
	}
	if len(summary.RecentSevere) != 0 {
		t.Fatalf("legacy info finding should not be recent severe: %+v", summary.RecentSevere)
	}
}

func TestBuildAnalysisSummaryUsesJobPayloadAuthorFallback(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 23}
	raw := []byte(`{
		"action":"opened",
		"number":23,
		"pull_request":{
			"poster":{"login":"dev-from-raw"},
			"base":{"ref":"main"},
			"head":{"ref":"feat","sha":"sha1"}
		},
		"repository":{
			"name":"widgets",
			"owner":{"username":"acme"},
			"clone_url":"https://git.example.com/acme/widgets.git"
		}
	}`)
	if _, _, err := st.EnqueueJob(ctx, &model.WebhookEvent{
		DeliveryID: "delivery-author-fallback",
		Event:      model.EventPullRequest,
		Action:     "opened",
		PR:         pr,
		BaseRef:    "main",
		HeadRef:    "feat",
		HeadSHA:    "sha1",
		Raw:        raw,
	}); err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	if err := st.UpsertPull(ctx, &model.PullState{PR: pr, HeadSHA: "sha0"}); err != nil {
		t.Fatalf("UpsertPull: %v", err)
	}
	pull, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	run := &model.ReviewRun{PullID: pull.ID, Agent: "minimax", HeadSHA: "sha1"}
	if err := st.CreateReviewRun(ctx, run); err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}
	if err := st.SaveFindings(ctx, pull.ID, "sha1", []model.Finding{{
		Path: "a.go", Line: 12, Side: model.SideNew,
		Severity: model.SeverityMedium, Title: "bug", Body: "x",
	}}, model.FindingSaveOptions{Agent: "minimax", ReviewRunID: run.ID}); err != nil {
		t.Fatalf("SaveFindings: %v", err)
	}
	if err := st.FinishReviewRun(ctx, run.ID, model.ReviewRunDone, "", 1); err != nil {
		t.Fatalf("FinishReviewRun: %v", err)
	}

	summary, err := st.BuildAnalysisSummary(ctx)
	if err != nil {
		t.Fatalf("BuildAnalysisSummary: %v", err)
	}
	if len(summary.ByDeveloper) != 1 {
		t.Fatalf("ByDeveloper len = %d, want 1: %+v", len(summary.ByDeveloper), summary.ByDeveloper)
	}
	dev := summary.ByDeveloper[0]
	if dev.Developer != "dev-from-raw" || dev.PullRequests != 1 || dev.ReviewRuns != 1 || dev.Findings != 1 {
		t.Fatalf("ByDeveloper fallback mismatch: %+v", dev)
	}
}

func TestSettingsRoundtrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Missing key => found=false.
	if _, found, err := st.GetSetting(ctx, "nope"); err != nil || found {
		t.Fatalf("GetSetting missing: found=%v err=%v", found, err)
	}

	if err := st.SetSetting(ctx, "model", "gpt-5", false); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := st.SetSetting(ctx, "token", "secret-val", true); err != nil {
		t.Fatalf("SetSetting secret: %v", err)
	}

	v, found, err := st.GetSetting(ctx, "model")
	if err != nil || !found {
		t.Fatalf("GetSetting model: found=%v err=%v", found, err)
	}
	if v != "gpt-5" {
		t.Fatalf("GetSetting model: %q, want gpt-5", v)
	}

	// Update existing key.
	if err := st.SetSetting(ctx, "model", "gpt-6", false); err != nil {
		t.Fatalf("SetSetting update: %v", err)
	}
	v, _, _ = st.GetSetting(ctx, "model")
	if v != "gpt-6" {
		t.Fatalf("GetSetting after update: %q, want gpt-6", v)
	}

	all, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if all["model"] != "gpt-6" || all["token"] != "secret-val" {
		t.Fatalf("AllSettings mismatch: %+v", all)
	}
}
