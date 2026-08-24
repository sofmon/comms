//go:build !darwin

// Portable fallbacks for the macOS FileProvider introspection in
// cloudfs_darwin.go. No other platform this program builds for has
// materialize-on-read cloud placeholders, so the honest answer everywhere
// here is "no, and there is nothing to turn off".

package cli

import "errors"

// errNoCloudFS marks the probes that have no meaningful answer off macOS.
// Callers treat it as "skip this check", not as a failure.
var errNoCloudFS = errors.New("cloud-storage introspection is only implemented on macOS")

// datalessFile always reports false: no evicted placeholders exist here, so
// verify hashes every file exactly as it always did.
func datalessFile(string) (bool, error) { return false, nil }

// trackedPath always reports false: UF_TRACKED is a Darwin file flag.
func trackedPath(string) (bool, error) { return false, nil }

// freeBytes is unimplemented; doctor drops the free-space line rather than
// guessing.
func freeBytes(string) (uint64, error) { return 0, errNoCloudFS }

// optimizeStorage is unknowable off macOS.
func optimizeStorage() (on bool, known bool) { return false, false }

// setNoMaterialize succeeds vacuously: with no materialize-on-read behaviour
// to disable, the post-condition ("a read cannot trigger a cloud download")
// already holds, and verify must not print a warning about it on every run.
func setNoMaterialize() error { return nil }
