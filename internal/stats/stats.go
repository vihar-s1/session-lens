// Package stats provides read-only aggregate queries over the sessions table:
// current-month summary, daily/weekly buckets, hourly granular series, per-model
// breakdowns, top-project rollups, and spike detection.
package stats

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Summary is the current-calendar-month rollup.
type Summary struct {
	TotalInput         int64   `json:"total_input"`
	TotalOutput        int64   `json:"total_output"`
	TotalCacheRead     int64   `json:"total_cache_read"`
	TotalCacheWrite    int64   `json:"total_cache_write"`
	TotalTokens        int64   `json:"total_tokens"`
	TotalCostUSD       float64 `json:"total_cost_usd"`
	SessionCount       int64   `json:"session_count"`
	PlanBudgetUSD      float64 `json:"plan_budget_usd"`
	PlanUtilisationPct float64 `json:"plan_utilisation_pct"`
}

// Bucket is a generic time-bucket aggregate (used for daily and weekly).
type Bucket struct {
	Bucket           string  `json:"bucket"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	SessionCount     int64   `json:"session_count"`
}

// Project is a per-project rollup row.
type Project struct {
	ProjectPath      string  `json:"project_path"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	SessionCount     int64   `json:"session_count"`
}

// ModelTotal is a per-family rollup across all sessions.
type ModelTotal struct {
	Family           string  `json:"family"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	SessionCount     int64   `json:"session_count"`
}

// DailyByModel is one date's usage broken down by family. Totals and
// Costs mirror each other key-wise (same families) so the client can
// toggle its metric without a round-trip.
type DailyByModel struct {
	Bucket string             `json:"bucket"`
	Totals map[string]int64   `json:"totals"` // family -> tokens
	Costs  map[string]float64 `json:"costs"`  // family -> USD
}

// ByModelResponse is what /v1/stats/by-model returns: family totals plus the
// daily stacked-bar series.
type ByModelResponse struct {
	Totals []ModelTotal   `json:"totals"`
	Daily  []DailyByModel `json:"daily"`
}

// MonthSummary aggregates sessions ended in the current UTC calendar month.
func MonthSummary(conn *sql.DB, planBudget float64) (Summary, error) {
	return monthSummaryAt(conn, planBudget, time.Now().UTC())
}

func monthSummaryAt(conn *sql.DB, planBudget float64, now time.Time) (Summary, error) {
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	const q = `
SELECT
  COALESCE(SUM(input_tokens),0),
  COALESCE(SUM(output_tokens),0),
  COALESCE(SUM(cache_read_tokens),0),
  COALESCE(SUM(cache_write_tokens),0),
  COALESCE(SUM(total_cost_usd),0),
  COUNT(1)
FROM sessions
WHERE ended_at >= ?
`
	var s Summary
	err := conn.QueryRow(q, monthStart.Format(time.RFC3339)).Scan(
		&s.TotalInput, &s.TotalOutput, &s.TotalCacheRead, &s.TotalCacheWrite,
		&s.TotalCostUSD, &s.SessionCount,
	)
	if err != nil {
		return Summary{}, fmt.Errorf("month summary: %w", err)
	}
	s.TotalTokens = s.TotalInput + s.TotalOutput + s.TotalCacheRead + s.TotalCacheWrite
	s.PlanBudgetUSD = planBudget
	if planBudget > 0 {
		s.PlanUtilisationPct = (s.TotalCostUSD / planBudget) * 100.0
	}
	return s, nil
}

// Daily returns one bucket per day for the last `days` days (newest last).
func Daily(conn *sql.DB, days int) ([]Bucket, error) {
	if days <= 0 {
		days = 30
	}
	if days > 365 {
		days = 365
	}
	since := time.Now().UTC().AddDate(0, 0, -days+1)
	cutoff := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	const q = `
SELECT
  substr(ended_at, 1, 10) AS bucket,
  COALESCE(SUM(input_tokens),0),
  COALESCE(SUM(output_tokens),0),
  COALESCE(SUM(cache_read_tokens),0),
  COALESCE(SUM(cache_write_tokens),0),
  COALESCE(SUM(total_cost_usd),0),
  COUNT(1)
FROM sessions
WHERE ended_at >= ?
GROUP BY bucket
ORDER BY bucket ASC
`
	return queryBuckets(conn, q, cutoff.Format(time.RFC3339))
}

// Weekly returns one ISO-week bucket per week for the last `weeks` weeks.
func Weekly(conn *sql.DB, weeks int) ([]Bucket, error) {
	if weeks <= 0 {
		weeks = 12
	}
	if weeks > 104 {
		weeks = 104
	}
	since := time.Now().UTC().AddDate(0, 0, -7*weeks+1)
	cutoff := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	// SQLite strftime('%Y-%W', ...) gives ISO-ish year-week pairs.
	const q = `
SELECT
  strftime('%Y-W%W', ended_at) AS bucket,
  COALESCE(SUM(input_tokens),0),
  COALESCE(SUM(output_tokens),0),
  COALESCE(SUM(cache_read_tokens),0),
  COALESCE(SUM(cache_write_tokens),0),
  COALESCE(SUM(total_cost_usd),0),
  COUNT(1)
FROM sessions
WHERE ended_at >= ?
GROUP BY bucket
ORDER BY bucket ASC
`
	return queryBuckets(conn, q, cutoff.Format(time.RFC3339))
}

// Granularity selects the bucket size for the timeseries Hourly query.
// "hour" stays the wire-default for backward compat with older clients; the
// dashboard sends an explicit value sized to the window.
type Granularity string

const (
	Gran15m  Granularity = "15m"
	GranHour Granularity = "hour"
	Gran6h   Granularity = "6h"
	GranDay  Granularity = "day"
)

// ParseGranularity is permissive: unknown / empty input falls back to hour
// so a malformed query string degrades gracefully rather than 500-ing.
func ParseGranularity(s string) Granularity {
	switch strings.ToLower(s) {
	case "15m", "15min", "quarter":
		return Gran15m
	case "6h", "6hr", "6hour":
		return Gran6h
	case "day", "d", "1d", "daily":
		return GranDay
	default:
		return GranHour
	}
}

// step returns the wall-clock duration each bucket covers. Used for
// gap-filling so the timeline stays contiguous.
func (g Granularity) step() time.Duration {
	switch g {
	case Gran15m:
		return 15 * time.Minute
	case Gran6h:
		return 6 * time.Hour
	case GranDay:
		return 24 * time.Hour
	default:
		return time.Hour
	}
}

// sqlBucket is the SQLite expression that maps a row's `ended_at` to the
// label for its bucket. The label format is always ISO 8601 ("...HH:MMZ")
// so the frontend can plot the series without any granularity-aware parser.
// `col` is the SQL column ref, e.g. "t.ended_at".
func (g Granularity) sqlBucket(col string) string {
	switch g {
	case Gran15m:
		return "strftime('%Y-%m-%dT%H:', " + col + ") || printf('%02d', (cast(strftime('%M', " + col + ") AS INTEGER) / 15) * 15) || 'Z'"
	case Gran6h:
		return "strftime('%Y-%m-%dT', " + col + ") || printf('%02d:00Z', (cast(strftime('%H', " + col + ") AS INTEGER) / 6) * 6)"
	case GranDay:
		return "strftime('%Y-%m-%dT00:00Z', " + col + ")"
	default:
		return "strftime('%Y-%m-%dT%H:00Z', " + col + ")"
	}
}

// truncate rounds a time down to the start of its bucket. Used by the
// gap-fill walk so the synthesized buckets land on the same boundaries the
// SQL bucketer produces.
func (g Granularity) truncate(t time.Time) time.Time {
	t = t.UTC()
	switch g {
	case Gran15m:
		min := (t.Minute() / 15) * 15
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), min, 0, 0, time.UTC)
	case Gran6h:
		h := (t.Hour() / 6) * 6
		return time.Date(t.Year(), t.Month(), t.Day(), h, 0, 0, 0, time.UTC)
	case GranDay:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	default:
		return t.Truncate(time.Hour)
	}
}

// labelFormat is the time.Format layout that matches the SQL label produced
// by sqlBucket. Used by the gap-fill walk so synthesized labels match the
// real ones — drift here would silently double up bucket keys.
func (g Granularity) labelFormat() string {
	switch g {
	case Gran15m:
		return "2006-01-02T15:04Z"
	case Gran6h, GranHour:
		return "2006-01-02T15:00Z"
	case GranDay:
		return "2006-01-02T00:00Z"
	}
	return "2006-01-02T15:00Z"
}

// Hourly returns one bucket per granularity-step for the last `days` days.
// The label format is ISO 8601 ("YYYY-MM-DDTHH:MMZ") so the UI can plot a
// time-series directly. project is optional — empty string aggregates across
// all projects; a non-empty value filters by exact project_path match.
// Granularity controls the bucket size: 15m / hour / 6h / day.
func Hourly(conn *sql.DB, days int, project string, gran Granularity) ([]Bucket, error) {
	if days <= 0 {
		days = 7
	}
	if days > 90 {
		days = 90
	}
	if gran == "" {
		gran = GranHour
	}
	// Rolling window — days=1 means "last 24h" (not "today UTC"), so the
	// 1d view never appears empty just because nothing happened in the
	// current UTC calendar day yet.
	//
	// Buckets aggregate per-turn rows by the turn's own timestamp, so a
	// long-running session contributes to every bucket it was active rather
	// than dumping its whole total into the bucket it last Stopped. The
	// session_count column is approximated as the number of distinct
	// sessions that contributed turns to the bucket — a turn-count would be
	// misleading at the row level.
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	bucketExpr := gran.sqlBucket("t.ended_at")
	q := `
SELECT
  ` + bucketExpr + ` AS bucket,
  COALESCE(SUM(t.input_tokens),0),
  COALESCE(SUM(t.output_tokens),0),
  COALESCE(SUM(t.cache_read_tokens),0),
  COALESCE(SUM(t.cache_write_tokens),0),
  COALESCE(SUM(t.cost_usd),0),
  COUNT(DISTINCT t.session_id)
FROM turns t`
	args := []any{}
	if project != "" {
		q += ` JOIN sessions s ON s.id = t.session_id WHERE t.ended_at >= ? AND s.project_path = ?`
		args = append(args, cutoff.Format(time.RFC3339), project)
	} else {
		q += ` WHERE t.ended_at >= ?`
		args = append(args, cutoff.Format(time.RFC3339))
	}
	q += `
GROUP BY bucket
ORDER BY bucket ASC
`
	got, err := queryBuckets(conn, q, args...)
	if err != nil {
		return nil, err
	}
	return FillTimeseriesGaps(got, cutoff, time.Now().UTC(), gran), nil
}

// FillTimeseriesGaps returns a contiguous series from floor(start) to
// floor(end) at the given granularity's step, with zero-valued buckets
// inserted for any slots absent from `have`. Without this the chart skips
// idle slots, making it impossible to distinguish "no usage" from "no data".
// Exported so the mock dataset can share the same fill semantics.
func FillTimeseriesGaps(have []Bucket, start, end time.Time, gran Granularity) []Bucket {
	if gran == "" {
		gran = GranHour
	}
	step := gran.step()
	start = gran.truncate(start)
	end = gran.truncate(end)
	if end.Before(start) {
		end = start
	}
	byKey := make(map[string]Bucket, len(have))
	for _, b := range have {
		byKey[b.Bucket] = b
	}
	layout := gran.labelFormat()
	slots := int(end.Sub(start)/step) + 1
	out := make([]Bucket, 0, slots)
	for t := start; !t.After(end); t = t.Add(step) {
		key := t.Format(layout)
		if b, ok := byKey[key]; ok {
			out = append(out, b)
		} else {
			out = append(out, Bucket{Bucket: key})
		}
	}
	return out
}

// FillHourlyGaps is the legacy hourly-only gap-filler retained so the mock
// dataset and any test using the old API keep working unchanged.
func FillHourlyGaps(have []Bucket, start, end time.Time) []Bucket {
	return FillTimeseriesGaps(have, start, end, GranHour)
}

func queryBuckets(conn *sql.DB, q string, args ...any) ([]Bucket, error) {
	rows, err := conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query buckets: %w", err)
	}
	defer rows.Close()

	out := make([]Bucket, 0, 32)
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(
			&b.Bucket, &b.InputTokens, &b.OutputTokens,
			&b.CacheReadTokens, &b.CacheWriteTokens,
			&b.TotalCostUSD, &b.SessionCount,
		); err != nil {
			return nil, fmt.Errorf("scan bucket: %w", err)
		}
		b.TotalTokens = b.InputTokens + b.OutputTokens + b.CacheReadTokens + b.CacheWriteTokens
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iter: %w", err)
	}
	return out, nil
}

// Projects returns top-N projects ordered by total_cost_usd DESC. If `since`
// is a zero time, the rollup spans all sessions; otherwise it includes only
// sessions whose ended_at is on or after `since`.
func Projects(conn *sql.DB, limit int, since time.Time) ([]Project, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	var (
		q    string
		args []any
	)
	if since.IsZero() {
		q = `
SELECT
  COALESCE(project_path,'(unknown)') AS project_path,
  COALESCE(SUM(input_tokens),0),
  COALESCE(SUM(output_tokens),0),
  COALESCE(SUM(cache_read_tokens),0),
  COALESCE(SUM(cache_write_tokens),0),
  COALESCE(SUM(total_cost_usd),0),
  COUNT(1)
FROM sessions
GROUP BY project_path
ORDER BY SUM(total_cost_usd) DESC
LIMIT ?
`
		args = []any{limit}
	} else {
		q = `
SELECT
  COALESCE(project_path,'(unknown)') AS project_path,
  COALESCE(SUM(input_tokens),0),
  COALESCE(SUM(output_tokens),0),
  COALESCE(SUM(cache_read_tokens),0),
  COALESCE(SUM(cache_write_tokens),0),
  COALESCE(SUM(total_cost_usd),0),
  COUNT(1)
FROM sessions
WHERE ended_at >= ?
GROUP BY project_path
ORDER BY SUM(total_cost_usd) DESC
LIMIT ?
`
		args = []any{since.Format(time.RFC3339), limit}
	}
	rows, err := conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("projects query: %w", err)
	}
	defer rows.Close()

	out := make([]Project, 0, 16)
	for rows.Next() {
		var p Project
		if err := rows.Scan(
			&p.ProjectPath, &p.InputTokens, &p.OutputTokens,
			&p.CacheReadTokens, &p.CacheWriteTokens,
			&p.TotalCostUSD, &p.SessionCount,
		); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		p.TotalTokens = p.InputTokens + p.OutputTokens + p.CacheReadTokens + p.CacheWriteTokens
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iter: %w", err)
	}
	return out, nil
}

// ModelRow is what ByModel scans from a session row before aggregation.
type ModelRow struct {
	EndedAt          string
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalCostUSD     float64
}

// ByModel returns per-model totals plus a per-day breakdown for the last `days`
// days. Family is one of "opus" | "sonnet" | "haiku" | "other".
func ByModel(conn *sql.DB, days int) (ByModelResponse, error) {
	if days <= 0 {
		days = 14
	}
	if days > 365 {
		days = 365
	}
	since := time.Now().UTC().AddDate(0, 0, -days+1)
	cutoff := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	const q = `
SELECT ended_at, COALESCE(model,''),
       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
       total_cost_usd
FROM sessions
WHERE ended_at >= ?
`
	rows, err := conn.Query(q, cutoff.Format(time.RFC3339))
	if err != nil {
		return ByModelResponse{}, fmt.Errorf("by-model query: %w", err)
	}
	defer rows.Close()

	collected := make([]ModelRow, 0, 64)
	for rows.Next() {
		var r ModelRow
		if err := rows.Scan(
			&r.EndedAt, &r.Model,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
			&r.TotalCostUSD,
		); err != nil {
			return ByModelResponse{}, fmt.Errorf("scan by-model: %w", err)
		}
		collected = append(collected, r)
	}
	if err := rows.Err(); err != nil {
		return ByModelResponse{}, fmt.Errorf("rows iter: %w", err)
	}
	return AggregateByModel(collected), nil
}

// AggregateByModel is the pure function that builds a ByModelResponse from a
// flat slice of rows. Exposed for testing and for the mock data path.
func AggregateByModel(rows []ModelRow) ByModelResponse {
	totals := map[string]*ModelTotal{}
	dailyTokens := map[string]map[string]int64{}   // bucket -> family -> tokens
	dailyCosts := map[string]map[string]float64{}  // bucket -> family -> USD

	for _, r := range rows {
		fam := ModelFamily(r.Model)
		t, ok := totals[fam]
		if !ok {
			t = &ModelTotal{Family: fam}
			totals[fam] = t
		}
		t.InputTokens += r.InputTokens
		t.OutputTokens += r.OutputTokens
		t.CacheReadTokens += r.CacheReadTokens
		t.CacheWriteTokens += r.CacheWriteTokens
		t.TotalCostUSD += r.TotalCostUSD
		t.SessionCount++
		tokens := r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWriteTokens
		t.TotalTokens += tokens

		day := r.EndedAt
		if len(day) >= 10 {
			day = day[:10]
		}
		if _, ok := dailyTokens[day]; !ok {
			dailyTokens[day] = map[string]int64{}
			dailyCosts[day] = map[string]float64{}
		}
		dailyTokens[day][fam] += tokens
		dailyCosts[day][fam] += r.TotalCostUSD
	}

	// Deterministic ordering for both slices.
	totalsOut := make([]ModelTotal, 0, len(totals))
	for _, v := range totals {
		totalsOut = append(totalsOut, *v)
	}
	sort.Slice(totalsOut, func(i, j int) bool {
		return totalsOut[i].TotalCostUSD > totalsOut[j].TotalCostUSD
	})

	days := make([]string, 0, len(dailyTokens))
	for d := range dailyTokens {
		days = append(days, d)
	}
	sort.Strings(days)
	dailyOut := make([]DailyByModel, 0, len(days))
	for _, d := range days {
		dailyOut = append(dailyOut, DailyByModel{
			Bucket: d,
			Totals: dailyTokens[d],
			Costs:  dailyCosts[d],
		})
	}
	return ByModelResponse{Totals: totalsOut, Daily: dailyOut}
}

// KnownModelFamilies is the curated list of Claude model families we ship
// with a stable color, pricing, and display slot for. When Anthropic ships
// a genuinely new model tier (e.g. "fable"), add it here so it gets a
// deterministic bucket and colored ordering instead of falling into the
// hash-derived "unknown" path. UI display order also follows this list.
var KnownModelFamilies = []string{"opus", "sonnet", "haiku", "fable"}

// ModelFamily classifies a model string into a coarse family bucket.
// Order of precedence:
//  1. Exact match against a KnownModelFamilies substring — cheap and
//     covers historical IDs like "claude-3-5-haiku-20241022".
//  2. Parse the family segment out of a "claude-<family>-<version>[...]"
//     shape. This makes brand-new model IDs auto-integrate: they show up
//     as their own bucket in the dashboard without a code change.
//  3. Fallback "other" — empty input or a string that isn't a Claude ID.
func ModelFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return "other"
	}
	for _, fam := range KnownModelFamilies {
		if strings.Contains(m, fam) {
			return fam
		}
	}
	if fam := parseClaudeFamily(m); fam != "" {
		return fam
	}
	return "other"
}

// parseClaudeFamily pulls the family segment out of a claude-<family>-...
// model id. Returns "" if the input doesn't match. Handles both historical
// "claude-3-5-haiku-YYYYMMDD" (family follows numeric version segments)
// and modern "claude-<family>-<major>-<minor>" shapes by returning the
// first non-numeric segment after the "claude-" prefix.
func parseClaudeFamily(model string) string {
	const prefix = "claude-"
	if !strings.HasPrefix(model, prefix) {
		return ""
	}
	for _, seg := range strings.Split(model[len(prefix):], "-") {
		if seg == "" {
			continue
		}
		if isAllDigit(seg) {
			continue
		}
		return seg
	}
	return ""
}

func isAllDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
