package stats

import (
	"database/sql"
	"testing"
	"time"

	dbpkg "github.com/viharshah/session-lens/internal/db"
)

func TestModelFamily(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-7":           "opus",
		"CLAUDE-OPUS-3":             "opus",
		"claude-sonnet-4-5":         "sonnet",
		"claude-3-5-haiku-20241022": "haiku",
		// Curated new family — explicitly listed in KnownModelFamilies.
		"claude-fable-1":          "fable",
		"claude-fable-2-20260701": "fable",
		// Dynamic recognition — a hypothetical brand-new Claude model that
		// isn't in the curated list still gets its own bucket via the
		// claude-<family>-<version> parser instead of collapsing to "other".
		"claude-prose-1-0":           "prose",
		"claude-4-2-nebula-20260501": "nebula",
		"CLAUDE-Codex-3":             "codex",
		// Non-Claude strings still go to "other".
		"foo-bar": "other",
		"":        "other",
		"   ":     "other",
	}
	for in, want := range cases {
		if got := ModelFamily(in); got != want {
			t.Errorf("ModelFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAggregateByModel(t *testing.T) {
	rows := []ModelRow{
		{EndedAt: "2026-05-20T10:00:00Z", Model: "claude-opus-4-7", InputTokens: 100, OutputTokens: 50, TotalCostUSD: 1.0},
		{EndedAt: "2026-05-20T12:00:00Z", Model: "claude-sonnet-4-5", InputTokens: 200, OutputTokens: 80, TotalCostUSD: 0.3},
		{EndedAt: "2026-05-21T09:00:00Z", Model: "claude-3-5-haiku-20241022", InputTokens: 500, OutputTokens: 100, TotalCostUSD: 0.05},
		{EndedAt: "2026-05-21T11:00:00Z", Model: "claude-opus-4-7", InputTokens: 50, OutputTokens: 50, TotalCostUSD: 0.6},
	}
	resp := AggregateByModel(rows)

	if len(resp.Totals) != 3 {
		t.Fatalf("Totals len = %d, want 3 (opus/sonnet/haiku)", len(resp.Totals))
	}
	// Top by cost should be opus (1.0 + 0.6 = 1.6).
	if resp.Totals[0].Family != "opus" {
		t.Errorf("top family = %s, want opus", resp.Totals[0].Family)
	}
	if resp.Totals[0].SessionCount != 2 {
		t.Errorf("opus sessions = %d, want 2", resp.Totals[0].SessionCount)
	}
	if resp.Totals[0].InputTokens != 150 {
		t.Errorf("opus input = %d, want 150", resp.Totals[0].InputTokens)
	}

	if len(resp.Daily) != 2 {
		t.Fatalf("Daily len = %d, want 2 days", len(resp.Daily))
	}
	if resp.Daily[0].Bucket != "2026-05-20" {
		t.Errorf("first day = %s", resp.Daily[0].Bucket)
	}
	if resp.Daily[0].Totals["opus"] != 150 { // 100+50
		t.Errorf("day-0 opus tokens = %d, want 150", resp.Daily[0].Totals["opus"])
	}
	if resp.Daily[0].Totals["sonnet"] != 280 { // 200+80
		t.Errorf("day-0 sonnet tokens = %d, want 280", resp.Daily[0].Totals["sonnet"])
	}
	if resp.Daily[1].Totals["haiku"] != 600 {
		t.Errorf("day-1 haiku tokens = %d, want 600", resp.Daily[1].Totals["haiku"])
	}
}

func TestAggregateByModelEmpty(t *testing.T) {
	resp := AggregateByModel(nil)
	if len(resp.Totals) != 0 || len(resp.Daily) != 0 {
		t.Errorf("expected empty response, got %+v", resp)
	}
}

func TestByModelDBRoundTrip(t *testing.T) {
	conn := openTestDB(t)
	defer conn.Close()
	seedByModel(t, conn)

	resp, err := ByModel(conn, 30)
	if err != nil {
		t.Fatalf("ByModel: %v", err)
	}
	if len(resp.Totals) == 0 {
		t.Fatalf("expected at least one model total")
	}
	// Validate sums roundtrip with seeded data.
	var sumIn int64
	for _, m := range resp.Totals {
		sumIn += m.InputTokens
	}
	if sumIn != 1000+2000+500 {
		t.Errorf("sum input = %d, want %d", sumIn, 1000+2000+500)
	}
}

func TestHourlyDBRoundTrip(t *testing.T) {
	conn := openTestDB(t)
	defer conn.Close()
	seedByModel(t, conn)
	out, err := Hourly(conn, 7, "", GranHour)
	if err != nil {
		t.Fatalf("Hourly: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("expected at least one hourly bucket")
	}
	for _, b := range out {
		if len(b.Bucket) != 17 { // "YYYY-MM-DDTHH:00Z" = 17 chars
			t.Errorf("unexpected bucket format %q", b.Bucket)
		}
	}
}

func seedByModel(t *testing.T, conn *sql.DB) {
	t.Helper()
	now := time.Now().UTC()
	fixtures := []dbpkg.Session{
		{ID: "m1", EndedAt: now.Format(time.RFC3339), Model: "claude-sonnet-4-5", InputTokens: 1000, OutputTokens: 200, TotalCostUSD: 1.0},
		{ID: "m2", EndedAt: now.Format(time.RFC3339), Model: "claude-opus-4-7", InputTokens: 2000, OutputTokens: 500, TotalCostUSD: 4.5},
		{ID: "m3", EndedAt: now.Format(time.RFC3339), Model: "claude-3-5-haiku-20241022", InputTokens: 500, OutputTokens: 100, TotalCostUSD: 0.05},
	}
	for _, s := range fixtures {
		if _, _, err := dbpkg.UpsertSession(conn, s); err != nil {
			t.Fatalf("seed %s: %v", s.ID, err)
		}
	}
}
