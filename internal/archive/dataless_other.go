//go:build !darwin

package archive

// isDataless is the portable fallback for the Darwin-only SF_DATALESS check.
//
// Content eviction behind a same-named placeholder is a macOS/iCloud
// FileProvider behaviour; no other platform this code builds on can present
// a file whose bytes are absent, so nothing is ever dataless here. Returning
// (false, nil) makes the writer's guard a no-op on Linux while keeping the
// call site — and its tests, which inject Writer.Dataless — identical
// everywhere.
func isDataless(string) (bool, error) { return false, nil }
