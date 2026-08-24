package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"save/internal/paths"
	"save/internal/policy"
	"save/internal/state"
)

// doctorText runs doctor over a config and returns its output. Credentials
// never exist here, so every account check stops before any network call —
// the [attachments] section is printed before those and is unaffected.
func doctorText(t *testing.T, cfgTOML string) string {
	t.Helper()
	cfgDir, stateHome := t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	writeFile(t, filepath.Join(cfgDir, "config.toml"), cfgTOML)
	var out strings.Builder
	_ = runDoctorWith(&out, fakeCloudEnv(t.TempDir()))
	return out.String()
}

// TestDoctorReportsTheEffectiveAttachmentPolicy: a compact summary — the
// shape of the allowlist, the contested flags, the caps and the digest — not
// a wall of 57 extensions.
func TestDoctorReportsTheEffectiveAttachmentPolicy(t *testing.T) {
	text := doctorText(t, plainConfig(t.TempDir()))

	for _, want := range []string{
		"[attachments] — attachment storage policy",
		"allowlist: 57 extension(s)",
		"65 executable type(s) can never be allowed",
		"contested defaults: zip allowed, svg denied, macro-Office denied",
		"50.0 MB per attachment",
		"5.00 GB free-space floor",
		`on_mismatch = "skip"`,
		"no scan_command configured",
		"policy digest ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("doctor does not report %q:\n%s", want, text)
		}
	}
	// Compact means compact: doctor must not dump the whole allowlist.
	if strings.Count(text, "and 49 more") != 1 {
		t.Errorf("doctor should elide the tail of the allowlist:\n%s", text)
	}
}

// TestDoctorQuarantineClaimIsHonest is a guard against the single most
// tempting lie in this program. XProtect has ZERO signatures for any type on
// save's allowlist — verified, all 94 gate on app bundles, installers and
// executables — so the quarantine tag scans nothing. doctor must describe the
// consent prompt and Protected View, and must never imply antivirus.
func TestDoctorQuarantineClaimIsHonest(t *testing.T) {
	text := doctorText(t, plainConfig(t.TempDir()))

	for _, want := range []string{
		"consent prompt",
		"Protected View",
		"It is NOT a malware scan",
		"The allowlist is the real boundary",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("doctor's quarantine note is missing %q:\n%s", want, text)
		}
	}
	lower := strings.ToLower(text)
	for _, forbidden := range []string{
		"scanned for malware", "scans for malware", "virus scan", "antivirus protection",
		"checked for viruses", "malware protection",
	} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("doctor claims %q, which is false for every allowlisted type:\n%s", forbidden, text)
		}
	}
}

// TestDoctorReportsPolicyOverridesAndDigestDrift: the operator's own
// widenings are echoed back, and a digest that no longer matches the recorded
// one is a warning with the recovery path — never an automatic fetch.
func TestDoctorReportsPolicyOverridesAndDigestDrift(t *testing.T) {
	cfgDir, stateHome := t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	writeFile(t, filepath.Join(cfgDir, "config.toml"), widenedConfig(t.TempDir()))

	// A state database whose recorded digest is the SHIPPED default: the
	// config above has since been widened.
	if err := paths.EnsureDir(paths.StateDir()); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(stateDBPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAttachmentPolicyDigest(policy.Default().PolicyDigest()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var out strings.Builder
	_ = runDoctorWith(&out, fakeCloudEnv(t.TempDir()))
	text := out.String()

	for _, want := range []string{
		"contested defaults: zip allowed, svg allowed, macro-Office allowed",
		"local overrides: allow_extensions [7z]",
		"deny always wins",
		`on_mismatch = "store-warn"`,
		"is STORED",
		"differs from the recorded " + policy.Default().PolicyDigest(),
		"Nothing is re-fetched automatically",
		"save refetch --dry-run",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("doctor does not report %q:\n%s", want, text)
		}
	}
	// Digest drift is a warning, not a problem: doctor must not fail the run
	// over a policy the user deliberately widened.
	if strings.Contains(text, "PROBLEM  policy digest") {
		t.Errorf("digest drift was reported as a problem:\n%s", text)
	}
}
