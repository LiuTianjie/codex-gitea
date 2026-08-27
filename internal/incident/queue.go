package incident

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

type Queue struct {
	store      model.AnalysisStore
	processor  TaskProcessor
	workers    int
	logger     *log.Logger
	wake       chan struct{}
	suppressed chan *model.AnalysisTask

	mu      sync.Mutex
	cancels map[int64]context.CancelFunc
}

func NewQueue(store model.AnalysisStore, processor TaskProcessor, workers int, logger *log.Logger) *Queue {
	if workers < 1 {
		workers = 1
	}
	return &Queue{store: store, processor: processor, workers: workers, logger: logger, wake: make(chan struct{}, 1), suppressed: make(chan *model.AnalysisTask, 1024), cancels: map[int64]context.CancelFunc{}}
}

func (q *Queue) Notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// NotifySuppressed schedules a lightweight Feishu duplicate card without
// occupying an analysis worker. The task itself is already durably recorded.
func (q *Queue) NotifySuppressed(task *model.AnalysisTask) {
	if task == nil {
		return
	}
	select {
	case q.suppressed <- task:
	default:
		q.logf("suppressed analysis notification queue full; task=%d", task.ID)
	}
}

func (q *Queue) Run(ctx context.Context) error {
	if err := q.store.RecoverAnalysisTasks(ctx); err != nil {
		return err
	}
	var wg sync.WaitGroup
	for i := 0; i < q.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.worker(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.suppressedWorker(ctx)
	}()
	wg.Wait()
	return ctx.Err()
}

func (q *Queue) suppressedWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-q.suppressed:
			q.processor.NotifyTerminal(ctx, task, "suppressed")
		}
	}
}

func (q *Queue) worker(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		task, err := q.store.ClaimAnalysisTask(ctx)
		if err != nil {
			q.logf("claim analysis task: %v", err)
		}
		if task != nil {
			q.run(ctx, task)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-ticker.C:
		}
	}
}

func (q *Queue) run(parent context.Context, task *model.AnalysisTask) {
	ctx, cancel := context.WithCancel(parent)
	q.mu.Lock()
	q.cancels[task.ID] = cancel
	q.mu.Unlock()
	if latest, err := q.store.GetAnalysisTask(context.Background(), task.ID); err == nil && latest.Status == model.AnalysisTaskCancelRequested {
		cancel()
	}
	defer func() {
		q.mu.Lock()
		delete(q.cancels, task.ID)
		q.mu.Unlock()
		cancel()
	}()

	resultJSON, err := q.processor.Process(ctx, task)
	status := model.AnalysisTaskSucceeded
	errorType, errorMessage := "", ""
	if err != nil {
		errorMessage = err.Error()
		if ctx.Err() != nil {
			status = model.AnalysisTaskCanceled
			errorType = "canceled"
		} else {
			status = model.AnalysisTaskFailed
			errorType = classifyAnalysisError(err)
		}
	}
	if finishErr := q.store.FinishAnalysisTask(context.Background(), task.ID, status, resultJSON, errorType, errorMessage); finishErr != nil && !errors.Is(finishErr, model.ErrAnalysisTaskTerminal) {
		q.logf("finish analysis task %d: %v", task.ID, finishErr)
		return
	}
	level := "info"
	message := "分析完成"
	if status == model.AnalysisTaskFailed {
		level, message = "error", "分析失败："+errorMessage
	} else if status == model.AnalysisTaskCanceled {
		level, message = "warning", "分析已取消"
	}
	_ = q.store.AppendAnalysisTaskEvent(context.Background(), task.ID, string(status), level, message, nil)
	task.Status = status
	task.Error = errorMessage
	if status != model.AnalysisTaskSucceeded {
		q.processor.NotifyTerminal(context.Background(), task, string(status))
	}
}

func (q *Queue) Cancel(ctx context.Context, id int64) (*model.AnalysisTask, error) {
	task, err := q.store.RequestAnalysisTaskCancel(ctx, id)
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	cancel := q.cancels[id]
	q.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if task.Status == model.AnalysisTaskCanceled {
		q.processor.NotifyTerminal(context.Background(), task, "canceled")
	}
	return task, nil
}

func (q *Queue) Retry(ctx context.Context, id int64) (*model.AnalysisTask, error) {
	original, err := q.store.GetAnalysisTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if original.ConfigID == nil {
		return nil, model.ErrAnalysisConfigNotFound
	}
	cfg, err := q.store.GetAnalysisConfig(ctx, *original.ConfigID)
	if err != nil {
		return nil, err
	}
	task, err := q.store.RetryAnalysisTask(ctx, id, cfg.Snapshot())
	if err == nil {
		q.Notify()
	}
	return task, err
}

func (q *Queue) logf(format string, args ...any) {
	if q.logger != nil {
		q.logger.Printf(format, args...)
	}
}

func classifyAnalysisError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "sls"), strings.Contains(msg, "logstore"):
		return "logs"
	case strings.Contains(msg, "git"), strings.Contains(msg, "repository"):
		return "git"
	case strings.Contains(msg, "auth"), strings.Contains(msg, "access key"), strings.Contains(msg, "unauthorized"):
		return "auth"
	case strings.Contains(msg, "codex"), strings.Contains(msg, "analyzer"):
		return "analyzer"
	default:
		return "unknown"
	}
}

// Control is the subset exposed to authenticated console handlers.
type Control interface {
	Cancel(context.Context, int64) (*model.AnalysisTask, error)
	Retry(context.Context, int64) (*model.AnalysisTask, error)
}

var _ Control = (*Queue)(nil)
