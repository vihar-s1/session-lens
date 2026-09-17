package transcript

import (
	"strconv"
	"strings"
)

// Pricing represents per-million-token USD pricing for a model family.
// CacheWritePerM uses the 5-minute cache-write rate (1.25× base input) —
// Anthropic also has a 1-hour rate (2× base input) but our schema stores
// a single cache_write_tokens field, so we can't distinguish the two.
// 5m is the common case for the Claude Code hook workload.
type Pricing struct {
	InputPerM      float64
	OutputPerM     float64
	CacheReadPerM  float64
	CacheWritePerM float64
}

// Per-million-token pricing for Claude model families. Numbers verified
// against https://platform.claude.com/docs/en/about-claude/pricing on
// 2026-07-08. Ratios: cache_read = 0.10× input, cache_write (5m) = 1.25×
// input, output ≈ 5× input across families.
var (
	// pricingOpusCurrent covers Opus 4.5, 4.6, 4.7, 4.8 (and later, until
	// Anthropic changes the tier again). Dropped from $15/$75 in the
	// Opus 4.1 era to $5/$25 starting with Opus 4.5.
	pricingOpusCurrent = Pricing{
		InputPerM:      5.00,
		OutputPerM:     25.00,
		CacheReadPerM:  0.50,
		CacheWritePerM: 6.25,
	}
	// pricingOpusLegacy covers Opus 4.1 and earlier (Opus 4, Opus 3, etc.).
	// Kept as its own tier because historical DB rows for those model ids
	// really did bill at these rates — collapsing them into the current
	// tier would under-estimate their true cost by 3×.
	pricingOpusLegacy = Pricing{
		InputPerM:      15.00,
		OutputPerM:     75.00,
		CacheReadPerM:  1.50,
		CacheWritePerM: 18.75,
	}
	pricingSonnet = Pricing{
		InputPerM:      3.00,
		OutputPerM:     15.00,
		CacheReadPerM:  0.30,
		CacheWritePerM: 3.75,
	}
	pricingHaiku = Pricing{
		InputPerM:      1.00,
		OutputPerM:     5.00,
		CacheReadPerM:  0.10,
		CacheWritePerM: 1.25,
	}
	// pricingFable — Fable 5 and Mythos 5 share the same rate card. ~5x
	// the sonnet tier. Verified 2026-07-08 against Anthropic's pricing
	// page.
	pricingFable = Pricing{
		InputPerM:      10.00,
		OutputPerM:     50.00,
		CacheReadPerM:  1.00,
		CacheWritePerM: 12.50,
	}
)

// PricingFor returns the pricing table for a model name.
//
// Opus routes through opusPricing to pick between the legacy ($15/$75)
// and current ($5/$25) tiers based on the parsed version — Opus 4.5+
// dropped to the new rate, everything before stayed on the old one.
//
// Other families do a plain substring match. Unknown models fall back to
// sonnet-tier so a brand-new Claude model bills approximately right until
// its rate card is added here.
func PricingFor(model string) Pricing {
	m := strings.ToLower(model)
	if strings.Contains(m, "opus") {
		return opusPricing(m)
	}
	// Order matters: haiku and fable are checked before sonnet because
	// sonnet is the fallback tier — no cascading required if we find
	// a specific family first.
	switch {
	case strings.Contains(m, "haiku"):
		return pricingHaiku
	case strings.Contains(m, "fable"), strings.Contains(m, "mythos"):
		return pricingFable
	case strings.Contains(m, "sonnet"):
		return pricingSonnet
	default:
		return pricingSonnet
	}
}

// opusPricing selects the correct Opus tier based on the parsed version.
// Opus 4.5 and later use the current $5/$25 rates; everything else stays
// on the legacy $15/$75 rates. An id we can't parse a version out of
// falls back to legacy — safer to slightly over-estimate an unknown
// legacy id than to silently under-report a real bill.
func opusPricing(modelLower string) Pricing {
	major, minor := parseOpusVersion(modelLower)
	if major > 4 || (major == 4 && minor >= 5) {
		return pricingOpusCurrent
	}
	return pricingOpusLegacy
}

// parseOpusVersion extracts (major, minor) from ids like "claude-opus-4-7"
// or "claude-opus-4-7-20260101". Returns (0, 0) when the shape can't be
// read — opusPricing treats that as legacy.
func parseOpusVersion(modelLower string) (int, int) {
	i := strings.Index(modelLower, "opus-")
	if i < 0 {
		return 0, 0
	}
	rest := modelLower[i+len("opus-"):]
	parts := strings.Split(rest, "-")
	if len(parts) == 0 {
		return 0, 0
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0
	}
	if len(parts) < 2 {
		return major, 0
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return major, 0
	}
	return major, minor
}

// ComputeCost returns the USD cost for a token-usage tuple under the given model.
// Negative token counts are clamped to 0 to prevent corrupting cost aggregations.
func ComputeCost(model string, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64) float64 {
	p := PricingFor(model)
	const perM = 1_000_000.0
	return (float64(clampTokens(inputTokens))*p.InputPerM +
		float64(clampTokens(outputTokens))*p.OutputPerM +
		float64(clampTokens(cacheReadTokens))*p.CacheReadPerM +
		float64(clampTokens(cacheWriteTokens))*p.CacheWritePerM) / perM
}

// clampTokens returns v if v >= 0, otherwise 0. Guards against negative token
// values that would corrupt cost calculations.
func clampTokens(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
