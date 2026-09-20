// sessionlens-hook is the Claude Code Stop-hook binary. It reads the hook
// JSON event from stdin, parses the referenced transcript JSONL, computes
// token usage + cost, and POSTs the result to the local sessionlens-server.
//
// Operational contract: this binary MUST NOT block or fail Claude Code.
// Every error path is swallowed, the process always exits 0, and a panic
// recover guards main.
//
// Retry + buffer policy:
//   - On POST failure (network error or 5xx) the event is retried up to 3 times
//     with exponential back-off: 1 s, 2 s, 4 s.
//   - 4xx responses are non-retryable (log + drop; server bug, not transient).
//   - If all retries exhaust the event is written to the on-disk buffer
//     ($XDG_STATE_HOME/sessionlens/buffer/ or ~/.local/state/sessionlens/buffer/).
//   - Before posting the current event, any previously buffered events are
//     drained: each file is attempted once; success → delete, failure → leave.
//   - The buffer is capped at 100 files; oldest are removed to make room.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/viharshah/session-lens/internal/transcript"
)

// hookInput is the subset of the Claude Code Stop-hook event we read.
type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
}

// sessionEvent is the payload POSTed to the server.
type sessionEvent struct {
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
	TurnEvents       []sessionTurnEvent `json:"turn_events,omitempty"`
}

// sessionTurnEvent mirrors server.SessionTurnEvent on the wire. Decoupled
// type so this binary stays free of the server-internal import cycle.
type sessionTurnEvent struct {
	Idx              int     `json:"idx"`
	EndedAt          string  `json:"ended_at"`
	Model            string  `json:"model"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

const (
	serverURLEnv  = "SESSION_LENS_URL"
	defaultServer = "http://127.0.0.1:7821/v1/sessions"
	hookLogPath   = "/tmp/session-lens-hook.log"
	hookTimeout   = 1500 * time.Millisecond
	bufferCap     = 100
)

// retryBackoff is the sequence of waits between attempts (3 tries total).
var retryBackoff = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

// httpPoster abstracts net/http for test injection.
type httpPoster interface {
	Do(*http.Request) (*http.Response, error)
}

func main() {
	// Belt-and-suspenders: a panic anywhere below must not propagate to Claude.
	defer func() {
		if r := recover(); r != nil {
			logErr(fmt.Errorf("panic: %v", r))
		}
		os.Exit(0)
	}()

	client := &http.Client{Timeout: hookTimeout}
	run(client)
}

func run(client httpPoster) {
	url := os.Getenv(serverURLEnv)
	if url == "" {
		url = defaultServer
	}

	bufDir := bufferDir()

	// Always drain buffered events before posting the current one.
	drainBuffer(client, url, bufDir)

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		logErr(fmt.Errorf("read stdin: %w", err))
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return
	}

	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		logErr(fmt.Errorf("parse hook json: %w", err))
		return
	}
	if in.TranscriptPath == "" || in.SessionID == "" {
		logErr(fmt.Errorf("hook missing transcript_path or session_id"))
		return
	}

	summary, err := transcript.ParseFile(in.TranscriptPath)
	if err != nil {
		logErr(fmt.Errorf("parse transcript: %w", err))
		return
	}

	endedAt := summary.EndedAt
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	startedAt := summary.StartedAt
	startedISO := ""
	if !startedAt.IsZero() {
		startedISO = startedAt.UTC().Format(time.RFC3339)
	}

	turnEvents := make([]sessionTurnEvent, 0, len(summary.TurnEvents))
	for _, t := range summary.TurnEvents {
		turnEvents = append(turnEvents, sessionTurnEvent{
			Idx:              t.Idx,
			EndedAt:          t.EndedAt.UTC().Format(time.RFC3339),
			Model:            t.Model,
			InputTokens:      t.InputTokens,
			OutputTokens:     t.OutputTokens,
			CacheReadTokens:  t.CacheReadTokens,
			CacheWriteTokens: t.CacheWriteTokens,
			CostUSD:          t.CostUSD,
		})
	}

	ev := sessionEvent{
		ID:               in.SessionID,
		ProjectPath:      in.CWD,
		StartedAt:        startedISO,
		EndedAt:          endedAt.UTC().Format(time.RFC3339),
		InputTokens:      summary.InputTokens,
		OutputTokens:     summary.OutputTokens,
		CacheReadTokens:  summary.CacheReadTokens,
		CacheWriteTokens: summary.CacheWriteTokens,
		TotalCostUSD:     summary.TotalCostUSD,
		Model:            summary.Model,
		Turns:            summary.Turns,
		RawPayload:       string(raw),
		TurnEvents:       turnEvents,
	}

	body, err := json.Marshal(ev)
	if err != nil {
		logErr(fmt.Errorf("encode event: %w", err))
		return
	}

	if err := postWithRetry(client, url, body); err != nil {
		logErr(fmt.Errorf("post failed after retries: %w; buffering", err))
		if bufErr := writeBuffer(bufDir, body); bufErr != nil {
			logErr(fmt.Errorf("buffer write: %w", bufErr))
		}
	}
}

// postWithRetry attempts to POST body to url up to len(retryBackoff)+1 times.
// Network errors and 5xx responses are retried; 4xx responses are dropped.
func postWithRetry(client httpPoster, url string, body []byte) error {
	var lastErr error
	attempts := len(retryBackoff) + 1
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(retryBackoff[i-1])
		}
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "sessionlens-hook/0.1")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("network: %w", err)
			logErr(fmt.Errorf("attempt %d/%d failed: %w", i+1, attempts, lastErr))
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("server %d", resp.StatusCode)
			logErr(fmt.Errorf("attempt %d/%d failed: %w", i+1, attempts, lastErr))
			continue
		}
		if resp.StatusCode >= 400 {
			// Non-retryable client error.
			return fmt.Errorf("non-retryable %d", resp.StatusCode)
		}
		// Success.
		return nil
	}
	return lastErr
}

// drainBuffer iterates over buffered event files and attempts to post each one.
// Successful deliveries are deleted; failures are renamed back for retry.
//
// Concurrency: multiple hook processes may run simultaneously. Each candidate
// file is renamed to ".processing-<pid>" before being read, so only one process
// owns it at a time. Files left in ".processing-*" form (e.g. crashed process)
// are reclaimed on the next drain that lands.
func drainBuffer(client httpPoster, url, dir string) {
	// First reclaim any leftover ".processing-*" files from prior crashes back to plain ".json".
	stale, _ := filepath.Glob(filepath.Join(dir, "*.json.processing-*"))
	for _, sf := range stale {
		orig := stripProcessingSuffix(sf)
		if orig != "" {
			_ = os.Rename(sf, orig)
		}
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		return
	}
	suffix := fmt.Sprintf(".processing-%d", os.Getpid())
	for _, f := range files {
		claimed := f + suffix
		if err := os.Rename(f, claimed); err != nil {
			// Another process likely won the race, or the file vanished. Skip silently.
			continue
		}
		data, err := os.ReadFile(claimed)
		if err != nil {
			logErr(fmt.Errorf("drain read %s: %w", claimed, err))
			_ = os.Rename(claimed, f)
			continue
		}
		if err := postWithRetry(client, url, data); err != nil {
			logErr(fmt.Errorf("drain post %s: %w", claimed, err))
			_ = os.Rename(claimed, f)
			continue
		}
		if err := os.Remove(claimed); err != nil {
			logErr(fmt.Errorf("drain delete %s: %w", claimed, err))
		}
	}
}

// stripProcessingSuffix returns the original filename if path ends in
// ".processing-<digits>", or "" if it doesn't match.
func stripProcessingSuffix(path string) string {
	const marker = ".processing-"
	i := lastIndex(path, marker)
	if i < 0 {
		return ""
	}
	// Verify everything after marker is digits.
	tail := path[i+len(marker):]
	if tail == "" {
		return ""
	}
	for _, c := range tail {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return path[:i]
}

func lastIndex(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// writeBuffer persists body to the buffer directory.
// If the buffer already holds bufferCap files the oldest are removed to make room.
func writeBuffer(dir string, body []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir buffer: %w", err)
	}

	// Enforce cap.
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err == nil && len(files) >= bufferCap {
		// Sort by name (timestamp prefix → chronological order).
		sort.Strings(files)
		toRemove := len(files) - bufferCap + 1
		for _, old := range files[:toRemove] {
			_ = os.Remove(old)
		}
	}

	name := fmt.Sprintf("%d-%06d.json", time.Now().UnixNano(), rand.Intn(1_000_000))
	path := filepath.Join(dir, name)
	// Write to a temp file first then rename, so a drainer can never read
	// a half-written buffer file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// bufferDir returns the path to the buffer directory.
func bufferDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/tmp"
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "sessionlens", "buffer")
}

// logErr appends an error line to the hook log. Failures here are silent.
func logErr(err error) {
	if err == nil {
		return
	}
	f, openErr := os.OpenFile(hookLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if openErr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %v\n", time.Now().UTC().Format(time.RFC3339), err)
}
