package db

import (
	"errors"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestBootstrapAndUpsert(t *testing.T) {
	conn, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	s := Session{
		ID:               "sess-1",
		ProjectPath:      "/tmp/proj",
		StartedAt:        "2026-05-27T10:00:00Z",
		EndedAt:          "2026-05-27T10:30:00Z",
		InputTokens:      100,
		OutputTokens:     50,
		CacheReadTokens:  10,
		CacheWriteTokens: 5,
		TotalCostUSD:     0.0123,
		Model:            "claude-sonnet-4-5",
		Turns:            3,
		RawPayload:       `{"session_id":"sess-1"}`,
	}

	got, inserted, err := UpsertSession(conn, s)
	if err != nil {
		t.Fatalf("UpsertSession insert: %v", err)
	}
	if !inserted {
		t.Errorf("expected inserted=true on first insert")
	}
	if got.ID != s.ID || got.Turns != 3 || got.InputTokens != 100 {
		t.Errorf("row mismatch after insert: %+v", got)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Errorf("created_at/updated_at not populated: %+v", got)
	}
	createdAt := got.CreatedAt
	firstUpdated := got.UpdatedAt

	// Ensure SQLite CURRENT_TIMESTAMP advances at least a second.
	time.Sleep(1100 * time.Millisecond)

	s.OutputTokens = 200
	s.Turns = 4
	got2, inserted2, err := UpsertSession(conn, s)
	if err != nil {
		t.Fatalf("UpsertSession update: %v", err)
	}
	if inserted2 {
		t.Errorf("expected inserted=false on update")
	}
	if got2.OutputTokens != 200 || got2.Turns != 4 {
		t.Errorf("row not updated: %+v", got2)
	}
	if got2.CreatedAt != createdAt {
		t.Errorf("created_at changed across update: %q -> %q", createdAt, got2.CreatedAt)
	}
	if got2.UpdatedAt == firstUpdated {
		t.Errorf("updated_at did not advance: %q", got2.UpdatedAt)
	}
}

func TestIndexesPresent(t *testing.T) {
	conn, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	rows, err := conn.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='sessions'`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer rows.Close()

	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		found[name] = true
	}
	for _, want := range []string{
		"idx_sessions_ended_at",
		"idx_sessions_project",
		"idx_sessions_project_ended_at",
		"idx_sessions_model_ended_at",
		"idx_sessions_created_at",
	} {
		if !found[want] {
			t.Errorf("missing index %q (found: %v)", want, found)
		}
	}
}

// TestExplainQueryPlanUsesIndex verifies that the three most performance-critical
// aggregation queries use an index rather than a bare SCAN TABLE. SQLite's
// EXPLAIN QUERY PLAN returns a row whose "detail" column starts with
// "SEARCH" (index seek) when an index is used, or "SCAN" without an index name
// when performing a full-table scan.
func TestExplainQueryPlanUsesIndex(t *testing.T) {
	conn, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{
			name: "MonthSummary (ended_at range)",
			query: `EXPLAIN QUERY PLAN
SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
       COALESCE(SUM(total_cost_usd),0), COUNT(1)
FROM sessions WHERE ended_at >= ?`,
			args: []any{"2026-01-01T00:00:00Z"},
		},
		{
			name: "Daily buckets (ended_at range + group by date)",
			query: `EXPLAIN QUERY PLAN
SELECT substr(ended_at, 1, 10) AS bucket,
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
       COALESCE(SUM(total_cost_usd),0), COUNT(1)
FROM sessions WHERE ended_at >= ?
GROUP BY bucket ORDER BY bucket ASC`,
			args: []any{"2026-01-01T00:00:00Z"},
		},
		{
			name: "ByModel (ended_at range with model read)",
			query: `EXPLAIN QUERY PLAN
SELECT ended_at, COALESCE(model,''),
       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
       total_cost_usd
FROM sessions WHERE ended_at >= ?`,
			args: []any{"2026-01-01T00:00:00Z"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := conn.Query(tc.query, tc.args...)
			if err != nil {
				t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
			}
			defer rows.Close()

			usesIndex := false
			for rows.Next() {
				// SQLite EXPLAIN QUERY PLAN columns: id, parent, notused, detail
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					t.Fatalf("scan plan row: %v", err)
				}
				// "SEARCH sessions USING INDEX ..." → index is used.
				if strings.Contains(detail, "USING INDEX") || strings.Contains(detail, "USING COVERING INDEX") {
					usesIndex = true
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows err: %v", err)
			}
			if !usesIndex {
				t.Errorf("query %q: no index used (EXPLAIN QUERY PLAN shows full table scan)", tc.name)
			}
		})
	}
}

func TestAnnotationsCRUD(t *testing.T) {
	conn, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	a, err := InsertAnnotation(conn, Annotation{
		At:    "2026-06-01T00:00:00Z",
		Title: "Started optimization experiment",
		Note:  "Baseline period ends today",
		Kind:  "experiment-start",
	})
	if err != nil {
		t.Fatalf("InsertAnnotation: %v", err)
	}
	if a.ID == 0 || a.CreatedAt == "" {
		t.Errorf("expected id and created_at populated: %+v", a)
	}
	if a.Kind != "experiment-start" {
		t.Errorf("Kind round-trip mismatch: %q", a.Kind)
	}

	// Default kind when omitted.
	b, err := InsertAnnotation(conn, Annotation{At: "2026-05-15T00:00:00Z", Title: "Baseline begins"})
	if err != nil {
		t.Fatalf("InsertAnnotation 2: %v", err)
	}
	if b.Kind != "event" {
		t.Errorf("default kind = %q, want %q", b.Kind, "event")
	}

	// Filtered list: from-only should drop the older row.
	got, err := ListAnnotations(conn, "2026-06-01T00:00:00Z", "")
	if err != nil {
		t.Fatalf("ListAnnotations: %v", err)
	}
	if len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("from filter unexpected result: %+v", got)
	}

	// Full list should be sorted ascending by `at`.
	all, err := ListAnnotations(conn, "", "")
	if err != nil {
		t.Fatalf("ListAnnotations all: %v", err)
	}
	if len(all) != 2 || all[0].ID != b.ID || all[1].ID != a.ID {
		t.Errorf("list order wrong: %+v", all)
	}

	// Update.
	a.Title = "Started optimization (v2)"
	a.Note = "Switched primary model to Haiku"
	updated, err := UpdateAnnotation(conn, a)
	if err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}
	if updated.Title != a.Title || updated.Note != a.Note {
		t.Errorf("update did not persist: %+v", updated)
	}

	// Delete.
	if err := DeleteAnnotation(conn, a.ID); err != nil {
		t.Fatalf("DeleteAnnotation: %v", err)
	}
	if err := DeleteAnnotation(conn, a.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("second DeleteAnnotation = %v, want sql.ErrNoRows", err)
	}
	if _, err := UpdateAnnotation(conn, a); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("UpdateAnnotation on deleted row = %v, want sql.ErrNoRows", err)
	}
}
