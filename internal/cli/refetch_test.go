package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"save/internal/archive"
	"save/internal/config"
	"save/internal/policy"
	"save/internal/state"
)

// --- fixtures ---------------------------------------------------------------

// seedSkip records one refused attachment, filling in the fields a test does
// not care about with valid values.
func seedSkip(t *testing.T, db *state.DB, row state.SkippedAttachment) state.SkippedAttachment {
	t.Helper()
	if row.SanitizedName == "" {
		row.SanitizedName = row.OrigName
	}
	if row.DeclaredExt == "" {
		row.DeclaredExt = policy.NormalizeExt(row.SanitizedName)
	}
	if row.DeclaredType == "" {
		row.DeclaredType = "application/octet-stream"
	}
	if row.PolicyDigest == "" {
		row.PolicyDigest = policy.Default().PolicyDigest()
	}
	if row.PartKey == "" {
		row.PartKey = "1"
	}
	if row.NoteRelPath == "" {
		row.NoteRelPath = "2026/08/07/" + state.Tag(row.Source) + "_" + row.StableID + ".md"
	}
	if row.DayBucket == "" {
		row.DayBucket = "2026-08-07"
	}
	if err := db.UpsertSkipped(row); err != nil {
		t.Fatalf("seed skip %s/%s/%s: %v", row.Source, row.StableID, row.PartKey, err)
	}
	return row
}

// skipDB opens a fresh state database for ledger tests.
func skipDB(t *testing.T) (*state.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// contestedRows is the set of refusals the contested defaults produce, one of
// each kind, all on one Gmail instance. Every one of them is recoverable by a
// specific config change — except payload.exe, which is recoverable by none.
func contestedRows(t *testing.T, db *state.DB) {
	t.Helper()
	base := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	for i, r := range []state.SkippedAttachment{
		{StableID: "m1", OrigName: "backup.7z", SniffedType: "application/x-7z-compressed",
			Reason: policy.ReasonNotAllowlistedExtension, SizeBytes: 1_000},
		{StableID: "m2", OrigName: "logo.svg", SniffedType: "image/svg+xml",
			Reason: policy.ReasonSVGDenied, SizeBytes: 2_000},
		{StableID: "m3", OrigName: "huge.pdf", SniffedType: "application/pdf",
			Reason: policy.ReasonOverSizeCap, SizeBytes: 200_000_000},
		{StableID: "m4", OrigName: "invoice.pdf", SniffedType: "application/zip",
			Reason: policy.ReasonExtensionContentMismatch, SizeBytes: 4_000},
		{StableID: "m5", OrigName: "budget.xlsm", SniffedType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			Reason: policy.ReasonMacroOffice, SizeBytes: 5_000},
		{StableID: "m6", OrigName: "payload.exe", SniffedType: "application/vnd.microsoft.portable-executable",
			Reason: policy.ReasonNotAllowlistedExtension, SizeBytes: 6_000},
		{StableID: "m7", OrigName: "late.pdf", SniffedType: "application/pdf",
			Reason: policy.ReasonOverRunBudget, SizeBytes: 7_000},
	} {
		r.Source = "gmail:work"
		r.FirstSeenAt = base.Add(time.Duration(i) * time.Minute)
		seedSkip(t, db, r)
	}
}

// widenedConfig turns on every contested flag at once and raises the caps, so
// one config recovers every recoverable row in contestedRows.
func widenedConfig(root string) string {
	return fmt.Sprintf(`archive_root = %q
timezone = "UTC"

[attachments]
max_size         = "500MB"
max_per_message  = "1GB"
allow_extensions = ["7z"]
allow_svg        = true
allow_macro_office = true
on_mismatch      = "store-warn"

[[google]]
label   = "work"
account = "you@example.com"
gmail   = true
chat    = true
`, root)
}

func plainConfig(root string) string {
	return fmt.Sprintf(`archive_root = %q
timezone = "UTC"

[[google]]
label   = "work"
account = "you@example.com"
gmail   = true
chat    = true
`, root)
}

func policyFor(t *testing.T, cfg *config.Config) *policy.Policy {
	t.Helper()
	p, err := cfg.Policy()
	if err != nil {
		t.Fatalf("cfg.Policy: %v", err)
	}
	return p
}

// --- selection --------------------------------------------------------------

// TestRefetchSelectionUnderDefaultPolicy: the shipped defaults must not
// re-fetch anything they refused on purpose. The one exception is the
// transient run-budget refusal, which describes a finished sync pass rather
// than a property of the attachment, and is therefore always worth retrying.
func TestRefetchSelectionUnderDefaultPolicy(t *testing.T) {
	db, _ := skipDB(t)
	contestedRows(t, db)
	cfg := loadTestConfig(t, plainConfig(t.TempDir()))

	rows, err := db.ListUnresolvedSkipped(0)
	if err != nil {
		t.Fatal(err)
	}
	plan := planRefetch(policyFor(t, cfg), rows, cfg, cfg.Instances(), "")

	if plan.Considered != 7 {
		t.Fatalf("considered %d rows, want 7", plan.Considered)
	}
	if plan.Candidates != 1 || plan.Bytes != 7_000 {
		t.Fatalf("candidates = %d (%d bytes), want exactly the over_run_budget row (1, 7000)", plan.Candidates, plan.Bytes)
	}
	if got := plan.Groups[0].StableID; got != "m7" {
		t.Errorf("selected message = %q, want m7 (the transient run-budget refusal)", got)
	}
	denied := plan.StillDenied["gmail:work"]
	for reason, want := range map[string]int{
		policy.ReasonNotAllowlistedExtension:  2, // backup.7z and payload.exe
		policy.ReasonSVGDenied:                1,
		policy.ReasonOverSizeCap:              1,
		policy.ReasonExtensionContentMismatch: 1,
		policy.ReasonMacroOffice:              1,
	} {
		if denied[reason] != want {
			t.Errorf("still denied %s = %d, want %d (all: %v)", reason, denied[reason], want, denied)
		}
	}
}

// TestRefetchSelectionUnderWidenedPolicy: every contested default is
// reversible by the documented one-line config change — and the hard-denied
// executable types are reversible by nothing at all.
func TestRefetchSelectionUnderWidenedPolicy(t *testing.T) {
	db, _ := skipDB(t)
	contestedRows(t, db)
	cfg := loadTestConfig(t, widenedConfig(t.TempDir()))

	rows, err := db.ListUnresolvedSkipped(0)
	if err != nil {
		t.Fatal(err)
	}
	plan := planRefetch(policyFor(t, cfg), rows, cfg, cfg.Instances(), "")

	var got []string
	for _, g := range plan.Groups {
		got = append(got, g.StableID)
	}
	want := "m1,m2,m3,m4,m5,m7" // everything except payload.exe
	if strings.Join(got, ",") != want {
		t.Fatalf("selected %v, want %v", got, want)
	}
	if plan.Candidates != 6 {
		t.Errorf("candidates = %d, want 6", plan.Candidates)
	}
	if plan.Bytes != 1_000+2_000+200_000_000+4_000+5_000+7_000 {
		t.Errorf("plan bytes = %d", plan.Bytes)
	}
	if n := plan.StillDenied["gmail:work"][policy.ReasonNotAllowlistedExtension]; n != 1 {
		t.Errorf("still denied not_allowlisted_extension = %d, want 1 (payload.exe can never be allowed)", n)
	}

	// The summary must state the byte total before anything is fetched: that
	// number is the entire reason refetch is explicit.
	var out strings.Builder
	printRefetchPlan(&out, plan)
	if !strings.Contains(out.String(), "WOULD FETCH: 6 attachment(s)") || !strings.Contains(out.String(), "200.0 MB") {
		t.Errorf("plan summary does not state the count and byte total:\n%s", out.String())
	}
}

// TestRefetchHardDenyIsNeverSelected: no configuration can select a
// macOS-executable or Windows-payload extension, so a refetch can never be
// talked into pulling one in.
func TestRefetchHardDenyIsNeverSelected(t *testing.T) {
	p := policyFor(t, loadTestConfig(t, widenedConfig(t.TempDir())))
	for _, ext := range policy.HardDeniedExtensions() {
		row := state.SkippedAttachment{
			Source: "gmail:work", StableID: "m", PartKey: "1",
			SanitizedName: "thing." + ext, SizeBytes: 10,
			Reason: policy.ReasonNotAllowlistedExtension,
		}
		if fetch, _, _ := reconsider(p, row); fetch {
			t.Errorf("reconsider selected hard-denied .%s", ext)
		}
	}
}

// TestRefetchExtensionlessRowIsSelected: a part with no filename cannot be
// re-decided offline (the extension has to be derived from bytes nobody has),
// so selection must err toward fetching and let the authoritative decision
// over the real content settle it.
func TestRefetchExtensionlessRowIsSelected(t *testing.T) {
	p := policyFor(t, loadTestConfig(t, plainConfig(t.TempDir())))
	row := state.SkippedAttachment{
		Source: "gmail:work", StableID: "m", PartKey: "2.1",
		SanitizedName: "", SizeBytes: 900,
		SniffedType: "application/octet-stream",
		Reason:      policy.ReasonNotAllowlistedContent,
	}
	if fetch, why, _ := reconsider(p, row); !fetch {
		t.Errorf("extensionless row was rejected offline as %q; it must be fetched and re-decided over its bytes", why)
	}
}

// TestRefetchGroupsByMessage: several refused parts of one message are one
// fetch, because the connector pays for the whole raw message either way and
// re-renders its note once.
func TestRefetchGroupsByMessage(t *testing.T) {
	db, _ := skipDB(t)
	at := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	for _, part := range []string{"2", "10", "3"} {
		seedSkip(t, db, state.SkippedAttachment{
			Source: "gmail:work", StableID: "m1", PartKey: part,
			OrigName: "late.pdf", SniffedType: "application/pdf",
			Reason: policy.ReasonOverRunBudget, SizeBytes: 100, FirstSeenAt: at,
		})
	}
	seedSkip(t, db, state.SkippedAttachment{
		Source: "gmail:work", StableID: "m2", PartKey: "1",
		OrigName: "other.pdf", SniffedType: "application/pdf",
		Reason: policy.ReasonOverRunBudget, SizeBytes: 50, FirstSeenAt: at.Add(time.Minute),
	})

	cfg := loadTestConfig(t, plainConfig(t.TempDir()))
	rows, err := db.ListUnresolvedSkipped(0)
	if err != nil {
		t.Fatal(err)
	}
	plan := planRefetch(policyFor(t, cfg), rows, cfg, cfg.Instances(), "")

	if len(plan.Groups) != 2 {
		t.Fatalf("groups = %d, want 2 (one per message)", len(plan.Groups))
	}
	if len(plan.Groups[0].Items) != 3 || plan.Groups[0].Bytes != 300 {
		t.Errorf("m1 group = %d part(s), %d bytes; want 3, 300", len(plan.Groups[0].Items), plan.Groups[0].Bytes)
	}
	if plan.Candidates != 4 || plan.Bytes != 350 {
		t.Errorf("plan = %d candidate(s), %d bytes; want 4, 350", plan.Candidates, plan.Bytes)
	}
	req := RefetchRequest{StableID: plan.Groups[0].StableID, Parts: plan.Groups[0].Items}
	// Lexicographic part-key order, matching AttachmentsForMessage.
	if got := strings.Join(req.PartKeys(), ","); got != "10,2,3" {
		t.Errorf("part keys = %s, want 10,2,3", got)
	}
}

// --- --source resolution reuse ---------------------------------------------

// TestRefetchSourceSelection: refetch narrows the ledger with exactly the
// selector grammar sync uses — a kind, an account label, or an instance id —
// and reports ledger rows belonging to an instance the config no longer
// declares instead of quietly ignoring them.
func TestRefetchSourceSelection(t *testing.T) {
	db, _ := skipDB(t)
	for _, src := range []string{"gmail:work", "gmail:personal", "gchat:work", "fastmail:fm", "gmail:oldname"} {
		seedSkip(t, db, state.SkippedAttachment{
			Source: src, StableID: "m1", OrigName: "late.pdf",
			SniffedType: "application/pdf", Reason: policy.ReasonOverRunBudget, SizeBytes: 10,
		})
	}
	cfgDir := t.TempDir()
	setTestEnv(t, cfgDir, t.TempDir())
	cfgPath := filepath.Join(cfgDir, "config.toml")
	writeFile(t, cfgPath, twoAccountConfig(t.TempDir(), filepath.Join(cfgDir, "google-client-personal.json")))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p := policyFor(t, cfg)
	rows, err := db.ListUnresolvedSkipped(0)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		selectors []string
		want      string
	}{
		{nil, "fastmail:fm,gchat:work,gmail:personal,gmail:work"},
		{[]string{"gmail"}, "gmail:personal,gmail:work"},
		{[]string{"work"}, "gchat:work,gmail:work"},
		{[]string{"gmail:work"}, "gmail:work"},
		{[]string{"gmail:work", "fastmail"}, "fastmail:fm,gmail:work"},
	} {
		insts, err := resolveInstances(cfg, tc.selectors)
		if err != nil {
			t.Fatalf("--source %v: %v", tc.selectors, err)
		}
		plan := planRefetch(p, rows, cfg, insts, "")
		got := strings.Join(plan.SelectedIDs(), ",")
		// SelectedIDs follows ledger order (source, first_seen_at, …); sort
		// for comparison so the assertion is about membership, not ordering.
		got = sortedCSV(got)
		if got != tc.want {
			t.Errorf("--source %v selected %q, want %q", tc.selectors, got, tc.want)
		}
		// The stranded instance is never fetchable and never silently dropped.
		if n := plan.Unconfigured["gmail:oldname"]; n != 1 {
			t.Errorf("--source %v: unconfigured gmail:oldname reported %d time(s), want 1", tc.selectors, n)
		}
		// A CONFIGURED account the user simply did not select is not stranded
		// state and must not be reported as such.
		for src := range plan.Unconfigured {
			if src != "gmail:oldname" {
				t.Errorf("--source %v: %s reported as unconfigured, but the config declares it", tc.selectors, src)
			}
		}
	}

	if _, err := resolveInstances(cfg, []string{"nope"}); err == nil {
		t.Error("an unknown --source must be an error, not an empty run")
	}
}

func sortedCSV(s string) string {
	if s == "" {
		return ""
	}
	parts := strings.Split(s, ",")
	for i := 0; i < len(parts); i++ {
		for j := i + 1; j < len(parts); j++ {
			if parts[j] < parts[i] {
				parts[i], parts[j] = parts[j], parts[i]
			}
		}
	}
	return strings.Join(parts, ",")
}

// TestRefetchReasonFilter: --reason narrows to one recorded reason, so a user
// who widened exactly one setting can pull in exactly what it unblocked.
func TestRefetchReasonFilter(t *testing.T) {
	db, _ := skipDB(t)
	contestedRows(t, db)
	cfg := loadTestConfig(t, widenedConfig(t.TempDir()))
	rows, err := db.ListUnresolvedSkipped(0)
	if err != nil {
		t.Fatal(err)
	}

	plan := planRefetch(policyFor(t, cfg), rows, cfg, cfg.Instances(), policy.ReasonSVGDenied)
	if plan.Considered != 1 || plan.Candidates != 1 {
		t.Fatalf("--reason svg_denied considered %d / selected %d, want 1 / 1", plan.Considered, plan.Candidates)
	}
	if plan.Groups[0].StableID != "m2" {
		t.Errorf("selected %q, want m2 (logo.svg)", plan.Groups[0].StableID)
	}

	// A partial run must never advance the recorded digest: the rest of the
	// backlog has not been looked at.
	plan.Scoped = true
	if !plan.Scoped {
		t.Fatal("unreachable")
	}
}

// TestRefetchRejectsUnknownReason keeps a typo from silently selecting the
// whole ledger. The check runs before the config is loaded and before the
// instance lock is taken, so a bad flag costs nothing.
func TestRefetchRejectsUnknownReason(t *testing.T) {
	setTestEnv(t, t.TempDir(), t.TempDir()) // no config.toml: reaching it would error differently
	var out strings.Builder
	err := runRefetch(&out, nil, true, "not_a_reason")
	if err == nil || !strings.Contains(err.Error(), "unknown --reason") {
		t.Fatalf("runRefetch with a bogus --reason returned %v, want an unknown-reason error", err)
	}
	if !strings.Contains(err.Error(), policy.ReasonSVGDenied) {
		t.Errorf("the error must list the valid reasons, got %q", err)
	}
	if out.Len() != 0 {
		t.Errorf("a rejected --reason printed a plan:\n%s", out.String())
	}
}

// TestRefetchPlanEmptyLedger: with nothing refused there is nothing to say
// beyond the digest, and certainly nothing to fetch.
func TestRefetchPlanEmptyLedger(t *testing.T) {
	cfg := loadTestConfig(t, plainConfig(t.TempDir()))
	plan := planRefetch(policyFor(t, cfg), nil, cfg, cfg.Instances(), "")
	if plan.Candidates != 0 || len(plan.Groups) != 0 {
		t.Fatalf("empty ledger produced %d candidate(s)", plan.Candidates)
	}
	var out strings.Builder
	printRefetchPlan(&out, plan)
	if !strings.Contains(out.String(), "nothing to fetch") {
		t.Errorf("plan summary:\n%s", out.String())
	}
}

// --- execution and ledger dispositions --------------------------------------

// fakeRefetcher is a connector stub: it answers one canned RefetchResult per
// stable id and records what it was asked for.
type fakeRefetcher struct {
	name    string
	results map[string]RefetchResult
	err     error
	asked   []RefetchRequest
}

func (f *fakeRefetcher) Name() string                { return f.name }
func (f *fakeRefetcher) Check(context.Context) error { return nil }
func (f *fakeRefetcher) Sync(context.Context) error  { return nil }
func (f *fakeRefetcher) RefetchMessage(_ context.Context, req RefetchRequest) (RefetchResult, error) {
	f.asked = append(f.asked, req)
	if f.err != nil {
		return RefetchResult{}, f.err
	}
	return f.results[req.StableID], nil
}

// plainRefetcher is a connector that has not learned to retro-fetch yet.
type plainRefetcher struct{ name string }

func (p *plainRefetcher) Name() string                { return p.name }
func (p *plainRefetcher) Check(context.Context) error { return nil }
func (p *plainRefetcher) Sync(context.Context) error  { return nil }

func refetchApp(t *testing.T, db *state.DB, cfg *config.Config) *app {
	t.Helper()
	return &app{
		cfg:    cfg,
		db:     db,
		policy: policyFor(t, cfg),
		writer: &archive.Writer{Root: t.TempDir(), TZ: time.UTC},
		log:    quietLogger(),
	}
}

// TestRefetchAppliesEveryDisposition: each of the three terminal resolutions
// is recorded for the right row, and a divergence is recorded WITHOUT
// resolving anything — the archive must never silently gain bytes that are
// not the ones its note describes.
func TestRefetchAppliesEveryDisposition(t *testing.T) {
	db, _ := skipDB(t)
	at := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	const src = "gmail:work"
	mk := func(id, part string, sha string) {
		seedSkip(t, db, state.SkippedAttachment{
			Source: src, StableID: id, PartKey: part, OrigName: "late.pdf",
			SniffedType: "application/pdf", Reason: policy.ReasonOverRunBudget,
			SizeBytes: 100, ContentSHA256: sha, FirstSeenAt: at,
		})
	}
	const goodSHA = "aa" + "00000000000000000000000000000000000000000000000000000000000000"
	mk("stored", "1", goodSHA)
	mk("denied", "1", "")
	mk("gone", "1", "")
	mk("diverged", "1", goodSHA)

	cfg := loadTestConfig(t, plainConfig(t.TempDir()))
	a := refetchApp(t, db, cfg)
	rows, err := db.ListUnresolvedSkipped(0)
	if err != nil {
		t.Fatal(err)
	}
	plan := planRefetch(a.policy, rows, cfg, cfg.Instances(), "")
	if plan.Candidates != 4 {
		t.Fatalf("candidates = %d, want 4", plan.Candidates)
	}

	fake := &fakeRefetcher{name: src, results: map[string]RefetchResult{
		"stored": {NoteRelPath: "2026/08/07/gmail-work_stored.md", Parts: []RefetchPart{
			{PartKey: "1", Stored: true, RelPath: "2026/08/07/x.d/late.pdf", SHA256: goodSHA}}},
		"denied": {NoteRelPath: "2026/08/07/gmail-work_denied.md", Parts: []RefetchPart{
			{PartKey: "1", Reason: policy.ReasonExtensionContentMismatch}}},
		"gone":     {SourceGone: true},
		"diverged": {Parts: []RefetchPart{{PartKey: "1", Diverged: true, SHA256: "bb" + strings.Repeat("0", 62)}}},
	}}
	byID := map[string]boundSource{src: {
		inst: config.Instance{ID: src, Kind: state.SourceGmail, Label: "work"},
		conn: fake,
	}}

	var out strings.Builder
	tally, err := a.executeRefetch(context.Background(), &out, plan, byID)
	if err != nil {
		t.Fatalf("executeRefetch: %v", err)
	}
	if tally.Fetched != 1 || tally.StillDenied != 1 || tally.SourceGone != 1 || tally.Diverged != 1 {
		t.Fatalf("tally = %+v", tally)
	}
	if tally.clean() {
		t.Error("a run with a divergence must not count as clean")
	}
	if len(fake.asked) != 4 {
		t.Errorf("connector was asked %d time(s), want 4 (one per message)", len(fake.asked))
	}

	for _, tc := range []struct{ id, want string }{
		{"stored", state.SkipResolutionFetched},
		{"denied", state.SkipResolutionStillDenied},
		{"gone", state.SkipResolutionSourceGone},
		{"diverged", ""}, // deliberately still open
	} {
		got := skipResolution(t, db, src, tc.id, "1")
		if got != tc.want {
			t.Errorf("%s resolved %q, want %q", tc.id, got, tc.want)
		}
	}

	// The divergence is persisted where a human will see it, not just logged.
	fails, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range fails {
		if f.ID == refetchFailureID("diverged", "1") {
			found = true
			if !strings.Contains(f.LastError, "nothing was written") {
				t.Errorf("divergence failure text = %q", f.LastError)
			}
		}
	}
	if !found {
		t.Errorf("no failures-ledger row for the divergence: %+v", fails)
	}
}

// TestRefetchLeavesUnaccountedPartsOpen: a connector that forgets to report
// on a requested part must not have that part quietly disappear from the
// ledger — that is the silent drop the whole design exists to prevent.
func TestRefetchLeavesUnaccountedPartsOpen(t *testing.T) {
	db, _ := skipDB(t)
	const src = "gmail:work"
	for _, part := range []string{"1", "2"} {
		seedSkip(t, db, state.SkippedAttachment{
			Source: src, StableID: "m1", PartKey: part, OrigName: "late.pdf",
			SniffedType: "application/pdf", Reason: policy.ReasonOverRunBudget, SizeBytes: 10,
		})
	}
	cfg := loadTestConfig(t, plainConfig(t.TempDir()))
	a := refetchApp(t, db, cfg)
	rows, _ := db.ListUnresolvedSkipped(0)
	plan := planRefetch(a.policy, rows, cfg, cfg.Instances(), "")

	fake := &fakeRefetcher{name: src, results: map[string]RefetchResult{
		"m1": {Parts: []RefetchPart{{PartKey: "1", Stored: true}}}, // part 2 forgotten
	}}
	tally, err := a.executeRefetch(context.Background(), &strings.Builder{}, plan,
		map[string]boundSource{src: {inst: config.Instance{ID: src, Kind: state.SourceGmail}, conn: fake}})
	if err != nil {
		t.Fatal(err)
	}
	if tally.Unaccounted != 1 {
		t.Errorf("unaccounted = %d, want 1", tally.Unaccounted)
	}
	if got := skipResolution(t, db, src, "m1", "2"); got != "" {
		t.Errorf("the unreported part was resolved %q; it must stay open", got)
	}
}

// TestRefetchUnsupportedConnectorResolvesNothing: a connector that cannot
// retro-fetch yet is reported once, and its ledger is untouched.
func TestRefetchUnsupportedConnectorResolvesNothing(t *testing.T) {
	db, _ := skipDB(t)
	const src = "gmail:work"
	for _, id := range []string{"m1", "m2"} {
		seedSkip(t, db, state.SkippedAttachment{
			Source: src, StableID: id, OrigName: "late.pdf", SniffedType: "application/pdf",
			Reason: policy.ReasonOverRunBudget, SizeBytes: 10,
		})
	}
	cfg := loadTestConfig(t, plainConfig(t.TempDir()))
	a := refetchApp(t, db, cfg)
	rows, _ := db.ListUnresolvedSkipped(0)
	plan := planRefetch(a.policy, rows, cfg, cfg.Instances(), "")

	tally, err := a.executeRefetch(context.Background(), &strings.Builder{}, plan,
		map[string]boundSource{src: {inst: config.Instance{ID: src, Kind: state.SourceGmail}, conn: &plainRefetcher{name: src}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tally.Unsupported) != 1 || tally.Unsupported[0] != src {
		t.Fatalf("unsupported = %v, want [%s] reported exactly once", tally.Unsupported, src)
	}
	if tally.clean() {
		t.Error("a run that skipped a whole account must not count as clean")
	}
	if got := skipResolution(t, db, src, "m1", "1"); got != "" {
		t.Errorf("an unsupported connector resolved a row as %q", got)
	}
}

// TestRefetchDigestAdvancesOnlyOnACleanFullRun: the recorded digest is the
// claim "the whole backlog has been considered under this policy". A partial
// or troubled run must not make that claim.
func TestRefetchDigestAdvancesOnlyOnACleanFullRun(t *testing.T) {
	cfg := loadTestConfig(t, widenedConfig(t.TempDir()))
	want := policyFor(t, cfg).PolicyDigest()

	for _, tc := range []struct {
		name    string
		plan    refetchPlan
		tally   refetchTally
		advance bool
	}{
		{"clean full run", refetchPlan{Digest: want, RecordedDigest: "0123456789abcdef"}, refetchTally{Fetched: 1}, true},
		{"scoped run", refetchPlan{Digest: want, RecordedDigest: "0123456789abcdef", Scoped: true}, refetchTally{Fetched: 1}, false},
		{"divergence", refetchPlan{Digest: want, RecordedDigest: "0123456789abcdef"}, refetchTally{Diverged: 1}, false},
		{"unsupported connector", refetchPlan{Digest: want, RecordedDigest: "0123456789abcdef"}, refetchTally{Unsupported: []string{"gmail:work"}}, false},
		{"failed message", refetchPlan{Digest: want, RecordedDigest: "0123456789abcdef"}, refetchTally{Failed: 1}, false},
		{"unaccounted part", refetchPlan{Digest: want, RecordedDigest: "0123456789abcdef"}, refetchTally{Unaccounted: 1}, false},
		{"interrupted", refetchPlan{Digest: want, RecordedDigest: "0123456789abcdef"}, refetchTally{Fetched: 1, Interrupted: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := skipDB(t)
			if err := db.SetAttachmentPolicyDigest("0123456789abcdef"); err != nil {
				t.Fatal(err)
			}
			a := refetchApp(t, db, cfg)
			_ = a.finishRefetch(&strings.Builder{}, tc.plan, tc.tally)

			got, _, err := db.AttachmentPolicyDigest()
			if err != nil {
				t.Fatal(err)
			}
			if tc.advance && got != want {
				t.Errorf("digest = %s, want it advanced to %s", got, want)
			}
			if !tc.advance && got != "0123456789abcdef" {
				t.Errorf("digest advanced to %s on a run that must not claim the backlog is done", got)
			}
		})
	}
}

// skipResolution reads one ledger row's resolution ("" when still open).
func skipResolution(t *testing.T, db *state.DB, src, stableID, partKey string) string {
	t.Helper()
	rows, err := db.SkippedForNote("2026/08/07/" + state.Tag(src) + "_" + stableID + ".md")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.StableID == stableID && r.PartKey == partKey {
			return r.Resolution
		}
	}
	t.Fatalf("no ledger row for %s/%s/%s", src, stableID, partKey)
	return ""
}

// --- byte rendering ---------------------------------------------------------

func TestByteSize(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1_000, "1.0 kB"},
		{50_000_000, "50.0 MB"},
		{2_000_000_000, "2.00 GB"},
	} {
		if got := byteSize(tc.n); got != tc.want {
			t.Errorf("byteSize(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
