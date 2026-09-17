package stats

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// BurstConfig tunes the sliding-window $/min burst detector. It is a
// separate signal from SpikeConfig: SpikeConfig answers "this session
// cost a lot in total"; BurstConfig answers "this session burned dollars
// extremely fast inside a short window." A long-running $50 session
// spread evenly across 8 hours is a total-cost spike but not a burst;
// a $5 session that burns $3 in 90 seconds is a burst but not a total
// spike. The two frequently disagree, and burst is the honest signal
// for "something is wrong right now."
type BurstConfig struct {
	// WindowMinutes is the sliding window we compute $/min over.
	WindowMinutes int
	// BaselineDays is how far back to look for the peer-set baseline.
	BaselineDays int
	// RatioVsBaseline: peak $/min must be at least this multiple of the
	// baseline P75 $/min to fire.
	RatioVsBaseline float64
	// MinAbsUSDPerMin: peak $/min must also exceed this absolute floor.
	// Guards against firing on infinity-ratios when the baseline is
	// near-zero (a brand-new install, or a user who normally runs
	// haiku-cheap sessions and does one small opus turn).
	MinAbsUSDPerMin float64
	// MinTurns: skip sessions with fewer than this many turns. A single
	// turn has no window to measure across.
	MinTurns int
}

// DefaultBurstConfig is tuned so a normal Sonnet coding session (which
// tops out around $0.03-$0.08 / min sustained) doesn't fire, but a
// runaway opus loop hitting $1+/min does.
func DefaultBurstConfig() BurstConfig {
	return BurstConfig{
		WindowMinutes:   5,
		BaselineDays:    30,
		RatioVsBaseline: 4.0,
		MinAbsUSDPerMin: 0.10,
		MinTurns:        2,
	}
}

// Burst describes the peak $/min window inside a single session.
type Burst struct {
	SessionID   string    `json:"session_id"`
	Model       string    `json:"model,omitempty"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	USDInWindow float64   `json:"usd_in_window"`
	USDPerMin   float64   `json:"usd_per_min"`
	BaselineP75 float64   `json:"baseline_p75_usd_per_min"`
	Ratio       float64   `json:"ratio"`
}

// TurnRow is the minimal turn view needed to score a burst. Mirrors the
// subset of db.Turn fields we care about here.
type TurnRow struct {
	EndedAt time.Time
	CostUSD float64
	Model   string
}

// PeakBurstInSession scans a session's turn series and returns the
// window with the highest $/min, or nil if the series is too short to
// score. The window is right-anchored on each turn's EndedAt: for turn
// i we sum costs of turns whose EndedAt falls in
// (turns[i].EndedAt - window, turns[i].EndedAt]. This treats each turn
// as "the moment we noticed" and asks what the burn rate was in the
// window leading up to it — the same shape a real-time monitor would use.
func PeakBurstInSession(turns []TurnRow, windowMinutes int) *Burst {
	if len(turns) < 2 || windowMinutes <= 0 {
		return nil
	}
	// Defensive sort — DB rows arrive ordered, but callers can construct
	// TurnRow slices by hand (tests, ad-hoc analysis) and out-of-order
	// input would produce garbage bursts.
	sort.Slice(turns, func(i, j int) bool { return turns[i].EndedAt.Before(turns[j].EndedAt) })
	window := time.Duration(windowMinutes) * time.Minute
	var best *Burst
	for i := range turns {
		end := turns[i].EndedAt
		start := end.Add(-window)
		sum := 0.0
		for j := i; j >= 0 && turns[j].EndedAt.After(start); j-- {
			sum += turns[j].CostUSD
		}
		if sum <= 0 {
			continue
		}
		rate := sum / float64(windowMinutes)
		if best == nil || rate > best.USDPerMin {
			b := Burst{
				WindowStart: start,
				WindowEnd:   end,
				USDInWindow: sum,
				USDPerMin:   rate,
				Model:       turns[i].Model,
			}
			best = &b
		}
	}
	return best
}

// BurstBaselineP75USDPerMin computes the P75 of peak $/min across every
// session that ended in the last `days` days and has enough turn data
// to score. Returns 0 when we don't have at least 3 peer sessions yet
// — early adopters get gated by the absolute floor instead of a ratio
// against a 1-sample baseline.
func BurstBaselineP75USDPerMin(conn *sql.DB, days, windowMinutes int) (float64, error) {
	if days <= 0 || windowMinutes <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339)
	rows, err := conn.Query(`
SELECT session_id, ended_at, cost_usd
FROM turns
WHERE session_id IN (SELECT id FROM sessions WHERE ended_at >= ?)
ORDER BY session_id, ended_at ASC`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("query turns for baseline: %w", err)
	}
	defer rows.Close()

	grouped := map[string][]TurnRow{}
	for rows.Next() {
		var sid, ts string
		var cost float64
		if err := rows.Scan(&sid, &ts, &cost); err != nil {
			return 0, fmt.Errorf("scan turn: %w", err)
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			// A malformed timestamp on one turn shouldn't corrupt the
			// baseline; skip it and keep going.
			continue
		}
		grouped[sid] = append(grouped[sid], TurnRow{EndedAt: t, CostUSD: cost})
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	peaks := make([]float64, 0, len(grouped))
	for _, ts := range grouped {
		if b := PeakBurstInSession(ts, windowMinutes); b != nil {
			peaks = append(peaks, b.USDPerMin)
		}
	}
	if len(peaks) < 3 {
		return 0, nil
	}
	return percentileFloat(peaks, 0.75), nil
}

// DetectBurstForSession scores one session against the current baseline.
// Returns nil (no error) when the session doesn't cross the fire
// threshold — the caller can treat that as "no burst worth alerting."
// The dual-guard fire condition is:
//
//	peak >= MinAbsUSDPerMin  AND  (baseline == 0 OR peak >= Ratio * baseline)
//
// When there's no baseline yet, the absolute floor gates alone.
func DetectBurstForSession(conn *sql.DB, sessionID string, cfg BurstConfig) (*Burst, error) {
	if cfg.WindowMinutes <= 0 || cfg.MinTurns <= 0 {
		return nil, fmt.Errorf("burst config: WindowMinutes and MinTurns must be positive")
	}
	rows, err := conn.Query(`
SELECT ended_at, cost_usd, COALESCE(model, '')
FROM turns
WHERE session_id = ?
ORDER BY ended_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query session turns: %w", err)
	}
	defer rows.Close()
	var turns []TurnRow
	for rows.Next() {
		var ts, model string
		var cost float64
		if err := rows.Scan(&ts, &cost, &model); err != nil {
			return nil, fmt.Errorf("scan turn: %w", err)
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		turns = append(turns, TurnRow{EndedAt: t, CostUSD: cost, Model: model})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(turns) < cfg.MinTurns {
		return nil, nil
	}
	best := PeakBurstInSession(turns, cfg.WindowMinutes)
	if best == nil || best.USDPerMin < cfg.MinAbsUSDPerMin {
		return nil, nil
	}
	baseline, err := BurstBaselineP75USDPerMin(conn, cfg.BaselineDays, cfg.WindowMinutes)
	if err != nil {
		return nil, fmt.Errorf("burst baseline: %w", err)
	}
	best.SessionID = sessionID
	best.BaselineP75 = baseline
	if baseline > 0 {
		best.Ratio = best.USDPerMin / baseline
		if best.USDPerMin < cfg.RatioVsBaseline*baseline {
			return nil, nil
		}
	}
	return best, nil
}
