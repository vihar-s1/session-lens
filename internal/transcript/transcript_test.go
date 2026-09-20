package transcript

import (
	"math"
	"strings"
	"testing"
)

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestPricingFor(t *testing.T) {
	cases := []struct {
		model string
		want  Pricing
	}{
		// Opus 4.5+ → current $5/$25 tier.
		{"claude-opus-4-7", pricingOpusCurrent},
		{"claude-opus-4-8", pricingOpusCurrent},
		{"claude-opus-4-5-20260101", pricingOpusCurrent},
		{"claude-opus-5-0", pricingOpusCurrent},
		// Opus 4.1 and earlier → legacy $15/$75 tier.
		{"claude-opus-4-1", pricingOpusLegacy},
		{"CLAUDE-OPUS-4", pricingOpusLegacy},
		{"claude-opus-3", pricingOpusLegacy},
		// Other families.
		{"claude-3-5-haiku-20241022", pricingHaiku},
		{"claude-sonnet-4-5", pricingSonnet},
		{"claude-fable-5", pricingFable},
		{"claude-mythos-5", pricingFable}, // Mythos shares the Fable rate card
		{"some-unknown-model", pricingSonnet},
		{"", pricingSonnet},
	}
	for _, c := range cases {
		got := PricingFor(c.model)
		if got != c.want {
			t.Errorf("PricingFor(%q) = %+v, want %+v", c.model, got, c.want)
		}
	}
}

func TestComputeCost(t *testing.T) {
	cases := []struct {
		name            string
		model           string
		in, out, cr, cw int64
		want            float64
	}{
		// Current opus tier — Opus 4.5+ dropped to $5/$25/$0.50/$6.25.
		{"opus 4.7 1M each", "claude-opus-4-7", 1_000_000, 1_000_000, 1_000_000, 1_000_000, 5 + 25 + 0.5 + 6.25},
		// Legacy opus tier — Opus 4.1 stayed at $15/$75/$1.50/$18.75.
		{"opus 4.1 1M each", "claude-opus-4-1", 1_000_000, 1_000_000, 1_000_000, 1_000_000, 15 + 75 + 1.5 + 18.75},
		// Sonnet, Haiku unchanged.
		{"sonnet 1M each", "claude-sonnet-4", 1_000_000, 1_000_000, 1_000_000, 1_000_000, 3 + 15 + 0.3 + 3.75},
		{"haiku 1M each", "claude-haiku-3", 1_000_000, 1_000_000, 1_000_000, 1_000_000, 1 + 5 + 0.1 + 1.25},
		// Fable — new tier at 5x sonnet.
		{"fable 1M each", "claude-fable-5", 1_000_000, 1_000_000, 1_000_000, 1_000_000, 10 + 50 + 1 + 12.5},
		{"zero", "claude-opus-4-7", 0, 0, 0, 0, 0},
		{"sonnet partial", "sonnet", 500_000, 100_000, 0, 0, (500_000*3 + 100_000*15) / 1_000_000.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ComputeCost(c.model, c.in, c.out, c.cr, c.cw)
			if !approxEqual(got, c.want) {
				t.Errorf("ComputeCost = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseAssistantMessages(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"user","timestamp":"2026-05-27T10:00:00Z","message":{"content":"hi"}}`,
		`{"type":"assistant","timestamp":"2026-05-27T10:00:01Z","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":10,"cache_creation_input_tokens":5}}}`,
		`{"type":"assistant","timestamp":"2026-05-27T10:00:05Z","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":200,"output_tokens":80,"cache_read_input_tokens":20,"cache_creation_input_tokens":0}}}`,
		`{"type":"system","timestamp":"2026-05-27T10:00:06Z"}`,
	}, "\n")
	s, err := Parse(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if s.Turns != 2 {
		t.Errorf("Turns = %d, want 2", s.Turns)
	}
	if s.InputTokens != 300 {
		t.Errorf("InputTokens = %d, want 300", s.InputTokens)
	}
	if s.OutputTokens != 130 {
		t.Errorf("OutputTokens = %d, want 130", s.OutputTokens)
	}
	if s.CacheReadTokens != 30 {
		t.Errorf("CacheReadTokens = %d, want 30", s.CacheReadTokens)
	}
	if s.CacheWriteTokens != 5 {
		t.Errorf("CacheWriteTokens = %d, want 5", s.CacheWriteTokens)
	}
	if s.TotalTokens != 300+130+30+5 {
		t.Errorf("TotalTokens = %d, want %d", s.TotalTokens, 300+130+30+5)
	}
	if s.Model != "claude-sonnet-4-5" {
		t.Errorf("Model = %q, want sonnet", s.Model)
	}
	if s.StartedAt.IsZero() || s.EndedAt.IsZero() {
		t.Errorf("timestamps not set: start=%v end=%v", s.StartedAt, s.EndedAt)
	}
	if !s.EndedAt.After(s.StartedAt) {
		t.Errorf("EndedAt should be after StartedAt")
	}
	if s.TotalCostUSD <= 0 {
		t.Errorf("TotalCostUSD should be > 0, got %v", s.TotalCostUSD)
	}
}

func TestParseIgnoresNonAssistantAndMalformed(t *testing.T) {
	jsonl := strings.Join([]string{
		`{this is not json}`,
		``,
		`{"type":"user","message":{"content":"hi"}}`,
		`{"type":"assistant","message":{"model":"claude-haiku","usage":{"input_tokens":10,"output_tokens":5}}}`,
		`malformed-line-with-no-braces`,
	}, "\n")
	s, err := Parse(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if s.Turns != 1 {
		t.Errorf("Turns = %d, want 1", s.Turns)
	}
	if s.InputTokens != 10 || s.OutputTokens != 5 {
		t.Errorf("tokens wrong: in=%d out=%d", s.InputTokens, s.OutputTokens)
	}
	if s.Model != "claude-haiku" {
		t.Errorf("Model = %q, want claude-haiku", s.Model)
	}
}

func TestParseEmpty(t *testing.T) {
	s, err := Parse(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if s.Turns != 0 || s.TotalTokens != 0 || s.TotalCostUSD != 0 {
		t.Errorf("expected empty summary, got %+v", s)
	}
}

// TestComputeCostNegativeInputsClamped verifies that ComputeCost clamps
// negative token values to 0 rather than producing a negative cost.
func TestComputeCostNegativeInputsClamped(t *testing.T) {
	cases := []struct {
		name            string
		in, out, cr, cw int64
		wantNonNegative bool
		wantExactZero   bool
	}{
		{"all negative clamp to zero", -1000, -500, -200, -100, true, true},
		{"negative input only", -100, 50, 0, 0, true, false},
		{"negative output only", 100, -50, 0, 0, true, false},
		{"mixed negative and positive", -100, 200, -50, 100, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ComputeCost("claude-sonnet", c.in, c.out, c.cr, c.cw)
			if got < 0 {
				t.Errorf("ComputeCost(%d,%d,%d,%d) = %v, want >= 0 (negative clamp failed)",
					c.in, c.out, c.cr, c.cw, got)
			}
			if c.wantExactZero && got != 0 {
				t.Errorf("ComputeCost(%d,%d,%d,%d) = %v, want exactly 0",
					c.in, c.out, c.cr, c.cw, got)
			}
		})
	}
}

// TestParseNegativeTokensProduceZeroCost verifies that a transcript line
// carrying negative token counts does not produce a negative TotalCostUSD.
// The parser must clamp each field to 0 before accumulating.
func TestParseNegativeTokensProduceZeroCost(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-05-27T10:00:00Z","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":-500,"output_tokens":-200,"cache_read_input_tokens":-50,"cache_creation_input_tokens":-10}}}`,
	}, "\n")

	s, err := Parse(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if s.TotalCostUSD < 0 {
		t.Errorf("TotalCostUSD = %v with all-negative tokens, want >= 0", s.TotalCostUSD)
	}
	if s.TotalCostUSD != 0 {
		t.Errorf("TotalCostUSD = %v with all-negative tokens clamped to 0, want exactly 0", s.TotalCostUSD)
	}
	// Accumulated token counts must not be negative.
	if s.InputTokens < 0 {
		t.Errorf("InputTokens = %d, want 0 (clamped)", s.InputTokens)
	}
	if s.OutputTokens < 0 {
		t.Errorf("OutputTokens = %d, want 0 (clamped)", s.OutputTokens)
	}
	if s.CacheReadTokens < 0 {
		t.Errorf("CacheReadTokens = %d, want 0 (clamped)", s.CacheReadTokens)
	}
	if s.CacheWriteTokens < 0 {
		t.Errorf("CacheWriteTokens = %d, want 0 (clamped)", s.CacheWriteTokens)
	}
}

// TestParseNegativeTokensMixedWithPositive verifies that a transcript with one
// negative-token line and one valid positive-token line produces the correct
// cost for the positive line only (the negative line contributes 0).
func TestParseNegativeTokensMixedWithPositive(t *testing.T) {
	jsonl := strings.Join([]string{
		// Negative line: should contribute 0 cost.
		`{"type":"assistant","timestamp":"2026-05-27T10:00:00Z","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":-100,"output_tokens":-50,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		// Valid positive line: 1_000_000 input tokens only.
		`{"type":"assistant","timestamp":"2026-05-27T10:00:01Z","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":1000000,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
	}, "\n")

	s, err := Parse(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Expected: 1_000_000 sonnet input tokens = $3.00 exactly.
	const wantCost = 3.00
	if !approxEqual(s.TotalCostUSD, wantCost) {
		t.Errorf("TotalCostUSD = %v, want %v (only positive line should contribute)", s.TotalCostUSD, wantCost)
	}
	// Accumulated input tokens: clamped negative (0) + 1_000_000 = 1_000_000.
	if s.InputTokens != 1_000_000 {
		t.Errorf("InputTokens = %d, want 1000000", s.InputTokens)
	}
}
