package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion is the current job_usage schema version, tracked in SQLite's
// PRAGMA user_version. v2 (aceteam#10876 C7) adds the work-attribution + typed-
// unit + receipt columns; v3 (citadel-cli#1287) adds the flow-attribution columns
// (flow_run_id / flow_node_id / delegated_node_id / node_uid / receipt_ids); v4
// (aceteam#11146 / citadel-cli#1291) adds the signed work-receipt bodies
// (receipts_json / receipts_truncated) so they survive the build -> Insert ->
// QueryUnsynced -> publish path and reach central's attestor. A fresh DB is
// created at the current version; an existing older DB is migrated idempotently by
// migrateSchema. This is a node-local SQLite file — no external migration tooling
// is involved, and every added column is NOT NULL DEFAULT ” so an OLDER binary
// reopening a newer DB still reads and writes cleanly.
const schemaVersion = 4

// schema is the full current-version table (applied to a fresh DB). An existing
// older DB is upgraded by migrateSchema's ADD COLUMN path, not by re-running this.
const schema = `
CREATE TABLE IF NOT EXISTS job_usage (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id                  TEXT NOT NULL UNIQUE,
    job_type                TEXT NOT NULL,
    backend                 TEXT NOT NULL DEFAULT '',
    model                   TEXT NOT NULL DEFAULT '',
    status                  TEXT NOT NULL,
    started_at              TEXT NOT NULL,
    completed_at            TEXT NOT NULL,
    duration_ms             INTEGER NOT NULL,
    prompt_tokens           INTEGER NOT NULL DEFAULT 0,
    completion_tokens       INTEGER NOT NULL DEFAULT 0,
    total_tokens            INTEGER NOT NULL DEFAULT 0,
    request_bytes           INTEGER NOT NULL DEFAULT 0,
    response_bytes          INTEGER NOT NULL DEFAULT 0,
    error_message           TEXT NOT NULL DEFAULT '',
    node_id                 TEXT NOT NULL DEFAULT '',
    origin                  TEXT NOT NULL DEFAULT '',
    org_id                  TEXT NOT NULL DEFAULT '',
    work_action             TEXT NOT NULL DEFAULT '',
    units_json              TEXT NOT NULL DEFAULT '',
    receipt_manifest_sha256 TEXT NOT NULL DEFAULT '',
    receipt_signed          INTEGER NOT NULL DEFAULT 0,
    flow_run_id             TEXT NOT NULL DEFAULT '',
    flow_node_id            TEXT NOT NULL DEFAULT '',
    delegated_node_id       TEXT NOT NULL DEFAULT '',
    node_uid                TEXT NOT NULL DEFAULT '',
    receipt_ids             TEXT NOT NULL DEFAULT '',
    receipts_json           TEXT NOT NULL DEFAULT '',
    receipts_truncated      INTEGER NOT NULL DEFAULT 0,
    synced                  INTEGER NOT NULL DEFAULT 0,
    created_at              TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_job_usage_synced ON job_usage(synced) WHERE synced = 0;
`

// v2Columns are the columns added in schema v2, applied via ADD COLUMN to an
// existing v1 DB. Each is nullable-or-defaulted so old rows read cleanly.
var v2Columns = []struct{ name, ddl string }{
	{"origin", "ALTER TABLE job_usage ADD COLUMN origin TEXT NOT NULL DEFAULT ''"},
	{"org_id", "ALTER TABLE job_usage ADD COLUMN org_id TEXT NOT NULL DEFAULT ''"},
	{"work_action", "ALTER TABLE job_usage ADD COLUMN work_action TEXT NOT NULL DEFAULT ''"},
	{"units_json", "ALTER TABLE job_usage ADD COLUMN units_json TEXT NOT NULL DEFAULT ''"},
	{"receipt_manifest_sha256", "ALTER TABLE job_usage ADD COLUMN receipt_manifest_sha256 TEXT NOT NULL DEFAULT ''"},
	{"receipt_signed", "ALTER TABLE job_usage ADD COLUMN receipt_signed INTEGER NOT NULL DEFAULT 0"},
}

// v3Columns are the flow-attribution columns added in schema v3 (citadel-cli#1287),
// applied via ADD COLUMN to an existing v1/v2 DB. All NOT NULL DEFAULT ” so both
// old rows and an older binary reopening the DB read and write cleanly.
var v3Columns = []struct{ name, ddl string }{
	{"flow_run_id", "ALTER TABLE job_usage ADD COLUMN flow_run_id TEXT NOT NULL DEFAULT ''"},
	{"flow_node_id", "ALTER TABLE job_usage ADD COLUMN flow_node_id TEXT NOT NULL DEFAULT ''"},
	{"delegated_node_id", "ALTER TABLE job_usage ADD COLUMN delegated_node_id TEXT NOT NULL DEFAULT ''"},
	{"node_uid", "ALTER TABLE job_usage ADD COLUMN node_uid TEXT NOT NULL DEFAULT ''"},
	{"receipt_ids", "ALTER TABLE job_usage ADD COLUMN receipt_ids TEXT NOT NULL DEFAULT ''"},
}

// v4Columns are the signed work-receipt-body columns added in schema v4
// (aceteam#11146 / citadel-cli#1291), applied via ADD COLUMN to an existing
// v1/v2/v3 DB. Both NOT NULL DEFAULT so old rows and an older binary reopening the
// DB read and write cleanly.
var v4Columns = []struct{ name, ddl string }{
	{"receipts_json", "ALTER TABLE job_usage ADD COLUMN receipts_json TEXT NOT NULL DEFAULT ''"},
	{"receipts_truncated", "ALTER TABLE job_usage ADD COLUMN receipts_truncated INTEGER NOT NULL DEFAULT 0"},
}

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

	// `citadel work` holds this DB open (WAL) while a separate `citadel rag index`
	// process inserts a local_cli record; a busy_timeout lets that insert wait for
	// the writer lock instead of failing with SQLITE_BUSY (aceteam#10876 C7).
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}

	// Create the table at the current version (no-op if it already exists), then
	// upgrade an older DB to the current schema version.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if err := migrateSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	return &Store{db: db}, nil
}

// migrateSchema brings an existing job_usage table up to schemaVersion. It is
// idempotent: a fresh DB (created from `schema` above, already current) only has
// its user_version stamped; a v1 DB gets each missing v2 column added. Columns
// are checked against PRAGMA table_info first because SQLite errors on a
// duplicate ADD COLUMN (and older SQLite lacks ADD COLUMN IF NOT EXISTS).
func migrateSchema(db *sql.DB) error {
	var userVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if userVersion >= schemaVersion {
		return nil
	}

	existing, err := tableColumns(db, "job_usage")
	if err != nil {
		return err
	}
	// Each added column is check-then-ADD against table_info (SQLite errors on a
	// duplicate ADD COLUMN and older SQLite lacks ADD COLUMN IF NOT EXISTS), so a
	// v1->v4 / v2->v4 / v3->v4 upgrade applies only the columns actually missing.
	var toAdd []struct{ name, ddl string }
	toAdd = append(toAdd, v2Columns...)
	toAdd = append(toAdd, v3Columns...)
	toAdd = append(toAdd, v4Columns...)
	for _, col := range toAdd {
		if _, ok := existing[col.name]; ok {
			continue
		}
		if _, err := db.Exec(col.ddl); err != nil {
			return fmt.Errorf("add column %s: %w", col.name, err)
		}
	}

	// PRAGMA user_version does not accept a bind parameter.
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}

// tableColumns returns the set of column names on table.
func tableColumns(db *sql.DB, table string) (map[string]struct{}, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("read table_info: %w", err)
	}
	defer rows.Close()
	cols := make(map[string]struct{})
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notNull    int
			dfltValue  sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("scan table_info: %w", err)
		}
		cols[name] = struct{}{}
	}
	return cols, rows.Err()
}

// Insert stores a usage record. Duplicate job_id inserts are silently ignored
// (INSERT OR IGNORE on the unique job_id) — the dedup that makes outbox replay
// exactly-once at the ledger.
func (s *Store) Insert(r UsageRecord) error {
	unitsJSON := ""
	if len(r.Units) > 0 {
		b, err := json.Marshal(r.Units)
		if err != nil {
			return fmt.Errorf("marshal units: %w", err)
		}
		unitsJSON = string(b)
	}
	receiptIDsJSON := ""
	if len(r.ReceiptIDs) > 0 {
		b, err := json.Marshal(r.ReceiptIDs)
		if err != nil {
			return fmt.Errorf("marshal receipt_ids: %w", err)
		}
		receiptIDsJSON = string(b)
	}
	receiptsJSON := ""
	if len(r.Receipts) > 0 {
		b, err := json.Marshal(r.Receipts)
		if err != nil {
			return fmt.Errorf("marshal receipts: %w", err)
		}
		receiptsJSON = string(b)
	}
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO job_usage (
			job_id, job_type, backend, model, status,
			started_at, completed_at, duration_ms,
			prompt_tokens, completion_tokens, total_tokens,
			request_bytes, response_bytes,
			error_message, node_id,
			origin, org_id, work_action, units_json,
			receipt_manifest_sha256, receipt_signed,
			flow_run_id, flow_node_id, delegated_node_id, node_uid, receipt_ids,
			receipts_json, receipts_truncated
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.JobID, r.JobType, r.Backend, r.Model, r.Status,
		r.StartedAt.UTC().Format(time.RFC3339), r.CompletedAt.UTC().Format(time.RFC3339), r.DurationMs,
		r.PromptTokens, r.CompletionTokens, r.TotalTokens,
		r.RequestBytes, r.ResponseBytes,
		r.ErrorMessage, r.NodeID,
		r.Origin, r.OrgID, r.WorkAction, unitsJSON,
		r.ReceiptManifestSHA256, boolToInt(r.ReceiptSigned),
		r.FlowRunID, r.FlowNodeID, r.DelegatedNodeID, r.NodeUID, receiptIDsJSON,
		receiptsJSON, r.ReceiptsTruncated,
	)
	if err != nil {
		return fmt.Errorf("insert usage record: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// QueryUnsynced returns up to limit records that have not been synced.
func (s *Store) QueryUnsynced(limit int) ([]UsageRecord, error) {
	rows, err := s.db.Query(`
		SELECT id, job_id, job_type, backend, model, status,
		       started_at, completed_at, duration_ms,
		       prompt_tokens, completion_tokens, total_tokens,
		       request_bytes, response_bytes,
		       error_message, node_id,
		       origin, org_id, work_action, units_json,
		       receipt_manifest_sha256, receipt_signed,
		       flow_run_id, flow_node_id, delegated_node_id, node_uid, receipt_ids,
		       receipts_json, receipts_truncated
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
		var startedAt, completedAt, unitsJSON, receiptIDsJSON, receiptsJSON string
		var receiptSigned int
		if err := rows.Scan(
			&r.ID, &r.JobID, &r.JobType, &r.Backend, &r.Model, &r.Status,
			&startedAt, &completedAt, &r.DurationMs,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens,
			&r.RequestBytes, &r.ResponseBytes,
			&r.ErrorMessage, &r.NodeID,
			&r.Origin, &r.OrgID, &r.WorkAction, &unitsJSON,
			&r.ReceiptManifestSHA256, &receiptSigned,
			&r.FlowRunID, &r.FlowNodeID, &r.DelegatedNodeID, &r.NodeUID, &receiptIDsJSON,
			&receiptsJSON, &r.ReceiptsTruncated,
		); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		if t, err := time.Parse(time.RFC3339, startedAt); err == nil {
			r.StartedAt = t
		}
		if t, err := time.Parse(time.RFC3339, completedAt); err == nil {
			r.CompletedAt = t
		}
		r.ReceiptSigned = receiptSigned != 0
		if unitsJSON != "" {
			var units map[string]int64
			if err := json.Unmarshal([]byte(unitsJSON), &units); err == nil {
				r.Units = units
			}
		}
		if receiptIDsJSON != "" {
			var ids []string
			if err := json.Unmarshal([]byte(receiptIDsJSON), &ids); err == nil {
				r.ReceiptIDs = ids
			}
		}
		if receiptsJSON != "" {
			var receipts []map[string]any
			if err := json.Unmarshal([]byte(receiptsJSON), &receipts); err == nil {
				r.Receipts = receipts
			}
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
