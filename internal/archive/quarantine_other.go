//go:build !darwin

package archive

// tagOrigin is the portable fallback for the Darwin-only provenance xattrs.
//
// com.apple.quarantine and com.apple.metadata:kMDItemWhereFroms are macOS
// constructs interpreted by Gatekeeper, LaunchServices, Spotlight and Finder;
// nothing on another platform reads or honours them, so there is nothing to
// set. Returning nil keeps the writer's call site — and its tests, which
// inject Writer.TagOrigin — identical everywhere.
func tagOrigin(string, Origin) error { return nil }
