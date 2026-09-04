package state_test

import (
	"strings"
	"testing"
	"time"

	"comms/internal/policy"
	"comms/internal/state"
)

var (
	skipInstA = state.InstanceID(state.SourceGmail, "work")
	skipInstB = state.InstanceID(state.SourceGmail, "personal")
	skipChat  = state.InstanceID(state.SourceGChat, "work")

	defaultDigest = policy.Default().PolicyDigest()

	hashA = strings.Repeat("a", 64)
	hashB = strings.Repeat("b", 64)
)

// firstSeen is a fixed timestamp so first_seen_at preservation is checkable.
var firstSeen = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

// skipRow is a complete, valid refusal: a Windows payload on a Gmail message.
func skipRow(source, stableID, partKey string) state.SkippedAttachment {
	return state.SkippedAttachment{
		Source:        source,
		StableID:      stableID,
		PartKey:       partKey,
		OrigName:      "invoice.pdf.exe",
		SanitizedName: "invoice.pdf.exe",
		SizeBytes:     4096,
		DeclaredType:  "application/octet-stream",
		DeclaredExt:   "exe",
		SniffedType:   "application/vnd.microsoft.portable-executable",
		Reason:        policy.ReasonNotAllowlistedExtension,
		PolicyDigest:  defaultDigest,
		ContentSHA256: hashA,
		NoteRelPath:   "2026/08/07/1200-acme-invoice.md",
		DayBucket:     "2026-08-07",
		FirstSeenAt:   firstSeen,
	}
}

func mustUpsertSkipped(t *testing.T, db *state.DB, s state.SkippedAttachment) {
	t.Helper()
	if err := db.UpsertSkipped(s); err != nil {
		t.Fatalf("UpsertSkipped(%s/%s/%s): %v", s.Source, s.StableID, s.PartKey, err)
	}
}

// onlyRow fails unless the ledger holds exactly one unresolved row.
func onlyRow(t *testing.T, db *state.DB) state.SkippedAttachment {
	t.Helper()
	rows, err := db.ListUnresolvedSkipped(0)
	if err != nil {
		t.Fatalf("ListUnresolvedSkipped: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListUnresolvedSkipped = %d rows; want 1", len(rows))
	}
	return rows[0]
}

func TestUpsertSkippedDedupsOnPrimaryKey(t *testing.T) {
	db := openTest(t)

	s := skipRow(skipInstA, "msg-1", "2.1")
	mustUpsertSkipped(t, db, s)
	mustUpsertSkipped(t, db, s) // exact replay

	got := onlyRow(t, db)
	if got.Reason != policy.ReasonNotAllowlistedExtension || got.SizeBytes != 4096 ||
		got.ContentSHA256 != hashA || got.SniffedType != "application/vnd.microsoft.portable-executable" {
		t.Fatalf("replayed row = %+v", got)
	}
	if !got.FirstSeenAt.Equal(firstSeen) {
		t.Fatalf("FirstSeenAt = %v; want %v", got.FirstSeenAt, firstSeen)
	}
	if got.Resolved() || !got.ResolvedAt.IsZero() {
		t.Fatalf("fresh row is resolved: %+v", got)
	}

	// A re-decision under a widened policy updates the mutable columns but
	// keeps the moment the attachment was FIRST refused.
	later := s
	later.FirstSeenAt = firstSeen.Add(48 * time.Hour)
	later.Reason = policy.ReasonOverSizeCap
	later.SizeBytes = 90 * 1000 * 1000
	later.SniffedType = "application/pdf"
	later.PolicyDigest = widenedDigest(t)
	later.ContentSHA256 = hashB
	later.NoteRelPath = "2026/08/07/1200-acme-invoice-renamed.md"
	later.DayBucket = "2026-08-08"
	mustUpsertSkipped(t, db, later)

	got = onlyRow(t, db)
	if !got.FirstSeenAt.Equal(firstSeen) {
		t.Fatalf("FirstSeenAt moved to %v; want the original %v", got.FirstSeenAt, firstSeen)
	}
	if got.Reason != policy.ReasonOverSizeCap || got.SizeBytes != 90*1000*1000 ||
		got.SniffedType != "application/pdf" || got.ContentSHA256 != hashB ||
		got.PolicyDigest != widenedDigest(t) ||
		got.NoteRelPath != "2026/08/07/1200-acme-invoice-renamed.md" || got.DayBucket != "2026-08-08" {
		t.Fatalf("re-decided row did not update: %+v", got)
	}
}

// A pre-download refusal has no bytes to hash. Re-recording one must not
// erase a hash an earlier, post-download refusal captured.
func TestUpsertSkippedNeverErasesAKnownHash(t *testing.T) {
	db := openTest(t)

	s := skipRow(skipInstA, "msg-1", "2.1")
	mustUpsertSkipped(t, db, s)

	noHash := s
	noHash.ContentSHA256 = ""
	mustUpsertSkipped(t, db, noHash)

	if got := onlyRow(t, db); got.ContentSHA256 != hashA {
		t.Fatalf("ContentSHA256 = %q; want the previously recorded %q", got.ContentSHA256, hashA)
	}
}

// A Chat blob refused by PreCheck was never fetched, so it legitimately has
// no hash at all — the column is nullable and "" round-trips as "".
func TestSkippedChatBlobHasNoContentHash(t *testing.T) {
	db := openTest(t)

	s := skipRow(skipChat, "spaces/AAA/messages/m1", "attachment-1")
	s.ContentSHA256 = ""
	s.SniffedType = "" // nothing was fetched, so nothing was sniffed
	s.Reason = policy.ReasonOverSizeCap
	s.NoteRelPath = "2026/08/07/chat-gchat-work-team.md"
	mustUpsertSkipped(t, db, s)

	got := onlyRow(t, db)
	if got.ContentSHA256 != "" || got.SniffedType != "" {
		t.Fatalf("chat pre-check row = %+v; want empty hash and sniffed type", got)
	}
}

func TestUnresolvedListingExcludesResolvedRows(t *testing.T) {
	db := openTest(t)

	for i, part := range []string{"1", "2", "3"} {
		s := skipRow(skipInstA, "msg-1", part)
		s.FirstSeenAt = firstSeen.Add(time.Duration(i) * time.Minute)
		mustUpsertSkipped(t, db, s)
	}

	if rows, _ := db.ListUnresolvedSkipped(0); len(rows) != 3 {
		t.Fatalf("initial unresolved = %d; want 3", len(rows))
	}
	// Ordered by first_seen_at within an instance.
	rows, _ := db.ListUnresolvedSkipped(0)
	if rows[0].PartKey != "1" || rows[2].PartKey != "3" {
		t.Fatalf("unresolved order = %q, %q, %q", rows[0].PartKey, rows[1].PartKey, rows[2].PartKey)
	}
	// limit is honored; limit <= 0 means no limit.
	if rows, _ := db.ListUnresolvedSkipped(2); len(rows) != 2 {
		t.Fatalf("ListUnresolvedSkipped(2) returned %d rows", len(rows))
	}

	// Every terminal resolution drops the row out of the refetch queue.
	for part, res := range map[string]string{
		"1": state.SkipResolutionFetched,
		"2": state.SkipResolutionSourceGone,
		"3": state.SkipResolutionStillDenied,
	} {
		if err := db.MarkSkippedResolved(skipInstA, "msg-1", part, res); err != nil {
			t.Fatalf("MarkSkippedResolved(%s, %s): %v", part, res, err)
		}
	}
	if rows, _ := db.ListUnresolvedSkipped(0); len(rows) != 0 {
		t.Fatalf("unresolved after resolving all = %d; want 0", len(rows))
	}

	// The rows themselves survive, carrying their resolution.
	all, err := db.SkippedForNote("2026/08/07/1200-acme-invoice.md")
	if err != nil {
		t.Fatalf("SkippedForNote: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("SkippedForNote = %d rows; want 3", len(all))
	}
	for _, r := range all {
		if !r.Resolved() || r.ResolvedAt.IsZero() {
			t.Fatalf("resolved row lost its resolution: %+v", r)
		}
	}

	// Recording the skip again means it is refused RIGHT NOW: the row re-opens.
	mustUpsertSkipped(t, db, skipRow(skipInstA, "msg-1", "2"))
	reopened, _ := db.ListUnresolvedSkipped(0)
	if len(reopened) != 1 || reopened[0].PartKey != "2" {
		t.Fatalf("re-recorded skip did not re-open: %+v", reopened)
	}
	if reopened[0].Resolved() || !reopened[0].ResolvedAt.IsZero() {
		t.Fatalf("re-opened row still carries a resolution: %+v", reopened[0])
	}
}

func TestMarkSkippedResolvedRejectsBadInput(t *testing.T) {
	db := openTest(t)
	mustUpsertSkipped(t, db, skipRow(skipInstA, "msg-1", "2.1"))

	if err := db.MarkSkippedResolved(skipInstA, "msg-1", "2.1", "sorted-out"); err == nil {
		t.Fatal("MarkSkippedResolved accepted an invalid resolution")
	}
	if err := db.MarkSkippedResolved(skipInstA, "msg-1", "nope", state.SkipResolutionFetched); err == nil {
		t.Fatal("MarkSkippedResolved accepted an unknown row")
	}
	// The valid row is untouched by either failure.
	if rows, _ := db.ListUnresolvedSkipped(0); len(rows) != 1 {
		t.Fatalf("unresolved = %d; want the untouched 1", len(rows))
	}
}

func TestCountSkippedPerSourceAndReason(t *testing.T) {
	db := openTest(t)

	add := func(source, stableID, part, reason string) {
		s := skipRow(source, stableID, part)
		s.Reason = reason
		mustUpsertSkipped(t, db, s)
	}
	add(skipInstA, "msg-1", "1", policy.ReasonNotAllowlistedExtension)
	add(skipInstA, "msg-1", "2", policy.ReasonNotAllowlistedExtension)
	add(skipInstA, "msg-2", "1", policy.ReasonOverSizeCap)
	add(skipInstB, "msg-9", "1", policy.ReasonMacroOffice)

	// Resolving one must move it out of the unresolved tallies but keep it in
	// the totals: the archive did refuse it once, and the note says so.
	if err := db.MarkSkippedResolved(skipInstA, "msg-1", "2", state.SkipResolutionFetched); err != nil {
		t.Fatalf("MarkSkippedResolved: %v", err)
	}

	counts, err := db.CountSkipped()
	if err != nil {
		t.Fatalf("CountSkipped: %v", err)
	}
	if len(counts) != 2 {
		t.Fatalf("CountSkipped covered %d instances; want 2 (%v)", len(counts), counts)
	}

	a := counts[skipInstA]
	if a.Total != 3 || a.Unresolved != 2 {
		t.Fatalf("%s totals = %d/%d; want 3 total, 2 unresolved", skipInstA, a.Total, a.Unresolved)
	}
	if a.ByReason[policy.ReasonNotAllowlistedExtension] != 2 || a.ByReason[policy.ReasonOverSizeCap] != 1 {
		t.Fatalf("%s ByReason = %v", skipInstA, a.ByReason)
	}
	if a.UnresolvedByReason[policy.ReasonNotAllowlistedExtension] != 1 ||
		a.UnresolvedByReason[policy.ReasonOverSizeCap] != 1 {
		t.Fatalf("%s UnresolvedByReason = %v", skipInstA, a.UnresolvedByReason)
	}
	if _, ok := a.ByReason[policy.ReasonMacroOffice]; ok {
		t.Fatalf("%s ByReason leaked the other account's reason: %v", skipInstA, a.ByReason)
	}

	b := counts[skipInstB]
	if b.Total != 1 || b.Unresolved != 1 || b.ByReason[policy.ReasonMacroOffice] != 1 {
		t.Fatalf("%s counts = %+v", skipInstB, b)
	}
}

// Two accounts can legitimately see the SAME logical attachment — the same
// message id and part key — and each keeps its own row, its own resolution
// and its own note.
func TestSkippedRowsAreAccountScoped(t *testing.T) {
	db := openTest(t)

	a := skipRow(skipInstA, "msg-shared", "2.1")
	a.NoteRelPath = "2026/08/07/1200-acme-gmail-work.md"
	b := skipRow(skipInstB, "msg-shared", "2.1")
	b.NoteRelPath = "2026/08/07/1200-acme-gmail-personal.md"
	mustUpsertSkipped(t, db, a)
	mustUpsertSkipped(t, db, b)

	rows, err := db.ListUnresolvedSkipped(0)
	if err != nil {
		t.Fatalf("ListUnresolvedSkipped: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("two accounts skipping the same attachment produced %d rows; want 2", len(rows))
	}
	if rows[0].Source == rows[1].Source {
		t.Fatalf("rows collapsed onto one instance: %+v", rows)
	}

	// Resolving one account's row leaves the other's alone.
	if err := db.MarkSkippedResolved(skipInstA, "msg-shared", "2.1", state.SkipResolutionFetched); err != nil {
		t.Fatalf("MarkSkippedResolved: %v", err)
	}
	left, _ := db.ListUnresolvedSkipped(0)
	if len(left) != 1 || left[0].Source != skipInstB {
		t.Fatalf("after resolving %s, unresolved = %+v", skipInstA, left)
	}

	// And each note only ever restates its own account's skip.
	forA, _ := db.SkippedForNote("2026/08/07/1200-acme-gmail-work.md")
	if len(forA) != 1 || forA[0].Source != skipInstA {
		t.Fatalf("SkippedForNote(A) = %+v", forA)
	}
}

func TestSkippedForNote(t *testing.T) {
	db := openTest(t)

	note := "2026/08/07/1200-acme-invoice.md"
	for _, part := range []string{"3", "1", "2"} {
		mustUpsertSkipped(t, db, skipRow(skipInstA, "msg-1", part))
	}
	other := skipRow(skipInstA, "msg-2", "1")
	other.NoteRelPath = "2026/08/07/1300-other.md"
	mustUpsertSkipped(t, db, other)

	rows, err := db.SkippedForNote(note)
	if err != nil {
		t.Fatalf("SkippedForNote: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("SkippedForNote = %d rows; want 3", len(rows))
	}
	for i, want := range []string{"1", "2", "3"} {
		if rows[i].PartKey != want {
			t.Fatalf("row %d part key = %q; want %q (must be part-key ordered)", i, rows[i].PartKey, want)
		}
	}
	if rows, _ := db.SkippedForNote("2026/08/07/nothing-here.md"); len(rows) != 0 {
		t.Fatalf("SkippedForNote on an unknown note = %d rows; want 0", len(rows))
	}
}

// Every reason the policy engine can produce must be storable: the schema's
// CHECK constraint is generated from policy.Reasons(), and this is the test
// that fails if the two ever drift.
func TestEveryPolicyReasonIsStorable(t *testing.T) {
	db := openTest(t)

	for i, reason := range policy.Reasons() {
		s := skipRow(skipInstA, "msg-1", string(rune('a'+i)))
		s.Reason = reason
		if err := db.UpsertSkipped(s); err != nil {
			t.Fatalf("UpsertSkipped(reason=%q): %v", reason, err)
		}
	}
	rows, _ := db.ListUnresolvedSkipped(0)
	if len(rows) != len(policy.Reasons()) {
		t.Fatalf("stored %d rows for %d reasons", len(rows), len(policy.Reasons()))
	}

	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Reason] = true
	}
	for _, reason := range policy.Reasons() {
		if !seen[reason] {
			t.Fatalf("reason %q did not round-trip", reason)
		}
	}
}

func TestUpsertSkippedValidation(t *testing.T) {
	db := openTest(t)

	cases := []struct {
		name  string
		mutfn func(*state.SkippedAttachment)
	}{
		{"bare kind as source", func(s *state.SkippedAttachment) { s.Source = state.SourceGmail }},
		{"empty source", func(s *state.SkippedAttachment) { s.Source = "" }},
		{"empty stable id", func(s *state.SkippedAttachment) { s.StableID = "" }},
		{"empty part key", func(s *state.SkippedAttachment) { s.PartKey = "" }},
		{"empty note path", func(s *state.SkippedAttachment) { s.NoteRelPath = "" }},
		{"empty day bucket", func(s *state.SkippedAttachment) { s.DayBucket = "" }},
		{"empty reason", func(s *state.SkippedAttachment) { s.Reason = policy.ReasonNone }},
		{"unknown reason", func(s *state.SkippedAttachment) { s.Reason = "because" }},
		{"empty digest", func(s *state.SkippedAttachment) { s.PolicyDigest = "" }},
		{"short digest", func(s *state.SkippedAttachment) { s.PolicyDigest = "fb70e01d" }},
		{"upper-case digest", func(s *state.SkippedAttachment) { s.PolicyDigest = strings.ToUpper(defaultDigest) }},
		{"canonical text as digest", func(s *state.SkippedAttachment) { s.PolicyDigest = policy.Default().Canonical() }},
		{"short content hash", func(s *state.SkippedAttachment) { s.ContentSHA256 = "abcd" }},
		{"upper-case content hash", func(s *state.SkippedAttachment) { s.ContentSHA256 = strings.ToUpper(hashA) }},
		{"negative size", func(s *state.SkippedAttachment) { s.SizeBytes = -1 }},
	}
	for _, tc := range cases {
		s := skipRow(skipInstA, "msg-1", "2.1")
		tc.mutfn(&s)
		if err := db.UpsertSkipped(s); err == nil {
			t.Errorf("UpsertSkipped accepted %s", tc.name)
		}
	}
	if rows, _ := db.ListUnresolvedSkipped(0); len(rows) != 0 {
		t.Fatalf("rejected rows still landed: %+v", rows)
	}

	// A 0-byte attachment and a nameless inline part are both legitimate.
	ok := skipRow(skipInstA, "msg-1", "2.1")
	ok.SizeBytes = 0
	ok.OrigName, ok.SanitizedName, ok.DeclaredExt = "", "", ""
	if err := db.UpsertSkipped(ok); err != nil {
		t.Fatalf("UpsertSkipped rejected a legitimate nameless 0-byte part: %v", err)
	}
}

func TestUpsertSkippedBatchIsAllOrNothing(t *testing.T) {
	db := openTest(t)

	good1 := skipRow(skipInstA, "msg-1", "1")
	good2 := skipRow(skipInstA, "msg-1", "2")
	bad := skipRow(skipInstA, "msg-1", "3")
	bad.Reason = "because"

	if err := db.UpsertSkippedBatch([]state.SkippedAttachment{good1, bad, good2}); err == nil {
		t.Fatal("UpsertSkippedBatch accepted an invalid row")
	}
	if rows, _ := db.ListUnresolvedSkipped(0); len(rows) != 0 {
		t.Fatalf("a rejected batch wrote %d rows; want none", len(rows))
	}

	if err := db.UpsertSkippedBatch(nil); err != nil {
		t.Fatalf("UpsertSkippedBatch(nil): %v", err)
	}
	if err := db.UpsertSkippedBatch([]state.SkippedAttachment{good1, good2}); err != nil {
		t.Fatalf("UpsertSkippedBatch: %v", err)
	}
	if rows, _ := db.ListUnresolvedSkipped(0); len(rows) != 2 {
		t.Fatalf("batch wrote %d rows; want 2", len(rows))
	}
}

func TestAttachmentPolicyDigestRoundTrip(t *testing.T) {
	db := openTest(t)

	if got, ok, err := db.AttachmentPolicyDigest(); err != nil || ok || got != "" {
		t.Fatalf("AttachmentPolicyDigest on a fresh archive = %q, %v, %v; want \"\", false, nil", got, ok, err)
	}

	if err := db.SetAttachmentPolicyDigest(defaultDigest); err != nil {
		t.Fatalf("SetAttachmentPolicyDigest: %v", err)
	}
	got, ok, err := db.AttachmentPolicyDigest()
	if err != nil || !ok || got != defaultDigest {
		t.Fatalf("AttachmentPolicyDigest = %q, %v, %v; want %q, true, nil", got, ok, err, defaultDigest)
	}

	// Widening the policy changes the digest, which is the signal `comms
	// status` uses to offer a refetch.
	wide := widenedDigest(t)
	if wide == defaultDigest {
		t.Fatal("widening the policy did not change the digest")
	}
	if err := db.SetAttachmentPolicyDigest(wide); err != nil {
		t.Fatalf("SetAttachmentPolicyDigest(widened): %v", err)
	}
	if got, _, _ := db.AttachmentPolicyDigest(); got != wide {
		t.Fatalf("after widening: got %q; want %q", got, wide)
	}

	// The stored digest is also reachable through the generic meta accessor,
	// under the documented key.
	if got, ok, _ := db.GetMeta(state.MetaAttachmentPolicyDigest); !ok || got != wide {
		t.Fatalf("GetMeta(%s) = %q, %v", state.MetaAttachmentPolicyDigest, got, ok)
	}

	// Junk is refused rather than poisoning the comparison forever.
	for _, bad := range []string{"", "not-hex-at-all!", "fb70e01d", strings.ToUpper(defaultDigest),
		defaultDigest + "00", policy.Default().Canonical()} {
		if err := db.SetAttachmentPolicyDigest(bad); err == nil {
			t.Errorf("SetAttachmentPolicyDigest(%q) was accepted", bad)
		}
	}
	if got, _, _ := db.AttachmentPolicyDigest(); got != wide {
		t.Fatalf("a rejected digest overwrote the stored one: %q", got)
	}
}

func TestSkipResolutionHelpers(t *testing.T) {
	for _, r := range state.SkipResolutions() {
		if !state.ValidSkipResolution(r) {
			t.Fatalf("ValidSkipResolution(%q) = false", r)
		}
	}
	for _, bad := range []string{"", "done", "FETCHED", "skipped"} {
		if state.ValidSkipResolution(bad) {
			t.Fatalf("ValidSkipResolution(%q) = true", bad)
		}
	}
}

// widenedDigest is the digest of a policy that differs from the default by
// exactly one flag.
func widenedDigest(t *testing.T) string {
	t.Helper()
	s := policy.DefaultSettings()
	s.AllowSVG = true
	p, err := policy.New(s)
	if err != nil {
		t.Fatalf("policy.New: %v", err)
	}
	return p.PolicyDigest()
}
