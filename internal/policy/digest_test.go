package policy

import (
	"strings"
	"testing"
)

func mustNew(t *testing.T, mut func(*Settings)) *Policy {
	t.Helper()
	s := DefaultSettings()
	if mut != nil {
		mut(&s)
	}
	p, err := New(s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// TestDigestIsStable: the same policy always hashes the same, across
// constructions and regardless of the order the operator wrote things in.
func TestDigestIsStable(t *testing.T) {
	a := mustNew(t, nil)
	b := mustNew(t, nil)
	if a.PolicyDigest() != b.PolicyDigest() {
		t.Fatalf("two default policies disagree: %s vs %s", a.PolicyDigest(), b.PolicyDigest())
	}
	if len(a.PolicyDigest()) != digestChars {
		t.Errorf("digest %q is %d chars, want %d", a.PolicyDigest(), len(a.PolicyDigest()), digestChars)
	}
	if strings.ToLower(a.PolicyDigest()) != a.PolicyDigest() {
		t.Errorf("digest %q must be lowercase hex", a.PolicyDigest())
	}
	// Recomputing from the same Policy is free of map-iteration nondeterminism.
	for i := 0; i < 50; i++ {
		if mustNew(t, nil).PolicyDigest() != a.PolicyDigest() {
			t.Fatal("digest is not deterministic across constructions")
		}
	}
}

// TestDigestIsOrderInsensitive: the digest describes the effective policy, not
// how it was typed.
func TestDigestIsOrderInsensitive(t *testing.T) {
	one := mustNew(t, func(s *Settings) {
		s.AllowExtensions = []string{"7z=application/zip,application/x-7z-compressed", "sql"}
		s.DenyExtensions = []string{"gif", "bmp"}
	})
	two := mustNew(t, func(s *Settings) {
		s.AllowExtensions = []string{"sql", "7z=application/x-7z-compressed|application/zip"}
		s.DenyExtensions = []string{"bmp", "gif"}
	})
	if one.PolicyDigest() != two.PolicyDigest() {
		t.Errorf("reordering the same policy changed the digest:\n%s\n---\n%s", one.Canonical(), two.Canonical())
	}
	// Spelling an extension with a dot or in caps is the same policy too.
	three := mustNew(t, func(s *Settings) {
		s.AllowExtensions = []string{".SQL", "7z=Application/Zip, application/x-7z-compressed"}
		s.DenyExtensions = []string{".GIF", "BMP"}
	})
	if three.PolicyDigest() != one.PolicyDigest() {
		t.Errorf("case/dot spelling changed the digest:\n%s\n---\n%s", one.Canonical(), three.Canonical())
	}
}

// TestDigestIsSensitive: everything that can change a Verdict must change the
// digest, or a widened policy would go unnoticed and `save refetch` would
// never be offered.
func TestDigestIsSensitive(t *testing.T) {
	base := mustNew(t, nil).PolicyDigest()
	for _, tc := range []struct {
		name string
		mut  func(*Settings)
	}{
		{"max_size", func(s *Settings) { s.MaxSize = 60 * 1000 * 1000 }},
		{"chat_max_size", func(s *Settings) { s.ChatMaxSize = 200 * 1000 * 1000 }},
		{"max_message_bytes", func(s *Settings) { s.MaxMessageBytes = 1 }},
		{"max_per_message", func(s *Settings) { s.MaxPerMessage = 1 }},
		{"max_parts_per_message", func(s *Settings) { s.MaxPartsPerMessage = 499 }},
		{"run_budget", func(s *Settings) { s.RunBudget = 1 }},
		{"free_space_floor", func(s *Settings) { s.FreeSpaceFloor = 1 }},
		{"allow_extensions", func(s *Settings) { s.AllowExtensions = []string{"sql"} }},
		{"allow_extensions types", func(s *Settings) { s.AllowExtensions = []string{"pdf=application/x-pdf"} }},
		{"deny_extensions", func(s *Settings) { s.DenyExtensions = []string{"gif"} }},
		{"allow_containers", func(s *Settings) { s.AllowContainers = false }},
		{"allow_svg", func(s *Settings) { s.AllowSVG = true }},
		{"allow_macro_office", func(s *Settings) { s.AllowMacroOffice = true }},
		{"on_mismatch", func(s *Settings) { s.OnMismatch = OnMismatchStoreWarn }},
		{"scan_action", func(s *Settings) { s.ScanAction = ScanActionReject; s.ScanCommand = []string{"x"} }},
		{"scan_command", func(s *Settings) { s.ScanCommand = []string{"clamdscan"} }},
		{"scan_command args", func(s *Settings) { s.ScanCommand = []string{"clamdscan", "--fdpass"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustNew(t, tc.mut).PolicyDigest(); got == base {
				t.Errorf("changing %s did not change the digest (%s)", tc.name, got)
			}
		})
	}
}

// TestDigestIgnoresQuarantine: quarantine changes how a stored file is tagged,
// never whether it is stored, so it must not invalidate stored skip rows.
func TestDigestIgnoresQuarantine(t *testing.T) {
	on := mustNew(t, func(s *Settings) { s.Quarantine = true })
	off := mustNew(t, func(s *Settings) { s.Quarantine = false })
	if on.PolicyDigest() != off.PolicyDigest() {
		t.Error("quarantine must not be part of the policy digest — it changes no storage decision")
	}
}

// TestCanonicalShape keeps the encoding readable and self-describing, since it
// is what `save doctor` prints when a digest changes unexpectedly.
func TestCanonicalShape(t *testing.T) {
	c := mustNew(t, nil).Canonical()
	if !strings.HasPrefix(c, "policy/v1\n") {
		t.Errorf("canonical encoding must start with its version: %.40q", c)
	}
	if !strings.Contains(c, "\ntolerations=v1\n") {
		t.Error("the tolerated-non-mismatch rules version must be in the digest input")
	}
	if !strings.Contains(c, "\next:pdf=application/pdf\n") {
		t.Error("the effective allowlist must be in the digest input")
	}
	if strings.Contains(c, "ext:exe") || strings.Contains(c, "ext:svg") || strings.Contains(c, "ext:docm") {
		t.Error("denied extensions must not appear in the effective allowlist")
	}
	// Lines are sorted within their two groups.
	var extLines []string
	for _, line := range strings.Split(c, "\n") {
		if strings.HasPrefix(line, "ext:") {
			extLines = append(extLines, line)
		}
	}
	if !sortedUnique(extLines) {
		t.Error("ext: lines must be sorted and unique")
	}
	if len(extLines) < 40 {
		t.Errorf("only %d allowlisted extensions; the business core should be much larger", len(extLines))
	}
}

// TestDigestSurvivesEquivalentRedundancy: naming a built-in extension in
// allow_extensions is a no-op, not a policy change.
func TestDigestSurvivesEquivalentRedundancy(t *testing.T) {
	base := mustNew(t, nil).PolicyDigest()
	redundant := mustNew(t, func(s *Settings) { s.AllowExtensions = []string{"pdf", "docx", ".PNG"} }).PolicyDigest()
	if redundant != base {
		t.Error("re-listing an already-allowed extension must not change the effective policy")
	}
}

// TestDigestNormalizesInheritedChatCap: ChatMaxSize <= 0 means "inherit
// MaxSize", so a Settings that leaves it zero and one that spells the
// inherited number out decide every attachment identically and must hash
// identically.
//
// This is a regression test, not a nicety. policy.DefaultSettings leaves
// ChatMaxSize at 0, while config.Attachments resolves it to max_size before
// calling PolicySettings. Encoding the raw field made policy.Default() and
// cfg.Policy() over a DEFAULT config disagree about the digest — and the
// digest is what the retro-fetch protocol keys on, so an archive whose
// recorded digest came from one path and whose running policy came from the
// other would report "the attachment policy has changed" on every run,
// forever, while nothing had actually changed.
func TestDigestNormalizesInheritedChatCap(t *testing.T) {
	inherited := mustNew(t, nil)
	spelledOut := mustNew(t, func(s *Settings) { s.ChatMaxSize = s.MaxSize })

	if inherited.MaxSizeFor(true) != spelledOut.MaxSizeFor(true) {
		t.Fatalf("the two policies do not agree on the effective chat cap: %d vs %d",
			inherited.MaxSizeFor(true), spelledOut.MaxSizeFor(true))
	}
	if inherited.PolicyDigest() != spelledOut.PolicyDigest() {
		t.Errorf("identical behaviour must hash identically:\n  chat_max_size unset      -> %s\n  chat_max_size = max_size -> %s",
			inherited.PolicyDigest(), spelledOut.PolicyDigest())
	}

	// A chat cap that genuinely differs from max_size must still move the
	// digest — the normalization must not flatten a real distinction.
	distinct := mustNew(t, func(s *Settings) { s.ChatMaxSize = s.MaxSize / 2 })
	if distinct.PolicyDigest() == inherited.PolicyDigest() {
		t.Error("a chat cap that differs from max_size must change the digest")
	}
}
