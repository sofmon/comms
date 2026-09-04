// Package source defines the contract every connector implements.
package source

import "context"

// Source is one archivable communication source (gmail, gchat, fastmail).
//
// Connectors receive their concrete dependencies (state store, archive
// writer, limiters, logger) at construction; this interface is what the
// daemon scheduler and the one-shot sync command drive.
type Source interface {
	Name() string

	// Check is a cheap auth/config probe used by `comms doctor` and
	// `comms status`. It must not mutate any state.
	Check(ctx context.Context) error

	// Sync runs one full pass: backfill when no cursor exists, otherwise
	// incremental. It must be safe to interrupt at any point and re-run:
	// files are written before their DB rows commit, cursors advance only
	// after the work they cover is durable, and deterministic paths plus
	// primary-key dedup make reprocessing a no-op.
	Sync(ctx context.Context) error
}
