package stats

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// SpikeKind enumerates the two anomaly types surfaced to the dashboard.
type SpikeKind string

const (
	// SpikeSession means a single session vastly exceeded the median session
	// cost over the recent window.
	SpikeSession SpikeKind = "session"
	// SpikeTrend means a calendar day's total tokens exceeded 2x the rolling
	// 7-day median.
	SpikeTrend SpikeKind = "trend"
)

// Spike is one anomaly worth surfacing to the user.
type Spike struct {
	Kind      SpikeKind `json:"kind"`
	Timestamp string    `json:"timestamp"`
	Project   string    `json:"project,omitempty"`
	Value     float64   `json:"value"`
	Baseline  float64   `json:"baseline"`
	Ratio     float64   `json:"ratio"`
	Severity  string    `json:"severity"` // low | medium | high
}

// SpikeConfig tunes the detection thresholds. SessionWindowN is interpreted
// as DAYS (the baseline is the median session cost over that many calendar
// days preceding each session — previously this field meant "prior N
// sessions", which gave very different results for high- vs low-volume
// users). The notification and dashboard panel share this baseline so the
// two signals stay in lockstep.
type SpikeConfig struct {
	SessionWindowN int     // baseline = median session cost over the prior N days
	SessionRatio   float64 // multiplier above median to count as a spike
	TrendRatio     float64 // day vs 7-day median multiplier
}

// DefaultSpikeConfig is the sensible default surfaced in the spec.
func DefaultSpikeConfig() SpikeConfig {
	return SpikeConfig{
		SessionWindowN: 20,
		SessionRatio:   3.0,
		TrendRatio:     2.0,
	}
}

// SessionRecord is the minimal session view needed for spike scoring.
type SessionRecord struct {
	ID          string
	EndedAt     time.Time
	ProjectPath string
	CostUSD     float64
	Tokens      int64
}

// DayRecord is the rollup view for trend-deviation scoring.
type DayRecord struct {
	Date   string // YYYY-MM-DD
	Tokens int64
}

// DetectSpikes runs both detectors and returns the union sorted newest-first.
// `sessions` must be sorted oldest-first; `days` must also be oldest-first.
func DetectSpikes(sessions []SessionRecord, days []DayRecord, cfg SpikeConfig) []Spike {
	if cfg.SessionWindowN <= 0 {
		cfg.SessionWindowN = 20
	}
	if cfg.SessionRatio <= 0 {
		cfg.SessionRatio = 3.0
	}
	if cfg.TrendRatio <= 0 {
		cfg.TrendRatio = 2.0
	}

	out := make([]Spike, 0, 16)
	out = append(out, detectSessionSpikes(sessions, cfg)...)
	out = append(out, detectTrendSpikes(days, cfg)...)

	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp > out[j].Timestamp
	})
	return out
}

func detectSessionSpikes(sessions []SessionRecord, cfg SpikeConfig) []Spike {
	if len(sessions) < 3 {
		return nil
	}
	windowDur := time.Duration(cfg.SessionWindowN) * 24 * time.Hour
	out := make([]Spike, 0)
	for i, s := range sessions {
		// Baseline = P75 cost of sessions ended in [s.EndedAt - windowDur,
		// s.EndedAt). Same shape as BaselineP75CostUSD but evaluated against
		// each session's contemporaneous history so an old spike is judged
		// against the cost regime that existed at the time, not today's.
		earliest := s.EndedAt.Add(-windowDur)
		lo := i - 1
		for lo >= 0 && !sessions[lo].EndedAt.Before(earliest) {
			lo--
		}
		// Window is sessions[lo+1 : i] — strictly older than this session and
		// no older than `windowDur` before it. Need at least 3 samples or the
		// P75 is too jittery to flag against.
		window := sessions[lo+1 : i]
		if len(window) < 3 {
			continue
		}
		baseline := p75Cost(window)
		if baseline <= 0 {
			continue
		}
		if s.CostUSD >= baseline*cfg.SessionRatio {
			ratio := s.CostUSD / baseline
			out = append(out, Spike{
				Kind:      SpikeSession,
				Timestamp: s.EndedAt.UTC().Format(time.RFC3339),
				Project:   s.ProjectPath,
				Value:     s.CostUSD,
				Baseline:  baseline,
				Ratio:     ratio,
				Severity:  severityFor(ratio, cfg.SessionRatio),
			})
		}
	}
	return out
}

func detectTrendSpikes(days []DayRecord, cfg SpikeConfig) []Spike {
	if len(days) < 4 {
		return nil
	}
	out := make([]Spike, 0)
	for i, d := range days {
		// Need at least three prior days to form a 7-day rolling baseline.
		start := i - 7
		if start < 0 {
			start = 0
		}
		if i-start < 3 {
			continue
		}
		window := days[start:i]
		baseline := medianTokens(window)
		if baseline <= 0 {
			continue
		}
		if float64(d.Tokens) >= baseline*cfg.TrendRatio {
			ratio := float64(d.Tokens) / baseline
			out = append(out, Spike{
				Kind:      SpikeTrend,
				Timestamp: d.Date,
				Value:     float64(d.Tokens),
				Baseline:  baseline,
				Ratio:     ratio,
				Severity:  severityFor(ratio, cfg.TrendRatio),
			})
		}
	}
	return out
}

func medianCost(rows []SessionRecord) float64 {
	if len(rows) == 0 {
		return 0
	}
	values := make([]float64, len(rows))
	for i, r := range rows {
		values[i] = r.CostUSD
	}
	return medianFloat(values)
}

// p75Cost returns the 75th percentile of session costs in `rows`. We use P75
// rather than median for session baselines because Claude Code generates a
// long tail of micro-sessions (<$0.50) that drag the median to noise levels —
// e.g. a $1 median triggered "spike" alerts for $5 sessions that are actually
// routine. P75 anchors the baseline on the upper half of meaningful sessions.
func p75Cost(rows []SessionRecord) float64 {
	if len(rows) == 0 {
		return 0
	}
	values := make([]float64, len(rows))
	for i, r := range rows {
		values[i] = r.CostUSD
	}
	return percentileFloat(values, 0.75)
}

func medianTokens(rows []DayRecord) float64 {
	if len(rows) == 0 {
		return 0
	}
	values := make([]float64, len(rows))
	for i, r := range rows {
		values[i] = float64(r.Tokens)
	}
	return medianFloat(values)
}

func medianFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2.0
}

// percentileFloat returns the p-th percentile (0..1) using linear
// interpolation between the two nearest ranks. Matches numpy's default
// `linear` method so results align with any external analysis.
func percentileFloat(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if p < 0 {
		p = 0
	} else if p > 1 {
		p = 1
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	n := len(sorted)
	if n == 1 {
		return sorted[0]
	}
	rank := float64(n-1) * p
	lo := int(rank)
	hi := lo + 1
	if hi >= n {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

func severityFor(ratio, threshold float64) string {
	switch {
	case ratio >= threshold*3:
		return "high"
	case ratio >= threshold*1.5:
		return "medium"
	default:
		return "low"
	}
}

// Spikes is the DB-backed convenience: pulls the recent sessions + daily
// totals, runs detection, and returns the result.
func Spikes(conn *sql.DB, cfg SpikeConfig) ([]Spike, error) {
	// 1) Pull recent sessions (90 days, ordered oldest-first).
	since := time.Now().UTC().AddDate(0, 0, -90)
	cutoff := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	const sessQ = `
SELECT id, ended_at, COALESCE(project_path,''),
       total_cost_usd,
       input_tokens + output_tokens + cache_read_tokens + cache_write_tokens
FROM sessions
WHERE ended_at >= ?
ORDER BY ended_at ASC
`
	rows, err := conn.Query(sessQ, cutoff.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("spikes sessions: %w", err)
	}
	sessions := make([]SessionRecord, 0, 64)
	for rows.Next() {
		var (
			id, endedAt, project string
			cost                 float64
			tokens               int64
		)
		if err := rows.Scan(&id, &endedAt, &project, &cost, &tokens); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan spike session: %w", err)
		}
		t, err := time.Parse(time.RFC3339, endedAt)
		if err != nil {
			// Best-effort: try date-only.
			if t2, err2 := time.Parse("2006-01-02", endedAt); err2 == nil {
				t = t2
			}
		}
		sessions = append(sessions, SessionRecord{
			ID: id, EndedAt: t, ProjectPath: project, CostUSD: cost, Tokens: tokens,
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iter sessions: %w", err)
	}

	// 2) Pull daily totals (90 days, ordered oldest-first).
	const dayQ = `
SELECT substr(ended_at,1,10) AS day,
       COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens),0)
FROM sessions
WHERE ended_at >= ?
GROUP BY day
ORDER BY day ASC
`
	drows, err := conn.Query(dayQ, cutoff.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("spikes days: %w", err)
	}
	days := make([]DayRecord, 0, 90)
	for drows.Next() {
		var d DayRecord
		if err := drows.Scan(&d.Date, &d.Tokens); err != nil {
			drows.Close()
			return nil, fmt.Errorf("scan spike day: %w", err)
		}
		days = append(days, d)
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return nil, fmt.Errorf("rows iter days: %w", err)
	}

	return DetectSpikes(sessions, days, cfg), nil
}

// BaselineP75CostUSD returns the 75th percentile total_cost_usd of sessions
// that ended in the `days` calendar days prior to today (today itself is
// excluded so the baseline is stable across the day). Returns 0 if there are
// fewer than 3 qualifying rows — too small a sample to flag against. Used by
// both the live spike notification and the dashboard Spikes panel so they
// share one definition of "normal". P75 (not median) keeps the baseline
// honest when the distribution is dominated by sub-dollar micro-sessions.
func BaselineP75CostUSD(conn *sql.DB, days int) (float64, error) {
	if days <= 0 {
		days = 20
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	windowStart := today.AddDate(0, 0, -days)

	const q = `
SELECT total_cost_usd
FROM sessions
WHERE ended_at >= ?
  AND ended_at < ?
`
	rows, err := conn.Query(q, windowStart.Format(time.RFC3339), today.Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("baseline median: %w", err)
	}
	defer rows.Close()
	var values []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return 0, fmt.Errorf("scan baseline row: %w", err)
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iter baseline rows: %w", err)
	}
	if len(values) < 3 {
		return 0, nil
	}
	return percentileFloat(values, 0.75), nil
}
