package stats

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// PeriodSummary aggregates session-lens metrics for one date range, suitable
// for side-by-side display on the Compare page. "Working" tokens are
// input + output (i.e. excludes cache replay) — the only signal the user
// actually controls. Without this split, cache_read dominates and masks
// whether real usage went up or down.
type PeriodSummary struct {
	From             string       `json:"from"` // RFC3339 inclusive
	To               string       `json:"to"`   // RFC3339 exclusive
	Days             int          `json:"days"` // wall-clock days in [from, to)
	Sessions         int          `json:"sessions"`
	Turns            int          `json:"turns"`
	InputTokens      int64        `json:"input_tokens"`
	OutputTokens     int64        `json:"output_tokens"`
	CacheReadTokens  int64        `json:"cache_read_tokens"`
	CacheWriteTokens int64        `json:"cache_write_tokens"`
	TotalTokens      int64        `json:"total_tokens"`
	WorkingTokens    int64        `json:"working_tokens"` // input + output
	CostUSD          float64      `json:"cost_usd"`
	TokensPerSession float64      `json:"tokens_per_session"`
	CostPerSession   float64      `json:"cost_per_session"`
	CacheHitRatio    float64      `json:"cache_hit_ratio"` // cache_read / (cache_read + input + output)
	ModelMix         []ModelSlice `json:"model_mix"`       // sorted by cost desc
}

// ModelSlice is one slice of the model-mix for a period.
type ModelSlice struct {
	Model    string  `json:"model"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
	Sessions int     `json:"sessions"`
	SharePct float64 `json:"share_pct"` // cost share within the period
}

// DailyPoint is one bucket of working-tokens for the overlay chart. DayIndex
// is 1-based from the start of the period so the two periods can be overlaid
// on a common x-axis regardless of calendar offset or length. Despite the
// name, buckets can be hourly when the compare granularity is "hour" — Date
// then carries the RFC3339 hour ("2026-08-13T14:00Z") and Bucket carries the
// display label ("14:00"). Kept the "Daily" name to preserve the wire
// contract.
type DailyPoint struct {
	DayIndex      int     `json:"day_index"`
	Date          string  `json:"date"`   // YYYY-MM-DD (day gran) or YYYY-MM-DDTHH:00Z (hour gran)
	Bucket        string  `json:"bucket"` // display label: "" for day (UI uses Day N), "HH:00" for hour
	WorkingTokens int64   `json:"working_tokens"`
	TotalTokens   int64   `json:"total_tokens"`
	CostUSD       float64 `json:"cost_usd"`
}

// CompareResult is the JSON body for /v1/compare.
//
// Granularity is "day" or "hour" — auto-selected by Compare based on the
// longer of the two periods. Single-day compares would collapse to one point
// under day bucketing, so we drop to hourly buckets when both periods are
// short. The UI reads Granularity to label the x-axis.
type CompareResult struct {
	A           PeriodSummary `json:"a"`
	B           PeriodSummary `json:"b"`
	Granularity string        `json:"granularity"` // "day" | "hour"
	DailyA      []DailyPoint  `json:"daily_a"`
	DailyB      []DailyPoint  `json:"daily_b"`
}

// Compare computes the two-period diff. Ranges use [from, to) semantics
// (from inclusive, to exclusive) to match the rest of stats.
//
// Granularity auto-selects: when neither period spans more than 2 wall-clock
// days, buckets are hourly (up to 48 points, still readable); otherwise daily.
// This is what makes day-vs-day and hour-of-day compares work — under pure
// daily bucketing a 1-day range collapses to a single dot.
func Compare(conn *sql.DB, aFrom, aTo, bFrom, bTo time.Time) (*CompareResult, error) {
	gran := pickGranularity(aFrom, aTo, bFrom, bTo)
	a, dailyA, err := computePeriod(conn, aFrom, aTo, gran)
	if err != nil {
		return nil, fmt.Errorf("period A: %w", err)
	}
	b, dailyB, err := computePeriod(conn, bFrom, bTo, gran)
	if err != nil {
		return nil, fmt.Errorf("period B: %w", err)
	}
	return &CompareResult{A: *a, B: *b, Granularity: gran, DailyA: dailyA, DailyB: dailyB}, nil
}

// pickGranularity returns "hour" if both periods span ≤ 2 days, else "day".
// Ceils to full days so a 25-hour range still counts as 2 days.
func pickGranularity(aFrom, aTo, bFrom, bTo time.Time) string {
	maxSpan := aTo.Sub(aFrom)
	if s := bTo.Sub(bFrom); s > maxSpan {
		maxSpan = s
	}
	if maxSpan <= 48*time.Hour {
		return "hour"
	}
	return "day"
}

func computePeriod(conn *sql.DB, from, to time.Time, gran string) (*PeriodSummary, []DailyPoint, error) {
	sum := &PeriodSummary{
		From: from.Format(time.RFC3339),
		To:   to.Format(time.RFC3339),
	}
	if !from.Before(to) {
		return sum, []DailyPoint{}, nil
	}
	sum.Days = int(to.Sub(from).Hours()/24.0 + 0.5)

	const sessQ = `
SELECT model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
       total_cost_usd, turns
FROM sessions
WHERE ended_at >= ? AND ended_at < ?
`
	rows, err := conn.Query(sessQ, from.Format(time.RFC3339), to.Format(time.RFC3339))
	if err != nil {
		return nil, nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	modelAgg := map[string]*ModelSlice{}
	for rows.Next() {
		var (
			model                  string
			inT, outT, crT, cwT    int64
			cost                   float64
			turns                  int
		)
		if err := rows.Scan(&model, &inT, &outT, &crT, &cwT, &cost, &turns); err != nil {
			return nil, nil, fmt.Errorf("scan session: %w", err)
		}
		sum.Sessions++
		sum.Turns += turns
		sum.InputTokens += inT
		sum.OutputTokens += outT
		sum.CacheReadTokens += crT
		sum.CacheWriteTokens += cwT
		sum.CostUSD += cost

		m := modelAgg[model]
		if m == nil {
			m = &ModelSlice{Model: model}
			modelAgg[model] = m
		}
		m.Tokens += inT + outT + crT + cwT
		m.CostUSD += cost
		m.Sessions++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iter sessions: %w", err)
	}

	sum.TotalTokens = sum.InputTokens + sum.OutputTokens + sum.CacheReadTokens + sum.CacheWriteTokens
	sum.WorkingTokens = sum.InputTokens + sum.OutputTokens
	if sum.Sessions > 0 {
		sum.TokensPerSession = float64(sum.TotalTokens) / float64(sum.Sessions)
		sum.CostPerSession = sum.CostUSD / float64(sum.Sessions)
	}
	// Cache hit ratio: of all tokens we generated/asked, how much was cached
	// context-replay vs fresh I/O? Higher = more session-context reuse, which
	// usually correlates with longer sessions.
	denom := sum.CacheReadTokens + sum.InputTokens + sum.OutputTokens
	if denom > 0 {
		sum.CacheHitRatio = float64(sum.CacheReadTokens) / float64(denom)
	}

	for _, m := range modelAgg {
		if sum.CostUSD > 0 {
			m.SharePct = (m.CostUSD / sum.CostUSD) * 100.0
		}
		sum.ModelMix = append(sum.ModelMix, *m)
	}
	sort.Slice(sum.ModelMix, func(i, j int) bool {
		return sum.ModelMix[i].CostUSD > sum.ModelMix[j].CostUSD
	})

	// Bucketed working/total tokens + cost, gap-filled to zero so the chart
	// is continuous and overlays cleanly across two periods of different
	// length. The GROUP BY expression and gap-fill stride switch on gran.
	var (
		bucketExpr string
		step       time.Duration
		fmtLayout  string
	)
	if gran == "hour" {
		// strftime is SQLite-specific but session-lens is SQLite-only.
		bucketExpr = `strftime('%Y-%m-%dT%H:00Z', ended_at)`
		step = time.Hour
		fmtLayout = "2006-01-02T15:04Z"
	} else {
		bucketExpr = `substr(ended_at, 1, 10)`
		step = 24 * time.Hour
		fmtLayout = "2006-01-02"
	}

	dayQ := `
SELECT ` + bucketExpr + ` AS bucket,
       COALESCE(SUM(input_tokens + output_tokens), 0) AS working,
       COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens), 0) AS total,
       COALESCE(SUM(total_cost_usd), 0) AS cost
FROM sessions
WHERE ended_at >= ? AND ended_at < ?
GROUP BY bucket
ORDER BY bucket ASC
`
	drows, err := conn.Query(dayQ, from.Format(time.RFC3339), to.Format(time.RFC3339))
	if err != nil {
		return nil, nil, fmt.Errorf("query buckets: %w", err)
	}
	defer drows.Close()

	byDate := map[string]DailyPoint{}
	for drows.Next() {
		var d DailyPoint
		if err := drows.Scan(&d.Date, &d.WorkingTokens, &d.TotalTokens, &d.CostUSD); err != nil {
			return nil, nil, fmt.Errorf("scan bucket: %w", err)
		}
		byDate[d.Date] = d
	}
	if err := drows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iter buckets: %w", err)
	}

	// Gap-fill: walk from `from` to `to` in `step` increments. Anchor hourly
	// to the top of the hour so bucket labels line up with strftime output;
	// daily anchors to UTC midnight naturally via YYYY-MM-DD.
	start := from
	if gran == "hour" {
		start = from.UTC().Truncate(time.Hour)
	}
	nBuckets := int(to.Sub(start) / step)
	if to.Sub(start)%step != 0 {
		nBuckets++
	}
	daily := make([]DailyPoint, 0, nBuckets)
	for i := 0; i < nBuckets; i++ {
		at := start.Add(time.Duration(i) * step)
		if !at.Before(to) {
			break
		}
		key := at.UTC().Format(fmtLayout)
		idx := i + 1
		point := DailyPoint{DayIndex: idx, Date: key}
		if existing, ok := byDate[key]; ok {
			point.WorkingTokens = existing.WorkingTokens
			point.TotalTokens = existing.TotalTokens
			point.CostUSD = existing.CostUSD
		}
		if gran == "hour" {
			point.Bucket = at.UTC().Format("15:04")
		}
		daily = append(daily, point)
	}
	return sum, daily, nil
}
