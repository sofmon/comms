package state

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"comms/internal/policy"
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

// TestSchemaV2SubstitutesEveryEnum: the triage step's CHECK lists are built
// from the Go constants, like v1's, and every constant must be in them.
func TestSchemaV2SubstitutesEveryEnum(t *testing.T) {
	s := schemaV2SQL()
	if strings.Contains(s, "@") {
		t.Fatalf("schemaV2SQL contains an unsubstituted placeholder:\n%s", s)
	}
	for _, d := range Dispositions() {
		if !strings.Contains(s, "'"+string(d)+"'") {
			t.Errorf("disposition %q is missing from the generated CHECK", d)
		}
	}
	for _, v := range TriageVerdicts() {
		if !strings.Contains(s, "'"+string(v)+"'") {
			t.Errorf("verdict %q is missing from the generated CHECK", v)
		}
	}
	for _, l := range TriageLayers() {
		if !strings.Contains(s, "'"+l+"'") {
			t.Errorf("layer %q is missing from the generated CHECK", l)
		}
	}
}

// TestMigrateV1ToV2 opens a database created by the v1 code — the base DDL
// alone, user_version 1, one archived message — and checks that Open
// upgrades it in place: the row is still there, reads as archive with no
// triage history, and the new ledger is usable. Then it opens the file again
// to prove the upgrade is a no-op the second time.
func TestMigrateV1ToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schemaSQL()); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		INSERT INTO messages (source, stable_id, ts_utc, day_bucket, rel_path, content_hash, archived_at)
		VALUES ('gmail:work', 'old-1', '2026-08-07T12:00:00.000000000Z', '2026-08-07', '2026/08/07/old.md', 'h', '2026-08-07T12:00:01Z')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	for round := 1; round <= 2; round++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("round %d: Open: %v", round, err)
		}
		var v int
		if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != schemaVersion {
			t.Fatalf("round %d: user_version = %d, %v; want %d", round, v, err, schemaVersion)
		}
		m, ok, err := db.GetMessage("gmail:work", "old-1")
		if err != nil || !ok {
			t.Fatalf("round %d: the v1 row is gone: ok %v, err %v", round, ok, err)
		}
		if m.Disposition != DispositionArchive || m.TriageDigest != "" || m.DispositionRule != "" {
			t.Errorf("round %d: v1 row reads as %+v; want archive with no triage history", round, m)
		}
		if err := db.UpsertTriageDecision(TriageDecision{
			Source: "gmail:work", StableID: "old-1", Verdict: TriageSignal,
			Layer: TriageLayerProtect, Rule: "own-address", Reason: "sent by the account itself", Digest: "d",
		}); err != nil {
			t.Errorf("round %d: the ledger is unusable: %v", round, err)
		}
		// The disposition CHECK is live on the upgraded table.
		if _, err := db.sql.Exec(`UPDATE messages SET disposition = 'trash' WHERE stable_id = 'old-1'`); err == nil {
			t.Errorf("round %d: the disposition CHECK accepted an unknown value", round)
		}
		db.Close()
	}
}

// TestOpenRefusesANewerSchema: a database written by a later build must not
// be silently reinterpreted by this one.
func TestOpenRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a database from the future")
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
