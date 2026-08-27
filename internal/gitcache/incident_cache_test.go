package gitcache

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testIncidentPolicy() IncidentCachePolicy {
	return IncidentCachePolicy{
		FetchDepth:      1,
		MaxRepositories: 3,
		MaxBytes:        1 << 30,
		MaxIdle:         24 * time.Hour,
		WorktreeTTL:     time.Hour,
		CleanupInterval: time.Minute,
		MinFreeBytes:    0,
	}
}

func TestPrepareRevisionCreatesShallowCache(t *testing.T) {
	cloneURL, _, _ := setupRemote(t)
	policy := testIncidentPolicy()
	c := New(t.TempDir(), t.TempDir(), WithIncidentCachePolicyFunc(func() IncidentCachePolicy { return policy }))

	wt, _, err := c.PrepareRevision(context.Background(), cloneURL, "main", 1)
	if err != nil {
		t.Fatalf("PrepareRevision: %v", err)
	}
	defer c.CleanupRevision(cloneURL, 1)

	mirror := filepath.Join(c.cacheDir, incidentKey(cloneURL)+".git")
	if got := gitOK(t, mirror, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("incident repository is shallow = %q, want true", got)
	}
	if _, err := os.Stat(filepath.Join(wt, "base.txt")); err != nil {
		t.Fatalf("worktree missing fetched content: %v", err)
	}
	if got := gitOK(t, mirror, "for-each-ref", "--format=%(refname)"); strings.Contains(got, "refs/pull/") || strings.Contains(got, "refs/tags/") {
		t.Fatalf("incident cache fetched unrelated refs:\n%s", got)
	}
}

func TestPrepareRevisionReplacesLegacyFullMirror(t *testing.T) {
	cloneURL, _, _ := setupRemote(t)
	cacheDir := t.TempDir()
	workDir := t.TempDir()
	mirror := filepath.Join(cacheDir, incidentKey(cloneURL)+".git")
	cmd := exec.Command("git", "clone", "--mirror", cloneURL, mirror)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create legacy mirror: %v\n%s", err, out)
	}
	sentinel := filepath.Join(mirror, "legacy-full-mirror")
	if err := os.WriteFile(sentinel, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}

	policy := testIncidentPolicy()
	c := New(cacheDir, workDir, WithIncidentCachePolicyFunc(func() IncidentCachePolicy { return policy }))
	if _, _, err := c.PrepareRevision(context.Background(), cloneURL, "main", 2); err != nil {
		t.Fatalf("PrepareRevision: %v", err)
	}
	defer c.CleanupRevision(cloneURL, 2)

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("legacy mirror was not replaced: %v", err)
	}
	version, err := os.ReadFile(filepath.Join(mirror, incidentVersionFile))
	if err != nil || strings.TrimSpace(string(version)) != incidentCacheVersion {
		t.Fatalf("cache version = %q err=%v", version, err)
	}
	if got := gitOK(t, mirror, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("migrated repository is shallow = %q, want true", got)
	}
}

func TestPruneIncidentCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cacheDir := t.TempDir()
	workDir := t.TempDir()
	policy := testIncidentPolicy()
	policy.MaxRepositories = 2
	c := New(cacheDir, workDir, WithIncidentCachePolicyFunc(func() IncidentCachePolicy { return policy }))

	oldest := makeFakeIncidentCache(t, cacheDir, "incident_oldest", time.Now().Add(-3*time.Hour), 8)
	middle := makeFakeIncidentCache(t, cacheDir, "incident_middle", time.Now().Add(-2*time.Hour), 8)
	newest := makeFakeIncidentCache(t, cacheDir, "incident_newest", time.Now().Add(-time.Hour), 8)
	if err := c.PruneIncidentCache(context.Background()); err != nil {
		t.Fatalf("PruneIncidentCache: %v", err)
	}
	if dirExists(oldest) {
		t.Fatal("least recently used cache was retained")
	}
	if !dirExists(middle) || !dirExists(newest) {
		t.Fatal("newer caches were unexpectedly evicted")
	}
}

func TestPrepareRevisionRejectsOversizedRepositoryAndCleansCache(t *testing.T) {
	cloneURL, _, _ := setupRemote(t)
	policy := testIncidentPolicy()
	policy.MaxBytes = 1
	c := New(t.TempDir(), t.TempDir(), WithIncidentCachePolicyFunc(func() IncidentCachePolicy { return policy }))

	if _, _, err := c.PrepareRevision(context.Background(), cloneURL, "main", 3); err == nil || !strings.Contains(err.Error(), "above 1-byte limit") {
		t.Fatalf("PrepareRevision error = %v, want cache size rejection", err)
	}
	if dirExists(filepath.Join(c.cacheDir, incidentKey(cloneURL)+".git")) {
		t.Fatal("oversized incident cache was not removed")
	}
	if entries, err := os.ReadDir(c.workDir); err != nil || len(entries) != 0 {
		t.Fatalf("oversized repository left worktree entries=%d err=%v", len(entries), err)
	}
}

func TestPruneIncidentCacheRemovesStaleWorktree(t *testing.T) {
	cacheDir := t.TempDir()
	workDir := t.TempDir()
	policy := testIncidentPolicy()
	policy.WorktreeTTL = time.Minute
	c := New(cacheDir, workDir, WithIncidentCachePolicyFunc(func() IncidentCachePolicy { return policy }))

	wt := filepath.Join(workDir, "incident_abcd__task99")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(wt, old, old); err != nil {
		t.Fatal(err)
	}
	if err := c.PruneIncidentCache(context.Background()); err != nil {
		t.Fatalf("PruneIncidentCache: %v", err)
	}
	if dirExists(wt) {
		t.Fatal("stale incident worktree was retained")
	}
}

func TestPruneIncidentCacheProtectsActiveCache(t *testing.T) {
	cacheDir := t.TempDir()
	policy := testIncidentPolicy()
	policy.MaxRepositories = 1
	c := New(cacheDir, t.TempDir(), WithIncidentCachePolicyFunc(func() IncidentCachePolicy { return policy }))

	activeKey := "incident_active"
	active := makeFakeIncidentCache(t, cacheDir, activeKey, time.Now().Add(-3*time.Hour), 8)
	other := makeFakeIncidentCache(t, cacheDir, "incident_other", time.Now().Add(-time.Hour), 8)
	c.markIncidentActive(activeKey, 1)
	defer c.markIncidentActive(activeKey, -1)

	if err := c.PruneIncidentCache(context.Background()); err != nil {
		t.Fatalf("PruneIncidentCache: %v", err)
	}
	if !dirExists(active) {
		t.Fatal("active cache was evicted")
	}
	if dirExists(other) {
		t.Fatal("inactive cache was retained while over repository limit")
	}
}

func makeFakeIncidentCache(t *testing.T, cacheDir, key string, usedAt time.Time, bytes int) string {
	t.Helper()
	path := filepath.Join(cacheDir, key+".git")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "payload"), make([]byte, bytes), 0o600); err != nil {
		t.Fatal(err)
	}
	lastUsed := filepath.Join(path, incidentLastUsedFile)
	if err := os.WriteFile(lastUsed, []byte("used"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lastUsed, usedAt, usedAt); err != nil {
		t.Fatal(err)
	}
	return path
}
