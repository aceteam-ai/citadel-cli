package usage

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS job_usage (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id            TEXT NOT NULL UNIQUE,
    job_type          TEXT NOT NULL,
    backend           TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL,
    started_at        TEXT NOT NULL,
    completed_at      TEXT NOT NULL,
    duration_ms       INTEGER NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    request_bytes     INTEGER NOT NULL DEFAULT 0,
    response_bytes    INTEGER NOT NULL DEFAULT 0,
    error_message     TEXT NOT NULL DEFAULT '',
    node_id           TEXT NOT NULL DEFAULT '',
    synced            INTEGER NOT NULL DEFAULT 0,
    created_at        TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_job_usage_synced ON job_usage(synced) WHERE synced = 0;
`

// Store provides SQLite-backed storage for usage records.
type Store struct {
	db *sql.DB
}

// OpenStore opens (or creates) the usage database at dbPath and runs migrations.
func OpenStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open usage db: %w", err)
	}

	// Enable WAL mode for concurrent reads during sync
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	// Run schema migrations
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return &Store{db: db}, nil
}

// Insert stores a usage record. Duplicate job_id inserts are silently ignored.
func (s *Store) Insert(r UsageRecord) error {
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO job_usage (
			job_id, job_type, backend, model, status,
			started_at, completed_at, duration_ms,
			prompt_tokens, completion_tokens, total_tokens,
			request_bytes, response_bytes,
			error_message, node_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.JobID, r.JobType, r.Backend, r.Model, r.Status,
		r.StartedAt.UTC().Format(time.RFC3339), r.CompletedAt.UTC().Format(time.RFC3339), r.DurationMs,
		r.PromptTokens, r.CompletionTokens, r.TotalTokens,
		r.RequestBytes, r.ResponseBytes,
		r.ErrorMessage, r.NodeID,
	)
	if err != nil {
		return fmt.Errorf("insert usage record: %w", err)
	}
	return nil
}

// QueryUnsynced returns up to limit records that have not been synced.
func (s *Store) QueryUnsynced(limit int) ([]UsageRecord, error) {
	rows, err := s.db.Query(`
		SELECT id, job_id, job_type, backend, model, status,
		       started_at, completed_at, duration_ms,
		       prompt_tokens, completion_tokens, total_tokens,
		       request_bytes, response_bytes,
		       error_message, node_id
		FROM job_usage
		WHERE synced = 0
		ORDER BY id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query unsynced: %w", err)
	}
	defer rows.Close()

	var records []UsageRecord
	for rows.Next() {
		var r UsageRecord
		var startedAt, completedAt string
		if err := rows.Scan(
			&r.ID, &r.JobID, &r.JobType, &r.Backend, &r.Model, &r.Status,
			&startedAt, &completedAt, &r.DurationMs,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens,
			&r.RequestBytes, &r.ResponseBytes,
			&r.ErrorMessage, &r.NodeID,
		); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		if t, err := time.Parse(time.RFC3339, startedAt); err == nil {
			r.StartedAt = t
		}
		if t, err := time.Parse(time.RFC3339, completedAt); err == nil {
			r.CompletedAt = t
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// MarkSynced sets the synced flag to 1 for the given record IDs.
func (s *Store) MarkSynced(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("UPDATE job_usage SET synced = 1 WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare update: %w", err)
	}
	defer stmt.Close()

	for _, id := range ids {
		if _, err := stmt.Exec(id); err != nil {
			return fmt.Errorf("mark synced id=%d: %w", id, err)
		}
	}

	return tx.Commit()
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
