// Package store implements model.Store on top of SQLite (modernc.org/sqlite,
// pure Go, no CGO). It persists the job queue, per-PR session state, findings,
// and console-editable settings.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/turning4th/codex-gitea/internal/secretbox"
	_ "modernc.org/sqlite"

	"github.com/turning4th/codex-gitea/internal/model"
)

//go:embed schema.sql
var schemaSQL string

// Store is the SQLite-backed implementation of model.Store.
type Store struct {
	db        *sql.DB
	secretBox *secretbox.Box
}

var _ model.Store = (*Store)(nil)

// Open opens (creating if needed) the SQLite database at dbPath, enables WAL,
// and applies the embedded schema.
type Option func(*Store) error

// WithSecretKey enables AES-GCM encryption for console-managed integration
// credentials. Existing callers may omit it when they do not use that feature.
func WithSecretKey(key string) Option {
	return func(s *Store) error {
		if strings.TrimSpace(key) == "" {
			return nil
		}
		box, err := secretbox.New(key)
		if err != nil {
			return err
		}
		s.secretBox = box
		return nil
	}
}

func Open(dbPath string, opts ...Option) (*Store, error) {
	db, err := sql.Open("sqlite", sqliteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// WAL allows readers to proceed while one writer is active. Keep the pool
	// bounded so console reads do not queue behind every worker write, while
	// still avoiding unbounded SQLite connections under bursty webhooks.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign_keys: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure store: %w", err)
		}
	}
	return s, nil
}

func sqliteDSN(dbPath string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "journal_mode(WAL)")

	sep := "?"
	if strings.Contains(dbPath, "?") {
		sep = "&"
	}
	return dbPath + sep + q.Encode()
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// querier abstracts over *sql.DB and *sql.Tx so helpers work in or out of a tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// nowRFC3339 returns the current time as an RFC3339 string (UTC).
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// parseTime parses an RFC3339 timestamp, returning the zero time on failure.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// nullableTime turns a nullable RFC3339 column into a *time.Time.
func nullableTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseTime(ns.String)
	return &t
}

func migrateSchema(db *sql.DB) error {
	hadDuplicateTaskColumn, err := columnExists(db, "analysis_tasks", "duplicate_of_task_id")
	if err != nil {
		return err
	}
	for _, col := range []struct {
		table string
		name  string
		def   string
	}{
		{table: "findings", name: "title", def: "TEXT"},
		{table: "findings", name: "body", def: "TEXT"},
		{table: "findings", name: "review_run_id", def: "INTEGER"},
		{table: "findings", name: "agent", def: "TEXT DEFAULT 'codex'"},
		{table: "findings", name: "last_seen_sha", def: "TEXT"},
		{table: "findings", name: "mapped_inline", def: "INTEGER DEFAULT 0"},
		{table: "findings", name: "tags", def: "TEXT"},
		{table: "pulls", name: "author", def: "TEXT"},
		{table: "jobs", name: "error_type", def: "TEXT"},
		{table: "jobs", name: "retryable", def: "INTEGER DEFAULT 0"},
		{table: "jobs", name: "next_attempt_at", def: "TEXT"},
		{table: "review_runs", name: "error_type", def: "TEXT"},
		{table: "analysis_tasks", name: "duplicate_of_task_id", def: "INTEGER REFERENCES analysis_tasks(id) ON DELETE SET NULL"},
		{table: "alert_analysis_configs", name: "feishu_mode", def: "TEXT NOT NULL DEFAULT 'webhook'"},
		{table: "alert_analysis_configs", name: "feishu_app_id", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "alert_analysis_configs", name: "feishu_app_secret", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "alert_analysis_configs", name: "feishu_chat_id", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "alert_analysis_configs", name: "feishu_mention_mapping", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "alert_analysis_configs", name: "ignored_error_codes", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "alert_analysis_configs", name: "concurrency", def: "INTEGER NOT NULL DEFAULT 2"},
		{table: "analysis_tasks", name: "feishu_message_id", def: "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureColumn(db, col.table, col.name, col.def); err != nil {
			return err
		}
	}
	if !hadDuplicateTaskColumn {
		// One-time upgrade from the original five-run throttle. Existing custom
		// fingerprints are preserved; only the shipped defaults are narrowed.
		if _, err := db.Exec(`UPDATE alert_analysis_configs
			SET throttle_threshold=1,
				throttle_cooldown_seconds=CASE WHEN throttle_cooldown_seconds=600 THEN 0 ELSE throttle_cooldown_seconds END,
				throttle_fields=CASE WHEN throttle_fields='environment,service,method,endpoint,error_code,error_message'
					THEN 'method,endpoint,error_code,error_message' ELSE throttle_fields END
			WHERE throttle_threshold=5`); err != nil {
			return fmt.Errorf("migrate alert analysis duplicate throttle: %w", err)
		}
	}
	if _, err := db.Exec(`UPDATE findings SET agent='codex' WHERE agent IS NULL OR agent=''`); err != nil {
		return fmt.Errorf("migrate findings agent: %w", err)
	}
	if _, err := db.Exec(`UPDATE findings SET last_seen_sha=first_seen_sha WHERE last_seen_sha IS NULL OR last_seen_sha=''`); err != nil {
		return fmt.Errorf("migrate findings last_seen_sha: %w", err)
	}
	if _, err := db.Exec(`UPDATE findings SET fingerprint='codex:' || fingerprint WHERE fingerprint NOT LIKE '%:%'`); err != nil {
		return fmt.Errorf("migrate findings fingerprints: %w", err)
	}
	if _, err := db.Exec(`
		INSERT INTO pull_reviewer_states(pull_id,agent,session_id,head_sha,base_ref,last_review_id,updated_at)
		SELECT id,'codex',session_id,head_sha,base_ref,last_review_id,updated_at
		FROM pulls
		WHERE COALESCE(session_id,'')<>'' OR COALESCE(last_review_id,0)<>0
		ON CONFLICT(pull_id,agent) DO NOTHING`); err != nil {
		return fmt.Errorf("migrate pull reviewer states: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS project_skills(
		id INTEGER PRIMARY KEY,
		owner TEXT NOT NULL,
		repo TEXT NOT NULL,
		slug TEXT NOT NULL,
		title TEXT NOT NULL,
		content TEXT NOT NULL,
		version INTEGER NOT NULL DEFAULT 1,
		source_finding_count INTEGER NOT NULL DEFAULT 0,
		created_at TEXT,
		updated_at TEXT,
		UNIQUE(owner,repo))`); err != nil {
		return fmt.Errorf("migrate project skills: %w", err)
	}
	return nil
}

func columnExists(db *sql.DB, table, name string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, fmt.Errorf("inspect %s schema: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			colName   string
			colType   string
			notNull   int
			defaultV  sql.NullString
			primaryKy int
		)
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultV, &primaryKy); err != nil {
			return false, fmt.Errorf("scan %s schema: %w", table, err)
		}
		if colName == name {
			return true, nil
		}
	}
	return false, rows.Err()
}

func ensureColumn(db *sql.DB, table, name, def string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			colName   string
			colType   string
			notNull   int
			defaultV  sql.NullString
			primaryKy int
		)
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultV, &primaryKy); err != nil {
			return fmt.Errorf("scan %s schema: %w", table, err)
		}
		if colName == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate %s schema: %w", table, err)
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + name + ` ` + def); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, name, err)
	}
	return nil
}

// ensureRepo upserts a repos row by (owner,name) and returns its id.
func ensureRepo(ctx context.Context, q querier, owner, name string) (int64, error) {
	if _, err := q.ExecContext(ctx,
		`INSERT INTO repos(owner,name) VALUES(?,?) ON CONFLICT(owner,name) DO NOTHING`,
		owner, name); err != nil {
		return 0, fmt.Errorf("ensure repo: %w", err)
	}
	var id int64
	if err := q.QueryRowContext(ctx,
		`SELECT id FROM repos WHERE owner=? AND name=?`, owner, name).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup repo id: %w", err)
	}
	return id, nil
}

// ensurePull upserts repos+pulls rows for pr and returns the pull id.
func ensurePull(ctx context.Context, q querier, pr model.PRRef) (int64, error) {
	repoID, err := ensureRepo(ctx, q, pr.Owner, pr.Repo)
	if err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO pulls(repo_id,number,updated_at) VALUES(?,?,?)
		 ON CONFLICT(repo_id,number) DO NOTHING`,
		repoID, pr.Number, nowRFC3339()); err != nil {
		return 0, fmt.Errorf("ensure pull: %w", err)
	}
	var id int64
	if err := q.QueryRowContext(ctx,
		`SELECT id FROM pulls WHERE repo_id=? AND number=?`, repoID, pr.Number).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup pull id: %w", err)
	}
	return id, nil
}

// lookupRepoID returns the repo id for owner/name, ok=false if absent.
func lookupRepoID(ctx context.Context, q querier, owner, name string) (int64, bool, error) {
	var id int64
	err := q.QueryRowContext(ctx,
		`SELECT id FROM repos WHERE owner=? AND name=?`, owner, name).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}
