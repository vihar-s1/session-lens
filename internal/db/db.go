// Package db owns the SQLite connection, schema bootstrap, and the
// SessionEvent CRUD used by the API layer.
package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  project_path TEXT,
  started_at TEXT,
  ended_at TEXT NOT NULL,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  total_cost_usd REAL NOT NULL DEFAULT 0,
  model TEXT,
  turns INTEGER NOT NULL DEFAULT 0,
  raw_payload TEXT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_sessions_ended_at ON sessions(ended_at);
CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_path);
CREATE INDEX IF NOT EXISTS idx_sessions_project_ended_at ON sessions(project_path, ended_at);
CREATE INDEX IF NOT EXISTS idx_sessions_model_ended_at ON sessions(model, ended_at);
CREATE INDEX IF NOT EXISTS idx_sessions_created_at ON sessions(created_at);

CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Per-assistant-turn usage. One row per assistant message with a usage payload.
-- (session_id, turn_idx) is the idempotency key — replaying ingestion for a
-- session inserts the same rows. INSERT OR IGNORE handles repeated Stop hooks
-- on a growing transcript: existing turn_idxes stay, new ones append.
CREATE TABLE IF NOT EXISTS turns (
  session_id TEXT NOT NULL,
  turn_idx INTEGER NOT NULL,
  ended_at TEXT NOT NULL,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0,
  model TEXT,
  PRIMARY KEY (session_id, turn_idx)
);
CREATE INDEX IF NOT EXISTS idx_turns_ended_at ON turns(ended_at);
CREATE INDEX IF NOT EXISTS idx_turns_session_ended_at ON turns(session_id, ended_at);

-- User-authored markers on the timeline (e.g., "started optimization
-- experiment", "switched primary model"). Surfaced in the Annotations
-- page and as vertical-line overlays on time-series charts. The id is
-- AUTOINCREMENT so deleted IDs aren't reused, which keeps client-side
-- caches honest after a delete.
CREATE TABLE IF NOT EXISTS annotations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL,
  title TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT 'event',
  color TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_annotations_at ON annotations(at);
`

// Session is the persisted row plus identity.
type Session struct {
	ID               string  `json:"id"`
	ProjectPath      string  `json:"project_path"`
	StartedAt        string  `json:"started_at"`
	EndedAt          string  `json:"ended_at"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	Model            string  `json:"model"`
	Turns            int     `json:"turns"`
	RawPayload       string  `json:"raw_payload,omitempty"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
}

// Open returns a connection pool to the given path (or ":memory:") with the
// schema applied. Caller owns Close().
func Open(path string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	conn.SetMaxOpenConns(8)
	conn.SetMaxIdleConns(4)
	conn.SetConnMaxLifetime(30 * time.Minute)

	if err := Bootstrap(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Bootstrap runs the schema DDL. Safe to call repeatedly.
func Bootstrap(conn *sql.DB) error {
	if _, err := conn.Exec(schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// UpsertSession inserts or updates by primary key (session id). Returns
// (session, inserted) where inserted is true on a fresh insert, false on
// update.
func UpsertSession(conn *sql.DB, s Session) (Session, bool, error) {
	// Detect existence first so we can report inserted vs updated.
	var exists int
	err := conn.QueryRow(`SELECT COUNT(1) FROM sessions WHERE id = ?`, s.ID).Scan(&exists)
	if err != nil {
		return Session{}, false, fmt.Errorf("check exists: %w", err)
	}

	const stmt = `
INSERT INTO sessions (
  id, project_path, started_at, ended_at,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
  total_cost_usd, model, turns, raw_payload
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  project_path = excluded.project_path,
  started_at = excluded.started_at,
  ended_at = excluded.ended_at,
  input_tokens = excluded.input_tokens,
  output_tokens = excluded.output_tokens,
  cache_read_tokens = excluded.cache_read_tokens,
  cache_write_tokens = excluded.cache_write_tokens,
  total_cost_usd = excluded.total_cost_usd,
  model = excluded.model,
  turns = excluded.turns,
  raw_payload = excluded.raw_payload,
  updated_at = CURRENT_TIMESTAMP
`
	if _, err := conn.Exec(stmt,
		s.ID, s.ProjectPath, s.StartedAt, s.EndedAt,
		s.InputTokens, s.OutputTokens, s.CacheReadTokens, s.CacheWriteTokens,
		s.TotalCostUSD, s.Model, s.Turns, s.RawPayload,
	); err != nil {
		return Session{}, false, fmt.Errorf("upsert: %w", err)
	}

	out, err := GetSession(conn, s.ID)
	if err != nil {
		return Session{}, false, err
	}
	return out, exists == 0, nil
}

// ListSessions returns the most-recent sessions ordered by ended_at DESC.
// limit is capped at 100; pass 0 to get the default of 20.
func ListSessions(conn *sql.DB, limit int) ([]Session, error) {
	return ListSessionsFiltered(conn, limit, "", "")
}

// ListSessionsFiltered is ListSessions plus optional ended_at range filter.
// from/to are inclusive RFC3339 strings (UTC); pass "" to omit either bound.
// Lexicographic comparison is correct because all timestamps are stored
// `Z`-suffixed by the hook.
func ListSessionsFiltered(conn *sql.DB, limit int, from, to string) ([]Session, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 10000 {
		limit = 10000
	}
	stmt := `
SELECT id, COALESCE(project_path,''), COALESCE(started_at,''), ended_at,
       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
       total_cost_usd, COALESCE(model,''), turns, COALESCE(raw_payload,''),
       created_at, updated_at
FROM sessions WHERE 1=1`
	args := []any{}
	if from != "" {
		stmt += ` AND ended_at >= ?`
		args = append(args, from)
	}
	if to != "" {
		stmt += ` AND ended_at <= ?`
		args = append(args, to)
	}
	stmt += ` ORDER BY ended_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := conn.Query(stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(
			&s.ID, &s.ProjectPath, &s.StartedAt, &s.EndedAt,
			&s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.CacheWriteTokens,
			&s.TotalCostUSD, &s.Model, &s.Turns, &s.RawPayload,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteSessions removes sessions whose ended_at falls in [from, to] and
// cascades to the matching turns rows. Either bound can be empty to disable
// that side. With both empty the call deletes every row. Returns the number
// of session rows removed.
func DeleteSessions(conn *sql.DB, from, to string) (int64, error) {
	where := ` WHERE 1=1`
	args := []any{}
	if from != "" {
		where += ` AND ended_at >= ?`
		args = append(args, from)
	}
	if to != "" {
		where += ` AND ended_at <= ?`
		args = append(args, to)
	}
	// Cascade turns first so we never orphan rows when the session delete
	// succeeds but a subsequent failure rolls things back partially.
	tx, err := conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	if _, err := tx.Exec(
		`DELETE FROM turns WHERE session_id IN (SELECT id FROM sessions`+where+`)`,
		args...,
	); err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("delete turns cascade: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM sessions`+where, args...)
	if err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("delete sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit delete: %w", err)
	}
	return n, nil
}

// Turn is one assistant-message-level usage row in the turns table.
type Turn struct {
	SessionID        string
	Idx              int
	EndedAt          string // RFC3339 UTC
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
}

// InsertTurns adds turn rows for a session. Uses INSERT OR IGNORE on
// (session_id, turn_idx) so replays from a re-firing Stop hook just append
// new turns instead of duplicating existing ones. All rows go through a
// single transaction for write throughput on long transcripts.
func InsertTurns(conn *sql.DB, rows []Turn) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	stmt, err := tx.Prepare(`
INSERT OR IGNORE INTO turns (
  session_id, turn_idx, ended_at, model,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("prepare turn insert: %w", err)
	}
	defer stmt.Close()
	for _, t := range rows {
		if _, err := stmt.Exec(
			t.SessionID, t.Idx, t.EndedAt, t.Model,
			t.InputTokens, t.OutputTokens, t.CacheReadTokens, t.CacheWriteTokens, t.CostUSD,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("insert turn %s/%d: %w", t.SessionID, t.Idx, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit turns: %w", err)
	}
	return nil
}

// DeleteTurnsBySession removes every turn row for a session. Used when the
// session itself is purged via DeleteSessions so the per-turn data doesn't
// linger as an orphan.
func DeleteTurnsBySession(conn *sql.DB, sessionID string) error {
	_, err := conn.Exec(`DELETE FROM turns WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("delete turns: %w", err)
	}
	return nil
}

// ListSettings returns every key/value pair in the settings table.
func ListSettings(conn *sql.DB) (map[string]string, error) {
	rows, err := conn.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// PutSetting upserts a single key/value pair.
func PutSetting(conn *sql.DB, key, value string) error {
	const stmt = `
INSERT INTO settings (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP
`
	if _, err := conn.Exec(stmt, key, value); err != nil {
		return fmt.Errorf("put setting %q: %w", key, err)
	}
	return nil
}

// Annotation is one user-authored marker on the session-lens timeline.
// `At` is RFC3339 UTC. `Kind` is a free-form category (the UI maps the
// built-in values "event" / "experiment-start" / "experiment-end" /
// "incident" to colors; unknown kinds render with the default color).
type Annotation struct {
	ID        int64  `json:"id"`
	At        string `json:"at"`
	Title     string `json:"title"`
	Note      string `json:"note,omitempty"`
	Kind      string `json:"kind"`
	Color     string `json:"color,omitempty"`
	CreatedAt string `json:"created_at"`
}

// InsertAnnotation appends a new annotation and returns it with the
// assigned id and created_at populated.
func InsertAnnotation(conn *sql.DB, a Annotation) (Annotation, error) {
	if a.Kind == "" {
		a.Kind = "event"
	}
	res, err := conn.Exec(
		`INSERT INTO annotations (at, title, note, kind, color) VALUES (?, ?, ?, ?, ?)`,
		a.At, a.Title, a.Note, a.Kind, a.Color,
	)
	if err != nil {
		return Annotation{}, fmt.Errorf("insert annotation: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Annotation{}, fmt.Errorf("last insert id: %w", err)
	}
	return GetAnnotation(conn, id)
}

// GetAnnotation fetches a single annotation by id.
func GetAnnotation(conn *sql.DB, id int64) (Annotation, error) {
	var out Annotation
	err := conn.QueryRow(
		`SELECT id, at, title, note, kind, color, created_at FROM annotations WHERE id = ?`, id,
	).Scan(&out.ID, &out.At, &out.Title, &out.Note, &out.Kind, &out.Color, &out.CreatedAt)
	if err != nil {
		return Annotation{}, fmt.Errorf("get annotation: %w", err)
	}
	return out, nil
}

// ListAnnotations returns every annotation, optionally filtered by an
// inclusive `at` range. Empty strings disable the corresponding bound.
// Ordered by `at` ascending so the chart overlay and the page list share
// a single deterministic order.
func ListAnnotations(conn *sql.DB, from, to string) ([]Annotation, error) {
	stmt := `SELECT id, at, title, note, kind, color, created_at FROM annotations WHERE 1=1`
	args := []any{}
	if from != "" {
		stmt += ` AND at >= ?`
		args = append(args, from)
	}
	if to != "" {
		stmt += ` AND at <= ?`
		args = append(args, to)
	}
	stmt += ` ORDER BY at ASC, id ASC`
	rows, err := conn.Query(stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("list annotations: %w", err)
	}
	defer rows.Close()
	out := []Annotation{}
	for rows.Next() {
		var a Annotation
		if err := rows.Scan(&a.ID, &a.At, &a.Title, &a.Note, &a.Kind, &a.Color, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan annotation: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAnnotation overwrites the mutable fields of an existing row.
// Returns the refreshed row. sql.ErrNoRows if the id doesn't exist.
func UpdateAnnotation(conn *sql.DB, a Annotation) (Annotation, error) {
	if a.Kind == "" {
		a.Kind = "event"
	}
	res, err := conn.Exec(
		`UPDATE annotations SET at = ?, title = ?, note = ?, kind = ?, color = ? WHERE id = ?`,
		a.At, a.Title, a.Note, a.Kind, a.Color, a.ID,
	)
	if err != nil {
		return Annotation{}, fmt.Errorf("update annotation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Annotation{}, fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return Annotation{}, sql.ErrNoRows
	}
	return GetAnnotation(conn, a.ID)
}

// DeleteAnnotation removes a row. Returns sql.ErrNoRows if the id wasn't
// found, so the HTTP layer can map it to 404.
func DeleteAnnotation(conn *sql.DB, id int64) error {
	res, err := conn.Exec(`DELETE FROM annotations WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete annotation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetSession fetches a single row by id.
func GetSession(conn *sql.DB, id string) (Session, error) {
	const stmt = `
SELECT id, COALESCE(project_path,''), COALESCE(started_at,''), ended_at,
       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
       total_cost_usd, COALESCE(model,''), turns, COALESCE(raw_payload,''),
       created_at, updated_at
FROM sessions WHERE id = ?`
	var s Session
	err := conn.QueryRow(stmt, id).Scan(
		&s.ID, &s.ProjectPath, &s.StartedAt, &s.EndedAt,
		&s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.CacheWriteTokens,
		&s.TotalCostUSD, &s.Model, &s.Turns, &s.RawPayload,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return s, nil
}
