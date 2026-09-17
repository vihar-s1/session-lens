package stats

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/viharshah/session-lens/internal/transcript"
)

// SwapRule describes how to reprice historical sessions. Three kinds:
//
//   - "flat": swap every session to Target regardless of what model it
//     actually ran on. Useful for the "what if I'd only used X" question.
//   - "family": swap only sessions whose current model id contains
//     FromFamily (case-insensitive substring). "What if every Opus
//     session had been Sonnet?"
//   - "conditional" (smart-router lens): swap only sessions in FromFamily
//     whose actual cost/turns fit inside the Max* caps. Models "keep the
//     expensive model for big work, route small work to the cheap one."
//
// SwapRule intentionally re-uses the pricing tables in
// transcript.PricingFor. That's the same function the live cost path
// runs through, so a hypothetical bill computed here matches what a
// real session on that model would have been billed.
type SwapRule struct {
	Kind       string  `json:"kind"`
	FromFamily string  `json:"from_family,omitempty"`
	Target     string  `json:"target"`
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
	MaxTurns   int     `json:"max_turns,omitempty"`
}

// matches decides whether this session gets repriced. Reprice() calls this
// per session — the ones it returns false for pass through at their
// original cost.
func (r SwapRule) matches(model string, costUSD float64, turns int) bool {
	if r.Target == "" {
		return false
	}
	modelLower := strings.ToLower(model)
	fam := strings.ToLower(r.FromFamily)
	switch strings.ToLower(r.Kind) {
	case "flat":
		return true
	case "family":
		return fam != "" && strings.Contains(modelLower, fam)
	case "conditional":
		if fam != "" && !strings.Contains(modelLower, fam) {
			return false
		}
		if r.MaxCostUSD > 0 && costUSD > r.MaxCostUSD {
			return false
		}
		if r.MaxTurns > 0 && turns > r.MaxTurns {
			return false
		}
		return true
	default:
		return false
	}
}

// SwapByModel aggregates original-vs-hypothetical dollars by the model
// each session originally ran on. Renders as a paired bar chart in the UI.
type SwapByModel struct {
	Model           string  `json:"model"`
	Sessions        int     `json:"sessions"`
	OriginalUSD     float64 `json:"original_usd"`
	HypotheticalUSD float64 `json:"hypothetical_usd"`
}

// SessionMove records one repriced session for the "top movers" list.
// Sorted by absolute delta so the biggest wins/losses land at the top —
// includes negative deltas because repricing to a more expensive tier
// (Sonnet → Opus) should surface too.
type SessionMove struct {
	SessionID       string  `json:"session_id"`
	Project         string  `json:"project,omitempty"`
	FromModel       string  `json:"from_model"`
	ToModel         string  `json:"to_model"`
	OriginalUSD     float64 `json:"original_usd"`
	HypotheticalUSD float64 `json:"hypothetical_usd"`
	DeltaUSD        float64 `json:"delta_usd"`
}

// SwapResult is the wire shape returned by /v1/repricing.
type SwapResult struct {
	From            time.Time     `json:"from"`
	To              time.Time     `json:"to"`
	Rule            SwapRule      `json:"rule"`
	Sessions        int           `json:"sessions"`
	Swapped         int           `json:"swapped"`
	OriginalUSD     float64       `json:"original_usd"`
	HypotheticalUSD float64       `json:"hypothetical_usd"`
	DeltaUSD        float64       `json:"delta_usd"`
	DeltaPct        float64       `json:"delta_pct"`
	ByModel         []SwapByModel `json:"by_model"`
	TopMoved        []SessionMove `json:"top_moved"`
	Caveat          string        `json:"caveat"`
}

// Reprice runs the SwapRule against every session in [from, to] and
// returns the counterfactual bill. Sessions the rule doesn't match are
// carried through at their original cost so DeltaUSD is a real
// portfolio-level number, not a partial estimate.
func Reprice(conn *sql.DB, from, to time.Time, rule SwapRule) (*SwapResult, error) {
	if rule.Target == "" {
		return nil, fmt.Errorf("repricing: rule.Target is required")
	}
	rows, err := conn.Query(`
SELECT id, COALESCE(project_path,''), COALESCE(model,''),
       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
       total_cost_usd, turns
FROM sessions
WHERE ended_at >= ? AND ended_at <= ?
ORDER BY ended_at ASC`, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("query sessions for repricing: %w", err)
	}
	defer rows.Close()

	// byModel keyed on the original model id — normalize to "unknown" for
	// null/empty so the chart still has a bucket to bin them into.
	byModel := map[string]*SwapByModel{}
	var moves []SessionMove
	var totalOrig, totalHypo float64
	var totalCount, swappedCount int

	for rows.Next() {
		var id, project, model string
		var in, out, cr, cw int64
		var cost float64
		var turns int
		if err := rows.Scan(&id, &project, &model, &in, &out, &cr, &cw, &cost, &turns); err != nil {
			return nil, fmt.Errorf("scan repricing row: %w", err)
		}
		totalCount++

		origModelKey := model
		if origModelKey == "" {
			origModelKey = "unknown"
		}
		bucket, ok := byModel[origModelKey]
		if !ok {
			bucket = &SwapByModel{Model: origModelKey}
			byModel[origModelKey] = bucket
		}
		bucket.Sessions++
		bucket.OriginalUSD += cost

		hypo := cost
		swapped := rule.matches(model, cost, turns)
		if swapped {
			hypo = transcript.ComputeCost(rule.Target, in, out, cr, cw)
			swappedCount++
			moves = append(moves, SessionMove{
				SessionID:       id,
				Project:         shortProject(project),
				FromModel:       origModelKey,
				ToModel:         rule.Target,
				OriginalUSD:     cost,
				HypotheticalUSD: hypo,
				DeltaUSD:        hypo - cost,
			})
		}
		bucket.HypotheticalUSD += hypo
		totalOrig += cost
		totalHypo += hypo
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Deterministic model ordering by original spend (descending) so the
	// UI legend matches the eye-order of the chart.
	byModelSlice := make([]SwapByModel, 0, len(byModel))
	for _, v := range byModel {
		byModelSlice = append(byModelSlice, *v)
	}
	sort.Slice(byModelSlice, func(i, j int) bool {
		return byModelSlice[i].OriginalUSD > byModelSlice[j].OriginalUSD
	})

	// Top movers: cap at 10 by |delta|. Negative deltas (repricing to a
	// pricier tier) belong here too — the caller should see both.
	sort.Slice(moves, func(i, j int) bool {
		return math.Abs(moves[i].DeltaUSD) > math.Abs(moves[j].DeltaUSD)
	})
	if len(moves) > 10 {
		moves = moves[:10]
	}

	delta := totalHypo - totalOrig
	var pct float64
	if totalOrig > 0 {
		pct = (delta / totalOrig) * 100
	}

	return &SwapResult{
		From:            from,
		To:              to,
		Rule:            rule,
		Sessions:        totalCount,
		Swapped:         swappedCount,
		OriginalUSD:     totalOrig,
		HypotheticalUSD: totalHypo,
		DeltaUSD:        delta,
		DeltaPct:        pct,
		ByModel:         byModelSlice,
		TopMoved:        moves,
		Caveat: "Token counts are held constant across the swap; Claude " +
			"families share a tokenizer within a generation, so the estimate " +
			"is close but not exact for cross-family swaps. Turn count, tool " +
			"use, and cache behavior are assumed unchanged.",
	}, nil
}

// shortProject trims a full project path to its basename for display.
// Duplicated (rather than imported from server) to keep this package
// dependency-free of the HTTP layer.
func shortProject(path string) string {
	if path == "" {
		return ""
	}
	// A minimal path.Base substitute — filepath would drag in an OS
	// package for something a slash split handles fine.
	last := strings.LastIndex(path, "/")
	if last < 0 || last == len(path)-1 {
		return path
	}
	return path[last+1:]
}
