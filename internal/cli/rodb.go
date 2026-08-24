package cli

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite" // registers driver "sqlite"

	"save/internal/state"
)

// openRO opens the state database read-only. The state package deliberately
// exposes no whole-table scans (connectors never need them), so the
// reporting commands (status, verify) and cursor clearing read the schema
// directly on a separate connection; mode=ro guarantees they cannot write,
// and WAL lets them run beside an active daemon.
func openRO(dbPath string) (*sql.DB, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("no state database at %s — run `save sync` first (%w)", dbPath, err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open state database read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open state database read-only: %w", err)
	}
	return db, nil
}

// cursorRow is one cursors-table row as reported by status and cleared by
// sync --full.
type cursorRow struct {
	Source    string    `json:"source"`
	Scope     string    `json:"scope,omitempty"`
	Kind      string    `json:"kind"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

func listCursorsRO(db *sql.DB, source string) ([]cursorRow, error) {
	rows, err := db.Query(`
		SELECT source, scope, kind, value, updated_at
		FROM cursors WHERE source = ? ORDER BY scope, kind`, source)
	if err != nil {
		return nil, fmt.Errorf("list cursors for %s: %w", source, err)
	}
	defer rows.Close()
	var out []cursorRow
	for rows.Next() {
		var c cursorRow
		var updated string
		if err := rows.Scan(&c.Source, &c.Scope, &c.Kind, &c.Value, &updated); err != nil {
			return nil, fmt.Errorf("list cursors for %s: %w", source, err)
		}
		if t, perr := time.Parse(time.RFC3339, updated); perr == nil {
			c.UpdatedAt = t
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list cursors for %s: %w", source, err)
	}
	return out, nil
}

// chatCounts summarizes one Chat instance's canonical rows and its slice of
// the day-file ledger.
type chatCounts struct {
	Messages  int64 `json:"messages"`
	Deleted   int64 `json:"deleted"`
	DayFiles  int64 `json:"day_files"`
	DirtyDays int64 `json:"dirty_days"`
}

func chatCountsRO(db *sql.DB, source string) (chatCounts, error) {
	var c chatCounts
	if err := db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(deleted), 0) FROM chat_messages WHERE source = ?`, source).
		Scan(&c.Messages, &c.Deleted); err != nil {
		return c, fmt.Errorf("chat message counts for %s: %w", source, err)
	}
	if err := db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(dirty), 0) FROM chat_day_files WHERE source = ?`, source).
		Scan(&c.DayFiles, &c.DirtyDays); err != nil {
		return c, fmt.Errorf("chat day-file counts for %s: %w", source, err)
	}
	return c, nil
}

// listSourcesRO returns every distinct instance id the state database holds
// rows for, sorted. status compares it against the configured instances to
// spot state stranded by a renamed or deleted account label.
func listSourcesRO(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`
		SELECT source FROM cursors
		UNION SELECT source FROM messages
		UNION SELECT source FROM attachments
		UNION SELECT source FROM chat_spaces
		UNION SELECT source FROM chat_messages
		UNION SELECT source FROM gmail_backfill
		UNION SELECT source FROM failures
		UNION SELECT source FROM skipped_attachments
		UNION SELECT source FROM sync_runs
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("list state sources: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("list state sources: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list state sources: %w", err)
	}
	return out, nil
}

// skippedBytes is one instance's slice of the never-drop-silently ledger,
// counted in BYTES rather than rows — the number that says how much of the
// correspondence is not actually in the archive.
type skippedBytes struct {
	// NotStored is the decoded size of every refused attachment whose bytes
	// are still not on disk. A row resolved "fetched" is excluded because
	// `save refetch` since put those bytes in the archive; rows resolved
	// "source_gone" and "still_denied" are INCLUDED, because they are exactly
	// the ones that are gone for good or refused for good.
	NotStored int64 `json:"not_stored"`

	// Unresolved is the subset whose skip is still an open question — the
	// bytes `save refetch` could still recover if the policy were widened.
	Unresolved int64 `json:"unresolved"`
}

// skippedBytesRO totals the refused bytes per instance. state deliberately
// exposes no whole-table scans, so the reporting commands read the ledger
// directly on the read-only connection, exactly as they do for cursors.
func skippedBytesRO(db *sql.DB) (map[string]skippedBytes, error) {
	rows, err := db.Query(`
		SELECT source,
		       COALESCE(SUM(CASE WHEN resolution IS NULL OR resolution <> ? THEN size_bytes ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN resolved_at IS NULL                  THEN size_bytes ELSE 0 END), 0)
		FROM skipped_attachments
		GROUP BY source`, state.SkipResolutionFetched)
	if err != nil {
		return nil, fmt.Errorf("skipped attachment bytes: %w", err)
	}
	defer rows.Close()
	out := make(map[string]skippedBytes)
	for rows.Next() {
		var src string
		var b skippedBytes
		if err := rows.Scan(&src, &b.NotStored, &b.Unresolved); err != nil {
			return nil, fmt.Errorf("skipped attachment bytes: %w", err)
		}
		out[src] = b
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skipped attachment bytes: %w", err)
	}
	return out, nil
}

// metaRO reads one meta value without opening the typed store (doctor must
// not create or migrate anything).
func metaRO(db *sql.DB, key string) (string, bool, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read meta %q: %w", key, err)
	}
	return v, true, nil
}
