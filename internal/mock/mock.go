// Package mock generates a deterministic in-memory dataset used to render the
// dashboard without any real session activity. The same seed always yields the
// same numbers so screenshots are reproducible.
package mock

import (
	"math/rand"
	"sort"
	"time"

	"github.com/viharshah/session-lens/internal/db"
	"github.com/viharshah/session-lens/internal/stats"
	"github.com/viharshah/session-lens/internal/transcript"
)

// Seed is the deterministic seed; exported so tests can pin it.
const Seed int64 = 0x5E551014

// Day count of the synthetic window.
const Days = 14

// Session is one synthetic record. Fields mirror the persisted Session.
type Session struct {
	ID               string
	ProjectPath      string
	EndedAt          time.Time
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalCostUSD     float64
	Model            string
}

// Dataset is the bundle the server returns when mock mode is on.
type Dataset struct {
	Sessions []Session
}

// Reference time pinned so the dataset is deterministic across runs even on
// different days. The dashboard month label still reads "today", which keeps
// the UI sensible.
var refNow = time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

// Now returns the reference instant the mock dataset is anchored to.
func Now() time.Time { return refNow }

// Generate builds a deterministic Dataset.
func Generate() Dataset {
	r := rand.New(rand.NewSource(Seed))
	models := []string{"claude-opus-4-7", "claude-sonnet-4-5", "claude-3-5-haiku-20241022"}
	projects := []string{"/Users/dev/work/session-lens", "/Users/dev/work/api-gateway", "/Users/dev/scratch"}

	sessions := make([]Session, 0, 64)
	// One spike day, chosen deterministically.
	spikeDay := 5

	for dOff := Days - 1; dOff >= 0; dOff-- {
		day := refNow.AddDate(0, 0, -dOff).UTC()
		// 3-6 sessions per day, weighted toward sonnet.
		count := 3 + r.Intn(4)
		if dOff == spikeDay {
			count = 12 // many sessions concentrated on the spike day
		}
		for i := 0; i < count; i++ {
			modelIdx := weightedModel(r)
			model := models[modelIdx]
			proj := projects[r.Intn(len(projects))]

			// Base scale per family (rough realism).
			scale := 1.0
			switch modelIdx {
			case 0: // opus
				scale = 2.0
			case 1: // sonnet
				scale = 1.0
			case 2: // haiku
				scale = 0.4
			}
			// Per-session magnitude with mild jitter.
			factor := 0.5 + r.Float64() // 0.5..1.5
			if dOff == spikeDay && i == 0 {
				factor *= 8 // immediate session-cost spike
				model = models[0]
				modelIdx = 0
				scale = 2.0
			}

			inTok := int64(float64(2000) * scale * factor * (0.8 + r.Float64()*0.4))
			outTok := int64(float64(900) * scale * factor * (0.8 + r.Float64()*0.4))
			cr := int64(float64(5000) * scale * factor * r.Float64())
			cw := int64(float64(600) * scale * factor * r.Float64())

			// Random hour-of-day skewed toward business hours.
			hour := businessHour(r)
			minute := r.Intn(60)
			ended := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, time.UTC)

			cost := transcript.ComputeCost(model, inTok, outTok, cr, cw)
			sessions = append(sessions, Session{
				ID:               sessionID(r, dOff, i),
				ProjectPath:      proj,
				EndedAt:          ended,
				InputTokens:      inTok,
				OutputTokens:     outTok,
				CacheReadTokens:  cr,
				CacheWriteTokens: cw,
				TotalCostUSD:     cost,
				Model:            model,
			})
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].EndedAt.Before(sessions[j].EndedAt)
	})
	return Dataset{Sessions: sessions}
}

func weightedModel(r *rand.Rand) int {
	// 20% opus, 60% sonnet, 20% haiku
	x := r.Intn(100)
	switch {
	case x < 20:
		return 0
	case x < 80:
		return 1
	default:
		return 2
	}
}

func businessHour(r *rand.Rand) int {
	// Triangular distribution roughly 7..22.
	a, b := 7, 22
	return a + r.Intn(b-a+1)
}

func sessionID(r *rand.Rand, dayOff, i int) string {
	// Deterministic-ish id from the generator state.
	letters := []byte("abcdefghijklmnopqrstuvwxyz0123456789")
	buf := make([]byte, 8)
	for k := range buf {
		buf[k] = letters[r.Intn(len(letters))]
	}
	return "mock-" + string(buf) + "-" + itoa(dayOff) + "-" + itoa(i)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := make([]byte, 0, 4)
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

// ToDBSession converts a mock Session to the db.Session wire format.
func (s Session) ToDBSession() db.Session {
	return db.Session{
		ID:               s.ID,
		ProjectPath:      s.ProjectPath,
		EndedAt:          s.EndedAt.UTC().Format(time.RFC3339),
		InputTokens:      s.InputTokens,
		OutputTokens:     s.OutputTokens,
		CacheReadTokens:  s.CacheReadTokens,
		CacheWriteTokens: s.CacheWriteTokens,
		TotalCostUSD:     s.TotalCostUSD,
		Model:            s.Model,
	}
}

// GetSession returns the mock session with the given id, or (Session{}, false)
// if no such id exists in the dataset.
func (d Dataset) GetSession(id string) (Session, bool) {
	for _, s := range d.Sessions {
		if s.ID == id {
			return s, true
		}
	}
	return Session{}, false
}

// ListSessions returns the most-recent mock sessions ordered by EndedAt DESC,
// capped at limit (0 → default 20, max 100).
func (d Dataset) ListSessions(limit int) []db.Session {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	// Sessions are stored oldest-first; iterate in reverse for DESC order.
	out := make([]db.Session, 0, limit)
	for i := len(d.Sessions) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, d.Sessions[i].ToDBSession())
	}
	return out
}

// --- Aggregation helpers that mirror the DB-backed stats.* functions ---

// Summary builds a stats.Summary equivalent to MonthSummary over this dataset.
func (d Dataset) Summary(planBudget float64) stats.Summary {
	var s stats.Summary
	monthStart := time.Date(refNow.Year(), refNow.Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, ss := range d.Sessions {
		if ss.EndedAt.Before(monthStart) {
			continue
		}
		s.TotalInput += ss.InputTokens
		s.TotalOutput += ss.OutputTokens
		s.TotalCacheRead += ss.CacheReadTokens
		s.TotalCacheWrite += ss.CacheWriteTokens
		s.TotalCostUSD += ss.TotalCostUSD
		s.SessionCount++
	}
	s.TotalTokens = s.TotalInput + s.TotalOutput + s.TotalCacheRead + s.TotalCacheWrite
	s.PlanBudgetUSD = planBudget
	if planBudget > 0 {
		s.PlanUtilisationPct = (s.TotalCostUSD / planBudget) * 100.0
	}
	return s
}

// Daily bucket aggregation for the dataset.
func (d Dataset) Daily(days int) []stats.Bucket {
	if days <= 0 {
		days = 30
	}
	since := refNow.AddDate(0, 0, -days+1)
	cutoff := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	buckets := map[string]*stats.Bucket{}
	for _, s := range d.Sessions {
		if s.EndedAt.Before(cutoff) {
			continue
		}
		key := s.EndedAt.UTC().Format("2006-01-02")
		b, ok := buckets[key]
		if !ok {
			b = &stats.Bucket{Bucket: key}
			buckets[key] = b
		}
		b.InputTokens += s.InputTokens
		b.OutputTokens += s.OutputTokens
		b.CacheReadTokens += s.CacheReadTokens
		b.CacheWriteTokens += s.CacheWriteTokens
		b.TotalCostUSD += s.TotalCostUSD
		b.SessionCount++
	}
	out := make([]stats.Bucket, 0, len(buckets))
	for _, b := range buckets {
		b.TotalTokens = b.InputTokens + b.OutputTokens + b.CacheReadTokens + b.CacheWriteTokens
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket < out[j].Bucket })
	return out
}

// Hourly bucket aggregation for the dataset. project is optional; empty
// string aggregates across all projects, non-empty filters by exact match.
// Granularity controls bucket size — same semantics as stats.Hourly.
func (d Dataset) Hourly(days int, project string, gran stats.Granularity) []stats.Bucket {
	if days <= 0 {
		days = 7
	}
	if gran == "" {
		gran = stats.GranHour
	}
	layout := granLayout(gran)
	cutoff := refNow.Add(-time.Duration(days) * 24 * time.Hour)
	buckets := map[string]*stats.Bucket{}
	for _, s := range d.Sessions {
		if s.EndedAt.Before(cutoff) {
			continue
		}
		if project != "" && s.ProjectPath != project {
			continue
		}
		key := truncateForMock(s.EndedAt.UTC(), gran).Format(layout)
		b, ok := buckets[key]
		if !ok {
			b = &stats.Bucket{Bucket: key}
			buckets[key] = b
		}
		b.InputTokens += s.InputTokens
		b.OutputTokens += s.OutputTokens
		b.CacheReadTokens += s.CacheReadTokens
		b.CacheWriteTokens += s.CacheWriteTokens
		b.TotalCostUSD += s.TotalCostUSD
		b.SessionCount++
	}
	out := make([]stats.Bucket, 0, len(buckets))
	for _, b := range buckets {
		b.TotalTokens = b.InputTokens + b.OutputTokens + b.CacheReadTokens + b.CacheWriteTokens
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket < out[j].Bucket })
	return stats.FillTimeseriesGaps(out, cutoff, refNow, gran)
}

// granLayout / truncateForMock mirror the unexported helpers on
// stats.Granularity so the mock bucketer produces identical keys.
func granLayout(g stats.Granularity) string {
	switch g {
	case stats.Gran15m:
		return "2006-01-02T15:04Z"
	case stats.GranDay:
		return "2006-01-02T00:00Z"
	default:
		return "2006-01-02T15:00Z"
	}
}

func truncateForMock(t time.Time, g stats.Granularity) time.Time {
	switch g {
	case stats.Gran15m:
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), (t.Minute()/15)*15, 0, 0, time.UTC)
	case stats.Gran6h:
		return time.Date(t.Year(), t.Month(), t.Day(), (t.Hour()/6)*6, 0, 0, 0, time.UTC)
	case stats.GranDay:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	default:
		return t.Truncate(time.Hour)
	}
}

// Weekly bucket aggregation for the dataset.
// The week key matches SQLite's strftime('%Y-W%W', ...) exactly:
// calendar year + Monday-based week number (00-53), where week 00 is
// the partial week before the first Monday of the year.
// Previously this used Go's ISOWeek(), which diverges at year boundaries
// (e.g. 2025-12-29 ISO = 2026-W01 but SQLite = 2025-W52).
func (d Dataset) Weekly(weeks int) []stats.Bucket {
	if weeks <= 0 {
		weeks = 12
	}
	since := refNow.AddDate(0, 0, -7*weeks+1)
	cutoff := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	buckets := map[string]*stats.Bucket{}
	for _, s := range d.Sessions {
		if s.EndedAt.Before(cutoff) {
			continue
		}
		key := sqliteWeekKey(s.EndedAt.UTC())
		b, ok := buckets[key]
		if !ok {
			b = &stats.Bucket{Bucket: key}
			buckets[key] = b
		}
		b.InputTokens += s.InputTokens
		b.OutputTokens += s.OutputTokens
		b.CacheReadTokens += s.CacheReadTokens
		b.CacheWriteTokens += s.CacheWriteTokens
		b.TotalCostUSD += s.TotalCostUSD
		b.SessionCount++
	}
	out := make([]stats.Bucket, 0, len(buckets))
	for _, b := range buckets {
		b.TotalTokens = b.InputTokens + b.OutputTokens + b.CacheReadTokens + b.CacheWriteTokens
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket < out[j].Bucket })
	return out
}

// sqliteWeekKey replicates SQLite's strftime('%Y-W%W', t) in Go.
// %W = Monday-based week number (00-53); %Y = calendar year.
// Week 01 begins on the first Monday of the year; week 00 contains
// any days before that first Monday.
func sqliteWeekKey(t time.Time) string {
	// Weekday of Jan 1 in the same year (Sun=0, Mon=1, ..., Sat=6).
	jan1 := time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	jan1Weekday := int(jan1.Weekday())
	// 1-indexed day-of-year on which the first Monday falls.
	// (8 - jan1Weekday) % 7 gives 0 when Jan 1 is Monday, 1 when Sunday, etc.
	firstMondayDOY := 1 + (8-jan1Weekday)%7
	// 0-indexed day-of-year of t.
	yearDay := t.YearDay() // 1-indexed
	// Distance from first Monday (may be negative, meaning week 00).
	dist := yearDay - firstMondayDOY
	var week int
	if dist < 0 {
		week = 0
	} else {
		week = dist/7 + 1
	}
	w := itoa(week)
	if len(w) == 1 {
		w = "0" + w
	}
	return itoa(t.Year()) + "-W" + w
}

// Projects rollup for the dataset. If `since` is a zero time, the rollup
// covers every session; otherwise it only counts sessions whose EndedAt is
// on or after `since`.
func (d Dataset) Projects(limit int, since time.Time) []stats.Project {
	if limit <= 0 {
		limit = 20
	}
	rows := map[string]*stats.Project{}
	for _, s := range d.Sessions {
		if !since.IsZero() && s.EndedAt.Before(since) {
			continue
		}
		key := s.ProjectPath
		if key == "" {
			key = "(unknown)"
		}
		p, ok := rows[key]
		if !ok {
			p = &stats.Project{ProjectPath: key}
			rows[key] = p
		}
		p.InputTokens += s.InputTokens
		p.OutputTokens += s.OutputTokens
		p.CacheReadTokens += s.CacheReadTokens
		p.CacheWriteTokens += s.CacheWriteTokens
		p.TotalCostUSD += s.TotalCostUSD
		p.SessionCount++
	}
	out := make([]stats.Project, 0, len(rows))
	for _, p := range rows {
		p.TotalTokens = p.InputTokens + p.OutputTokens + p.CacheReadTokens + p.CacheWriteTokens
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalCostUSD > out[j].TotalCostUSD })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ByModel reshapes the dataset into the per-model response.
func (d Dataset) ByModel(days int) stats.ByModelResponse {
	if days <= 0 {
		days = 14
	}
	since := refNow.AddDate(0, 0, -days+1)
	cutoff := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	rows := make([]stats.ModelRow, 0, len(d.Sessions))
	for _, s := range d.Sessions {
		if s.EndedAt.Before(cutoff) {
			continue
		}
		rows = append(rows, stats.ModelRow{
			EndedAt:          s.EndedAt.UTC().Format(time.RFC3339),
			Model:            s.Model,
			InputTokens:      s.InputTokens,
			OutputTokens:     s.OutputTokens,
			CacheReadTokens:  s.CacheReadTokens,
			CacheWriteTokens: s.CacheWriteTokens,
			TotalCostUSD:     s.TotalCostUSD,
		})
	}
	return stats.AggregateByModel(rows)
}

// Forecast computes a burn-rate forecast from the mock dataset anchored to refNow.
func (d Dataset) Forecast(budgetUSD float64) stats.Forecast {
	monthStart := time.Date(refNow.Year(), refNow.Month(), 1, 0, 0, 0, 0, time.UTC)

	// Month-to-date sum.
	var mtd float64
	for _, s := range d.Sessions {
		if !s.EndedAt.Before(monthStart) {
			mtd += s.TotalCostUSD
		}
	}

	// Daily costs for last 7 days (capped at month start).
	window := 7
	windowStart := refNow.AddDate(0, 0, -(window - 1))
	if windowStart.Before(monthStart) {
		windowStart = monthStart
	}
	cutoff := time.Date(windowStart.Year(), windowStart.Month(), windowStart.Day(), 0, 0, 0, 0, time.UTC)

	dayMap := map[string]float64{}
	for _, s := range d.Sessions {
		if s.EndedAt.Before(cutoff) {
			continue
		}
		key := s.EndedAt.UTC().Format("2006-01-02")
		dayMap[key] += s.TotalCostUSD
	}
	dailyCosts := make([]float64, 0, len(dayMap))
	for _, c := range dayMap {
		dailyCosts = append(dailyCosts, c)
	}

	return stats.ComputeForecast(mtd, dailyCosts, budgetUSD, refNow)
}

// Spikes runs the detector against the dataset.
// Annotations returns a small fixed list of mock annotations anchored to
// refNow. They demonstrate the chart-overlay behavior and let the page list
// render real-looking rows when the server is in mock mode. from/to use the
// same inclusive `at` semantics as db.ListAnnotations.
func (d Dataset) Annotations(from, to string) []db.Annotation {
	all := []db.Annotation{
		{
			ID:        1,
			At:        refNow.AddDate(0, 0, -10).UTC().Format(time.RFC3339),
			Title:     "Started optimization experiment",
			Note:      "Switched primary model to Haiku for routine edits",
			Kind:      "experiment-start",
			CreatedAt: refNow.AddDate(0, 0, -10).UTC().Format(time.RFC3339),
		},
		{
			ID:        2,
			At:        refNow.AddDate(0, 0, -5).UTC().Format(time.RFC3339),
			Title:     "Enabled prompt caching",
			Note:      "Expected: cache_read tokens up, billable input down",
			Kind:      "event",
			CreatedAt: refNow.AddDate(0, 0, -5).UTC().Format(time.RFC3339),
		},
	}
	out := make([]db.Annotation, 0, len(all))
	for _, a := range all {
		if from != "" && a.At < from {
			continue
		}
		if to != "" && a.At > to {
			continue
		}
		out = append(out, a)
	}
	return out
}

func (d Dataset) Spikes(cfg stats.SpikeConfig) []stats.Spike {
	sessions := make([]stats.SessionRecord, 0, len(d.Sessions))
	for _, s := range d.Sessions {
		sessions = append(sessions, stats.SessionRecord{
			ID:          s.ID,
			EndedAt:     s.EndedAt,
			ProjectPath: s.ProjectPath,
			CostUSD:     s.TotalCostUSD,
			Tokens:      s.InputTokens + s.OutputTokens + s.CacheReadTokens + s.CacheWriteTokens,
		})
	}
	// Daily token totals.
	days := map[string]int64{}
	for _, s := range d.Sessions {
		key := s.EndedAt.UTC().Format("2006-01-02")
		days[key] += s.InputTokens + s.OutputTokens + s.CacheReadTokens + s.CacheWriteTokens
	}
	keys := make([]string, 0, len(days))
	for k := range days {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	dayRecs := make([]stats.DayRecord, 0, len(keys))
	for _, k := range keys {
		dayRecs = append(dayRecs, stats.DayRecord{Date: k, Tokens: days[k]})
	}
	return stats.DetectSpikes(sessions, dayRecs, cfg)
}
