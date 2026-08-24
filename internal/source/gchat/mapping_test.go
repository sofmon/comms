package gchat

import (
	"path"
	"strings"
	"testing"
	"time"

	chat "google.golang.org/api/chat/v1"

	"save/internal/archive"
	"save/internal/naming"
	"save/internal/state"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load zone %s: %v", name, err)
	}
	return loc
}

// TestListFilterRewind pins the mandatory 1-second cursor rewind: the
// documented filter grammar has only a strict '>', so a message sharing the
// cursor's exact createTime is invisible to a plain '>' filter and must be
// caught by the overlap. This is the regression test that keeps a
// "simplification" back to '>' from silently dropping same-timestamp
// messages.
func TestListFilterRewind(t *testing.T) {
	cursor := time.Date(2026, 8, 6, 22, 30, 0, 500_000_000, time.UTC)
	f := listFilter(cursor)

	i, j := strings.Index(f, `"`), strings.LastIndex(f, `"`)
	if i < 0 || j <= i {
		t.Fatalf("filter %q has no quoted timestamp", f)
	}
	filterTime, err := time.Parse(time.RFC3339Nano, f[i+1:j])
	if err != nil {
		t.Fatalf("filter %q timestamp unparseable: %v", f, err)
	}
	if !filterTime.Equal(cursor.Add(-time.Second)) {
		t.Fatalf("filter time = %v, want cursor-1s = %v", filterTime, cursor.Add(-time.Second))
	}
	if !strings.HasPrefix(f, "createTime > ") {
		t.Fatalf("filter %q does not use the createTime > form", f)
	}

	// A second message created at the cursor's exact timestamp:
	msgCreate := cursor
	// ...a plain '>' on the cursor itself would skip it (this is the bug the
	// rewind exists to prevent)...
	if msgCreate.After(cursor) {
		t.Fatal("sanity: equal timestamps must not satisfy a strict '>'")
	}
	// ...while the rewound filter includes it.
	if !msgCreate.After(filterTime) {
		t.Fatal("rewound filter must match a message sharing the cursor timestamp")
	}

	if got := listFilter(time.Time{}); got != "" {
		t.Fatalf("zero cursor must mean no filter, got %q", got)
	}
}

func TestBuildPageMapping(t *testing.T) {
	amsterdam := mustZone(t, "Europe/Amsterdam") // UTC+2 in August
	sp := spaceMeta{Name: "spaces/AAA", Type: "space", Slug: "team-platform"}
	stem := naming.ChatStem(testTag, sp.Type, sp.Slug, naming.Hash8(sp.Name))

	msgs := []*chat.Message{
		{
			Name:       "spaces/AAA/messages/M1",
			CreateTime: "2026-08-06T20:00:00.123456Z", // 22:00 local — day 2026-08-06
			Sender:     &chat.User{Name: "users/1"},
			Thread:     &chat.Thread{Name: "spaces/AAA/threads/T1"},
			Text:       "hello",
		},
		{
			Name:           "spaces/AAA/messages/M2",
			CreateTime:     "2026-08-06T22:30:00Z", // 00:30 local — crosses midnight into 2026-08-07
			LastUpdateTime: "2026-08-07T09:00:00Z",
			Sender:         &chat.User{Name: "users/2"},
			Attachment: []*chat.Attachment{
				{
					Source:            "UPLOADED_CONTENT",
					ContentName:       "notes.pdf",
					AttachmentDataRef: &chat.AttachmentDataRef{ResourceName: "media-ref-1"},
				},
				{
					Source:       "DRIVE_FILE",
					ContentName:  "spec.doc",
					DriveDataRef: &chat.DriveDataRef{DriveFileId: "drive-1"},
				},
				{
					Source:            "UPLOADED_CONTENT",
					ContentName:       "notes.pdf", // duplicate name in one message
					AttachmentDataRef: &chat.AttachmentDataRef{ResourceName: "media-ref-2"},
				},
			},
		},
		{
			Name:       "spaces/AAA/messages/M3",
			CreateTime: "2026-08-06T21:00:00Z",
			DeleteTime: "2026-08-07T10:00:00Z",
		},
		{
			Name:       "spaces/AAA/messages/BAD",
			CreateTime: "not-a-time",
		},
	}

	page, _, maxCreate, errs, _ := buildPage(testSrc, sp, msgs, amsterdam, time.Time{}, false, nil, nil)

	if page.Space != sp.Name {
		t.Fatalf("page space = %q", page.Space)
	}
	if len(errs) != 1 || errs[0].ID != "spaces/AAA/messages/BAD" {
		t.Fatalf("errs = %+v, want one for BAD", errs)
	}
	if len(page.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(page.Messages))
	}

	byName := map[string]state.ChatMessage{}
	for _, m := range page.Messages {
		byName[m.Name] = m
	}

	m1 := byName["spaces/AAA/messages/M1"]
	if m1.DayBucket != "2026-08-06" {
		t.Fatalf("M1 day = %q, want 2026-08-06", m1.DayBucket)
	}
	if m1.Thread != "spaces/AAA/threads/T1" || m1.SenderID != "users/1" {
		t.Fatalf("M1 thread/sender = %q/%q", m1.Thread, m1.SenderID)
	}
	if m1.Edited || m1.Deleted {
		t.Fatalf("M1 flags edited=%v deleted=%v, want false/false", m1.Edited, m1.Deleted)
	}
	if !strings.Contains(m1.RawJSON, `"text":"hello"`) {
		t.Fatalf("M1 raw_json missing text: %s", m1.RawJSON)
	}

	m2 := byName["spaces/AAA/messages/M2"]
	if m2.DayBucket != "2026-08-07" {
		t.Fatalf("M2 day = %q, want 2026-08-07 (midnight crossing in +02:00)", m2.DayBucket)
	}
	if !m2.Edited {
		t.Fatal("M2 with lastUpdateTime must be flagged edited")
	}
	if m2.LastUpdateTime.IsZero() {
		t.Fatal("M2 LastUpdateTime not parsed")
	}

	m3 := byName["spaces/AAA/messages/M3"]
	if !m3.Deleted || m3.DeletedAt.IsZero() {
		t.Fatalf("M3 deleted=%v deletedAt=%v, want tombstone", m3.Deleted, m3.DeletedAt)
	}

	// Attachments: only the two uploads, at deterministic paths under the
	// day dir of the OWNING message's local day, duplicate name uniqued.
	if len(page.Attachments) != 2 {
		t.Fatalf("got %d attachment rows, want 2 (Drive files excluded): %+v", len(page.Attachments), page.Attachments)
	}
	local := time.Date(2026, 8, 7, 0, 30, 0, 0, amsterdam)
	msgHash := naming.Hash8("spaces/AAA/messages/M2")
	want0 := path.Join("2026/08/07", naming.AttachDir(stem), naming.ChatAttachmentName(local, msgHash, "notes.pdf"))
	want1 := path.Join("2026/08/07", naming.AttachDir(stem), naming.ChatAttachmentName(local, msgHash, "notes_2.pdf"))
	if page.Attachments[0].RelPath != want0 {
		t.Fatalf("att[0].RelPath = %q, want %q", page.Attachments[0].RelPath, want0)
	}
	if page.Attachments[1].RelPath != want1 {
		t.Fatalf("att[1].RelPath = %q, want %q (deterministic _2 suffix)", page.Attachments[1].RelPath, want1)
	}
	if page.Attachments[0].PartKey != "media-ref-1" || page.Attachments[1].PartKey != "media-ref-2" {
		t.Fatalf("part keys = %q/%q", page.Attachments[0].PartKey, page.Attachments[1].PartKey)
	}
	if page.Attachments[0].StableID != "spaces/AAA/messages/M2" {
		t.Fatalf("att stable id = %q", page.Attachments[0].StableID)
	}

	// Cursor: max createTime across the page.
	wantMax := time.Date(2026, 8, 6, 22, 30, 0, 0, time.UTC)
	if !maxCreate.Equal(wantMax) {
		t.Fatalf("maxCreate = %v, want %v", maxCreate, wantMax)
	}
	if page.Cursor != formatCursor(wantMax) {
		t.Fatalf("page cursor = %q, want %q", page.Cursor, formatCursor(wantMax))
	}
}

// TestBuildPageCursorNeverRegresses: a refetched overlap page whose max
// createTime is at or before the durable cursor must not move the cursor.
func TestBuildPageCursorNeverRegresses(t *testing.T) {
	sp := spaceMeta{Name: "spaces/AAA", Type: "space", Slug: "s"}
	prev := time.Date(2026, 8, 6, 22, 30, 0, 0, time.UTC)
	msgs := []*chat.Message{{
		Name:       "spaces/AAA/messages/OLD",
		CreateTime: "2026-08-06T22:29:59.5Z", // inside the 1s overlap
	}}
	page, _, maxCreate, errs, _ := buildPage(testSrc, sp, msgs, time.UTC, prev, false, nil, nil)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if page.Cursor != "" {
		t.Fatalf("cursor = %q, want empty (no regression)", page.Cursor)
	}
	if !maxCreate.Equal(prev) {
		t.Fatalf("maxCreate = %v, want unchanged %v", maxCreate, prev)
	}
}

// TestBuildPageStopsAtFailedMessage pins the poison-message protocol at the
// mapping layer: an unmappable message that is not yet skip-listed freezes
// the cursor at the last good message BEFORE it and stops the page (later
// messages must not drag the cursor past the failure — that would lose the
// message forever, since gchat has no per-message retry pass). Once
// skip-listed it is passed over and later messages advance the cursor again.
func TestBuildPageStopsAtFailedMessage(t *testing.T) {
	sp := spaceMeta{Name: "spaces/AAA", Type: "space", Slug: "s"}
	msgs := []*chat.Message{
		{Name: "spaces/AAA/messages/G1", CreateTime: "2026-08-06T10:00:00Z"},
		{Name: "spaces/AAA/messages/BAD", CreateTime: "not-a-time"},
		{Name: "spaces/AAA/messages/G2", CreateTime: "2026-08-06T11:00:00Z"},
	}
	t1 := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC)

	// Not yet skip-listed: stop at BAD, cursor frozen at G1.
	page, _, maxCreate, errs, stopped := buildPage(testSrc, sp, msgs, time.UTC, time.Time{}, false, nil, func(string) bool { return false })
	if !stopped {
		t.Fatal("page with a non-skipped poison message must report stopped")
	}
	if len(page.Messages) != 1 || page.Messages[0].Name != "spaces/AAA/messages/G1" {
		t.Fatalf("messages after stop = %+v, want only G1", page.Messages)
	}
	if len(errs) != 1 || errs[0].ID != "spaces/AAA/messages/BAD" {
		t.Fatalf("errs = %+v, want one for BAD", errs)
	}
	if !maxCreate.Equal(t1) || page.Cursor != formatCursor(t1) {
		t.Fatalf("cursor advanced past the poison message: max %v cursor %q, want frozen at %v", maxCreate, page.Cursor, t1)
	}

	// Skip-listed: passed over silently, later messages advance the cursor.
	page2, _, max2, errs2, stopped2 := buildPage(testSrc, sp, msgs, time.UTC, time.Time{}, false, nil, func(id string) bool {
		return id == "spaces/AAA/messages/BAD"
	})
	if stopped2 || len(errs2) != 0 {
		t.Fatalf("skip-listed message must be passed over: stopped=%v errs=%+v", stopped2, errs2)
	}
	if len(page2.Messages) != 2 {
		t.Fatalf("messages with skip-listed BAD = %+v, want G1+G2", page2.Messages)
	}
	if !max2.Equal(t2) || page2.Cursor != formatCursor(t2) {
		t.Fatalf("cursor with skip-listed BAD = %v/%q, want %v", max2, page2.Cursor, t2)
	}

	// nil isSkipped (the events-refetch path, no cursor at stake): failures
	// are reported and mapping continues.
	page3, _, _, errs3, stopped3 := buildPage(testSrc, sp, msgs, time.UTC, time.Time{}, false, nil, nil)
	if stopped3 || len(errs3) != 1 || len(page3.Messages) != 2 {
		t.Fatalf("nil isSkipped must record and continue: stopped=%v errs=%+v msgs=%d", stopped3, errs3, len(page3.Messages))
	}
}

// TestAttachmentPathDeterminism: identical inputs yield byte-identical rel
// paths across runs; distinct messages in the same second stay distinct via
// the message hash.
func TestAttachmentPathDeterminism(t *testing.T) {
	amsterdam := mustZone(t, "Europe/Amsterdam")
	sp := spaceMeta{Name: "spaces/AAA", Type: "dm", Slug: "dm"}
	mk := func(name string) *chat.Message {
		return &chat.Message{
			Name:       name,
			CreateTime: "2026-08-06T22:30:00Z",
			Attachment: []*chat.Attachment{{
				Source:            "UPLOADED_CONTENT",
				ContentName:       "тест файл.PDF", // exercises sanitizer passthrough
				AttachmentDataRef: &chat.AttachmentDataRef{ResourceName: "ref"},
			}},
		}
	}
	pageA, _, _, _, _ := buildPage(testSrc, sp, []*chat.Message{mk("spaces/AAA/messages/X")}, amsterdam, time.Time{}, false, nil, nil)
	pageB, _, _, _, _ := buildPage(testSrc, sp, []*chat.Message{mk("spaces/AAA/messages/X")}, amsterdam, time.Time{}, false, nil, nil)
	if pageA.Attachments[0].RelPath != pageB.Attachments[0].RelPath {
		t.Fatalf("same input, different paths: %q vs %q", pageA.Attachments[0].RelPath, pageB.Attachments[0].RelPath)
	}
	pageC, _, _, _, _ := buildPage(testSrc, sp, []*chat.Message{mk("spaces/AAA/messages/Y")}, amsterdam, time.Time{}, false, nil, nil)
	if pageA.Attachments[0].RelPath == pageC.Attachments[0].RelPath {
		t.Fatalf("distinct messages in the same second must not collide: %q", pageC.Attachments[0].RelPath)
	}
	// Structure: dayDir/<stem>.d/HHMMSS_hash8_name
	rel := pageA.Attachments[0].RelPath
	if !strings.HasPrefix(rel, "2026/08/07/"+testTag+"_dm_dm_") || !strings.Contains(rel, ".d/003000_") {
		t.Fatalf("unexpected path shape: %q", rel)
	}
}

func TestClampEventsStart(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-eventsWindow + eventsClampMargin)
	tests := []struct {
		name      string
		cursor    time.Time
		wantStart time.Time
		wantGap   bool
	}{
		{"no cursor", time.Time{}, oldest, false},
		{"older than window", now.Add(-40 * 24 * time.Hour), oldest, true},
		{"just inside window", now.Add(-24 * time.Hour), now.Add(-24 * time.Hour), false},
		{"recent", now.Add(-time.Minute), now.Add(-time.Minute), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, gap := clampEventsStart(tc.cursor, now)
			if !start.Equal(tc.wantStart) || gap != tc.wantGap {
				t.Fatalf("clampEventsStart(%v) = (%v, %v), want (%v, %v)",
					tc.cursor, start, gap, tc.wantStart, tc.wantGap)
			}
		})
	}
}

// TestEventsListStart pins the events cursor math: the listing overlaps the
// stored cursor by eventsRewind (events committing server-side mid-pass, or
// hidden by clock skew, are re-observed; replays are idempotent), the 28-day
// clamp holds, and the gap warning keys off the raw cursor — never off the
// rewind.
func TestEventsListStart(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-eventsWindow + eventsClampMargin)
	tests := []struct {
		name      string
		cursor    time.Time
		wantStart time.Time
		wantGap   bool
	}{
		{"no cursor", time.Time{}, oldest, false},
		{"recent cursor rewound", now.Add(-time.Hour), now.Add(-time.Hour - eventsRewind), false},
		{"rewind clipped at window edge", oldest.Add(time.Minute), oldest, false},
		{"older than window", now.Add(-40 * 24 * time.Hour), oldest, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, gap := eventsListStart(tc.cursor, now)
			if !start.Equal(tc.wantStart) || gap != tc.wantGap {
				t.Fatalf("eventsListStart(%v) = (%v, %v), want (%v, %v)",
					tc.cursor, start, gap, tc.wantStart, tc.wantGap)
			}
		})
	}
}

func TestSmallHelpers(t *testing.T) {
	if got := spaceOfMessage("spaces/AAA/messages/M1"); got != "spaces/AAA" {
		t.Fatalf("spaceOfMessage = %q", got)
	}
	if day, ok := dayFromRelPath("2026/08/07/stem.d/file.pdf"); !ok || day != "2026-08-07" {
		t.Fatalf("dayFromRelPath = %q, %v", day, ok)
	}
	if _, ok := dayFromRelPath("stem.d/file.pdf"); ok {
		t.Fatal("dayFromRelPath accepted a path without a day prefix")
	}
	if got := spaceTypeOf("DIRECT_MESSAGE"); got != "dm" {
		t.Fatalf("spaceTypeOf DM = %q", got)
	}
	if got := spaceTypeOf("GROUP_CHAT"); got != "group" {
		t.Fatalf("spaceTypeOf GROUP_CHAT = %q", got)
	}
	if got := spaceTypeOf("SPACE"); got != "space" {
		t.Fatalf("spaceTypeOf SPACE = %q", got)
	}
	if slugFallback("dm") != "dm" || slugFallback("group") != "group" || slugFallback("space") != "untitled" {
		t.Fatal("slugFallback mapping changed")
	}
	// Cursor round-trip at fixed width.
	ts := time.Date(2026, 8, 6, 22, 30, 0, 1, time.UTC)
	back, err := parseCursor(formatCursor(ts))
	if err != nil || !back.Equal(ts) {
		t.Fatalf("cursor round-trip: %v %v", back, err)
	}
}

// TestBuildPageDriveMirroring pins the mirror_drive_files mapping contract:
// DRIVE_FILE attachments get pending rows only when mirroring is on, part-
// keyed "drive:<fileId>", and Google-native types carry the extension of the
// Office format they will be exported as (chosen from the message's
// contentType so the path stays deterministic).
func TestBuildPageDriveMirroring(t *testing.T) {
	amsterdam := mustZone(t, "Europe/Amsterdam")
	sp := spaceMeta{Name: "spaces/AAA", Type: "space", Slug: "team-platform"}
	msgs := []*chat.Message{{
		Name:       "spaces/AAA/messages/M1",
		CreateTime: "2026-08-06T22:30:00Z", // 00:30 local — day 2026-08-07
		Attachment: []*chat.Attachment{
			{
				Source:            "UPLOADED_CONTENT",
				ContentName:       "notes.pdf",
				AttachmentDataRef: &chat.AttachmentDataRef{ResourceName: "media-ref-1"},
			},
			{
				Source:       "DRIVE_FILE",
				ContentName:  "Design spec",
				ContentType:  "application/vnd.google-apps.document",
				DriveDataRef: &chat.DriveDataRef{DriveFileId: "drive-doc"},
			},
			{
				Source:       "DRIVE_FILE",
				ContentName:  "photo.jpg",
				ContentType:  "image/jpeg",
				DriveDataRef: &chat.DriveDataRef{DriveFileId: "drive-bin"},
			},
			{
				Source:      "DRIVE_FILE", // no file id: nothing to fetch by
				ContentName: "mystery",
			},
			{
				Source:       "DRIVE_FILE",
				ContentName:  "Report.DOCX", // extension already present (any case)
				ContentType:  "application/vnd.google-apps.document",
				DriveDataRef: &chat.DriveDataRef{DriveFileId: "drive-named"},
			},
		},
	}}

	// Off: Drive files get no rows (the renderer links them from raw_json).
	pageOff, _, _, _, _ := buildPage(testSrc, sp, msgs, amsterdam, time.Time{}, false, nil, nil)
	if len(pageOff.Attachments) != 1 || pageOff.Attachments[0].PartKey != "media-ref-1" {
		t.Fatalf("mirroring off: rows = %+v, want only the upload", pageOff.Attachments)
	}

	// On: the upload plus every fetchable Drive file.
	pageOn, _, _, _, _ := buildPage(testSrc, sp, msgs, amsterdam, time.Time{}, true, nil, nil)
	if len(pageOn.Attachments) != 4 {
		t.Fatalf("mirroring on: rows = %+v, want upload + 3 drive rows", pageOn.Attachments)
	}
	stem := naming.ChatStem(testTag, sp.Type, sp.Slug, naming.Hash8(sp.Name))
	local := time.Date(2026, 8, 7, 0, 30, 0, 0, amsterdam)
	msgHash := naming.Hash8("spaces/AAA/messages/M1")

	doc := pageOn.Attachments[1]
	if doc.PartKey != archive.DrivePartKeyPrefix+"drive-doc" {
		t.Fatalf("doc part key = %q, want the drive: namespace", doc.PartKey)
	}
	wantDoc := path.Join("2026/08/07", naming.AttachDir(stem), naming.ChatAttachmentName(local, msgHash, "Design spec.docx"))
	if doc.RelPath != wantDoc {
		t.Fatalf("doc rel = %q, want %q (export extension appended)", doc.RelPath, wantDoc)
	}
	bin := pageOn.Attachments[2]
	if bin.PartKey != archive.DrivePartKeyPrefix+"drive-bin" || !strings.HasSuffix(bin.RelPath, "photo.jpg") {
		t.Fatalf("binary drive row = %+v, want content name untouched", bin)
	}
	named := pageOn.Attachments[3]
	if !strings.HasSuffix(named.RelPath, "Report.DOCX") {
		t.Fatalf("named doc rel = %q, want no doubled extension", named.RelPath)
	}
}

// TestDriveExport pins the export format selection for Google-native types;
// everything else downloads byte-identically via alt=media.
func TestDriveExport(t *testing.T) {
	tests := []struct {
		mime, wantMime, wantExt string
		wantOK                  bool
	}{
		{"application/vnd.google-apps.document", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".docx", true},
		{"application/vnd.google-apps.spreadsheet", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", ".xlsx", true},
		{"application/vnd.google-apps.presentation", "application/vnd.openxmlformats-officedocument.presentationml.presentation", ".pptx", true},
		{"application/pdf", "", "", false},
		{"application/vnd.google-apps.form", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range tests {
		gotMime, gotExt, ok := driveExport(tc.mime)
		if gotMime != tc.wantMime || gotExt != tc.wantExt || ok != tc.wantOK {
			t.Errorf("driveExport(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.mime, gotMime, gotExt, ok, tc.wantMime, tc.wantExt, tc.wantOK)
		}
	}
}

func TestAttachmentRetryDelay(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{9, 4*time.Hour + 16*time.Minute},
		{10, 6 * time.Hour}, // capped
		{20, 6 * time.Hour}, // stays capped
	}
	for _, tc := range tests {
		if got := attachmentRetryDelay(tc.attempts); got != tc.want {
			t.Fatalf("attachmentRetryDelay(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}
