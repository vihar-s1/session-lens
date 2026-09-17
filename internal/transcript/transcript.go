// Package transcript parses Claude Code session transcript JSONL files and
// aggregates per-session token usage for billing/reporting.
package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// Summary is the per-session aggregate computed from a transcript file.
type Summary struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalTokens      int64
	TotalCostUSD     float64
	Model            string
	Turns            int
	StartedAt        time.Time
	EndedAt          time.Time
	// TurnEvents is the per-assistant-message usage series, ordered by
	// appearance in the transcript. Populated alongside the aggregate fields.
	// Empty for transcripts with no assistant usage entries.
	TurnEvents []TurnEvent
}

// TurnEvent is one assistant message's usage + timestamp + cost. The Idx
// reflects appearance order (0-based) within the transcript; combined with
// the parent session id it gives a stable PK for idempotent re-ingestion.
type TurnEvent struct {
	Idx              int
	EndedAt          time.Time
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
}

// rawLine is the minimal subset of a transcript JSONL line we care about.
type rawLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   *struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// ParseFile opens path and aggregates usage. Malformed JSON lines are skipped.
func ParseFile(path string) (*Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads JSONL from r and aggregates usage. Returns a non-nil Summary
// even when the file is empty; only IO errors are returned.
func Parse(r io.Reader) (*Summary, error) {
	s := &Summary{}
	scanner := bufio.NewScanner(r)
	// Allow very large lines (transcript messages can be big — tool results
	// containing screenshots or large file contents have been seen at >16MB).
	const maxLine = 64 * 1024 * 1024
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, maxLine)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rl rawLine
		if err := json.Unmarshal(line, &rl); err != nil {
			// Malformed JSON line: ignore and continue.
			continue
		}
		// Track timestamps across all line types (parse best-effort).
		if rl.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339Nano, rl.Timestamp); err == nil {
				if s.StartedAt.IsZero() || t.Before(s.StartedAt) {
					s.StartedAt = t
				}
				if t.After(s.EndedAt) {
					s.EndedAt = t
				}
			}
		}
		if rl.Type != "assistant" || rl.Message == nil {
			continue
		}
		s.Turns++
		if rl.Message.Model != "" {
			s.Model = rl.Message.Model
		}
		if rl.Message.Usage != nil {
			// Clamp each field to 0 before accumulating; negative token values
			// from a malformed or adversarial transcript must not corrupt the
			// running sums or the downstream cost calculation.
			in := clampTokens(rl.Message.Usage.InputTokens)
			out := clampTokens(rl.Message.Usage.OutputTokens)
			cr := clampTokens(rl.Message.Usage.CacheReadInputTokens)
			cw := clampTokens(rl.Message.Usage.CacheCreationInputTokens)
			s.InputTokens += in
			s.OutputTokens += out
			s.CacheReadTokens += cr
			s.CacheWriteTokens += cw
			// Per-turn row — timestamp falls back to the session's running
			// EndedAt if this line is missing one (rare but seen on truncated
			// transcripts). Cost is computed per-turn using the turn's model
			// so mixed-model sessions price each turn correctly.
			turnEnd := s.EndedAt
			if rl.Timestamp != "" {
				if t, err := time.Parse(time.RFC3339Nano, rl.Timestamp); err == nil {
					turnEnd = t
				}
			}
			s.TurnEvents = append(s.TurnEvents, TurnEvent{
				Idx:              len(s.TurnEvents),
				EndedAt:          turnEnd.UTC(),
				Model:            rl.Message.Model,
				InputTokens:      in,
				OutputTokens:     out,
				CacheReadTokens:  cr,
				CacheWriteTokens: cw,
				CostUSD:          ComputeCost(rl.Message.Model, in, out, cr, cw),
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan transcript: %w", err)
	}

	s.TotalTokens = s.InputTokens + s.OutputTokens + s.CacheReadTokens + s.CacheWriteTokens
	s.TotalCostUSD = ComputeCost(s.Model, s.InputTokens, s.OutputTokens, s.CacheReadTokens, s.CacheWriteTokens)
	return s, nil
}
