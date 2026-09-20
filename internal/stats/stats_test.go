package stats

import (
	"database/sql"
	"testing"
	"time"

	dbpkg "github.com/viharshah/session-lens/internal/db"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := dbpkg.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return conn
}

func seed(t *testing.T, conn *sql.DB) {
	t.Helper()
	now := time.Now().UTC()
	fixtures := []dbpkg.Session{
		{
			ID: "s1", ProjectPath: "/proj/a", StartedAt: now.Format(time.RFC3339),
			EndedAt:     now.Format(time.RFC3339),
			InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 100, CacheWriteTokens: 50,
			TotalCostUSD: 1.50, Model: "claude-sonnet", Turns: 3,
		},
		{
			ID: "s2", ProjectPath: "/proj/a", StartedAt: now.Format(time.RFC3339),
			EndedAt:     now.Format(time.RFC3339),
			InputTokens: 2000, OutputTokens: 800, CacheReadTokens: 0, CacheWriteTokens: 0,
			TotalCostUSD: 2.25, Model: "claude-sonnet", Turns: 5,
		},
		{
			ID: "s3", ProjectPath: "/proj/b", StartedAt: now.Format(time.RFC3339),
			EndedAt:     now.Format(time.RFC3339),
			InputTokens: 500, OutputTokens: 200, CacheReadTokens: 0, CacheWriteTokens: 0,
			TotalCostUSD: 0.50, Model: "claude-haiku", Turns: 2,
		},
	}
	for _, s := range fixtures {
		if _, _, err := dbpkg.UpsertSession(conn, s); err != nil {
			t.Fatalf("seed insert %s: %v", s.ID, err)
		}
	}
}

func TestMonthSummary(t *testing.T) {
	conn := openTestDB(t)
	defer conn.Close()
	seed(t, conn)

	s, err := MonthSummary(conn, 20.0)
	if err != nil {
		t.Fatalf("MonthSummary: %v", err)
	}
	if s.SessionCount != 3 {
		t.Errorf("SessionCount = %d, want 3", s.SessionCount)
	}
	if s.TotalInput != 3500 {
		t.Errorf("TotalInput = %d, want 3500", s.TotalInput)
	}
	if s.TotalOutput != 1500 {
		t.Errorf("TotalOutput = %d, want 1500", s.TotalOutput)
	}
	wantTotal := int64(3500 + 1500 + 100 + 50)
	if s.TotalTokens != wantTotal {
		t.Errorf("TotalTokens = %d, want %d", s.TotalTokens, wantTotal)
	}
	if s.TotalCostUSD < 4.24 || s.TotalCostUSD > 4.26 {
		t.Errorf("TotalCostUSD = %v, want ~4.25", s.TotalCostUSD)
	}
	if s.PlanBudgetUSD != 20.0 {
		t.Errorf("PlanBudgetUSD = %v, want 20", s.PlanBudgetUSD)
	}
	wantPct := (4.25 / 20.0) * 100.0
	if s.PlanUtilisationPct < wantPct-0.1 || s.PlanUtilisationPct > wantPct+0.1 {
		t.Errorf("PlanUtilisationPct = %v, want ~%v", s.PlanUtilisationPct, wantPct)
	}
}

// TestMonthSummaryUtilisationUnclamped verifies that plan utilisation reports
// the true ratio even when it exceeds 100%. The card label "999.9%" was a
// rendering lie that hid real overspend signals; the value must be raw now.
func TestMonthSummaryUtilisationUnclamped(t *testing.T) {
	conn := openTestDB(t)
	defer conn.Close()
	huge := dbpkg.Session{
		ID: "huge", ProjectPath: "/proj/x",
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		EndedAt:      time.Now().UTC().Format(time.RFC3339),
		TotalCostUSD: 9999.0, Model: "claude-opus", Turns: 1,
	}
	if _, _, err := dbpkg.UpsertSession(conn, huge); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := MonthSummary(conn, 20.0)
	if err != nil {
		t.Fatalf("MonthSummary: %v", err)
	}
	want := (9999.0 / 20.0) * 100.0
	if s.PlanUtilisationPct < want-0.1 || s.PlanUtilisationPct > want+0.1 {
		t.Errorf("PlanUtilisationPct = %v, want ~%v (unclamped)", s.PlanUtilisationPct, want)
	}
}

func TestDaily(t *testing.T) {
	conn := openTestDB(t)
	defer conn.Close()
	seed(t, conn)

	buckets, err := Daily(conn, 30)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	if len(buckets) == 0 {
		t.Fatalf("expected at least one daily bucket")
	}
	var totalSessions int64
	for _, b := range buckets {
		totalSessions += b.SessionCount
		if b.TotalTokens != b.InputTokens+b.OutputTokens+b.CacheReadTokens+b.CacheWriteTokens {
			t.Errorf("TotalTokens mismatch in bucket %s", b.Bucket)
		}
	}
	if totalSessions != 3 {
		t.Errorf("sum of bucket sessions = %d, want 3", totalSessions)
	}
}

func TestProjects(t *testing.T) {
	conn := openTestDB(t)
	defer conn.Close()
	seed(t, conn)

	rows, err := Projects(conn, 20, time.Time{})
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (paths)", len(rows))
	}
	if rows[0].ProjectPath != "/proj/a" {
		t.Errorf("top project = %q, want /proj/a (highest cost)", rows[0].ProjectPath)
	}
	if rows[0].SessionCount != 2 {
		t.Errorf("/proj/a session count = %d, want 2", rows[0].SessionCount)
	}
	if rows[1].ProjectPath != "/proj/b" {
		t.Errorf("second project = %q, want /proj/b", rows[1].ProjectPath)
	}
	// Ordering by cost DESC.
	if rows[0].TotalCostUSD < rows[1].TotalCostUSD {
		t.Errorf("not ordered by cost DESC: %+v", rows)
	}
}
