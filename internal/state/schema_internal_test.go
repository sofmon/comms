package state

import (
	"path/filepath"
	"strings"
	"testing"

	"save/internal/policy"
)

// The schema's enum CHECKs are generated from policy.Reasons() and
// SkipResolutions(). These tests are INTERNAL because the typed API validates
// in Go before SQL ever sees the value, so the constraints themselves are
// unreachable from outside the package — and an unsubstituted placeholder
// would leave a CHECK that silently passes everything: SQLite reads
// "reason IN (@REASONS@)" as a comparison against an unbound named parameter,
// which is NULL, which a CHECK treats as satisfied.

func TestSchemaSQLSubstitutesEveryEnum(t *testing.T) {
	s := schemaSQL()
	if strings.Contains(s, reasonsToken) || strings.Contains(s, resolutionsToken) {
		t.Fatalf("schemaSQL left a placeholder unsubstituted:\n%s", s)
	}
	for _, r := range policy.Reasons() {
		if !strings.Contains(s, "'"+r+"'") {
			t.Errorf("policy reason %q is missing from the generated CHECK", r)
		}
	}
	for _, r := range SkipResolutions() {
		if !strings.Contains(s, "'"+r+"'") {
			t.Errorf("resolution %q is missing from the generated CHECK", r)
		}
	}
}

func TestSkippedAttachmentsCheckConstraints(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	insert := func(reason, resolvedAt, resolution any) error {
		_, err := db.sql.Exec(`
			INSERT INTO skipped_attachments (`+skippedColumns+`)
			VALUES ('gmail:work','msg-1','2.1','a.exe','a.exe',1,'application/octet-stream',
			        'exe','application/x-msdownload',?,'fb70e01d2e46670e',NULL,
			        '2026/08/07/n.md','2026-08-07','2026-08-07T12:00:00Z',?,?)`,
			reason, resolvedAt, resolution)
		return err
	}

	// A reason outside policy.Reasons() is refused by the database itself.
	if err := insert("because", nil, nil); err == nil {
		t.Fatal("the reason CHECK accepted a reason that is not a policy.Reason* constant")
	}
	// So is a resolution outside SkipResolutions().
	if err := insert(policy.ReasonOverSizeCap, "2026-08-09T12:00:00Z", "sorted-out"); err == nil {
		t.Fatal("the resolution CHECK accepted an unknown resolution")
	}
	// resolved_at and resolution must move together, in both directions.
	if err := insert(policy.ReasonOverSizeCap, "2026-08-09T12:00:00Z", nil); err == nil {
		t.Fatal("a resolved_at with no resolution was accepted")
	}
	if err := insert(policy.ReasonOverSizeCap, nil, SkipResolutionFetched); err == nil {
		t.Fatal("a resolution with no resolved_at was accepted")
	}
	// The valid shapes go in.
	if err := insert(policy.ReasonOverSizeCap, nil, nil); err != nil {
		t.Fatalf("an unresolved row was refused: %v", err)
	}
	if _, err := db.sql.Exec(`DELETE FROM skipped_attachments`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := insert(policy.ReasonOverSizeCap, "2026-08-09T12:00:00Z", SkipResolutionSourceGone); err != nil {
		t.Fatalf("a resolved row was refused: %v", err)
	}
}

// Every write validates a digest against policyDigestChars, which is taken
// from the DEFAULT policy at init. That is only sound if the width is a
// property of the digest rather than of one particular policy, so check it
// against policies that differ from the default in each way that matters.
func TestPolicyDigestWidthIsStableAcrossPolicies(t *testing.T) {
	settings := map[string]func(*policy.Settings){
		"svg allowed":        func(s *policy.Settings) { s.AllowSVG = true },
		"containers denied":  func(s *policy.Settings) { s.AllowContainers = false },
		"macro office":       func(s *policy.Settings) { s.AllowMacroOffice = true },
		"store-warn":         func(s *policy.Settings) { s.OnMismatch = policy.OnMismatchStoreWarn },
		"extension added":    func(s *policy.Settings) { s.AllowExtensions = []string{"7z=application/x-7z-compressed"} },
		"extension denied":   func(s *policy.Settings) { s.DenyExtensions = []string{"pdf"} },
		"caps raised":        func(s *policy.Settings) { s.MaxSize = 4 << 30 },
		"no caps at all":     func(s *policy.Settings) { *s = policy.Settings{} },
		"scan hook rejects":  func(s *policy.Settings) { s.ScanCommand, s.ScanAction = []string{"clamdscan"}, policy.ScanActionReject },
		"everything default": func(s *policy.Settings) {},
	}
	seen := map[string]string{}
	for name, mut := range settings {
		s := policy.DefaultSettings()
		mut(&s)
		p, err := policy.New(s)
		if err != nil {
			t.Fatalf("%s: policy.New: %v", name, err)
		}
		d := p.PolicyDigest()
		if len(d) != policyDigestChars {
			t.Errorf("%s: digest %q is %d chars; policyDigestChars is %d", name, d, len(d), policyDigestChars)
		}
		if !isLowerHex(d, policyDigestChars) {
			t.Errorf("%s: digest %q is not lowercase hex", name, d)
		}
		if prev, dup := seen[d]; dup && prev != name {
			t.Errorf("%s and %s produced the same digest %q", prev, name, d)
		}
		seen[d] = name
	}
}
