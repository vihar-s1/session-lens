// Package server wires the HTTP routes for the session-lens API and the
// static dashboard UI.
package server

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/viharshah/session-lens/internal/alerter"
	"github.com/viharshah/session-lens/internal/db"
	"github.com/viharshah/session-lens/internal/mock"
	"github.com/viharshah/session-lens/internal/stats"
	"github.com/viharshah/session-lens/internal/transcript"
)

// flusher is satisfied by http.ResponseWriters that support streaming.
type flusher interface {
	Flush()
}

// Version is reported by /healthz.
const Version = "0.2.0"

// shortProjectName returns the last segment of a project path, suitable for
// embedding in a one-line notification. "/Users/v/Desktop/.../session-lens"
// → "session-lens". Empty input returns "".
func shortProjectName(projectPath string) string {
	if projectPath == "" {
		return ""
	}
	base := filepath.Base(projectPath)
	if base == "." || base == "/" {
		return ""
	}
	return base
}

// dashboardOpenURL is the base URL the notification click-through opens.
// Mirrors alerter's dashboardURL() — duplicated here so we can append a
// "/#session/<id>" fragment without bouncing through the alerter package.
func dashboardOpenURL() string {
	if u := os.Getenv("SESSIONLENS_DASHBOARD_URL"); u != "" {
		return u
	}
	return "http://localhost:7821"
}

// spikeNotified holds session IDs we've already fired a cost-spike notification
// for during this process lifetime. The Claude Code hook fires on every turn
// with the cumulative session cost, so without this set a long expensive
// session would re-notify on every $0.01-$0.50 increment. In-memory is fine —
// at worst, an autodeploy restart re-arms one notification per still-active
// spiking session, which is acceptable noise vs the cost of a schema migration.
var spikeNotified sync.Map

// burstNotified is the sibling dedupe for burst alerts. Kept separate
// from spikeNotified so the two signals can fire independently on the
// same session — a session can both burst (short-window burn rate) and
// spike (total-cost anomaly), and each is worth one alert.
var burstNotified sync.Map

// Config holds the runtime knobs for the server.
type Config struct {
	DB            *sql.DB
	DBPath        string // on-disk path; used by /v1/metrics to report size. Pass "" to skip.
	BufferDir     string // hook buffer directory; used by /v1/metrics. Pass "" to skip.
	StaticDir     string
	PlanBudgetUSD float64
	MockDefault   bool             // initial state of mock mode (driven by env var)
	Hub           *Hub             // SSE broadcast hub; if nil a no-op hub is used
	Notifier      alerter.Notifier // desktop notifications; if nil uses alerter.Default()
}

// modeFlag holds a thread-safe bool; UI toggles it via POST /v1/mode.
type modeFlag struct{ on atomic.Bool }

func (m *modeFlag) Set(v bool) { m.on.Store(v) }
func (m *modeFlag) Get() bool  { return m.on.Load() }

// New builds the http handler for the server.
func New(cfg Config) http.Handler {
	mux := http.NewServeMux()
	mode := &modeFlag{}
	mode.Set(cfg.MockDefault)
	// Cache the mock dataset once per process: it's deterministic.
	dataset := mock.Generate()

	// Use caller-supplied hub or a fresh one if none was provided.
	hub := cfg.Hub
	if hub == nil {
		hub = NewHub()
	}

	// Use caller-supplied notifier or the platform default.
	notifier := cfg.Notifier
	if notifier == nil {
		notifier = alerter.Default()
	}

	// Per-IP rate limit for the ingest endpoint: 10 events/sec sustained,
	// burst of 60. Far above any realistic Stop-hook traffic but blocks a
	// misconfigured loop from filling the DB.
	ingestLimiter := newRateLimiter(10, 60)

	// Process-wide observability counters surfaced via GET /v1/metrics.
	m := newMetrics()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version, "mock": mode.Get()})
	})

	mux.HandleFunc("GET /v1/mode", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"mock": mode.Get()})
	})
	mux.HandleFunc("POST /v1/mode", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Mock bool `json:"mock"`
		}
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json: %w", err))
			return
		}
		mode.Set(body.Mock)
		writeJSON(w, http.StatusOK, map[string]any{"mock": mode.Get()})
	})

	// GET /v1/events — SSE live-tail of ingested sessions.
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		handleSSE(w, r, hub)
	})

	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if !ingestLimiter.Allow(clientIP(r)) {
			m.ingestRateLimted.Add(1)
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, errors.New("rate limit exceeded"))
			return
		}
		status := handleCreateSession(w, r, cfg, hub, notifier)
		if status >= 200 && status < 300 {
			m.ingestAccepted.Add(1)
		} else {
			m.ingestFailed.Add(1)
		}
	})

	mux.HandleFunc("GET /v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.Snapshot(cfg.DB, cfg.DBPath, cfg.BufferDir))
	})

	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		limit := intParam(r, "limit", 20)
		from := r.URL.Query().Get("from")
		to := r.URL.Query().Get("to")
		if isMock(r, mode) {
			writeJSON(w, http.StatusOK, filterByEndedAt(dataset.ListSessions(limit), from, to))
			return
		}
		rows, err := db.ListSessionsFiltered(cfg.DB, limit, from, to)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})

	mux.HandleFunc("GET /v1/sessions/{id}/breakdown", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if isMock(r, mode) {
			// Mock mode has no transcript files; synthesise an empty breakdown.
			writeJSON(w, http.StatusOK, transcript.Breakdown{ToolCounts: map[string]int{}})
			return
		}
		s, err := db.GetSession(cfg.DB, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, fmt.Errorf("session %q not found", id))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// raw_payload is the original Stop-hook event JSON; pull transcript_path out of it.
		var hook struct {
			TranscriptPath string `json:"transcript_path"`
		}
		if s.RawPayload == "" {
			writeError(w, http.StatusUnprocessableEntity, errors.New("no raw_payload stored for this session"))
			return
		}
		if err := json.Unmarshal([]byte(s.RawPayload), &hook); err != nil {
			writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("parse raw_payload: %w", err))
			return
		}
		if hook.TranscriptPath == "" {
			writeError(w, http.StatusUnprocessableEntity, errors.New("raw_payload missing transcript_path"))
			return
		}
		bd, err := transcript.BreakdownFile(hook.TranscriptPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("breakdown: %w", err))
			return
		}
		writeJSON(w, http.StatusOK, bd)
	})

	mux.HandleFunc("GET /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if isMock(r, mode) {
			s, ok := dataset.GetSession(id)
			if !ok {
				writeError(w, http.StatusNotFound, fmt.Errorf("session %q not found", id))
				return
			}
			writeJSON(w, http.StatusOK, s.ToDBSession())
			return
		}
		s, err := db.GetSession(cfg.DB, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, fmt.Errorf("session %q not found", id))
				return
			}
			// GetSession wraps the error; check the underlying cause via string match.
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, s)
	})

	mux.HandleFunc("GET /v1/stats/summary", func(w http.ResponseWriter, r *http.Request) {
		budget := planBudgetUSD(cfg.DB, cfg.PlanBudgetUSD)
		if isMock(r, mode) {
			writeJSON(w, http.StatusOK, dataset.Summary(budget))
			return
		}
		s, err := stats.MonthSummary(cfg.DB, budget)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	mux.HandleFunc("GET /v1/stats/daily", func(w http.ResponseWriter, r *http.Request) {
		days := intParam(r, "days", 30)
		if isMock(r, mode) {
			writeJSON(w, http.StatusOK, dataset.Daily(days))
			return
		}
		out, err := stats.Daily(cfg.DB, days)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /v1/stats/weekly", func(w http.ResponseWriter, r *http.Request) {
		weeks := intParam(r, "weeks", 12)
		if isMock(r, mode) {
			writeJSON(w, http.StatusOK, dataset.Weekly(weeks))
			return
		}
		out, err := stats.Weekly(cfg.DB, weeks)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /v1/stats/projects", func(w http.ResponseWriter, r *http.Request) {
		limit := intParam(r, "limit", 20)
		since := parseSinceParam(r, 30*24*time.Hour)
		if isMock(r, mode) {
			// Mock dataset is anchored to mock.Now(); translate "since" off
			// the mock reference instead of wall-clock so historical mock
			// sessions stay visible under non-all windows.
			mockSince := time.Time{}
			if !since.IsZero() {
				delta := time.Since(since)
				mockSince = mock.Now().Add(-delta)
			}
			writeJSON(w, http.StatusOK, dataset.Projects(limit, mockSince))
			return
		}
		out, err := stats.Projects(cfg.DB, limit, since)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /v1/stats/hourly", func(w http.ResponseWriter, r *http.Request) {
		days := intParam(r, "days", 7)
		project := r.URL.Query().Get("project")
		gran := stats.ParseGranularity(r.URL.Query().Get("gran"))
		if isMock(r, mode) {
			writeJSON(w, http.StatusOK, dataset.Hourly(days, project, gran))
			return
		}
		out, err := stats.Hourly(cfg.DB, days, project, gran)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /v1/stats/by-model", func(w http.ResponseWriter, r *http.Request) {
		days := intParam(r, "days", 14)
		if isMock(r, mode) {
			writeJSON(w, http.StatusOK, dataset.ByModel(days))
			return
		}
		out, err := stats.ByModel(cfg.DB, days)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	// /v1/compare powers the Compare page: side-by-side metrics for two
	// arbitrary date ranges. The defaults the UI sends are "previous calendar
	// month" vs "current calendar month", but any [from, to) pair works. Mock
	// mode is intentionally not handled — Compare is for real before/after
	// analysis and synthesizing the diff against mock data would mislead.
	mux.HandleFunc("GET /v1/compare", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		aFrom, err := parseCompareDate(q.Get("a_from"))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("a_from: %w", err))
			return
		}
		aTo, err := parseCompareDate(q.Get("a_to"))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("a_to: %w", err))
			return
		}
		bFrom, err := parseCompareDate(q.Get("b_from"))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("b_from: %w", err))
			return
		}
		bTo, err := parseCompareDate(q.Get("b_to"))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("b_to: %w", err))
			return
		}
		if cfg.DB == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("database unavailable"))
			return
		}
		res, err := stats.Compare(cfg.DB, aFrom, aTo, bFrom, bTo)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	// /v1/repricing runs a model-swap what-if against historical data.
	// Query params:
	//   from, to        RFC3339 or YYYY-MM-DD (required)
	//   target          target model id (required)
	//   kind            "flat" | "family" | "conditional" (default "flat")
	//   from_family     substring to match on original model (family/conditional)
	//   max_cost_usd    conditional: only swap sessions <= this cost
	//   max_turns       conditional: only swap sessions <= this turn count
	// Mock mode is intentionally NOT handled — the point of repricing is
	// counterfactual reasoning about *your* real spend, not a synthetic
	// dataset that would give misleading dollar deltas.
	mux.HandleFunc("GET /v1/repricing", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		from, err := parseCompareDate(q.Get("from"))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("from: %w", err))
			return
		}
		to, err := parseCompareDate(q.Get("to"))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("to: %w", err))
			return
		}
		target := strings.TrimSpace(q.Get("target"))
		if target == "" {
			writeError(w, http.StatusBadRequest, errors.New("target is required"))
			return
		}
		kind := strings.TrimSpace(q.Get("kind"))
		if kind == "" {
			kind = "flat"
		}
		rule := stats.SwapRule{
			Kind:       kind,
			FromFamily: strings.TrimSpace(q.Get("from_family")),
			Target:     target,
		}
		if v := q.Get("max_cost_usd"); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("max_cost_usd: %w", err))
				return
			}
			rule.MaxCostUSD = f
		}
		rule.MaxTurns = intParam(r, "max_turns", 0)
		if cfg.DB == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("database unavailable"))
			return
		}
		res, err := stats.Reprice(cfg.DB, from, to, rule)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	// Annotations CRUD — markers placed on the session-lens timeline.
	// The dashboard uses these to overlay vertical lines on the daily charts
	// (so a "started optimizing on X" marker shows up visually against
	// before/after spend) and renders a list on its own page for editing.
	mux.HandleFunc("GET /v1/annotations", func(w http.ResponseWriter, r *http.Request) {
		from := r.URL.Query().Get("from")
		to := r.URL.Query().Get("to")
		if isMock(r, mode) {
			writeJSON(w, http.StatusOK, dataset.Annotations(from, to))
			return
		}
		if cfg.DB == nil {
			writeJSON(w, http.StatusOK, []db.Annotation{})
			return
		}
		rows, err := db.ListAnnotations(cfg.DB, from, to)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})
	mux.HandleFunc("POST /v1/annotations", func(w http.ResponseWriter, r *http.Request) {
		if cfg.DB == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("database unavailable"))
			return
		}
		a, err := decodeAnnotation(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		out, err := db.InsertAnnotation(cfg.DB, a)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, out)
	})
	mux.HandleFunc("PUT /v1/annotations/{id}", func(w http.ResponseWriter, r *http.Request) {
		if cfg.DB == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("database unavailable"))
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("invalid id"))
			return
		}
		a, err := decodeAnnotation(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		a.ID = id
		out, err := db.UpdateAnnotation(cfg.DB, a)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, fmt.Errorf("annotation %d not found", id))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("DELETE /v1/annotations/{id}", func(w http.ResponseWriter, r *http.Request) {
		if cfg.DB == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("database unavailable"))
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("invalid id"))
			return
		}
		if err := db.DeleteAnnotation(cfg.DB, id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, fmt.Errorf("annotation %d not found", id))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	})

	mux.HandleFunc("GET /v1/stats/spikes", func(w http.ResponseWriter, r *http.Request) {
		spikeCfg := spikeConfigFor(cfg.DB)
		if isMock(r, mode) {
			writeJSON(w, http.StatusOK, dataset.Spikes(spikeCfg))
			return
		}
		out, err := stats.Spikes(cfg.DB, spikeCfg)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})

	// GET /v1/sessions/{id}/burst — the peak $/min window inside one
	// session. Returns 204 when the session has too few turns or its
	// peak sits below the absolute floor (no burst worth surfacing);
	// 404 when the session id is unknown.
	mux.HandleFunc("GET /v1/sessions/{id}/burst", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("missing session id"))
			return
		}
		if _, err := db.GetSession(cfg.DB, id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, errors.New("session not found"))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		burst, err := stats.DetectBurstForSession(cfg.DB, id, stats.DefaultBurstConfig())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if burst == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, burst)
	})

	mux.HandleFunc("GET /v1/forecast", func(w http.ResponseWriter, r *http.Request) {
		budgetUSD := apiRateAlertUSD(cfg.DB)
		if isMock(r, mode) {
			writeJSON(w, http.StatusOK, dataset.Forecast(budgetUSD))
			return
		}
		out, err := stats.MonthForecast(cfg.DB, budgetUSD)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})

	// GET /v1/settings — all persisted user settings, plus resolved effective
	// values (settings → env → built-in default).
	mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		view, err := loadSettingsView(cfg.DB, cfg.PlanBudgetUSD)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	})

	// PUT /v1/settings — partial upsert. Body is a flat object of string-
	// valued keys. Unknown keys are stored verbatim (forward-compatible).
	mux.HandleFunc("PUT /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json: %w", err))
			return
		}
		for k, v := range body {
			var s string
			switch tv := v.(type) {
			case string:
				s = tv
			case float64:
				s = strconv.FormatFloat(tv, 'f', -1, 64)
			case bool:
				if tv {
					s = "1"
				} else {
					s = "0"
				}
			case nil:
				s = ""
			default:
				writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported value type for %q", k))
				return
			}
			if err := db.PutSetting(cfg.DB, k, s); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		view, err := loadSettingsView(cfg.DB, cfg.PlanBudgetUSD)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	})

	// DELETE /v1/sessions?from=&to= — purge sessions in a date range. Either
	// bound is optional; with both empty every row is removed. Returns
	// {deleted: N}.
	mux.HandleFunc("DELETE /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		from := r.URL.Query().Get("from")
		to := r.URL.Query().Get("to")
		n, err := db.DeleteSessions(cfg.DB, from, to)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
	})

	// GET /v1/export/daily.csv — daily cost summary as CSV.
	mux.HandleFunc("GET /v1/export/daily.csv", func(w http.ResponseWriter, r *http.Request) {
		days := intParam(r, "days", 30)
		var buckets []stats.Bucket
		if isMock(r, mode) {
			buckets = dataset.Daily(days)
		} else {
			var err error
			buckets, err = stats.Daily(cfg.DB, days)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		filename := "daily-" + time.Now().UTC().Format("2006-01-02") + ".csv"
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"date", "sessions", "total_cost_usd", "input_tokens", "output_tokens", "cache_write_tokens", "cache_read_tokens"})
		for _, b := range buckets {
			_ = cw.Write([]string{
				b.Bucket,
				strconv.FormatInt(b.SessionCount, 10),
				strconv.FormatFloat(b.TotalCostUSD, 'f', 6, 64),
				strconv.FormatInt(b.InputTokens, 10),
				strconv.FormatInt(b.OutputTokens, 10),
				strconv.FormatInt(b.CacheWriteTokens, 10),
				strconv.FormatInt(b.CacheReadTokens, 10),
			})
		}
		cw.Flush()
	})

	// GET /v1/export/sessions.csv — full session list as CSV.
	mux.HandleFunc("GET /v1/export/sessions.csv", func(w http.ResponseWriter, r *http.Request) {
		limit := intParam(r, "limit", 1000)
		if limit > 10000 {
			limit = 10000
		}
		var sessions []db.Session
		if isMock(r, mode) {
			// dataset.ListSessions caps at 100; bypass to return more rows.
			all := dataset.Sessions
			count := limit
			if count > len(all) {
				count = len(all)
			}
			sessions = make([]db.Session, 0, count)
			for i := len(all) - 1; i >= len(all)-count; i-- {
				sessions = append(sessions, all[i].ToDBSession())
			}
		} else {
			var err error
			sessions, err = db.ListSessions(cfg.DB, limit)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		filename := "sessions-" + time.Now().UTC().Format("2006-01-02") + ".csv"
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"id", "started_at", "ended_at", "project_path", "model", "turns", "input_tokens", "output_tokens", "cache_write_tokens", "cache_read_tokens", "total_cost_usd"})
		for _, s := range sessions {
			_ = cw.Write([]string{
				s.ID,
				s.StartedAt,
				s.EndedAt,
				s.ProjectPath,
				s.Model,
				strconv.Itoa(s.Turns),
				strconv.FormatInt(s.InputTokens, 10),
				strconv.FormatInt(s.OutputTokens, 10),
				strconv.FormatInt(s.CacheWriteTokens, 10),
				strconv.FormatInt(s.CacheReadTokens, 10),
				strconv.FormatFloat(s.TotalCostUSD, 'f', 6, 64),
			})
		}
		cw.Flush()
	})

	// Static UI.
	if cfg.StaticDir != "" {
		fs := http.FileServer(http.Dir(cfg.StaticDir))
		mux.Handle("GET /static/", http.StripPrefix("/static/", fs))
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, filepath.Join(cfg.StaticDir, "index.html"))
		})
	}

	return logMiddleware(countingMiddleware(mux, m))
}

// countingMiddleware bumps the request_count counter on every request.
func countingMiddleware(next http.Handler, m *metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requestCount.Add(1)
		next.ServeHTTP(w, r)
	})
}

// isMock returns true if the request should be served mock data — either
// because the server-side flag is on or the caller passed `?mock=1`.
func isMock(r *http.Request, flag *modeFlag) bool {
	if flag.Get() {
		return true
	}
	q := r.URL.Query().Get("mock")
	return q == "1" || q == "true"
}

// SessionEvent is the POST body for /v1/sessions.
type SessionEvent struct {
	ID               string             `json:"id"`
	ProjectPath      string             `json:"project_path"`
	StartedAt        string             `json:"started_at"`
	EndedAt          string             `json:"ended_at"`
	InputTokens      int64              `json:"input_tokens"`
	OutputTokens     int64              `json:"output_tokens"`
	CacheReadTokens  int64              `json:"cache_read_tokens"`
	CacheWriteTokens int64              `json:"cache_write_tokens"`
	TotalCostUSD     float64            `json:"total_cost_usd"`
	Model            string             `json:"model"`
	Turns            int                `json:"turns"`
	RawPayload       string             `json:"raw_payload,omitempty"`
	TurnEvents       []SessionTurnEvent `json:"turn_events,omitempty"`
}

// SessionTurnEvent is one assistant-message-level usage row inside a
// SessionEvent. Ordered by Idx (0-based). Optional in the POST body — older
// hooks that don't emit it still work; the server simply won't have per-turn
// data for those sessions until they're re-ingested.
type SessionTurnEvent struct {
	Idx              int     `json:"idx"`
	EndedAt          string  `json:"ended_at"`
	Model            string  `json:"model"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// handleCreateSession returns the HTTP status code that was written to w.
func handleCreateSession(w http.ResponseWriter, r *http.Request, cfg Config, hub *Hub, notifier alerter.Notifier) int {
	defer r.Body.Close()
	var ev SessionEvent
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ev); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json: %w", err))
		return http.StatusBadRequest
	}
	if ev.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id required"))
		return http.StatusBadRequest
	}
	if ev.EndedAt == "" {
		ev.EndedAt = time.Now().UTC().Format(time.RFC3339)
	}
	row := db.Session{
		ID:               ev.ID,
		ProjectPath:      ev.ProjectPath,
		StartedAt:        ev.StartedAt,
		EndedAt:          ev.EndedAt,
		InputTokens:      ev.InputTokens,
		OutputTokens:     ev.OutputTokens,
		CacheReadTokens:  ev.CacheReadTokens,
		CacheWriteTokens: ev.CacheWriteTokens,
		TotalCostUSD:     ev.TotalCostUSD,
		Model:            ev.Model,
		Turns:            ev.Turns,
		RawPayload:       ev.RawPayload,
	}
	out, inserted, err := db.UpsertSession(cfg.DB, row)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return http.StatusInternalServerError
	}
	// Persist per-turn rows for hourly bucketing. Failure here is not fatal —
	// the session aggregate is still recorded; we just lose hourly resolution
	// for this session. Log so the symptom is visible.
	if len(ev.TurnEvents) > 0 {
		turns := make([]db.Turn, 0, len(ev.TurnEvents))
		for _, t := range ev.TurnEvents {
			turns = append(turns, db.Turn{
				SessionID:        ev.ID,
				Idx:              t.Idx,
				EndedAt:          t.EndedAt,
				Model:            t.Model,
				InputTokens:      t.InputTokens,
				OutputTokens:     t.OutputTokens,
				CacheReadTokens:  t.CacheReadTokens,
				CacheWriteTokens: t.CacheWriteTokens,
				CostUSD:          t.CostUSD,
			})
		}
		if err := db.InsertTurns(cfg.DB, turns); err != nil {
			log.Printf("insert turns for %s: %v", ev.ID, err)
		}
	}
	// Notify SSE subscribers of the new/updated session.
	hub.Broadcast(out)

	// Fire a desktop notification if the session cost exceeds the configured
	// spike-session ratio (default 3x) of the N-day P75 session cost. Same
	// baseline drives /v1/stats/spikes so the two signals stay in lockstep.
	// Per-session dedupe: the Claude Code hook fires on every turn with
	// cumulative cost, so without this guard we'd re-notify on each turn.
	if cfg.DB != nil && ev.TotalCostUSD > 0 {
		spikeCfg := spikeConfigFor(cfg.DB)
		conn := cfg.DB
		sessionID := ev.ID
		projectShort := shortProjectName(ev.ProjectPath)
		go func(costUSD float64, ratio float64, windowDays int) {
			baseline, err := stats.BaselineP75CostUSD(conn, windowDays)
			if err != nil {
				log.Printf("spike baseline: %v", err)
				return
			}
			if baseline > 0 && costUSD >= ratio*baseline {
				if _, alreadyNotified := spikeNotified.LoadOrStore(sessionID, true); alreadyNotified {
					return
				}
				// Identify the offending session: project name + short ID prefix in
				// the body, deep-link in the click-through. Without these, the user
				// only sees a dollar amount and has no way to find which session
				// blew the budget.
				idPrefix := sessionID
				if len(idPrefix) > 8 {
					idPrefix = idPrefix[:8]
				}
				header := projectShort
				if header == "" {
					header = "unknown project"
				}
				msg := fmt.Sprintf("[%s · %s] $%.2f — %.1fx the %d-day P75 ($%.2f)",
					header, idPrefix, costUSD, costUSD/baseline, windowDays, baseline)
				openURL := fmt.Sprintf("%s/#session/%s", dashboardOpenURL(), url.PathEscape(sessionID))
				notifier.Notify("session-lens: cost spike", msg, openURL)
			}
		}(ev.TotalCostUSD, spikeCfg.SessionRatio, spikeCfg.SessionWindowN)
	}

	// Burst detector — orthogonal to the total-cost spike above. Answers
	// "is this session burning dollars fast RIGHT NOW inside a small
	// window" using the per-turn cost timeline. Requires turn data; the
	// detector returns nil (silent) for sessions that don't fire, so we
	// only notify on genuine bursts. Ran async so a slow DB scan on the
	// baseline query can't stall the hook.
	if cfg.DB != nil && len(ev.TurnEvents) >= 2 {
		conn := cfg.DB
		sessionID := ev.ID
		projectShort := shortProjectName(ev.ProjectPath)
		go func() {
			burst, err := stats.DetectBurstForSession(conn, sessionID, stats.DefaultBurstConfig())
			if err != nil {
				log.Printf("burst detect %s: %v", sessionID, err)
				return
			}
			if burst == nil {
				return
			}
			if _, already := burstNotified.LoadOrStore(sessionID, true); already {
				return
			}
			idPrefix := sessionID
			if len(idPrefix) > 8 {
				idPrefix = idPrefix[:8]
			}
			header := projectShort
			if header == "" {
				header = "unknown project"
			}
			// Message shape: dollars-per-minute is the headline; ratio
			// vs baseline is the "why this is unusual" secondary. When
			// no baseline yet, print the absolute number alone —
			// "6.7x baseline (0)" is meaningless.
			var msg string
			if burst.BaselineP75 > 0 {
				msg = fmt.Sprintf("[%s · %s] $%.2f/min burst — %.1fx your typical peak ($%.2f/min)",
					header, idPrefix, burst.USDPerMin, burst.Ratio, burst.BaselineP75)
			} else {
				msg = fmt.Sprintf("[%s · %s] $%.2f/min burst over a %d-min window",
					header, idPrefix, burst.USDPerMin, stats.DefaultBurstConfig().WindowMinutes)
			}
			openURL := fmt.Sprintf("%s/#session/%s", dashboardOpenURL(), url.PathEscape(sessionID))
			notifier.Notify("session-lens: burn-rate burst", msg, openURL)
		}()
	}

	status := http.StatusOK
	if inserted {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
	return status
}

// handleSSE streams newly-ingested sessions as Server-Sent Events.
// Each event looks like:
//
//	event: session
//	data: {"id":"...","project_path":"...",...}
//
// The connection stays open until the client disconnects.
func handleSSE(w http.ResponseWriter, r *http.Request, hub *Hub) {
	f, ok := w.(flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disable any proxy buffering (nginx et al.).
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Send an initial comment so the browser's EventSource knows the stream is open.
	fmt.Fprint(w, ": connected\n\n")
	f.Flush()

	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(s)
			if err != nil {
				log.Printf("sse marshal: %v", err)
				continue
			}
			fmt.Fprintf(w, "event: session\ndata: %s\n\n", data)
			f.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// filterByEndedAt is the mock-mode equivalent of the SQL ended_at filter.
// from/to are inclusive RFC3339 strings; "" disables that bound.
func filterByEndedAt(sessions []db.Session, from, to string) []db.Session {
	if from == "" && to == "" {
		return sessions
	}
	out := make([]db.Session, 0, len(sessions))
	for _, s := range sessions {
		if from != "" && s.EndedAt < from {
			continue
		}
		if to != "" && s.EndedAt > to {
			continue
		}
		out = append(out, s)
	}
	return out
}

// decodeAnnotation parses a POST/PUT body into a db.Annotation, validating
// the two required fields. `at` is accepted as either RFC3339 or
// YYYY-MM-DD (the latter is normalised to midnight UTC) so the dashboard
// can post a plain `<input type="date">` value without local-tz conversion
// gymnastics.
func decodeAnnotation(r *http.Request) (db.Annotation, error) {
	defer r.Body.Close()
	var body struct {
		At    string `json:"at"`
		Title string `json:"title"`
		Note  string `json:"note"`
		Kind  string `json:"kind"`
		Color string `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return db.Annotation{}, fmt.Errorf("invalid json: %w", err)
	}
	body.At = strings.TrimSpace(body.At)
	body.Title = strings.TrimSpace(body.Title)
	if body.At == "" {
		return db.Annotation{}, errors.New("at required")
	}
	if body.Title == "" {
		return db.Annotation{}, errors.New("title required")
	}
	// Normalise the timestamp so we store a comparable RFC3339 UTC string.
	if t, err := time.Parse(time.RFC3339, body.At); err == nil {
		body.At = t.UTC().Format(time.RFC3339)
	} else if t, err := time.Parse("2006-01-02", body.At); err == nil {
		body.At = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	} else {
		return db.Annotation{}, fmt.Errorf("unrecognized at %q (want YYYY-MM-DD or RFC3339)", body.At)
	}
	return db.Annotation{
		At:    body.At,
		Title: body.Title,
		Note:  body.Note,
		Kind:  body.Kind,
		Color: body.Color,
	}, nil
}

// parseSinceParam reads ?since=30d|90d|all (also accepts a bare integer day
// count, e.g. ?since=7). Returns a zero time.Time for "all" (no filter) or
// when the param is malformed AND `def` is zero. `def` is the fallback window
// applied when the param is absent or invalid.
// parseCompareDate accepts YYYY-MM-DD (interpreted as midnight UTC) or
// RFC3339. Empty is an error — Compare's caller is responsible for filling
// defaults so partial input doesn't quietly compare against zero/now.
func parseCompareDate(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, errors.New("required")
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	return time.Time{}, fmt.Errorf("unrecognized date %q (want YYYY-MM-DD or RFC3339)", raw)
}

func parseSinceParam(r *http.Request, def time.Duration) time.Time {
	raw := r.URL.Query().Get("since")
	if raw == "all" {
		return time.Time{}
	}
	var dur time.Duration
	switch {
	case raw == "":
		dur = def
	case len(raw) > 1 && raw[len(raw)-1] == 'd':
		if n, err := strconv.Atoi(raw[:len(raw)-1]); err == nil && n > 0 {
			dur = time.Duration(n) * 24 * time.Hour
		} else {
			dur = def
		}
	default:
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			dur = time.Duration(n) * 24 * time.Hour
		} else {
			dur = def
		}
	}
	if dur <= 0 {
		return time.Time{}
	}
	return time.Now().UTC().Add(-dur)
}

func intParam(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// monthlyBudget reads SESSIONLENS_MONTHLY_BUDGET_USD, defaulting to 100.
// Kept for backwards compatibility; new code should call apiRateAlertUSD.
func monthlyBudget() float64 {
	raw := os.Getenv("SESSIONLENS_MONTHLY_BUDGET_USD")
	if raw == "" {
		return 100.0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return 100.0
	}
	return v
}

// Setting keys persisted in the SQLite settings table. Adding a new knob?
// Add the key constant here AND surface it in loadSettingsView.
const (
	settingPlanTier         = "plan_tier"
	settingPlanBudgetUSD    = "plan_budget_usd"
	settingAPIRateAlertUSD  = "api_rate_alert_usd"
	settingSpikeSessRatio   = "spike_session_ratio"
	settingSpikeSessWindowN = "spike_session_window_n"
	settingSpikeTrendRatio  = "spike_trend_ratio"
	settingTZDisplay        = "tz_display" // "local" or "utc"
)

// SettingsView is the shape returned by GET/PUT /v1/settings. Numeric fields
// hold the effective value after resolution (DB → env → built-in default);
// PlanTier is whatever the user picked, or "" if never set.
type SettingsView struct {
	PlanTier            string  `json:"plan_tier"`
	PlanBudgetUSD       float64 `json:"plan_budget_usd"`
	APIRateAlertUSD     float64 `json:"api_rate_alert_usd"`
	SpikeSessionRatio   float64 `json:"spike_session_ratio"`
	SpikeSessionWindowN int     `json:"spike_session_window_n"`
	SpikeTrendRatio     float64 `json:"spike_trend_ratio"`
	TZDisplay           string  `json:"tz_display"` // "local" or "utc"
}

// loadSettingsView reads the persisted settings KV and merges them with env
// variables and built-in defaults to produce the effective configuration.
// Precedence per knob: settings table > env var > hardcoded default.
func loadSettingsView(conn *sql.DB, planBudgetFallback float64) (SettingsView, error) {
	def := stats.DefaultSpikeConfig()
	v := SettingsView{
		PlanBudgetUSD:       planBudgetFallback,
		APIRateAlertUSD:     monthlyBudget(),
		SpikeSessionRatio:   def.SessionRatio,
		SpikeSessionWindowN: def.SessionWindowN,
		SpikeTrendRatio:     def.TrendRatio,
		TZDisplay:           "local",
	}
	// Env overrides still apply when nothing is persisted.
	if raw := os.Getenv("SESSIONLENS_SPIKE_SESSION_RATIO"); raw != "" {
		if n, err := strconv.ParseFloat(raw, 64); err == nil && n > 0 {
			v.SpikeSessionRatio = n
		}
	}
	if raw := os.Getenv("SESSIONLENS_SPIKE_SESSION_WINDOW_N"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			v.SpikeSessionWindowN = n
		}
	}
	if raw := os.Getenv("SESSIONLENS_SPIKE_TREND_RATIO"); raw != "" {
		if n, err := strconv.ParseFloat(raw, 64); err == nil && n > 0 {
			v.SpikeTrendRatio = n
		}
	}
	if conn == nil {
		return v, nil
	}
	rows, err := db.ListSettings(conn)
	if err != nil {
		return v, err
	}
	if s, ok := rows[settingPlanTier]; ok {
		v.PlanTier = s
	}
	if s, ok := rows[settingPlanBudgetUSD]; ok {
		if n, err := strconv.ParseFloat(s, 64); err == nil && n >= 0 {
			v.PlanBudgetUSD = n
		}
	}
	if s, ok := rows[settingAPIRateAlertUSD]; ok {
		if n, err := strconv.ParseFloat(s, 64); err == nil && n >= 0 {
			v.APIRateAlertUSD = n
		}
	}
	if s, ok := rows[settingSpikeSessRatio]; ok {
		if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
			v.SpikeSessionRatio = n
		}
	}
	if s, ok := rows[settingSpikeSessWindowN]; ok {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			v.SpikeSessionWindowN = n
		}
	}
	if s, ok := rows[settingSpikeTrendRatio]; ok {
		if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
			v.SpikeTrendRatio = n
		}
	}
	if s, ok := rows[settingTZDisplay]; ok && (s == "local" || s == "utc") {
		v.TZDisplay = s
	}
	return v, nil
}

// planBudgetUSD returns the user's "plan utilisation" baseline. Falls back to
// the supplied env-driven default when no setting has been persisted yet or
// the DB is unavailable (mock mode in tests).
func planBudgetUSD(conn *sql.DB, fallback float64) float64 {
	if conn == nil {
		return fallback
	}
	v, err := loadSettingsView(conn, fallback)
	if err != nil {
		return fallback
	}
	return v.PlanBudgetUSD
}

// apiRateAlertUSD returns the "OVER BUDGET" pill threshold. A value of zero
// means the pill is suppressed (appropriate for flat-rate Max plans).
func apiRateAlertUSD(conn *sql.DB) float64 {
	if conn == nil {
		return monthlyBudget()
	}
	v, err := loadSettingsView(conn, 0)
	if err != nil {
		return monthlyBudget()
	}
	return v.APIRateAlertUSD
}

// spikeConfigFromEnv returns the env-only spike-detector tuning. Kept for
// the rare case where a caller has no DB handle.
func spikeConfigFromEnv() stats.SpikeConfig {
	c := stats.DefaultSpikeConfig()
	if raw := os.Getenv("SESSIONLENS_SPIKE_SESSION_RATIO"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
			c.SessionRatio = v
		}
	}
	if raw := os.Getenv("SESSIONLENS_SPIKE_SESSION_WINDOW_N"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			c.SessionWindowN = v
		}
	}
	if raw := os.Getenv("SESSIONLENS_SPIKE_TREND_RATIO"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
			c.TrendRatio = v
		}
	}
	return c
}

// spikeConfigFor returns the effective spike-detector tuning for this server,
// merging settings DB > env > defaults.
func spikeConfigFor(conn *sql.DB) stats.SpikeConfig {
	c := spikeConfigFromEnv()
	if conn == nil {
		return c
	}
	v, err := loadSettingsView(conn, 0)
	if err != nil {
		return c
	}
	c.SessionRatio = v.SpikeSessionRatio
	c.SessionWindowN = v.SpikeSessionWindowN
	c.TrendRatio = v.SpikeTrendRatio
	return c
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
