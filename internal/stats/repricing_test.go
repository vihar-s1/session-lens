package stats

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/viharshah/session-lens/internal/db"
	"github.com/viharshah/session-lens/internal/transcript"
)

func newRepricingDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func seedSessionForReprice(t *testing.T, conn *sql.DB, id, project, model string, endedAt time.Time, in, out, cr, cw int64, turns int) {
	t.Helper()
	cost := transcript.ComputeCost(model, in, out, cr, cw)
	_, _, err := db.UpsertSession(conn, db.Session{
		ID:               id,
		ProjectPath:      project,
		StartedAt:        endedAt.Add(-5 * time.Minute).UTC().Format(time.RFC3339),
		EndedAt:          endedAt.UTC().Format(time.RFC3339),
		InputTokens:      in,
		OutputTokens:     out,
		CacheReadTokens:  cr,
		CacheWriteTokens: cw,
		TotalCostUSD:     cost,
		Model:            model,
		Turns:            turns,
	})
	if err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

func TestReprice_FlatSwap_OpusToSonnet(t *testing.T) {
	conn := newRepricingDB(t)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)

	// Two opus sessions and one already-sonnet session inside the window.
	seedSessionForReprice(t, conn, "o1", "/p/a", "claude-opus-4-7", from.Add(24*time.Hour), 1_000_000, 200_000, 500_000, 100_000, 5)
	seedSessionForReprice(t, conn, "o2", "/p/b", "claude-opus-4-7", from.Add(48*time.Hour), 500_000, 100_000, 0, 0, 3)
	seedSessionForReprice(t, conn, "s1", "/p/a", "claude-sonnet-5", from.Add(72*time.Hour), 100_000, 20_000, 0, 0, 2)

	res, err := Reprice(conn, from, to, SwapRule{Kind: "flat", Target: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if res.Sessions != 3 || res.Swapped != 3 {
		t.Errorf("Sessions=%d Swapped=%d, want 3/3", res.Sessions, res.Swapped)
	}
	// Flat swap to sonnet: hypothetical must be lower than original
	// (opus is pricier than sonnet across every field).
	if res.HypotheticalUSD >= res.OriginalUSD {
		t.Errorf("expected savings, got orig=%v hypo=%v", res.OriginalUSD, res.HypotheticalUSD)
	}
	if res.DeltaUSD != res.HypotheticalUSD-res.OriginalUSD {
		t.Errorf("DeltaUSD %v != hypo-orig %v", res.DeltaUSD, res.HypotheticalUSD-res.OriginalUSD)
	}
	// The s1 session, already sonnet, should reprice to identical
	// dollars — same model + same tokens = same cost.
	var s1 *SessionMove
	for i := range res.TopMoved {
		if res.TopMoved[i].SessionID == "s1" {
			s1 = &res.TopMoved[i]
		}
	}
	if s1 != nil && s1.DeltaUSD != 0 {
		t.Errorf("s1 delta should be 0, got %v", s1.DeltaUSD)
	}
}

func TestReprice_FamilySwap_OnlyMatchingFamily(t *testing.T) {
	conn := newRepricingDB(t)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)

	seedSessionForReprice(t, conn, "o1", "/p/a", "claude-opus-4-7", from.Add(24*time.Hour), 500_000, 100_000, 0, 0, 4)
	seedSessionForReprice(t, conn, "s1", "/p/a", "claude-sonnet-5", from.Add(48*time.Hour), 500_000, 100_000, 0, 0, 4)

	res, err := Reprice(conn, from, to, SwapRule{
		Kind:       "family",
		FromFamily: "opus",
		Target:     "claude-sonnet-5",
	})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if res.Swapped != 1 {
		t.Errorf("Swapped=%d, want 1 (only the opus session)", res.Swapped)
	}
	if len(res.TopMoved) != 1 || res.TopMoved[0].SessionID != "o1" {
		t.Errorf("TopMoved should have exactly o1, got %+v", res.TopMoved)
	}
}

func TestReprice_ConditionalSwap_SmartRouter(t *testing.T) {
	conn := newRepricingDB(t)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)

	// Two opus sessions: one small ($1), one big ($20+). Rule: swap
	// opus→sonnet only when session <= $5. Expect only the small one
	// to swap; the big one keeps its original cost.
	seedSessionForReprice(t, conn, "opus_small", "/p/a", "claude-opus-4-7", from.Add(24*time.Hour), 100_000, 20_000, 0, 0, 2)
	seedSessionForReprice(t, conn, "opus_big", "/p/a", "claude-opus-4-7", from.Add(48*time.Hour), 5_000_000, 1_000_000, 0, 0, 40)
	seedSessionForReprice(t, conn, "sonnet_untouched", "/p/a", "claude-sonnet-5", from.Add(72*time.Hour), 200_000, 40_000, 0, 0, 3)

	res, err := Reprice(conn, from, to, SwapRule{
		Kind:       "conditional",
		FromFamily: "opus",
		Target:     "claude-sonnet-5",
		MaxCostUSD: 5.0,
	})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if res.Sessions != 3 {
		t.Errorf("Sessions=%d, want 3", res.Sessions)
	}
	if res.Swapped != 1 {
		t.Errorf("Swapped=%d, want 1 (only opus_small under $5)", res.Swapped)
	}
	if len(res.TopMoved) != 1 || res.TopMoved[0].SessionID != "opus_small" {
		t.Errorf("TopMoved should be [opus_small], got %+v", res.TopMoved)
	}
	// opus_big must be counted at its original cost in the totals.
	if res.OriginalUSD < 20 {
		t.Errorf("OriginalUSD should include opus_big's ~$20+, got %v", res.OriginalUSD)
	}
}

func TestReprice_EmptyRange_NoRows(t *testing.T) {
	conn := newRepricingDB(t)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)

	res, err := Reprice(conn, from, to, SwapRule{Kind: "flat", Target: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if res.Sessions != 0 || res.Swapped != 0 {
		t.Errorf("empty range: Sessions=%d Swapped=%d", res.Sessions, res.Swapped)
	}
	if res.OriginalUSD != 0 || res.HypotheticalUSD != 0 || res.DeltaUSD != 0 || res.DeltaPct != 0 {
		t.Errorf("empty range totals should be 0, got %+v", res)
	}
}

func TestReprice_MissingTargetErrors(t *testing.T) {
	conn := newRepricingDB(t)
	_, err := Reprice(conn, time.Now().Add(-24*time.Hour), time.Now(), SwapRule{Kind: "flat"})
	if err == nil {
		t.Fatal("expected error on empty Target")
	}
}
