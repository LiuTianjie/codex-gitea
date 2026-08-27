package gitcache

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	incidentCacheVersion = "2"
	incidentVersionFile  = ".codex-incident-cache-version"
	incidentLastUsedFile = ".codex-incident-last-used"
)

// IncidentCachePolicy bounds alert-analysis repository caches. MaxBytes and
// MinFreeBytes include only the shared cache filesystem; worktrees are always
// temporary and additionally guarded by the free-space watermark.
type IncidentCachePolicy struct {
	FetchDepth      int
	MaxRepositories int
	MaxBytes        int64
	MaxIdle         time.Duration
	WorktreeTTL     time.Duration
	CleanupInterval time.Duration
	MinFreeBytes    int64
}

func DefaultIncidentCachePolicy() IncidentCachePolicy {
	return IncidentCachePolicy{
		FetchDepth:      200,
		MaxRepositories: 3,
		MaxBytes:        5 << 30,
		MaxIdle:         7 * 24 * time.Hour,
		WorktreeTTL:     time.Hour,
		CleanupInterval: 10 * time.Minute,
		MinFreeBytes:    1 << 30,
	}
}

func normalizeIncidentPolicy(p IncidentCachePolicy) IncidentCachePolicy {
	d := DefaultIncidentCachePolicy()
	if p.FetchDepth <= 0 {
		p.FetchDepth = d.FetchDepth
	}
	if p.MaxRepositories <= 0 {
		p.MaxRepositories = d.MaxRepositories
	}
	if p.MaxBytes <= 0 {
		p.MaxBytes = d.MaxBytes
	}
	if p.MaxIdle <= 0 {
		p.MaxIdle = d.MaxIdle
	}
	if p.WorktreeTTL <= 0 {
		p.WorktreeTTL = d.WorktreeTTL
	}
	if p.CleanupInterval <= 0 {
		p.CleanupInterval = d.CleanupInterval
	}
	if p.MinFreeBytes < 0 {
		p.MinFreeBytes = d.MinFreeBytes
	}
	return p
}

type incidentCacheState struct {
	mu     sync.Mutex
	active map[string]int
}

type incidentCacheEntry struct {
	key      string
	path     string
	lastUsed time.Time
	size     int64
}

func incidentKey(cloneURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(cloneURL)))
	return fmt.Sprintf("incident_%x", sum[:8])
}

func (c *Cache) currentIncidentPolicy() IncidentCachePolicy {
	if c.incidentPolicy == nil {
		return DefaultIncidentCachePolicy()
	}
	return normalizeIncidentPolicy(c.incidentPolicy())
}

func (c *Cache) markIncidentActive(key string, delta int) {
	c.incidentState.mu.Lock()
	defer c.incidentState.mu.Unlock()
	c.incidentState.active[key] += delta
	if c.incidentState.active[key] <= 0 {
		delete(c.incidentState.active, key)
	}
}

func (c *Cache) incidentIsActive(key string) bool {
	c.incidentState.mu.Lock()
	defer c.incidentState.mu.Unlock()
	return c.incidentState.active[key] > 0
}

// PrepareRevision creates a bounded shallow bare cache for one configured
// branch/SHA, then checks it out into an isolated temporary worktree. Unlike PR
// mirrors, it never fetches every branch, tag, pull ref, or the full history.
func (c *Cache) PrepareRevision(ctx context.Context, cloneURL, revision string, taskID int64) (worktree, resolvedSHA string, err error) {
	cloneURL = strings.TrimSpace(cloneURL)
	if cloneURL == "" {
		return "", "", fmt.Errorf("gitcache: empty repository URL")
	}
	revision = strings.TrimSpace(revision)
	if revision == "" {
		revision = "main"
	}
	key := incidentKey(cloneURL)
	c.markIncidentActive(key, 1)
	succeeded := false
	defer func() {
		if !succeeded {
			c.markIncidentActive(key, -1)
		}
	}()

	_ = c.pruneIncidentCache(ctx, false)
	policy := c.currentIncidentPolicy()
	if err := c.ensureIncidentFreeSpace(policy); err != nil {
		return "", "", err
	}

	unlock := c.locks.Lock(key)
	defer unlock()
	mirror := filepath.Join(c.cacheDir, key+".git")
	wt := filepath.Join(c.workDir, fmt.Sprintf("%s__task%d", key, taskID))
	if err := c.prepareIncidentBareRepo(ctx, mirror, cloneURL); err != nil {
		return "", "", err
	}
	if err := c.runGit(ctx, true, "-C", mirror, "fetch", "--force", "--no-tags", "--depth="+strconv.Itoa(policy.FetchDepth), "origin", revision); err != nil {
		return "", "", fmt.Errorf("gitcache: shallow fetch incident revision %q: %w", revision, err)
	}
	resolved, err := c.runGitOutput(ctx, false, "-C", mirror, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("gitcache: resolve fetched revision %q: %w", revision, err)
	}
	resolvedSHA = strings.TrimSpace(resolved)
	if err := c.runGit(ctx, false, "-C", mirror, "update-ref", "refs/codex/incident", resolvedSHA); err != nil {
		return "", "", fmt.Errorf("gitcache: retain fetched incident revision: %w", err)
	}
	if err := touchIncidentCache(mirror); err != nil {
		return "", "", err
	}
	if size, err := directorySize(mirror); err != nil {
		return "", "", err
	} else if policy.MaxBytes > 0 && size > policy.MaxBytes {
		_ = removeIncidentMirror(c.cacheDir, mirror)
		return "", "", fmt.Errorf("gitcache: incident repository cache is %d bytes, above %d-byte limit", size, policy.MaxBytes)
	}
	if err := c.ensureIncidentFreeSpace(policy); err != nil {
		_ = removeIncidentMirror(c.cacheDir, mirror)
		return "", "", err
	}
	if err := c.cleanWorktree(ctx, mirror, wt); err != nil {
		return "", "", fmt.Errorf("gitcache: clean stale incident worktree: %w", err)
	}
	if err := os.MkdirAll(c.workDir, 0o755); err != nil {
		return "", "", fmt.Errorf("gitcache: mkdir work dir: %w", err)
	}
	if err := c.runGit(ctx, true, "-C", mirror, "worktree", "add", "--force", "--detach", wt, resolvedSHA); err != nil {
		return "", "", fmt.Errorf("gitcache: add incident worktree: %w", err)
	}
	if err := c.ensureIncidentFreeSpace(policy); err != nil {
		_ = c.cleanWorktree(context.Background(), mirror, wt)
		return "", "", err
	}
	succeeded = true
	return wt, resolvedSHA, nil
}

func (c *Cache) prepareIncidentBareRepo(ctx context.Context, mirror, cloneURL string) error {
	version, _ := os.ReadFile(filepath.Join(mirror, incidentVersionFile))
	if dirExists(mirror) && strings.TrimSpace(string(version)) != incidentCacheVersion {
		if err := removeIncidentMirror(c.cacheDir, mirror); err != nil {
			return fmt.Errorf("gitcache: replace legacy full incident mirror: %w", err)
		}
	}
	if !dirExists(mirror) {
		if err := os.MkdirAll(c.cacheDir, 0o755); err != nil {
			return fmt.Errorf("gitcache: mkdir cache dir: %w", err)
		}
		if err := c.runGit(ctx, false, "init", "--bare", mirror); err != nil {
			return fmt.Errorf("gitcache: init shallow incident cache: %w", err)
		}
		if err := c.runGit(ctx, false, "-C", mirror, "remote", "add", "origin", cloneURL); err != nil {
			return fmt.Errorf("gitcache: add incident origin: %w", err)
		}
		if err := os.WriteFile(filepath.Join(mirror, incidentVersionFile), []byte(incidentCacheVersion+"\n"), 0o600); err != nil {
			return fmt.Errorf("gitcache: write incident cache version: %w", err)
		}
	} else if err := c.runGit(ctx, false, "-C", mirror, "remote", "set-url", "origin", cloneURL); err != nil {
		return fmt.Errorf("gitcache: update incident origin: %w", err)
	}
	return nil
}

// CleanupRevision always removes the temporary worktree, then makes the bare
// cache eligible for quota/LRU eviction.
func (c *Cache) CleanupRevision(cloneURL string, taskID int64) error {
	key := incidentKey(cloneURL)
	unlock := c.locks.Lock(key)
	mirror := filepath.Join(c.cacheDir, key+".git")
	wt := filepath.Join(c.workDir, fmt.Sprintf("%s__task%d", key, taskID))
	err := c.cleanWorktree(context.Background(), mirror, wt)
	if dirExists(mirror) {
		_ = touchIncidentCache(mirror)
	}
	unlock()
	c.markIncidentActive(key, -1)
	pruneErr := c.pruneIncidentCache(context.Background(), false)
	if err != nil {
		return err
	}
	return pruneErr
}

// RunIncidentJanitor continuously removes stale worktrees and evicts idle/LRU
// bare caches. Policy changes are picked up before scheduling every pass.
func (c *Cache) RunIncidentJanitor(ctx context.Context) error {
	for {
		if err := c.pruneIncidentCache(ctx, true); err != nil && !errors.Is(err, context.Canceled) {
			// Cache cleanup is best-effort; a later pass retries it.
		}
		interval := c.currentIncidentPolicy().CleanupInterval
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// PruneIncidentCache performs an immediate full janitor pass. It is also used
// by tests and operational tooling.
func (c *Cache) PruneIncidentCache(ctx context.Context) error {
	return c.pruneIncidentCache(ctx, true)
}

func (c *Cache) pruneIncidentCache(ctx context.Context, cleanWorktrees bool) error {
	if err := os.MkdirAll(c.cacheDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(c.workDir, 0o755); err != nil {
		return err
	}
	policy := c.currentIncidentPolicy()
	if cleanWorktrees {
		if err := c.removeStaleIncidentWorktrees(policy.WorktreeTTL); err != nil {
			return err
		}
	}
	entries, err := c.scanIncidentCaches()
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].lastUsed.Before(entries[j].lastUsed) })
	total := int64(0)
	for _, entry := range entries {
		total += entry.size
	}
	now := time.Now()
	remaining := len(entries)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		idleExpired := policy.MaxIdle > 0 && now.Sub(entry.lastUsed) > policy.MaxIdle
		overLimit := remaining > policy.MaxRepositories || total > policy.MaxBytes
		lowDisk := false
		if policy.MinFreeBytes > 0 {
			free, freeErr := filesystemFreeBytes(c.cacheDir)
			lowDisk = freeErr == nil && free < policy.MinFreeBytes
		}
		if !idleExpired && !overLimit && !lowDisk {
			continue
		}
		if c.incidentIsActive(entry.key) {
			continue
		}
		unlock := c.locks.Lock(entry.key)
		if !c.incidentIsActive(entry.key) {
			err = removeIncidentMirror(c.cacheDir, entry.path)
		}
		unlock()
		if err != nil {
			return err
		}
		remaining--
		total -= entry.size
	}
	return nil
}

func (c *Cache) scanIncidentCaches() ([]incidentCacheEntry, error) {
	dirs, err := os.ReadDir(c.cacheDir)
	if err != nil {
		return nil, err
	}
	var out []incidentCacheEntry
	for _, dir := range dirs {
		if !dir.IsDir() || !strings.HasPrefix(dir.Name(), "incident_") || !strings.HasSuffix(dir.Name(), ".git") {
			continue
		}
		path := filepath.Join(c.cacheDir, dir.Name())
		size, err := directorySize(path)
		if err != nil {
			return nil, err
		}
		lastUsed := time.Time{}
		if info, statErr := os.Stat(filepath.Join(path, incidentLastUsedFile)); statErr == nil {
			lastUsed = info.ModTime()
		} else if info, statErr = os.Stat(path); statErr == nil {
			lastUsed = info.ModTime()
		}
		out = append(out, incidentCacheEntry{key: strings.TrimSuffix(dir.Name(), ".git"), path: path, lastUsed: lastUsed, size: size})
	}
	return out, nil
}

func (c *Cache) removeStaleIncidentWorktrees(ttl time.Duration) error {
	dirs, err := os.ReadDir(c.workDir)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-ttl)
	for _, dir := range dirs {
		name := dir.Name()
		if !dir.IsDir() || !strings.HasPrefix(name, "incident_") || !strings.Contains(name, "__task") {
			continue
		}
		key := strings.SplitN(name, "__task", 2)[0]
		if c.incidentIsActive(key) {
			continue
		}
		info, err := dir.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		unlock := c.locks.Lock(key)
		if !c.incidentIsActive(key) {
			mirror := filepath.Join(c.cacheDir, key+".git")
			if err := c.cleanWorktree(context.Background(), mirror, filepath.Join(c.workDir, name)); err != nil {
				unlock()
				return err
			}
		}
		unlock()
	}
	return nil
}

func (c *Cache) ensureIncidentFreeSpace(policy IncidentCachePolicy) error {
	if policy.MinFreeBytes <= 0 {
		return nil
	}
	free, err := filesystemFreeBytes(c.cacheDir)
	if err != nil {
		return fmt.Errorf("gitcache: inspect free disk space: %w", err)
	}
	if free < policy.MinFreeBytes {
		return fmt.Errorf("gitcache: storage pressure: %d bytes free, minimum is %d", free, policy.MinFreeBytes)
	}
	return nil
}

func filesystemFreeBytes(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

func touchIncidentCache(mirror string) error {
	path := filepath.Join(mirror, incidentLastUsedFile)
	if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return fmt.Errorf("gitcache: update incident cache access time: %w", err)
	}
	return nil
}

func directorySize(root string) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func removeIncidentMirror(cacheDir, mirror string) error {
	cleanRoot, err := filepath.Abs(filepath.Clean(cacheDir))
	if err != nil {
		return err
	}
	cleanMirror, err := filepath.Abs(filepath.Clean(mirror))
	if err != nil {
		return err
	}
	name := filepath.Base(cleanMirror)
	if filepath.Dir(cleanMirror) != cleanRoot || !strings.HasPrefix(name, "incident_") || !strings.HasSuffix(name, ".git") {
		return fmt.Errorf("refusing to remove non-incident cache path %q", cleanMirror)
	}
	return os.RemoveAll(cleanMirror)
}
