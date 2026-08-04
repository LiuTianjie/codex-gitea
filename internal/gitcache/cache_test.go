package gitcache

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

// gitOK runs a git command in dir and fails the test on error.
func gitOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// setupRemote builds a normal git repo that stands in for the Gitea remote:
// a `main` base branch plus a refs/pull/1/head ref. It returns the file:// URL,
// the base ref name, and the head SHA at refs/pull/1/head.
func setupRemote(t *testing.T) (cloneURL, baseRef, headSHA string) {
	t.Helper()
	remote := t.TempDir()

	id := []string{"-c", "user.email=test@example.com", "-c", "user.name=Test"}
	gitOK(t, remote, "init", "-b", "main", ".")

	// Base commit on main.
	if err := os.WriteFile(filepath.Join(remote, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOK(t, remote, "add", "base.txt")
	gitOK(t, remote, append(append([]string{}, id...), "commit", "-m", "base")...)

	// PR head commit (child of base) carrying a distinctive file.
	if err := os.WriteFile(filepath.Join(remote, "pr.txt"), []byte("pr-content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOK(t, remote, "add", "pr.txt")
	gitOK(t, remote, append(append([]string{}, id...), "commit", "-m", "pr head")...)
	headSHA = gitOK(t, remote, "rev-parse", "HEAD")

	// Expose it as the Gitea PR head ref, and rewind main so the PR commit is
	// only reachable via refs/pull/1/head (proving we fetch that ref).
	gitOK(t, remote, "update-ref", "refs/pull/1/head", headSHA)
	gitOK(t, remote, "update-ref", "refs/heads/main", "HEAD~1")

	return "file://" + remote, "main", headSHA
}

func TestPrepareCleanupRoundTrip(t *testing.T) {
	cloneURL, baseRef, headSHA := setupRemote(t)

	cacheDir := t.TempDir()
	workDir := t.TempDir()
	c := New(cacheDir, workDir)

	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 1}
	ctx := context.Background()

	wt, err := c.Prepare(ctx, pr, cloneURL, baseRef, "refs/pull/1/head", headSHA)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Deterministic worktree path.
	wantWT := filepath.Join(workDir, "acme__widgets__pr1")
	if wt != wantWT {
		t.Fatalf("worktree path = %q, want %q", wt, wantWT)
	}

	// Worktree exists and holds the PR head content.
	got, err := os.ReadFile(filepath.Join(wt, "pr.txt"))
	if err != nil {
		t.Fatalf("read checked-out file: %v", err)
	}
	if string(got) != "pr-content\n" {
		t.Fatalf("pr.txt = %q, want %q", got, "pr-content\n")
	}

	// Mirror exists at the expected path.
	mirror := filepath.Join(cacheDir, "acme__widgets.git")
	if !dirExists(mirror) {
		t.Fatalf("mirror dir %q missing", mirror)
	}

	// Token must never be persisted; here no token is set, but assert the
	// mirror config carries no Authorization header regardless.
	if cfg, err := os.ReadFile(filepath.Join(mirror, "config")); err == nil {
		if strings.Contains(string(cfg), "extraHeader") || strings.Contains(string(cfg), "Authorization") {
			t.Fatalf("mirror config leaked credentials:\n%s", cfg)
		}
	}

	// Simulate a Gitea migration leaving the persisted mirror on an obsolete
	// origin. The next Prepare must reconcile it with the webhook clone URL
	// before fetching.
	gitOK(t, mirror, "remote", "set-url", "origin", "file:///obsolete-gitea/widgets.git")

	// Second Prepare hits the "mirror already exists" path, repairs origin,
	// and rebuilds the worktree with the same content.
	wt2, err := c.Prepare(ctx, pr, cloneURL, baseRef, "refs/pull/1/head", headSHA)
	if err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if got := gitOK(t, mirror, "remote", "get-url", "origin"); got != cloneURL {
		t.Fatalf("origin URL = %q, want %q", got, cloneURL)
	}
	if wt2 != wantWT {
		t.Fatalf("second worktree path = %q, want %q", wt2, wantWT)
	}
	if got, err := os.ReadFile(filepath.Join(wt2, "pr.txt")); err != nil || string(got) != "pr-content\n" {
		t.Fatalf("second checkout content = %q err=%v", got, err)
	}

	// Cleanup removes the worktree but keeps the mirror.
	if err := c.Cleanup(pr); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if dirExists(wt) {
		t.Fatalf("worktree %q still present after Cleanup", wt)
	}
	if !dirExists(mirror) {
		t.Fatalf("mirror %q removed by Cleanup, should be retained", mirror)
	}

	// Cleanup again is a no-op (missing worktree is not an error).
	if err := c.Cleanup(pr); err != nil {
		t.Fatalf("idempotent Cleanup: %v", err)
	}
}

// TestPrepareEmptyHeadSHA guards the early validation.
func TestPrepareEmptyHeadSHA(t *testing.T) {
	c := New(t.TempDir(), t.TempDir())
	pr := model.PRRef{Owner: "a", Repo: "b", Number: 2}
	if _, err := c.Prepare(context.Background(), pr, "file:///nope", "main", "refs/pull/2/head", ""); err == nil {
		t.Fatal("expected error for empty headSHA, got nil")
	}
}

// TestWithTokenDoesNotBreakClone ensures the http.extraHeader injection path is
// harmless over transports that ignore it (file://), and never persists.
func TestWithTokenDoesNotBreakClone(t *testing.T) {
	cloneURL, baseRef, headSHA := setupRemote(t)
	cacheDir := t.TempDir()
	c := New(cacheDir, t.TempDir(), WithToken("s3cr3t-token"))

	pr := model.PRRef{Owner: "acme", Repo: "private", Number: 1}
	wt, err := c.Prepare(context.Background(), pr, cloneURL, baseRef, "refs/pull/1/head", headSHA)
	if err != nil {
		t.Fatalf("Prepare with token: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "pr.txt")); err != nil {
		t.Fatalf("expected checkout to succeed with token set: %v", err)
	}
	cfg, err := os.ReadFile(filepath.Join(cacheDir, "acme__private.git", "config"))
	if err != nil {
		t.Fatalf("read mirror config: %v", err)
	}
	if strings.Contains(string(cfg), "s3cr3t-token") {
		t.Fatalf("token leaked into mirror config:\n%s", cfg)
	}
}

func TestWithTokenFuncIsLazy(t *testing.T) {
	cloneURL, baseRef, headSHA := setupRemote(t)
	tokens := []string{"first-token", "second-token", "third-token"}
	var calls int
	c := New(t.TempDir(), t.TempDir(), WithTokenFunc(func() (string, error) {
		if calls >= len(tokens) {
			return tokens[len(tokens)-1], nil
		}
		token := tokens[calls]
		calls++
		return token, nil
	}))

	pr := model.PRRef{Owner: "acme", Repo: "private", Number: 1}
	if _, err := c.Prepare(context.Background(), pr, cloneURL, baseRef, "refs/pull/1/head", headSHA); err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	if calls != 2 {
		t.Fatalf("token calls after first prepare = %d, want clone+fetch", calls)
	}
	if _, err := c.Prepare(context.Background(), pr, cloneURL, baseRef, "refs/pull/1/head", headSHA); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if calls != 3 {
		t.Fatalf("token calls after second prepare = %d, want another fetch", calls)
	}
}

// TestKeyedMutexSerializesSameKey verifies same-key ops are serialized while
// distinct keys run concurrently.
func TestKeyedMutexSerializesSameKey(t *testing.T) {
	km := newKeyedMutex()

	// Same key: a held lock must block a second acquire on the same key.
	unlock := km.Lock("repo")
	acquired := make(chan struct{})
	go func() {
		u := km.Lock("repo")
		close(acquired)
		u()
	}()
	select {
	case <-acquired:
		t.Fatal("second Lock on same key acquired while first was held")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}
	unlock()
	select {
	case <-acquired:
		// expected: unblocked after release
	case <-time.After(time.Second):
		t.Fatal("second Lock did not acquire after release")
	}

	// Distinct keys do not block each other.
	uA := km.Lock("A")
	done := make(chan struct{})
	go func() {
		uB := km.Lock("B")
		uB()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Lock on distinct key B blocked while A was held")
	}
	uA()

	// Concurrency stress: same-key critical sections never overlap.
	var concurrent, max int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := km.Lock("hot")
			defer u()
			n := atomic.AddInt32(&concurrent, 1)
			for {
				old := atomic.LoadInt32(&max)
				if n <= old || atomic.CompareAndSwapInt32(&max, old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&concurrent, -1)
		}()
	}
	wg.Wait()
	if max != 1 {
		t.Fatalf("max concurrent holders of same key = %d, want 1", max)
	}
}
