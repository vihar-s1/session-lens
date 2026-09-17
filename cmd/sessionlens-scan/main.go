// sessionlens-scan walks ~/.claude/projects/<project-hash>/<session-id>.jsonl
// and ingests any session not already present in the local sessions DB.
//
// This closes the missed-hook gap: when Claude Code is killed before the Stop
// hook runs (terminal close, kill -9, system crash), no event is delivered.
// Running this reconciliation periodically — or just before reviewing the
// dashboard — recovers those sessions from the on-disk transcripts.
//
// Usage:
//
//	sessionlens-scan                     # uses default paths
//	sessionlens-scan -db ./data/sessions.db -root ~/.claude/projects
//	sessionlens-scan -dry-run            # report what would be ingested, write nothing
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/viharshah/session-lens/internal/db"
	"github.com/viharshah/session-lens/internal/transcript"
)

func main() {
	var (
		dbPath = flag.String("db", "./data/sessions.db", "path to the sessions SQLite DB")
		root   = flag.String("root", defaultProjectsRoot(), "Claude Code projects root")
		dryRun = flag.Bool("dry-run", false, "list missing sessions without writing")
	)
	flag.Parse()

	conn, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	matches, err := filepath.Glob(filepath.Join(*root, "*", "*.jsonl"))
	if err != nil {
		log.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		fmt.Printf("no transcripts found under %s\n", *root)
		return
	}

	known, err := loadKnownIDs(conn)
	if err != nil {
		log.Fatalf("load known ids: %v", err)
	}
	withTurns, err := loadSessionsWithTurns(conn)
	if err != nil {
		log.Fatalf("load turn coverage: %v", err)
	}

	var scanned, missing, ingested, failed, skippedEmpty, turnsBackfilled int
	for _, path := range matches {
		scanned++
		id := strings.TrimSuffix(filepath.Base(path), ".jsonl")

		needsSession := !known[id]
		needsTurns := !withTurns[id]
		if !needsSession && !needsTurns {
			continue
		}
		if needsSession {
			missing++
		}

		if *dryRun {
			fmt.Printf("would ingest %s  (%s)  session=%v turns=%v\n", id, path, needsSession, needsTurns)
			continue
		}

		s, err := transcript.ParseFile(path)
		if err != nil {
			log.Printf("parse %s: %v", path, err)
			failed++
			continue
		}
		// Skip empty/degenerate transcripts (aborted before any usage).
		if s.Turns == 0 && s.InputTokens == 0 && s.OutputTokens == 0 {
			skippedEmpty++
			continue
		}

		project := projectFromDir(filepath.Base(filepath.Dir(path)))
		ended := s.EndedAt
		if ended.IsZero() {
			fi, statErr := os.Stat(path)
			if statErr == nil {
				ended = fi.ModTime().UTC()
			} else {
				ended = time.Now().UTC()
			}
		}
		started := ""
		if !s.StartedAt.IsZero() {
			started = s.StartedAt.UTC().Format(time.RFC3339)
		}

		if needsSession {
			row := db.Session{
				ID:               id,
				ProjectPath:      project,
				StartedAt:        started,
				EndedAt:          ended.UTC().Format(time.RFC3339),
				InputTokens:      s.InputTokens,
				OutputTokens:     s.OutputTokens,
				CacheReadTokens:  s.CacheReadTokens,
				CacheWriteTokens: s.CacheWriteTokens,
				TotalCostUSD:     s.TotalCostUSD,
				Model:            s.Model,
				Turns:            s.Turns,
			}
			if _, _, err := db.UpsertSession(conn, row); err != nil {
				log.Printf("upsert %s: %v", id, err)
				failed++
				continue
			}
			ingested++
			known[id] = true
		}

		// Always (re)write turns for any session we touched. INSERT OR IGNORE
		// keeps existing rows intact and just appends new turns; backfill on
		// a session that had no turns yet picks up the entire series.
		if len(s.TurnEvents) > 0 {
			turns := make([]db.Turn, 0, len(s.TurnEvents))
			for _, t := range s.TurnEvents {
				turns = append(turns, db.Turn{
					SessionID:        id,
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
			if err := db.InsertTurns(conn, turns); err != nil {
				log.Printf("insert turns %s: %v", id, err)
			} else if needsTurns {
				turnsBackfilled++
				withTurns[id] = true
			}
		}
	}

	fmt.Printf("scanned=%d missing=%d ingested=%d turns_backfilled=%d skipped_empty=%d failed=%d dry-run=%v\n",
		scanned, missing, ingested, turnsBackfilled, skippedEmpty, failed, *dryRun)
}

// loadSessionsWithTurns returns the set of session ids that already have at
// least one row in the turns table. Used to identify sessions imported before
// the turns schema landed that still need a backfill from their transcript.
func loadSessionsWithTurns(conn *sql.DB) (map[string]bool, error) {
	rows, err := conn.Query(`SELECT DISTINCT session_id FROM turns`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool, 1024)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// loadKnownIDs returns the set of session ids already in the DB.
func loadKnownIDs(conn *sql.DB) (map[string]bool, error) {
	rows, err := conn.Query(`SELECT id FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool, 1024)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// projectFromDir reverses the directory-name encoding Claude Code uses for
// projects (`/Users/x/y` → `-Users-x-y`). Lossy for paths whose components
// contain `-` natively — best-effort recovery.
func projectFromDir(dir string) string {
	if dir == "" {
		return ""
	}
	if dir[0] != '-' {
		return dir
	}
	return strings.ReplaceAll(dir, "-", "/")
}

func defaultProjectsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}
