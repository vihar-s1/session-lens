package stats

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/viharshah/session-lens/internal/db"
)

func TestPeakBurstInSession_FindsHighestWindow(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// Turn schedule (minutes offset, cost): a slow warm-up followed by
	// a tight burst; the burst window should win over the warm-up.
	turns := []TurnRow{
		{EndedAt: base.Add(0 * time.Minute), CostUSD: 0.01},
		{EndedAt: base.Add(2 * time.Minute), CostUSD: 0.02},
		{EndedAt: base.Add(10 * time.Minute), CostUSD: 0.50},
		{EndedAt: base.Add(11 * time.Minute), CostUSD: 0.60},
		{EndedAt: base.Add(12 * time.Minute), CostUSD: 0.90},
		{EndedAt: base.Add(30 * time.Minute), CostUSD: 0.05},
	}
	b := PeakBurstInSession(turns, 5)
	if b == nil {
		t.Fatal("expected burst, got nil")
	}
	// Peak window ends at t=12min and covers (7,12], catching the three
	// burst turns: 0.50 + 0.60 + 0.90 = 2.00 → 0.40/min over 5min.
	if diff := b.USDInWindow - 2.00; diff > 0.001 || diff < -0.001 {
		t.Errorf("USDInWindow = %v, want 2.00", b.USDInWindow)
	}
	if diff := b.USDPerMin - 0.40; diff > 0.001 || diff < -0.001 {
		t.Errorf("USDPerMin = %v, want 0.40", b.USDPerMin)
	}
	if !b.WindowEnd.Equal(base.Add(12 * time.Minute)) {
		t.Errorf("WindowEnd = %v, want %v", b.WindowEnd, base.Add(12*time.Minute))
	}
}

func TestPeakBurstInSession_TooFewTurns(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if b := PeakBurstInSession(nil, 5); b != nil {
		t.Errorf("empty turns → burst %+v", b)
	}
	one := []TurnRow{{EndedAt: base, CostUSD: 5}}
	if b := PeakBurstInSession(one, 5); b != nil {
		t.Errorf("single turn → burst %+v", b)
	}
}

func TestPeakBurstInSession_OutOfOrderInputIsSorted(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	turns := []TurnRow{
		{EndedAt: base.Add(12 * time.Minute), CostUSD: 0.90},
		{EndedAt: base.Add(11 * time.Minute), CostUSD: 0.60},
		{EndedAt: base.Add(10 * time.Minute), CostUSD: 0.50},
		{EndedAt: base.Add(2 * time.Minute), CostUSD: 0.02},
		{EndedAt: base.Add(0 * time.Minute), CostUSD: 0.01},
	}
	b := PeakBurstInSession(turns, 5)
	if b == nil {
		t.Fatal("expected burst")
	}
	if diff := b.USDInWindow - 2.00; diff > 0.001 || diff < -0.001 {
		t.Errorf("USDInWindow = %v, want 2.00", b.USDInWindow)
	}
}

func TestDetectBurstForSession_FiresOnFloor_NoBaseline(t *testing.T) {
	conn := newBurstTestDB(t)
	// $1.50 across 3 min → peak ≈ 0.30 $/min in a 5min window. No
	// peer sessions → baseline 0 → the absolute floor (0.10) gates
	// alone. Should fire.
	base := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	seedSessionAndTurns(t, conn, "hot", base, []turnSeed{
		{off: 0 * time.Minute, cost: 0.50, model: "opus"},
		{off: 1 * time.Minute, cost: 0.50, model: "opus"},
		{off: 2 * time.Minute, cost: 0.50, model: "opus"},
	})
	got, err := DetectBurstForSession(conn, "hot", DefaultBurstConfig())
	if err != nil {
		t.Fatalf("DetectBurstForSession: %v", err)
	}
	if got == nil {
		t.Fatal("expected burst to fire above floor")
	}
	if got.SessionID != "hot" {
		t.Errorf("SessionID = %q, want hot", got.SessionID)
	}
	if got.BaselineP75 != 0 {
		t.Errorf("baseline should be 0 with <3 peers, got %v", got.BaselineP75)
	}
	if got.USDPerMin < 0.10 {
		t.Errorf("USDPerMin %v under floor", got.USDPerMin)
	}
}

func TestDetectBurstForSession_SuppressedBelowFloor(t *testing.T) {
	conn := newBurstTestDB(t)
	base := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	seedSessionAndTurns(t, conn, "slow", base, []turnSeed{
		{off: 0 * time.Minute, cost: 0.01},
		{off: 1 * time.Minute, cost: 0.01},
		{off: 2 * time.Minute, cost: 0.01},
	})
	got, err := DetectBurstForSession(conn, "slow", DefaultBurstConfig())
	if err != nil {
		t.Fatalf("DetectBurstForSession: %v", err)
	}
	if got != nil {
		t.Errorf("expected no burst under floor, got %+v", got)
	}
}

func TestDetectBurstForSession_SuppressedBelowRatio(t *testing.T) {
	conn := newBurstTestDB(t)
	// Peer set: 4 sessions each peaking at $0.30/min. Baseline P75 = 0.30.
	// Candidate peaks well below 4× (1.20). Must not fire.
	// Anchor times to time.Now() because BurstBaselineP75USDPerMin
	// filters against wall-clock via `BaselineDays`.
	now := time.Now().UTC()
	base := now.Add(-2 * 24 * time.Hour)
	for i, sid := range []string{"peer1", "peer2", "peer3", "peer4"} {
		start := base.Add(time.Duration(i) * time.Hour)
		seedSessionAndTurns(t, conn, sid, start, []turnSeed{
			{off: 0 * time.Minute, cost: 0.50},
			{off: 1 * time.Minute, cost: 0.50},
			{off: 2 * time.Minute, cost: 0.50},
		})
	}
	candBase := now.Add(-1 * time.Hour)
	seedSessionAndTurns(t, conn, "cand", candBase, []turnSeed{
		{off: 0 * time.Minute, cost: 0.66},
		{off: 5 * time.Minute, cost: 0.66},
		{off: 10 * time.Minute, cost: 0.68},
	})
	got, err := DetectBurstForSession(conn, "cand", DefaultBurstConfig())
	if err != nil {
		t.Fatalf("DetectBurstForSession: %v", err)
	}
	if got != nil {
		t.Errorf("expected no burst under 4x baseline, got %+v", got)
	}
}

// Helpers.

type turnSeed struct {
	off   time.Duration
	cost  float64
	model string
}

func newBurstTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func seedSessionAndTurns(t *testing.T, conn *sql.DB, id string, base time.Time, turns []turnSeed) {
	t.Helper()
	end := base
	if len(turns) > 0 {
		end = base.Add(turns[len(turns)-1].off)
	}
	_, _, err := db.UpsertSession(conn, db.Session{
		ID:          id,
		ProjectPath: "/tmp/test",
		StartedAt:   base.UTC().Format(time.RFC3339),
		EndedAt:     end.UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("upsert session %s: %v", id, err)
	}
	rows := make([]db.Turn, 0, len(turns))
	for i, ts := range turns {
		rows = append(rows, db.Turn{
			SessionID: id,
			Idx:       i,
			EndedAt:   base.Add(ts.off).UTC().Format(time.RFC3339),
			Model:     ts.model,
			CostUSD:   ts.cost,
		})
	}
	if err := db.InsertTurns(conn, rows); err != nil {
		t.Fatalf("insert turns %s: %v", id, err)
	}
}
