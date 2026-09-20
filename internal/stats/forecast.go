package stats

import (
	"database/sql"
	"fmt"
	"time"
)

// Forecast holds the month-end cost projection.
type Forecast struct {
	MonthToDateUSD         float64 `json:"month_to_date_usd"`
	ProjectedMonthTotalUSD float64 `json:"projected_month_total_usd"`
	DailyAvgLast7DUSD      float64 `json:"daily_avg_last_7d_usd"`
	DaysRemaining          int     `json:"days_remaining"`
	BudgetUSD              float64 `json:"budget_usd"`
	OnTrack                bool    `json:"on_track"`
	OverageEstimateUSD     float64 `json:"overage_estimate_usd"`
}

// MonthForecast computes a burn-rate forecast anchored to now.
func MonthForecast(conn *sql.DB, budgetUSD float64) (Forecast, error) {
	return monthForecastAt(conn, budgetUSD, time.Now().UTC())
}

func monthForecastAt(conn *sql.DB, budgetUSD float64, now time.Time) (Forecast, error) {
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// Month-to-date total.
	const mtdQ = `
SELECT COALESCE(SUM(total_cost_usd),0)
FROM sessions
WHERE ended_at >= ?
`
	var mtd float64
	if err := conn.QueryRow(mtdQ, monthStart.Format(time.RFC3339)).Scan(&mtd); err != nil {
		return Forecast{}, fmt.Errorf("forecast mtd: %w", err)
	}

	// Daily costs for the last 7 days (or fewer if month started less than 7 days ago).
	window := 7
	windowStart := now.AddDate(0, 0, -(window - 1))
	if windowStart.Before(monthStart) {
		windowStart = monthStart
	}
	cutoff := time.Date(windowStart.Year(), windowStart.Month(), windowStart.Day(), 0, 0, 0, 0, time.UTC)

	const dailyQ = `
SELECT substr(ended_at,1,10) AS day, COALESCE(SUM(total_cost_usd),0)
FROM sessions
WHERE ended_at >= ?
GROUP BY day
ORDER BY day ASC
`
	rows, err := conn.Query(dailyQ, cutoff.Format(time.RFC3339))
	if err != nil {
		return Forecast{}, fmt.Errorf("forecast daily: %w", err)
	}
	defer rows.Close()

	var dailyCosts []float64
	for rows.Next() {
		var day string
		var cost float64
		if err := rows.Scan(&day, &cost); err != nil {
			return Forecast{}, fmt.Errorf("forecast scan: %w", err)
		}
		dailyCosts = append(dailyCosts, cost)
	}
	if err := rows.Err(); err != nil {
		return Forecast{}, fmt.Errorf("forecast rows: %w", err)
	}

	return computeForecast(mtd, dailyCosts, budgetUSD, now), nil
}

// ComputeForecast is the pure math function, exposed for testing.
// dailyCosts is an ordered slice of per-day costs covering (up to) the last 7
// days within the current month.
func ComputeForecast(mtd float64, dailyCosts []float64, budgetUSD float64, now time.Time) Forecast {
	return computeForecast(mtd, dailyCosts, budgetUSD, now)
}

func computeForecast(mtd float64, dailyCosts []float64, budgetUSD float64, now time.Time) Forecast {
	// Average daily cost.
	var avgDaily float64
	if len(dailyCosts) > 0 {
		var sum float64
		for _, c := range dailyCosts {
			sum += c
		}
		avgDaily = sum / float64(len(dailyCosts))
	}

	// Days remaining in the month (today counts as partial — we project from
	// tomorrow onward, i.e. full remaining days after today).
	daysInMonth := daysIn(now.Year(), now.Month())
	daysRemaining := daysInMonth - now.Day()

	projected := mtd + avgDaily*float64(daysRemaining)

	var overage float64
	if budgetUSD > 0 && projected > budgetUSD {
		overage = projected - budgetUSD
	}
	onTrack := budgetUSD <= 0 || projected <= budgetUSD

	return Forecast{
		MonthToDateUSD:         mtd,
		ProjectedMonthTotalUSD: projected,
		DailyAvgLast7DUSD:      avgDaily,
		DaysRemaining:          daysRemaining,
		BudgetUSD:              budgetUSD,
		OnTrack:                onTrack,
		OverageEstimateUSD:     overage,
	}
}

func daysIn(year int, month time.Month) int {
	// First day of next month minus one day.
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
