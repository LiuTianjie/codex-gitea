package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/turning4th/codex-gitea/internal/model"
)

// sign computes the hex HMAC-SHA256 the same way Gitea does, for test inputs.
func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerify(t *testing.T) {
	secret := "topsecret"
	body := []byte(`{"action":"opened","number":12}`)
	good := sign(body, secret)

	if !Verify(body, good, secret) {
		t.Errorf("Verify: correct signature should pass")
	}
	if Verify(body, sign(body, "wrong"), secret) {
		t.Errorf("Verify: signature made with wrong secret should fail")
	}
	if Verify(body, good[:len(good)-1]+"0", secret) {
		t.Errorf("Verify: tampered signature should fail")
	}
	if Verify(body, "", secret) {
		t.Errorf("Verify: empty signature should fail")
	}
	if Verify(body, "zzzz", secret) {
		t.Errorf("Verify: non-hex signature should fail")
	}
	if Verify([]byte("other body"), good, secret) {
		t.Errorf("Verify: signature for a different body should fail")
	}
}

const pullRequestBody = `{
	"action":"opened",
	"number":12,
	"pull_request":{
		"user":{"username":"dev-a"},
		"base":{"ref":"main"},
		"head":{"ref":"feat","sha":"abc123"}
	},
	"repository":{
		"name":"repo",
		"owner":{"username":"alice"},
		"clone_url":"https://git.example.com/alice/repo.git"
	}
}`

const issueCommentPRBody = `{
	"action":"created",
	"issue":{
		"number":12,
		"user":{"username":"dev-b"},
		"pull_request":{"merged":false}
	},
	"comment":{
		"body":"/review please",
		"user":{"username":"bob"}
	},
	"repository":{
		"name":"repo",
		"owner":{"username":"alice"},
		"clone_url":"https://git.example.com/alice/repo.git"
	}
}`

const issueCommentNotPRBody = `{
	"action":"created",
	"issue":{
		"number":7,
		"user":{"username":"dev-c"},
		"pull_request":null
	},
	"comment":{
		"body":"just a thought",
		"user":{"username":"carol"}
	},
	"repository":{
		"name":"repo",
		"owner":{"username":"alice"}
	}
}`

func TestParsePullRequest(t *testing.T) {
	ev, err := Parse("pull_request", []byte(pullRequestBody))
	if err != nil {
		t.Fatalf("Parse pull_request: unexpected error: %v", err)
	}
	if ev.Event != model.EventPullRequest {
		t.Errorf("Event = %q, want %q", ev.Event, model.EventPullRequest)
	}
	if ev.Action != "opened" {
		t.Errorf("Action = %q, want opened", ev.Action)
	}
	want := model.PRRef{Owner: "alice", Repo: "repo", Number: 12}
	if ev.PR != want {
		t.Errorf("PR = %+v, want %+v", ev.PR, want)
	}
	if ev.Author != "dev-a" {
		t.Errorf("Author = %q, want dev-a", ev.Author)
	}
	if ev.BaseRef != "main" {
		t.Errorf("BaseRef = %q, want main", ev.BaseRef)
	}
	if ev.HeadRef != "feat" {
		t.Errorf("HeadRef = %q, want feat", ev.HeadRef)
	}
	if ev.HeadSHA != "abc123" {
		t.Errorf("HeadSHA = %q, want abc123", ev.HeadSHA)
	}
	if ev.CloneURL != "https://git.example.com/alice/repo.git" {
		t.Errorf("CloneURL = %q", ev.CloneURL)
	}
	if string(ev.Raw) != pullRequestBody {
		t.Errorf("Raw not preserved verbatim")
	}
}

func TestParsePullRequestUsesPosterAuthorFallback(t *testing.T) {
	body := strings.Replace(pullRequestBody, `"user":{"username":"dev-a"},`, `"poster":{"login":"dev-poster"},`, 1)
	ev, err := Parse("pull_request", []byte(body))
	if err != nil {
		t.Fatalf("Parse pull_request: unexpected error: %v", err)
	}
	if ev.Author != "dev-poster" {
		t.Errorf("Author = %q, want dev-poster", ev.Author)
	}
}

func TestParsePullRequestSync(t *testing.T) {
	body := strings.Replace(pullRequestBody, `"action":"opened"`, `"action":"synchronized"`, 1)
	ev, err := Parse("pull_request_sync", []byte(body))
	if err != nil {
		t.Fatalf("Parse pull_request_sync: unexpected error: %v", err)
	}
	if ev.Event != model.EventPullRequest {
		t.Errorf("Event = %q, want normalized pull_request", ev.Event)
	}
	if ev.Action != "synchronized" {
		t.Errorf("Action = %q, want synchronized", ev.Action)
	}
	if ev.PR.Number != 12 || ev.HeadSHA != "abc123" {
		t.Errorf("sync event parsed incorrectly: %+v", ev)
	}
}

func TestParseIssueCommentIsPR(t *testing.T) {
	ev, err := Parse("issue_comment", []byte(issueCommentPRBody))
	if err != nil {
		t.Fatalf("Parse issue_comment (PR): unexpected error: %v", err)
	}
	if ev.Event != model.EventIssueComment {
		t.Errorf("Event = %q, want %q", ev.Event, model.EventIssueComment)
	}
	if ev.Action != "created" {
		t.Errorf("Action = %q, want created", ev.Action)
	}
	if !ev.IsPR {
		t.Errorf("IsPR = false, want true (issue.pull_request is non-null)")
	}
	want := model.PRRef{Owner: "alice", Repo: "repo", Number: 12}
	if ev.PR != want {
		t.Errorf("PR = %+v, want %+v", ev.PR, want)
	}
	if ev.CommentBody != "/review please" {
		t.Errorf("CommentBody = %q", ev.CommentBody)
	}
	if ev.CommentUser != "bob" {
		t.Errorf("CommentUser = %q, want bob", ev.CommentUser)
	}
	if ev.Author != "dev-b" {
		t.Errorf("Author = %q, want dev-b", ev.Author)
	}
}

func TestParseIssueCommentNotPR(t *testing.T) {
	ev, err := Parse("issue_comment", []byte(issueCommentNotPRBody))
	if err != nil {
		t.Fatalf("Parse issue_comment (not PR): unexpected error: %v", err)
	}
	if ev.IsPR {
		t.Errorf("IsPR = true, want false (issue.pull_request is null)")
	}
	if ev.PR.Number != 7 {
		t.Errorf("PR.Number = %d, want 7", ev.PR.Number)
	}
	if ev.CommentUser != "carol" {
		t.Errorf("CommentUser = %q, want carol", ev.CommentUser)
	}
}

func TestParseUnknownEvent(t *testing.T) {
	if _, err := Parse("push", []byte(`{}`)); err == nil {
		t.Errorf("Parse: unknown event type should return an error")
	}
}

func TestServerValidSignatureInvokesOnEvent(t *testing.T) {
	const secret = "shh"
	var got *model.WebhookEvent
	h := NewHandler(secret, func(_ context.Context, ev *model.WebhookEvent) error {
		got = ev
		return nil
	})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	body := []byte(pullRequestBody)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook", strings.NewReader(string(body)))
	req.Header.Set(headerEvent, "pull_request")
	req.Header.Set(headerDelivery, "delivery-uuid-1")
	req.Header.Set(headerSignature, sign(body, secret))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got == nil {
		t.Fatalf("OnEvent was not invoked")
	}
	if got.DeliveryID != "delivery-uuid-1" {
		t.Errorf("DeliveryID = %q, want delivery-uuid-1", got.DeliveryID)
	}
	if got.PR.Number != 12 {
		t.Errorf("PR.Number = %d, want 12", got.PR.Number)
	}
}

func TestServerPrefersSpecificEventTypeHeader(t *testing.T) {
	const secret = "shh"
	var got *model.WebhookEvent
	h := NewHandler(secret, func(_ context.Context, ev *model.WebhookEvent) error {
		got = ev
		return nil
	})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	body := []byte(strings.Replace(pullRequestBody, `"action":"opened"`, `"action":"synchronized"`, 1))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook", strings.NewReader(string(body)))
	req.Header.Set(headerEvent, "pull_request")
	req.Header.Set(headerEventType, "pull_request_sync")
	req.Header.Set(headerDelivery, "delivery-sync-1")
	req.Header.Set(headerSignature, sign(body, secret))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got == nil {
		t.Fatalf("OnEvent was not invoked")
	}
	if got.Event != model.EventPullRequest || got.Action != "synchronized" {
		t.Fatalf("event = %s/%s, want pull_request/synchronized", got.Event, got.Action)
	}
	if got.DeliveryID != "delivery-sync-1" {
		t.Errorf("DeliveryID = %q, want delivery-sync-1", got.DeliveryID)
	}
}

func TestServerInvalidSignatureRejected(t *testing.T) {
	const secret = "shh"
	called := false
	h := NewHandler(secret, func(_ context.Context, _ *model.WebhookEvent) error {
		called = true
		return nil
	})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	body := []byte(pullRequestBody)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook", strings.NewReader(string(body)))
	req.Header.Set(headerEvent, "pull_request")
	req.Header.Set(headerSignature, sign(body, "wrong-secret"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if called {
		t.Errorf("OnEvent must not be called when signature is invalid")
	}
}

func TestServerSignedPushProbeIsAcknowledgedWithoutEvent(t *testing.T) {
	const secret = "shh"
	called := false
	h := NewHandler(secret, func(_ context.Context, _ *model.WebhookEvent) error {
		called = true
		return nil
	})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	body := []byte(`{"ref":"refs/heads/main"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook", strings.NewReader(string(body)))
	req.Header.Set(headerEvent, "push")
	req.Header.Set(headerSignature, sign(body, secret))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if called {
		t.Errorf("OnEvent must not be called for a push connectivity probe")
	}
}

func TestServerDynamicSecret(t *testing.T) {
	secret := "old-secret"
	called := 0
	h := NewHandlerWithSecretFunc(func() string { return secret }, func(_ context.Context, _ *model.WebhookEvent) error {
		called++
		return nil
	})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	body := []byte(pullRequestBody)
	post := func(sigSecret string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook", strings.NewReader(string(body)))
		req.Header.Set(headerEvent, "pull_request")
		req.Header.Set(headerSignature, sign(body, sigSecret))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if status := post("old-secret"); status != http.StatusOK {
		t.Fatalf("old secret status = %d, want 200", status)
	}
	secret = "new-secret"
	if status := post("old-secret"); status != http.StatusUnauthorized {
		t.Fatalf("old secret after update status = %d, want 401", status)
	}
	if status := post("new-secret"); status != http.StatusOK {
		t.Fatalf("new secret status = %d, want 200", status)
	}
	if called != 2 {
		t.Fatalf("called = %d, want 2", called)
	}
}

func TestServerBadPayloadReturns400(t *testing.T) {
	const secret = "shh"
	h := NewHandler(secret, func(_ context.Context, _ *model.WebhookEvent) error { return nil })
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	body := []byte(`not json`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook", strings.NewReader(string(body)))
	req.Header.Set(headerEvent, "pull_request")
	req.Header.Set(headerSignature, sign(body, secret))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServerHealthz(t *testing.T) {
	h := NewHandler("shh")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
