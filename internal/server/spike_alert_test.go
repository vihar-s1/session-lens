package server_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/viharshah/session-lens/internal/alerter"
	"github.com/viharshah/session-lens/internal/db"
	"github.com/viharshah/session-lens/internal/server"
)

// stubNotifier records Notify calls; safe for concurrent use.
type stubNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
}

type notifyCall struct {
	title   string
	message string
	openURL string
}

func (s *stubNotifier) Notify(title, message, openURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, notifyCall{title, message, openURL})
}

func (s *stubNotifier) Calls() []notifyCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]notifyCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// newSpikeTestServer creates a test server with a real DB and the given stub notifier.
func newSpikeTestServer(t *testing.T, stub alerter.Notifier) (*httptest.Server, *sql.DB) {
	t.Helper()
	conn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	h := server.New(server.Config{
		DB:          conn,
		MockDefault: false,
		Notifier:    stub,
	})
	ts := httptest.NewServer(h)
	return ts, conn
}

// postSession sends a POST /v1/sessions and returns the HTTP status.
func postSession(t *testing.T, ts *httptest.Server, s db.Session) int {
	t.Helper()
	body, _ := json.Marshal(server.SessionEvent{
		ID:           s.ID,
		ProjectPath:  s.ProjectPath,
		StartedAt:    s.StartedAt,
		EndedAt:      s.EndedAt,
		TotalCostUSD: s.TotalCostUSD,
		Model:        s.Model,
		Turns:        s.Turns,
	})
	resp, err := http.Post(ts.URL+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST session: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// daysAgo returns an RFC3339 timestamp N days before today at noon UTC.
func daysAgo(n int) string {
	t := time.Now().UTC().AddDate(0, 0, -n).Truncate(24 * time.Hour).Add(12 * time.Hour)
	return t.Format(time.RFC3339)
}

// TestSpikeNotification verifies that a session costing more than 2x the
// 7-day rolling average fires the notifier, while a normal session does not.
func TestSpikeNotification(t *testing.T) {
	stub := &stubNotifier{}
	ts, conn := newSpikeTestServer(t, stub)
	defer ts.Close()

	// Seed 5 "normal" sessions in the prior 7 days (each costs $0.01).
	// These establish the rolling avg = $0.01.
	for i := 1; i <= 5; i++ {
		row := db.Session{
			ID:           "baseline-" + time.Now().Format("150405") + "-" + string(rune('0'+i)),
			StartedAt:    daysAgo(i + 1),
			EndedAt:      daysAgo(i),
			TotalCostUSD: 0.01,
			Model:        "claude-sonnet",
			Turns:        1,
		}
		if _, _, err := db.UpsertSession(conn, row); err != nil {
			t.Fatalf("seed baseline: %v", err)
		}
	}

	// Post a spike session today: $0.05 > 2 * $0.01 = $0.02 → should notify.
	spikeSession := db.Session{
		ID:           "spike-session-1",
		StartedAt:    time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
		EndedAt:      time.Now().UTC().Format(time.RFC3339),
		TotalCostUSD: 0.05,
		Model:        "claude-sonnet",
		Turns:        3,
	}
	statusCode := postSession(t, ts, spikeSession)
	if statusCode != http.StatusCreated {
		t.Fatalf("expected 201 got %d", statusCode)
	}

	// The goroutine fires async; give it a short window to complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(stub.Calls()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	calls := stub.Calls()
	if len(calls) == 0 {
		t.Fatal("expected at least one Notify call for spike session, got none")
	}
	if calls[0].title != "session-lens: cost spike" {
		t.Errorf("unexpected title: %q", calls[0].title)
	}
}

// TestNoSpikeNotification verifies that a session within normal range does NOT
// fire the notifier.
func TestNoSpikeNotification(t *testing.T) {
	stub := &stubNotifier{}
	ts, conn := newSpikeTestServer(t, stub)
	defer ts.Close()

	// Seed 5 baseline sessions each at $0.10.
	for i := 1; i <= 5; i++ {
		row := db.Session{
			ID:           "baseline-ns-" + string(rune('0'+i)),
			StartedAt:    daysAgo(i + 1),
			EndedAt:      daysAgo(i),
			TotalCostUSD: 0.10,
			Model:        "claude-sonnet",
			Turns:        1,
		}
		if _, _, err := db.UpsertSession(conn, row); err != nil {
			t.Fatalf("seed baseline: %v", err)
		}
	}

	// Post a normal session at $0.11 — less than 2x $0.10 → no notification.
	normalSession := db.Session{
		ID:           "normal-session-1",
		StartedAt:    time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
		EndedAt:      time.Now().UTC().Format(time.RFC3339),
		TotalCostUSD: 0.11,
		Model:        "claude-sonnet",
		Turns:        2,
	}
	statusCode := postSession(t, ts, normalSession)
	if statusCode != http.StatusCreated {
		t.Fatalf("expected 201 got %d", statusCode)
	}

	// Wait briefly to let any goroutine complete.
	time.Sleep(100 * time.Millisecond)

	calls := stub.Calls()
	if len(calls) != 0 {
		t.Fatalf("expected no Notify calls for normal session, got %d: %+v", len(calls), calls)
	}
}

// TestNoNotification_ZeroCostSession verifies that a session with TotalCostUSD
// == 0 does NOT fire the notifier, regardless of the rolling average.
// This pins the `ev.TotalCostUSD > 0` guard in server.go.
func TestNoNotification_ZeroCostSession(t *testing.T) {
	stub := &stubNotifier{}
	ts, conn := newSpikeTestServer(t, stub)
	defer ts.Close()

	// Seed a high baseline so the zero-cost session would exceed 2x IF the guard
	// were absent.
	for i := 1; i <= 3; i++ {
		row := db.Session{
			ID:           "base-zc-" + string(rune('0'+i)),
			StartedAt:    daysAgo(i + 1),
			EndedAt:      daysAgo(i),
			TotalCostUSD: 0.50,
			Model:        "claude-sonnet",
			Turns:        1,
		}
		if _, _, err := db.UpsertSession(conn, row); err != nil {
			t.Fatalf("seed baseline: %v", err)
		}
	}

	zero := db.Session{
		ID:           "zero-cost-sess",
		StartedAt:    time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339),
		EndedAt:      time.Now().UTC().Format(time.RFC3339),
		TotalCostUSD: 0,
		Model:        "claude-sonnet",
		Turns:        1,
	}
	status := postSession(t, ts, zero)
	if status != http.StatusCreated {
		t.Fatalf("expected 201 got %d", status)
	}

	// Give the goroutine time to run (it would fire quickly if unguarded).
	time.Sleep(150 * time.Millisecond)

	calls := stub.Calls()
	if len(calls) != 0 {
		t.Fatalf("expected no Notify calls for zero-cost session, got %d: %+v", len(calls), calls)
	}
}

// TestNoNotification_EmptyRollingWindow verifies that the very first session
// (empty rolling window → avg=0) does NOT fire the notifier even if its cost
// is large. This pins the `avg > 0` guard in server.go.
func TestNoNotification_EmptyRollingWindow(t *testing.T) {
	stub := &stubNotifier{}
	ts, _ := newSpikeTestServer(t, stub)
	defer ts.Close()

	// No baseline sessions — rolling window is empty.
	first := db.Session{
		ID:           "first-ever-session",
		StartedAt:    time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339),
		EndedAt:      time.Now().UTC().Format(time.RFC3339),
		TotalCostUSD: 99.99, // very large cost, but avg==0 so no notification expected
		Model:        "claude-opus",
		Turns:        10,
	}
	status := postSession(t, ts, first)
	if status != http.StatusCreated {
		t.Fatalf("expected 201 got %d", status)
	}

	// Give the goroutine time to run.
	time.Sleep(150 * time.Millisecond)

	calls := stub.Calls()
	if len(calls) != 0 {
		t.Fatalf("expected no Notify on first session (empty window avg=0), got %d: %+v", len(calls), calls)
	}
}
