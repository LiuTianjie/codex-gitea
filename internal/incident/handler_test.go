package incident

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
	"github.com/turning4th/codex-gitea/internal/store"
)

func createIncidentTestStore(t *testing.T) (*store.Store, model.AnalysisConfig, string) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "incident.db"), store.WithSecretKey("test-secret-key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	token := "ingest-token"
	sum := sha256.Sum256([]byte(token))
	cfg := model.AnalysisConfig{
		Name: "serverx", Enabled: true, RepositoryURL: "https://example.com/serverx.git", RepositoryRef: "main",
		SLSEndpoint: "cn-beijing.log.aliyuncs.com", SLSProject: "p", SLSLogstore: "raw",
		SLSAccessKeyID: "id", SLSAccessKeySecret: "secret", ThrottleEnabled: true,
		ThrottleThreshold: 1, ThrottleCooldownSecs: 0, IngestTokenHash: hex.EncodeToString(sum[:]),
	}
	created, err := s.CreateAnalysisConfig(context.Background(), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, *created, token
}

func TestHandlerSuppressesSameEndpointAndErrorAfterFirstAnalysis(t *testing.T) {
	s, cfg, token := createIncidentTestStore(t)
	var suppressed *model.AnalysisTask
	h := &Handler{
		Store: s,
		NotifySuppressed: func(task *model.AnalysisTask) {
			suppressed = task
		},
	}
	request := func(delivery, trace string) *httptest.ResponseRecorder {
		body := []byte(`{"delivery_id":"` + delivery + `","method":"POST","endpoint":"/api/test","trace_id":"` + trace + `","error_code":"40310","error_message":"会员能力不可用"}`)
		r := httptest.NewRequest(http.MethodPost, "/hooks/alert-analysis/"+itoa(cfg.ID)+"/"+token, bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if first := request("delivery-1", "trace-1"); first.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	if second := request("delivery-2", "trace-2"); second.Code != http.StatusAccepted {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	if suppressed == nil || suppressed.Status != model.AnalysisTaskSuppressed || suppressed.DuplicateOfTaskID == nil {
		t.Fatalf("suppressed task = %+v", suppressed)
	}
	if *suppressed.DuplicateOfTaskID == suppressed.ID {
		t.Fatalf("duplicate task points to itself: %+v", suppressed)
	}
}

func TestHandlerEnqueuesAndDeduplicatesAlert(t *testing.T) {
	s, cfg, token := createIncidentTestStore(t)
	wakes := 0
	h := &Handler{Store: s, Wake: func() { wakes++ }}
	body := []byte(`{"delivery_id":"delivery-1","environment":"PROD","method":"post","endpoint":"/api/test","trace_id":"trace","error_code":500,"authorization":"should-not-persist"}`)
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("/hooks/alert-analysis/" + itoa(cfg.ID) + "/" + token)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	w = request("/hooks/alert-analysis/" + itoa(cfg.ID) + "/" + token)
	if w.Code != http.StatusAccepted || wakes != 1 {
		t.Fatalf("redelivery status=%d wakes=%d body=%s", w.Code, wakes, w.Body.String())
	}
	tasks, err := s.ListAnalysisTasks(context.Background(), model.AnalysisTaskFilter{Limit: 10})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%d err=%v", len(tasks), err)
	}
	if tasks[0].Alert.Method != "POST" || tasks[0].Alert.Raw["authorization"] != "***redacted***" {
		t.Fatalf("alert=%+v", tasks[0].Alert)
	}
	if bad := request("/hooks/alert-analysis/" + itoa(cfg.ID) + "/wrong"); bad.Code != http.StatusNotFound {
		t.Fatalf("bad token status=%d", bad.Code)
	}
}

type blockingProcessor struct{ started chan struct{} }

func (p *blockingProcessor) Process(ctx context.Context, _ *model.AnalysisTask) (string, error) {
	close(p.started)
	<-ctx.Done()
	return "", ctx.Err()
}
func (*blockingProcessor) NotifyTerminal(context.Context, *model.AnalysisTask, string) {}

func TestQueueCancelsRunningTask(t *testing.T) {
	s, cfg, _ := createIncidentTestStore(t)
	task, _, err := s.EnqueueAnalysisTask(context.Background(), cfg, model.AlertEnvelope{AlertID: "cancel"}, "cancel-delivery", "cancel-fingerprint")
	if err != nil {
		t.Fatal(err)
	}
	processor := &blockingProcessor{started: make(chan struct{})}
	q := NewQueue(s, processor, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	q.Notify()
	select {
	case <-processor.started:
	case <-time.After(3 * time.Second):
		t.Fatal("task did not start")
	}
	if _, err := q.Cancel(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, err := s.GetAnalysisTask(context.Background(), task.ID)
		if err == nil && current.Status == model.AnalysisTaskCanceled {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	current, _ := s.GetAnalysisTask(context.Background(), task.ID)
	t.Fatalf("task status=%s", current.Status)
}

func itoa(id int64) string {
	if id == 0 {
		return "0"
	}
	buf := make([]byte, 0, 20)
	for id > 0 {
		buf = append([]byte{byte('0' + id%10)}, buf...)
		id /= 10
	}
	return string(buf)
}
