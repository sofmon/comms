package gchat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chat "google.golang.org/api/chat/v1"

	"save/internal/naming"
	"save/internal/policy"
	"save/internal/state"
)

// Attachment-policy behaviour of the Google Chat connector.
//
// Chat is the one source where a refusal is free: the blob lives behind a
// separate media.download, so deciding from the message metadata avoids the
// transfer entirely. What must never be free is FORGETTING — every refusal is
// a skipped_attachments row and a visible entry in the conversation-day file.

// --- helpers --------------------------------------------------------------

func msgWithAttachments(name, createTime string, atts ...*chat.Attachment) *chat.Message {
	return &chat.Message{
		Name:       name,
		CreateTime: createTime,
		Sender:     &chat.User{Name: "users/1"},
		Text:       "see attached",
		Attachment: atts,
	}
}

func uploaded(contentName, contentType, resource string) *chat.Attachment {
	return &chat.Attachment{
		Source:            "UPLOADED_CONTENT",
		ContentName:       contentName,
		ContentType:       contentType,
		AttachmentDataRef: &chat.AttachmentDataRef{ResourceName: resource},
	}
}

// spaceOnly seeds a fake with the standard space and no messages.
func spaceOnly(f *fakeAPI) {
	f.spaces = []*chat.Space{{
		Name:              "spaces/AAA",
		SpaceType:         "SPACE",
		DisplayName:       "Team Platform",
		SpaceHistoryState: "HISTORY_ON",
	}}
	f.members["spaces/AAA"] = []*chat.Membership{
		{Member: &chat.User{Name: "users/1", DisplayName: "Jane Doe"}},
	}
}

func skips(t *testing.T, db *state.DB) []state.SkippedAttachment {
	t.Helper()
	rows, err := db.ListUnresolvedSkipped(0)
	if err != nil {
		t.Fatalf("list skipped: %v", err)
	}
	return rows
}

// dayFileBody reads the one conversation-day file the tests produce for the
// given day.
func dayFileBody(t *testing.T, db *state.DB, root, day string) string {
	t.Helper()
	sp, ok, err := db.GetSpace(testSrc, "spaces/AAA")
	if err != nil || !ok {
		t.Fatalf("space: %v (ok=%v)", err, ok)
	}
	d, err := time.ParseInLocation("2006-01-02", day, mustZone(t, "Europe/Amsterdam"))
	if err != nil {
		t.Fatal(err)
	}
	rel := dayNoteRel(state.Tag(testSrc), sp.Type, sp.DisplaySlug, "spaces/AAA", d)
	return string(readFile(t, root, rel))
}

func archivedNames(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			out = append(out, filepath.Base(p))
		}
		return nil
	})
	return out
}

// --- tests ----------------------------------------------------------------

// TestDeniedChatAttachmentIsNeverDownloadedButRecorded is the whole point of
// evaluating the policy before the fetch: the bytes never cross the network,
// no pending row is queued, and the refusal is nonetheless a durable record
// and a visible line in the day file.
func TestDeniedChatAttachmentIsNeverDownloadedButRecorded(t *testing.T) {
	fake := newFakeAPI()
	spaceOnly(fake)
	fake.messages["spaces/AAA"] = []*chat.Message{
		msgWithAttachments("spaces/AAA/messages/U1", "2026-08-06T22:30:00Z",
			uploaded("payroll.exe", "application/octet-stream", "media-evil"),
			uploaded("notes.pdf", "application/pdf", "media-ok"),
		),
	}
	fake.media["media-ok"] = pdfBytes()
	fake.media["media-evil"] = exeBytes()

	c, db, w := newTestConnector(t, fake, testAcct())
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Never fetched.
	for _, got := range fake.downloads {
		if got == "media-evil" {
			t.Fatal("the denied blob was downloaded; Chat must refuse it before the fetch")
		}
	}
	if len(fake.downloads) != 1 || fake.downloads[0] != "media-ok" {
		t.Errorf("downloads = %v, want exactly [media-ok]", fake.downloads)
	}

	// No pending row was ever queued for it.
	atts, err := db.AttachmentsForMessage(testSrc, "spaces/AAA/messages/U1")
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 || atts[0].PartKey != "media-ok" {
		t.Fatalf("attachment rows = %+v, want only the allowlisted one", atts)
	}
	if atts[0].Status != state.AttachmentDone {
		t.Errorf("allowlisted attachment status = %q", atts[0].Status)
	}

	// Recorded, with the identity a retro-fetch needs.
	rows := skips(t, db)
	if len(rows) != 1 {
		t.Fatalf("skipped rows = %d, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Source != testSrc || r.StableID != "spaces/AAA/messages/U1" || r.PartKey != "media-evil" {
		t.Errorf("skip identity = %s/%s/%s", r.Source, r.StableID, r.PartKey)
	}
	if r.Reason != policy.ReasonNotAllowlistedExtension {
		t.Errorf("reason = %q", r.Reason)
	}
	if r.OrigName != "payroll.exe" || r.DeclaredExt != "exe" {
		t.Errorf("orig name %q / declared ext %q", r.OrigName, r.DeclaredExt)
	}
	if r.SniffedType != "" {
		t.Errorf("sniffed type = %q, want empty — nothing was fetched, so nothing was sniffed", r.SniffedType)
	}
	if r.SizeBytes != 0 {
		t.Errorf("size = %d, want 0 — Chat publishes no size before the download", r.SizeBytes)
	}
	if r.ContentSHA256 != "" {
		t.Errorf("content sha256 = %q, want empty — there were no bytes to hash", r.ContentSHA256)
	}
	if r.DayBucket != "2026-08-07" { // 22:30Z is 00:30 in Europe/Amsterdam
		t.Errorf("day bucket = %q", r.DayBucket)
	}
	if r.PolicyDigest != policy.Default().PolicyDigest() {
		t.Errorf("policy digest = %q", r.PolicyDigest)
	}

	// Visible in the day file, worded as never fetched.
	body := dayFileBody(t, db, w.Root, "2026-08-07")
	if !strings.Contains(body, "payroll.exe") {
		t.Errorf("the day file does not mention the refusal:\n%s", body)
	}
	if !strings.Contains(body, policy.ReasonNotAllowlistedExtension) {
		t.Errorf("the day file does not name the reason:\n%s", body)
	}
	if !strings.Contains(body, "never downloaded") {
		t.Errorf("the day file must say the bytes were never downloaded:\n%s", body)
	}
	if r.NoteRelPath == "" || !strings.HasSuffix(r.NoteRelPath, ".md") {
		t.Errorf("note rel path = %q", r.NoteRelPath)
	}

	// Nothing named payroll reached the vault.
	for _, n := range archivedNames(t, w.Root) {
		if strings.Contains(n, "payroll") {
			t.Errorf("the denied attachment reached the archive as %q", n)
		}
	}
	// A policy skip is not an item failure.
	fails, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 0 {
		t.Errorf("policy skips reached the failures ledger: %+v", fails)
	}
}

// TestDownloadedChatAttachmentRefusedOnContent covers what PreCheck cannot:
// the extension is allowlisted, so the blob IS fetched, and only the sniffed
// content exposes it. Passing PreCheck is not an authorization to store.
func TestDownloadedChatAttachmentRefusedOnContent(t *testing.T) {
	fake := newFakeAPI()
	spaceOnly(fake)
	fake.messages["spaces/AAA"] = []*chat.Message{
		msgWithAttachments("spaces/AAA/messages/U1", "2026-08-06T22:30:00Z",
			uploaded("invoice.pdf", "application/pdf", "media-1")),
	}
	fake.media["media-1"] = exeBytes() // a PE binary wearing a .pdf name

	c, db, w := newTestConnector(t, fake, testAcct())
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if len(fake.downloads) != 1 {
		t.Fatalf("downloads = %v, want the blob fetched (only content can expose this)", fake.downloads)
	}
	for _, n := range archivedNames(t, w.Root) {
		if strings.Contains(n, "invoice") {
			t.Fatalf("the refused bytes were written as %q", n)
		}
	}

	rows := skips(t, db)
	if len(rows) != 1 {
		t.Fatalf("skipped rows = %d, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Reason != policy.ReasonExtensionContentMismatch {
		t.Errorf("reason = %q, want %q", r.Reason, policy.ReasonExtensionContentMismatch)
	}
	if r.SniffedType == "" {
		t.Error("sniffed type is empty, but these bytes were in hand")
	}
	if len(r.ContentSHA256) != 64 {
		t.Errorf("content sha256 = %q, want the hash of the bytes we did fetch", r.ContentSHA256)
	}
	if r.SizeBytes != int64(len(exeBytes())) {
		t.Errorf("size = %d, want %d", r.SizeBytes, len(exeBytes()))
	}

	// The ledger row is terminal, so the download is not retried forever.
	atts, err := db.AttachmentsForMessage(testSrc, "spaces/AAA/messages/U1")
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 || atts[0].Status != state.AttachmentFailed {
		t.Fatalf("attachment rows = %+v, want one terminal row", atts)
	}
	if !strings.Contains(atts[0].LastError, "attachment policy") {
		t.Errorf("last_error = %q, should name the policy so `save status` does not read as a network fault", atts[0].LastError)
	}

	// The day file explains it, and says the bytes were discarded — not that
	// they were never fetched, because they were.
	body := dayFileBody(t, db, w.Root, "2026-08-07")
	if !strings.Contains(body, "invoice.pdf") || !strings.Contains(body, policy.ReasonExtensionContentMismatch) {
		t.Errorf("the day file does not explain the refusal:\n%s", body)
	}
	if strings.Contains(body, "never downloaded") {
		t.Errorf("the day file claims the bytes were never downloaded, but they were:\n%s", body)
	}
	if !strings.Contains(body, "were discarded") {
		t.Errorf("the day file must say the fetched bytes were discarded:\n%s", body)
	}
}

// TestMixedDispositionClaimsNothing: a day holding both a never-fetched and a
// fetched-and-discarded refusal cannot make one claim about the bytes, so it
// makes none. Vague is allowed; wrong is not.
func TestMixedDispositionClaimsNothing(t *testing.T) {
	fake := newFakeAPI()
	spaceOnly(fake)
	fake.messages["spaces/AAA"] = []*chat.Message{
		msgWithAttachments("spaces/AAA/messages/U1", "2026-08-06T22:30:00Z",
			uploaded("payroll.exe", "application/octet-stream", "media-evil"),
			uploaded("invoice.pdf", "application/pdf", "media-liar")),
	}
	fake.media["media-liar"] = exeBytes()

	c, db, w := newTestConnector(t, fake, testAcct())
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n := len(skips(t, db)); n != 2 {
		t.Fatalf("skipped rows = %d, want 2", n)
	}
	body := dayFileBody(t, db, w.Root, "2026-08-07")
	if strings.Contains(body, "never downloaded") || strings.Contains(body, "were discarded") {
		t.Errorf("a mixed day must not claim either disposition:\n%s", body)
	}
	if !strings.Contains(body, "No copy was kept.") {
		t.Errorf("a mixed day should fall back to the claim-nothing wording:\n%s", body)
	}
	if !strings.Contains(body, "payroll.exe") || !strings.Contains(body, "invoice.pdf") {
		t.Errorf("both refusals must still be visible:\n%s", body)
	}
}

// TestChatFreeSpaceFloor: below the floor the blob is refused after download
// and the conversation-day file is still written. The sync never aborts.
func TestChatFreeSpaceFloor(t *testing.T) {
	fake := newFakeAPI()
	spaceOnly(fake)
	fake.messages["spaces/AAA"] = []*chat.Message{
		msgWithAttachments("spaces/AAA/messages/U1", "2026-08-06T22:30:00Z",
			uploaded("notes.pdf", "application/pdf", "media-1")),
	}
	fake.media["media-1"] = pdfBytes()

	floor := policy.DefaultSettings().FreeSpaceFloor
	c, db, w := newTestConnector(t, fake, testAcct(),
		WithFreeSpace(func(string) int64 { return floor + 8 }))
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync must not abort when the disk is nearly full: %v", err)
	}

	rows := skips(t, db)
	if len(rows) != 1 || rows[0].Reason != policy.ReasonFreeSpaceFloor {
		t.Fatalf("skipped = %+v, want one %s", rows, policy.ReasonFreeSpaceFloor)
	}
	body := dayFileBody(t, db, w.Root, "2026-08-07")
	if !strings.Contains(body, "see attached") {
		t.Errorf("the conversation itself was not archived:\n%s", body)
	}
	for _, n := range archivedNames(t, w.Root) {
		if strings.Contains(n, "notes.pdf") {
			t.Errorf("the attachment was written below the floor, as %q", n)
		}
	}
}

// TestChatRunBudgetCutoff: once the pass has spent attachments.run_budget the
// next blob is refused, transiently, and the conversation keeps archiving.
func TestChatRunBudgetCutoff(t *testing.T) {
	pdf := pdfBytes()
	set := policy.DefaultSettings()
	set.RunBudget = int64(len(pdf)) + 1 // room for exactly one
	set.FreeSpaceFloor = 0
	pol, err := policy.New(set)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	fake := newFakeAPI()
	spaceOnly(fake)
	fake.messages["spaces/AAA"] = []*chat.Message{
		msgWithAttachments("spaces/AAA/messages/U1", "2026-08-06T22:30:00Z",
			uploaded("a.pdf", "application/pdf", "media-a")),
		msgWithAttachments("spaces/AAA/messages/U2", "2026-08-06T22:31:00Z",
			uploaded("b.pdf", "application/pdf", "media-b")),
	}
	fake.media["media-a"] = pdf
	fake.media["media-b"] = pdf

	c, db, w := newTestConnector(t, fake, testAcct(), WithPolicy(pol))
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rows := skips(t, db)
	if len(rows) != 1 {
		t.Fatalf("skipped rows = %d, want exactly one over-budget refusal: %+v", len(rows), rows)
	}
	if rows[0].Reason != policy.ReasonOverRunBudget {
		t.Fatalf("reason = %q, want %q", rows[0].Reason, policy.ReasonOverRunBudget)
	}
	if !policy.Transient(rows[0].Reason) {
		t.Error("over_run_budget must be transient: the next pass has a fresh budget")
	}
	body := dayFileBody(t, db, w.Root, "2026-08-07")
	if !strings.Contains(body, "see attached") {
		t.Errorf("the conversation was not archived:\n%s", body)
	}
}

// TestChatSizeCapAppliesAfterDownload: chat.Attachment publishes no size, so
// attachments.chat_max_size can only bind once the bytes are in hand. That is
// exactly where it must bind — otherwise the cap would be unenforced on the
// one source that allows 200 MB uploads.
func TestChatSizeCapAppliesAfterDownload(t *testing.T) {
	set := policy.DefaultSettings()
	set.ChatMaxSize = 16
	set.FreeSpaceFloor = 0
	pol, err := policy.New(set)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	fake := newFakeAPI()
	spaceOnly(fake)
	fake.messages["spaces/AAA"] = []*chat.Message{
		msgWithAttachments("spaces/AAA/messages/U1", "2026-08-06T22:30:00Z",
			uploaded("notes.pdf", "application/pdf", "media-1")),
	}
	fake.media["media-1"] = pdfBytes() // comfortably over 16 bytes

	c, db, _ := newTestConnector(t, fake, testAcct(), WithPolicy(pol))
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	rows := skips(t, db)
	if len(rows) != 1 || rows[0].Reason != policy.ReasonOverSizeCap {
		t.Fatalf("skipped = %+v, want one %s", rows, policy.ReasonOverSizeCap)
	}
	if rows[0].SizeBytes != int64(len(pdfBytes())) {
		t.Errorf("size = %d, want the real downloaded byte count %d", rows[0].SizeBytes, len(pdfBytes()))
	}
}

// TestDayNoteRelMatchesState pins this package's day-file path derivation to
// the state store's. They are computed independently — state owns the
// chat_day_files rel_path, this package owns the note path a skip points at —
// and a skip whose note path does not name the file the renderer writes is a
// skip nobody can find.
func TestDayNoteRelMatchesState(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake)
	c, db, _ := newTestConnector(t, fake, testAcct())
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	sp, ok, err := db.GetSpace(testSrc, "spaces/AAA")
	if err != nil || !ok {
		t.Fatalf("space: %v (ok=%v)", err, ok)
	}
	// Every day file state produced must be reproduced exactly by dayNoteRel.
	if err := db.MarkDayDirty(testSrc, "spaces/AAA", "2026-08-06"); err != nil {
		t.Fatal(err)
	}
	files, err := db.DirtyDayFiles(testSrc)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no day files to compare against")
	}
	tz := mustZone(t, "Europe/Amsterdam")
	for _, f := range files {
		d, perr := time.ParseInLocation("2006-01-02", f.DayBucket, tz)
		if perr != nil {
			t.Fatal(perr)
		}
		got := dayNoteRel(state.Tag(testSrc), sp.Type, sp.DisplaySlug, f.Space, d)
		if got != f.RelPath {
			t.Errorf("dayNoteRel = %q, state says %q — the two derivations have drifted", got, f.RelPath)
		}
	}
}

// TestPolicyKeysOnFinalOnDiskName: the decision must run on the name that
// actually lands in the vault, prefix and iCloud guard included. Keying on
// the sender's raw filename instead would let the naming layer's ".bin"
// rewrite change the extension out from under the allowlist.
func TestPolicyKeysOnFinalOnDiskName(t *testing.T) {
	local := time.Date(2026, 8, 7, 1, 30, 0, 0, time.UTC)
	sp := spaceMeta{Name: "spaces/AAA", Type: "space", Slug: "team"}
	m := msgWithAttachments("spaces/AAA/messages/U1", "2026-08-06T22:30:00Z",
		uploaded("payroll.exe", "application/octet-stream", "media-evil"))
	stem := naming.ChatStem(state.Tag(testSrc), sp.Type, sp.Slug, naming.Hash8(sp.Name))

	plan := pendingAttachments(testSrc, state.Tag(testSrc), stem, sp, m, local, false, policy.Default())
	if len(plan.Pending) != 0 {
		t.Fatalf("pending = %+v, want none", plan.Pending)
	}
	if len(plan.Skipped) != 1 {
		t.Fatalf("skipped = %+v, want one", plan.Skipped)
	}
	got := plan.Skipped[0].SanitizedName
	want := naming.ChatAttachmentName(local, naming.Hash8(m.Name), "payroll.exe")
	if got != want {
		t.Errorf("sanitized name = %q, want the final on-disk basename %q", got, want)
	}
}
