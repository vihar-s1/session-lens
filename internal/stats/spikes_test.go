package stats

import (
	"testing"
	"time"

	"github.com/viharshah/session-lens/internal/db"
)

// TestBaselineP75CostUSD_EmptyWindow verifies that the baseline returns
// exactly 0 (no panic, no error) when there are no sessions in the window.
func TestBaselineP75CostUSD_EmptyWindow(t *testing.T) {
	conn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	baseline, err := BaselineP75CostUSD(conn, 20)
	if err != nil {
		t.Fatalf("BaselineP75CostUSD error on empty DB: %v", err)
	}
	if baseline != 0 {
		t.Errorf("BaselineP75CostUSD on empty DB = %v, want 0", baseline)
	}
}

func TestDetectSessionSpikes(t *testing.T) {
	base := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	sessions := make([]SessionRecord, 0, 12)
	// 10 baseline sessions costing $0.50 each
	for i := 0; i < 10; i++ {
		sessions = append(sessions, SessionRecord{
			ID:      "base",
			EndedAt: base.Add(time.Duration(i) * time.Hour),
			CostUSD: 0.50,
		})
	}
	// 1 spike at $5 (10x median).
	sessions = append(sessions, SessionRecord{
		ID:          "spike",
		EndedAt:     base.Add(time.Duration(15) * time.Hour),
		ProjectPath: "/proj/big",
		CostUSD:     5.0,
	})
	// 1 normal again
	sessions = append(sessions, SessionRecord{
		ID:      "normal",
		EndedAt: base.Add(time.Duration(20) * time.Hour),
		CostUSD: 0.50,
	})

	out := DetectSpikes(sessions, nil, DefaultSpikeConfig())
	gotSession := 0
	for _, s := range out {
		if s.Kind == SpikeSession {
			gotSession++
			if s.Project != "/proj/big" {
				t.Errorf("spike project = %q", s.Project)
			}
			if s.Ratio < 9 {
				t.Errorf("spike ratio = %v, want ~10", s.Ratio)
			}
			if s.Severity != "high" {
				t.Errorf("spike severity = %q, want high", s.Severity)
			}
		}
	}
	if gotSession != 1 {
		t.Errorf("session spikes detected = %d, want 1", gotSession)
	}
}

func TestDetectSessionSpikesIgnoresBelowThreshold(t *testing.T) {
	base := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	sessions := []SessionRecord{
		{ID: "a", EndedAt: base, CostUSD: 1},
		{ID: "b", EndedAt: base.Add(time.Hour), CostUSD: 1},
		{ID: "c", EndedAt: base.Add(2 * time.Hour), CostUSD: 1},
		{ID: "d", EndedAt: base.Add(3 * time.Hour), CostUSD: 2.5}, // 2.5x: below threshold
	}
	out := DetectSpikes(sessions, nil, DefaultSpikeConfig())
	for _, s := range out {
		if s.Kind == SpikeSession {
			t.Errorf("unexpected session spike: %+v", s)
		}
	}
}

func TestDetectTrendSpikes(t *testing.T) {
	days := []DayRecord{
		{Date: "2026-05-10", Tokens: 1000},
		{Date: "2026-05-11", Tokens: 1100},
		{Date: "2026-05-12", Tokens: 950},
		{Date: "2026-05-13", Tokens: 1050},
		{Date: "2026-05-14", Tokens: 1000},
		{Date: "2026-05-15", Tokens: 1200}, // baseline
		{Date: "2026-05-16", Tokens: 4000}, // big spike: 4x baseline median ~ 1050
	}
	out := DetectSpikes(nil, days, DefaultSpikeConfig())
	gotTrend := 0
	for _, s := range out {
		if s.Kind == SpikeTrend {
			gotTrend++
			if s.Timestamp != "2026-05-16" {
				t.Errorf("trend day = %s, want 2026-05-16", s.Timestamp)
			}
			if s.Ratio < 2 {
				t.Errorf("trend ratio = %v, want >= 2", s.Ratio)
			}
		}
	}
	if gotTrend != 1 {
		t.Errorf("trend spikes = %d, want 1", gotTrend)
	}
}

func TestDetectSpikesEmpty(t *testing.T) {
	out := DetectSpikes(nil, nil, DefaultSpikeConfig())
	if len(out) != 0 {
		t.Errorf("expected no spikes, got %d", len(out))
	}
}

func TestMedianFloat(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{[]float64{1, 2, 3}, 2},
		{[]float64{1, 2, 3, 4}, 2.5},
		{[]float64{5}, 5},
		{[]float64{}, 0},
		{[]float64{3, 1, 2}, 2}, // unsorted input
	}
	for _, c := range cases {
		if got := medianFloat(c.in); got != c.want {
			t.Errorf("medianFloat(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSeverityFor(t *testing.T) {
	cases := []struct {
		ratio, threshold float64
		want             string
	}{
		{3.5, 3.0, "low"},
		{5.0, 3.0, "medium"},
		{10.0, 3.0, "high"},
		{2.5, 2.0, "low"},
		{6.0, 2.0, "high"},
	}
	for _, c := range cases {
		if got := severityFor(c.ratio, c.threshold); got != c.want {
			t.Errorf("severityFor(%v, %v) = %s, want %s", c.ratio, c.threshold, got, c.want)
		}
	}
}

func TestSpikesNewestFirst(t *testing.T) {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	sessions := []SessionRecord{
		{ID: "a", EndedAt: base, CostUSD: 1},
		{ID: "b", EndedAt: base.Add(time.Hour), CostUSD: 1},
		{ID: "c", EndedAt: base.Add(2 * time.Hour), CostUSD: 1},
		{ID: "spike1", EndedAt: base.Add(3 * time.Hour), CostUSD: 10},
		{ID: "d", EndedAt: base.Add(4 * time.Hour), CostUSD: 1},
		{ID: "spike2", EndedAt: base.Add(5 * time.Hour), CostUSD: 12},
	}
	out := DetectSpikes(sessions, nil, DefaultSpikeConfig())
	if len(out) < 2 {
		t.Fatalf("expected at least 2 spikes, got %d", len(out))
	}
	if out[0].Timestamp <= out[1].Timestamp {
		t.Errorf("not newest-first: %s then %s", out[0].Timestamp, out[1].Timestamp)
	}
}
