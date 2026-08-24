package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"save/internal/config"
	"save/internal/policy"
	"save/internal/state"
)

// statusFixture seeds a ledger, records digest as the meta digest, and
// returns the closed database's path ready for buildStatus.
func statusFixture(t *testing.T, digest string, seed func(*state.DB)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if digest != "" {
		if err := db.SetAttachmentPolicyDigest(digest); err != nil {
			t.Fatal(err)
		}
	}
	if seed != nil {
		seed(db)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestStatusDetectsPolicyChange is the startup half of the retro-fetch
// protocol: when the digest the config now produces differs from the one the
// archive was reconsidered under, status must say so, say how much of the
// backlog the new policy would take, and point at `save refetch` — WITHOUT
// fetching anything.
func TestStatusDetectsPolicyChange(t *testing.T) {
	dbPath := statusFixture(t, policy.Default().PolicyDigest(), func(db *state.DB) {
		contestedRows(t, db)
	})
	cfg := loadTestConfig(t, widenedConfig(t.TempDir()))

	rep, err := buildStatus(cfg, dbPath)
	if err != nil {
		t.Fatalf("buildStatus: %v", err)
	}
	ps := rep.AttachmentPolicy
	if ps == nil {
		t.Fatal("status carries no attachment policy block")
	}
	if !ps.Changed {
		t.Fatalf("policy change not detected: digest %s, recorded %s", ps.Digest, ps.RecordedDigest)
	}
	if ps.RecordedDigest != policy.Default().PolicyDigest() {
		t.Errorf("recorded digest = %q, want the default policy's", ps.RecordedDigest)
	}
	if ps.Digest == ps.RecordedDigest {
		t.Error("a widened config must produce a different digest")
	}
	if ps.Unresolved != 7 {
		t.Errorf("unresolved skips = %d, want 7", ps.Unresolved)
	}
	if ps.NowAccepted != 6 || ps.NowAcceptedBytes != 200_019_000 {
		t.Errorf("now accepted = %d (%d bytes), want 6 (200019000) — everything but the hard-denied .exe",
			ps.NowAccepted, ps.NowAcceptedBytes)
	}

	var out strings.Builder
	printStatus(&out, rep)
	text := out.String()
	for _, want := range []string{
		"policy CHANGED",
		policy.Default().PolicyDigest(),
		"6 (200.0 MB) would be accepted",
		"nothing is ever re-fetched automatically",
		"save refetch --dry-run",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status output is missing %q:\n%s", want, text)
		}
	}
}

// TestStatusPolicyUnchanged: the same config that produced the recorded
// digest must not nag. The transient run-budget row is still reported as
// acceptable, because it is — running again is all it needs.
func TestStatusPolicyUnchanged(t *testing.T) {
	cfg := loadTestConfig(t, plainConfig(t.TempDir()))
	pol := policyFor(t, cfg)
	dbPath := statusFixture(t, pol.PolicyDigest(), func(db *state.DB) {
		contestedRows(t, db)
	})

	rep, err := buildStatus(cfg, dbPath)
	if err != nil {
		t.Fatalf("buildStatus: %v", err)
	}
	if rep.AttachmentPolicy.Changed {
		t.Error("an unchanged config must not report a policy change")
	}
	if rep.AttachmentPolicy.NowAccepted != 1 {
		t.Errorf("now accepted = %d, want 1 (the transient over_run_budget row)", rep.AttachmentPolicy.NowAccepted)
	}

	var out strings.Builder
	printStatus(&out, rep)
	if text := out.String(); strings.Contains(text, "policy CHANGED") {
		t.Errorf("status claims the policy changed:\n%s", text)
	}
}

// TestStatusNoDigestRecordedYet: a fresh archive has nothing to compare
// against and must not pretend the policy changed.
func TestStatusNoDigestRecordedYet(t *testing.T) {
	dbPath := statusFixture(t, "", nil)
	cfg := loadTestConfig(t, plainConfig(t.TempDir()))

	rep, err := buildStatus(cfg, dbPath)
	if err != nil {
		t.Fatalf("buildStatus: %v", err)
	}
	if rep.AttachmentPolicy.Changed || rep.AttachmentPolicy.RecordedDigest != "" {
		t.Errorf("fresh archive reported %+v", rep.AttachmentPolicy)
	}
}

// TestStatusSkippedAttachmentsPerSource: the refused-attachment ledger is
// reported per account, split by reason, in bytes — and the bytes a later
// refetch actually recovered stop counting as "not stored", while the ones
// that are gone for good keep counting.
func TestStatusSkippedAttachmentsPerSource(t *testing.T) {
	cfgDir := t.TempDir()
	setTestEnv(t, cfgDir, t.TempDir())
	cfgPath := filepath.Join(cfgDir, "config.toml")
	writeFile(t, cfgPath, twoAccountConfig(t.TempDir(), filepath.Join(cfgDir, "google-client-personal.json")))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	dbPath := statusFixture(t, "", func(db *state.DB) {
		// gmail:work — two reasons, one of them since recovered.
		seedSkip(t, db, state.SkippedAttachment{
			Source: "gmail:work", StableID: "m1", PartKey: "1", OrigName: "a.exe",
			SniffedType: "application/vnd.microsoft.portable-executable",
			Reason:      policy.ReasonNotAllowlistedExtension, SizeBytes: 1_000, FirstSeenAt: at,
		})
		seedSkip(t, db, state.SkippedAttachment{
			Source: "gmail:work", StableID: "m2", PartKey: "1", OrigName: "b.svg",
			SniffedType: "image/svg+xml",
			Reason:      policy.ReasonSVGDenied, SizeBytes: 2_000, FirstSeenAt: at,
		})
		seedSkip(t, db, state.SkippedAttachment{
			Source: "gmail:work", StableID: "m3", PartKey: "1", OrigName: "c.svg",
			SniffedType: "image/svg+xml",
			Reason:      policy.ReasonSVGDenied, SizeBytes: 4_000, FirstSeenAt: at,
		})
		if err := db.MarkSkippedResolved("gmail:work", "m3", "1", state.SkipResolutionFetched); err != nil {
			t.Fatal(err)
		}
		// fastmail:fm — one over-size refusal, never recoverable upstream.
		seedSkip(t, db, state.SkippedAttachment{
			Source: "fastmail:fm", StableID: "e1", PartKey: "2", OrigName: "video.mp4",
			SniffedType: "video/mp4",
			Reason:      policy.ReasonOverSizeCap, SizeBytes: 90_000_000, FirstSeenAt: at,
		})
		if err := db.MarkSkippedResolved("fastmail:fm", "e1", "2", state.SkipResolutionSourceGone); err != nil {
			t.Fatal(err)
		}
	})

	rep, err := buildStatus(cfg, dbPath)
	if err != nil {
		t.Fatalf("buildStatus: %v", err)
	}
	byID := make(map[string]sourceStatus)
	for _, ss := range rep.Sources {
		byID[ss.Name] = ss
	}

	work := byID["gmail:work"].SkippedAttachments
	if work == nil {
		t.Fatal("gmail:work carries no skipped-attachment block")
	}
	if work.Total != 3 || work.Unresolved != 2 {
		t.Errorf("gmail:work = %d total / %d unresolved, want 3 / 2", work.Total, work.Unresolved)
	}
	// c.svg was fetched, so its 4000 bytes ARE in the archive now.
	if work.NotStoredBytes != 3_000 || work.UnresolvedBytes != 3_000 {
		t.Errorf("gmail:work bytes = %d not stored / %d unresolved, want 3000 / 3000",
			work.NotStoredBytes, work.UnresolvedBytes)
	}
	if work.ByReason[policy.ReasonSVGDenied] != 2 || work.UnresolvedByReason[policy.ReasonSVGDenied] != 1 {
		t.Errorf("gmail:work svg_denied = %d total / %d unresolved, want 2 / 1",
			work.ByReason[policy.ReasonSVGDenied], work.UnresolvedByReason[policy.ReasonSVGDenied])
	}

	fm := byID["fastmail:fm"].SkippedAttachments
	if fm == nil {
		t.Fatal("fastmail:fm carries no skipped-attachment block")
	}
	// source_gone is resolved but the bytes are still NOT in the archive.
	if fm.Unresolved != 0 || fm.NotStoredBytes != 90_000_000 || fm.UnresolvedBytes != 0 {
		t.Errorf("fastmail:fm = %d unresolved, %d not stored, %d unresolved bytes; want 0 / 90000000 / 0",
			fm.Unresolved, fm.NotStoredBytes, fm.UnresolvedBytes)
	}
	if byID["gchat:work"].SkippedAttachments != nil {
		t.Error("gchat:work has no skips and must carry no block (another account's must not leak)")
	}

	var out strings.Builder
	printStatus(&out, rep)
	text := out.String()
	for _, want := range []string{
		"refused:      3 attachment(s) by the attachment policy, 3.0 kB not stored (2 unresolved, 3.0 kB)",
		"not_allowlisted_extension    1 (1 unresolved)",
		"svg_denied                   2 (1 unresolved)",
		"refused:      1 attachment(s) by the attachment policy, 90.0 MB not stored (0 unresolved, 0 B)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status output is missing %q:\n%s", want, text)
		}
	}
}

// TestStatusListsUnconfiguredSkipLedger: a ledger row is enough to make an
// instance "known", so a renamed label cannot hide a backlog of refused
// attachments that nothing will ever fetch again.
func TestStatusListsUnconfiguredSkipLedger(t *testing.T) {
	dbPath := statusFixture(t, "", func(db *state.DB) {
		seedSkip(t, db, state.SkippedAttachment{
			Source: "gmail:oldname", StableID: "m1", OrigName: "a.svg",
			SniffedType: "image/svg+xml", Reason: policy.ReasonSVGDenied, SizeBytes: 10,
		})
	})
	cfg := loadTestConfig(t, plainConfig(t.TempDir()))

	rep, err := buildStatus(cfg, dbPath)
	if err != nil {
		t.Fatalf("buildStatus: %v", err)
	}
	if strings.Join(rep.UnconfiguredSources, ",") != "gmail:oldname" {
		t.Errorf("unconfigured sources = %v, want [gmail:oldname]", rep.UnconfiguredSources)
	}
}
