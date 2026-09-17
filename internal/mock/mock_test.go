package mock

import (
	"testing"
	"time"

	"github.com/viharshah/session-lens/internal/stats"
)

func TestGenerateDeterministic(t *testing.T) {
	a := Generate()
	b := Generate()
	if len(a.Sessions) != len(b.Sessions) {
		t.Fatalf("session count differs across runs: %d vs %d", len(a.Sessions), len(b.Sessions))
	}
	for i := range a.Sessions {
		if a.Sessions[i].ID != b.Sessions[i].ID {
			t.Errorf("ID at %d differs: %q vs %q", i, a.Sessions[i].ID, b.Sessions[i].ID)
		}
		if a.Sessions[i].InputTokens != b.Sessions[i].InputTokens {
			t.Errorf("InputTokens at %d differs: %d vs %d", i, a.Sessions[i].InputTokens, b.Sessions[i].InputTokens)
		}
		if a.Sessions[i].TotalCostUSD != b.Sessions[i].TotalCostUSD {
			t.Errorf("Cost at %d differs: %v vs %v", i, a.Sessions[i].TotalCostUSD, b.Sessions[i].TotalCostUSD)
		}
	}
}

func TestGenerateHasAllThreeModels(t *testing.T) {
	d := Generate()
	seen := map[string]bool{}
	for _, s := range d.Sessions {
		seen[s.Model] = true
	}
	if len(seen) < 3 {
		t.Errorf("expected sessions across all 3 model strings, saw %d: %v", len(seen), seen)
	}
}

func TestGenerateSorted(t *testing.T) {
	d := Generate()
	for i := 1; i < len(d.Sessions); i++ {
		if d.Sessions[i].EndedAt.Before(d.Sessions[i-1].EndedAt) {
			t.Fatalf("sessions not sorted oldest-first at index %d", i)
		}
	}
}

func TestDatasetAggregations(t *testing.T) {
	d := Generate()

	summary := d.Summary(20.0)
	if summary.SessionCount == 0 {
		t.Errorf("summary session count = 0")
	}
	if summary.TotalCostUSD <= 0 {
		t.Errorf("summary cost = %v, want > 0", summary.TotalCostUSD)
	}

	daily := d.Daily(30)
	if len(daily) != Days {
		t.Errorf("daily buckets = %d, want %d", len(daily), Days)
	}

	hourly := d.Hourly(7, "", stats.GranHour)
	if len(hourly) == 0 {
		t.Errorf("hourly buckets empty")
	}

	projects := d.Projects(10, time.Time{})
	if len(projects) < 2 {
		t.Errorf("expected at least 2 projects, got %d", len(projects))
	}

	byModel := d.ByModel(14)
	if len(byModel.Totals) < 3 {
		t.Errorf("by-model families = %d, want >= 3", len(byModel.Totals))
	}
	if len(byModel.Daily) != Days {
		t.Errorf("by-model daily count = %d, want %d", len(byModel.Daily), Days)
	}
}

func TestDatasetHasSpikes(t *testing.T) {
	d := Generate()
	spikes := d.Spikes(stats.DefaultSpikeConfig())
	if len(spikes) == 0 {
		t.Errorf("expected at least one spike from synthetic data")
	}
}
