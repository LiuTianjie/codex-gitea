// Package gitcache manages per-repository bare mirrors and throwaway worktrees.
//
// Each repository is cloned once as a bare --mirror (so large repos are never
// re-cloned), then incrementally fetched on each event. Worktrees are checked
// out at deterministic paths so codex can resume against a stable cwd, and are
// torn down after review while the mirror is retained. Fetch/worktree work is
// serialized per repository via a keyed mutex.
package gitcache

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/turning4th/codex-gitea/internal/model"
)

// Cache is the filesystem-backed implementation of model.GitCache.
type Cache struct {
	cacheDir string // holds bare mirrors: <cacheDir>/<key>.git
	workDir  string // holds worktrees:   <workDir>/<key>__pr<n>

	// tokenFunc, if set, supplies a credential injected per-command via
	// `-c http.extraHeader=...`. It is never written to any .git/config.
	tokenFunc func() (string, error)

	locks *keyedMutex
}

var _ model.GitCache = (*Cache)(nil)

// Option configures a Cache.
type Option func(*Cache)

// WithToken injects a static credential for cloning/fetching private repos.
// The token is passed on the command line (http.extraHeader) for network
// commands only, so it never lands in the mirror's persistent config.
func WithToken(token string) Option {
	return func(c *Cache) {
		c.tokenFunc = func() (string, error) { return token, nil }
	}
}

// WithTokenFunc injects a credential lazily (e.g. for short-lived tokens). It
// is called once per network git command; returning an error aborts the op.
func WithTokenFunc(f func() (string, error)) Option {
	return func(c *Cache) { c.tokenFunc = f }
}

// New returns a Cache rooted at cacheDir (mirrors) and workDir (worktrees).
func New(cacheDir, workDir string, opts ...Option) *Cache {
	c := &Cache{
		cacheDir: cacheDir,
		workDir:  workDir,
		locks:    newKeyedMutex(),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// mirrorPath is the bare mirror directory for pr's repository.
func (c *Cache) mirrorPath(pr model.PRRef) string {
	return filepath.Join(c.cacheDir, pr.Key()+".git")
}

// worktreePath is the deterministic worktree directory for pr.
func (c *Cache) worktreePath(pr model.PRRef) string {
	return filepath.Join(c.workDir, fmt.Sprintf("%s__pr%d", pr.Key(), pr.Number))
}

// Prepare ensures the mirror exists, fetches the latest refs, and checks out a
// clean detached worktree at headSHA. baseRef/headRef are accepted for the
// contract; the mirror's +refs/*:refs/* fetch already refreshes the base
// branch and every refs/pull/*/head, so listing them explicitly is redundant.
func (c *Cache) Prepare(ctx context.Context, pr model.PRRef, cloneURL, baseRef, headRef, headSHA string) (string, error) {
	if headSHA == "" {
		return "", fmt.Errorf("gitcache: empty headSHA for %s#%d", pr.Key(), pr.Number)
	}

	unlock := c.locks.Lock(pr.Key())
	defer unlock()

	mirror := c.mirrorPath(pr)
	wt := c.worktreePath(pr)

	// 1. Ensure the bare mirror exists.
	if !dirExists(mirror) {
		if err := os.MkdirAll(c.cacheDir, 0o755); err != nil {
			return "", fmt.Errorf("gitcache: mkdir cache dir: %w", err)
		}
		if err := c.runGit(ctx, true, "clone", "--mirror", cloneURL, mirror); err != nil {
			return "", fmt.Errorf("gitcache: clone mirror %s: %w", pr.Key(), err)
		}
	} else {
		// The repository URL can change when Gitea moves to a new host or port.
		// A persisted mirror otherwise keeps fetching its original URL forever.
		if err := c.runGit(ctx, false, "-C", mirror, "remote", "set-url", "origin", cloneURL); err != nil {
			return "", fmt.Errorf("gitcache: update origin %s: %w", pr.Key(), err)
		}
	}

	// 2. Incremental sync. A --mirror clone sets remote.origin.mirror=true
	// (fetch = +refs/*:refs/*), so this refreshes baseRef and all pull refs,
	// making headSHA reachable.
	if err := c.runGit(ctx, true, "-C", mirror, "fetch", "--prune", "origin"); err != nil {
		return "", fmt.Errorf("gitcache: fetch %s: %w", pr.Key(), err)
	}

	// 3. Rebuild the worktree so it holds exactly headSHA, never stale state.
	if err := c.cleanWorktree(ctx, mirror, wt); err != nil {
		return "", fmt.Errorf("gitcache: clean stale worktree %s: %w", pr.Key(), err)
	}
	if err := os.MkdirAll(c.workDir, 0o755); err != nil {
		return "", fmt.Errorf("gitcache: mkdir work dir: %w", err)
	}
	if err := c.runGit(ctx, false, "-C", mirror, "worktree", "add", "--force", "--detach", wt, headSHA); err != nil {
		return "", fmt.Errorf("gitcache: worktree add %s at %s: %w", pr.Key(), headSHA, err)
	}

	return wt, nil
}

// Cleanup removes pr's worktree (and any stale registration) but keeps the
// mirror. Missing worktrees are not an error.
func (c *Cache) Cleanup(pr model.PRRef) error {
	unlock := c.locks.Lock(pr.Key())
	defer unlock()

	if err := c.cleanWorktree(context.Background(), c.mirrorPath(pr), c.worktreePath(pr)); err != nil {
		return fmt.Errorf("gitcache: cleanup worktree %s: %w", pr.Key(), err)
	}
	return nil
}

// cleanWorktree best-effort unregisters wt from the mirror, prunes stale
// metadata, then guarantees the directory is gone. git errors (e.g. "not a
// working tree" when already removed) are intentionally ignored; only a real
// filesystem removal failure is surfaced.
func (c *Cache) cleanWorktree(ctx context.Context, mirror, wt string) error {
	if dirExists(mirror) {
		_ = c.runGit(ctx, false, "-C", mirror, "worktree", "remove", "--force", wt)
		_ = c.runGit(ctx, false, "-C", mirror, "worktree", "prune")
	}
	if err := os.RemoveAll(wt); err != nil {
		return fmt.Errorf("remove worktree dir %s: %w", wt, err)
	}
	return nil
}

// runGit executes a git command. When useToken is true and a token source is
// configured, the credential is injected for that single invocation via
// `-c http.extraHeader`, keeping it out of any persisted config and out of
// error messages (only the non-credential args are echoed).
func (c *Cache) runGit(ctx context.Context, useToken bool, args ...string) error {
	full := make([]string, 0, len(args)+2)
	if useToken && c.tokenFunc != nil {
		tok, err := c.tokenFunc()
		if err != nil {
			return fmt.Errorf("obtain git token: %w", err)
		}
		if tok != "" {
			full = append(full, "-c", "http.extraHeader=Authorization: token "+tok)
		}
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, "git", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return nil
}

// dirExists reports whether p exists and is a directory.
func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
