package gchat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"

	"comms/internal/archive"
	"comms/internal/config"
	"comms/internal/naming"
	"comms/internal/ratelimit"
	"comms/internal/state"
)

// fakeAPI implements chatAPI in memory. listMessages applies the createTime
// filter with the server's strict '>' semantics — so the end-to-end tests
// exercise the 1s-rewind behavior against a faithful model.
type fakeAPI struct {
	mu       sync.Mutex
	spaces   []*chat.Space
	members  map[string][]*chat.Membership
	messages map[string][]*chat.Message
	events   map[string][]*chat.SpaceEvent
	gets     map[string]*chat.Message
	media    map[string][]byte
	pageSize int   // messages per page; <=0 = all in one page
	msgErr   error // when set, listMessages fails with it

	// downloads records every media.download the connector actually made, so
	// a test can assert that a policy-refused blob was never fetched at all.
	downloads []string

	msgFilters   map[string][]string
	eventFilters map[string][]string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		members:      map[string][]*chat.Membership{},
		messages:     map[string][]*chat.Message{},
		events:       map[string][]*chat.SpaceEvent{},
		gets:         map[string]*chat.Message{},
		media:        map[string][]byte{},
		msgFilters:   map[string][]string{},
		eventFilters: map[string][]string{},
	}
}

func (f *fakeAPI) probeSpaces(context.Context) error { return nil }

func (f *fakeAPI) listSpaces(context.Context, string) (*chat.ListSpacesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &chat.ListSpacesResponse{Spaces: f.spaces}, nil
}

func (f *fakeAPI) listMembers(_ context.Context, space, _ string) (*chat.ListMembershipsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &chat.ListMembershipsResponse{Memberships: f.members[space]}, nil
}

func (f *fakeAPI) listMessages(_ context.Context, space, filter, pageToken string, showDeleted bool) (*chat.ListMessagesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.msgErr != nil {
		return nil, f.msgErr
	}
	if pageToken == "" {
		f.msgFilters[space] = append(f.msgFilters[space], filter)
	}
	var after time.Time
	if filter != "" {
		i, j := strings.Index(filter, `"`), strings.LastIndex(filter, `"`)
		t, err := time.Parse(time.RFC3339Nano, filter[i+1:j])
		if err != nil {
			return nil, fmt.Errorf("fake: bad filter %q: %w", filter, err)
		}
		after = t
	}
	var sel []*chat.Message
	for _, m := range f.messages[space] {
		if !showDeleted && m.DeleteTime != "" {
			continue
		}
		ct, err := time.Parse(time.RFC3339Nano, m.CreateTime)
		if err != nil {
			// A malformed createTime still comes back from the server; the
			// connector's mapping layer owns rejecting it (poison tests).
			sel = append(sel, m)
			continue
		}
		if after.IsZero() || ct.After(after) { // server semantics: strict '>'
			sel = append(sel, m)
		}
	}
	sort.SliceStable(sel, func(i, j int) bool { return sel[i].CreateTime < sel[j].CreateTime })

	start := 0
	if pageToken != "" {
		var err error
		if start, err = strconv.Atoi(pageToken); err != nil {
			return nil, err
		}
	}
	end := len(sel)
	next := ""
	if f.pageSize > 0 && start+f.pageSize < len(sel) {
		end = start + f.pageSize
		next = strconv.Itoa(end)
	}
	if start > len(sel) {
		start = len(sel)
	}
	return &chat.ListMessagesResponse{Messages: sel[start:end], NextPageToken: next}, nil
}

func (f *fakeAPI) getMessage(_ context.Context, name string) (*chat.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.gets[name]; ok {
		return m, nil
	}
	return nil, &googleapi.Error{Code: 404, Message: "not found"}
}

func (f *fakeAPI) listSpaceEvents(_ context.Context, space, filter, _ string) (*chat.ListSpaceEventsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventFilters[space] = append(f.eventFilters[space], filter)
	return &chat.ListSpaceEventsResponse{SpaceEvents: f.events[space]}, nil
}

func (f *fakeAPI) downloadMedia(_ context.Context, resourceName string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads = append(f.downloads, resourceName)
	b, ok := f.media[resourceName]
	if !ok {
		return nil, &googleapi.Error{Code: 404, Message: "no such media"}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// fakeDriveAPI implements driveAPI in memory.
type fakeDriveAPI struct {
	mu          sync.Mutex
	mimes       map[string]string // fileID → mimeType
	blobs       map[string][]byte // alt=media payloads
	exports     map[string][]byte // export payloads
	exportMimes map[string]string // fileID → last requested export MIME
}

func newFakeDriveAPI() *fakeDriveAPI {
	return &fakeDriveAPI{
		mimes:       map[string]string{},
		blobs:       map[string][]byte{},
		exports:     map[string][]byte{},
		exportMimes: map[string]string{},
	}
}

func (f *fakeDriveAPI) getFileMimeType(_ context.Context, fileID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.mimes[fileID]
	if !ok {
		return "", &googleapi.Error{Code: 404, Message: "no such file"}
	}
	return m, nil
}

func (f *fakeDriveAPI) downloadFile(_ context.Context, fileID string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.blobs[fileID]
	if !ok {
		return nil, &googleapi.Error{Code: 404, Message: "no media"}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeDriveAPI) exportFile(_ context.Context, fileID, mimeType string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.exports[fileID]
	if !ok {
		return nil, &googleapi.Error{Code: 404, Message: "no export"}
	}
	f.exportMimes[fileID] = mimeType
	return io.NopCloser(bytes.NewReader(b)), nil
}

// The single-account tests all archive as the "work" account: testSrc is the
// instance id every state row is keyed by, testTag the file tag every stem
// carries.
var (
	testSrc = state.InstanceID(state.SourceGChat, "work")
	testTag = state.Tag(testSrc)
)

// testAcct is the [[google]] block behind testSrc; mirrorAcct is the same
// account with mirror_drive_files on.
func testAcct() config.GoogleAccount {
	return config.GoogleAccount{Label: "work", Account: "you@example.com", Chat: true}
}

func mirrorAcct() config.GoogleAccount {
	a := testAcct()
	a.MirrorDriveFiles = true
	return a
}

func newTestConnector(t *testing.T, api chatAPI, acct config.GoogleAccount, opts ...Option) (*Connector, *state.DB, *archive.Writer) {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	w := &archive.Writer{Root: filepath.Join(dir, "archive"), TZ: mustZone(t, "Europe/Amsterdam")}
	return connectorOn(t, api, acct, db, w, opts...), db, w
}

// connectorOn builds a connector for one account on an EXISTING store and
// archive root, so several accounts can share them (as they do in reality).
func connectorOn(t *testing.T, api chatAPI, acct config.GoogleAccount, db *state.DB, w *archive.Writer, opts ...Option) *Connector {
	t.Helper()
	lim := ratelimit.NewKeyed(1e6, 1000, 0, 0)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Tests must not depend on how full the machine's disk is: "unknown"
	// disables the free-space floor. Later options win, so a floor test can
	// override this.
	opts = append([]Option{WithFreeSpace(func(string) int64 { return 0 })}, opts...)
	return newConnector(state.InstanceID(state.SourceGChat, acct.Label), acct, db, w, lim, log, api, nil, opts...)
}

func seedFake(f *fakeAPI) {
	f.spaces = []*chat.Space{{
		Name:              "spaces/AAA",
		SpaceType:         "SPACE",
		DisplayName:       "Team Platform",
		SpaceHistoryState: "HISTORY_ON",
	}}
	f.members["spaces/AAA"] = []*chat.Membership{
		{Member: &chat.User{Name: "users/1", DisplayName: "Jane Doe"}},
	}
	f.messages["spaces/AAA"] = []*chat.Message{
		{
			Name:       "spaces/AAA/messages/U1",
			CreateTime: "2026-08-06T20:00:00Z", // local 22:00 → day 2026-08-06
			Sender:     &chat.User{Name: "users/1"},
			Text:       "first message",
		},
		{
			Name:       "spaces/AAA/messages/U2",
			CreateTime: "2026-08-06T22:30:00Z", // local 00:30 → day 2026-08-07
			Sender:     &chat.User{Name: "users/1"},
			Text:       "message with attachment",
			Attachment: []*chat.Attachment{{
				Source:            "UPLOADED_CONTENT",
				ContentName:       "notes.pdf",
				AttachmentDataRef: &chat.AttachmentDataRef{ResourceName: "media-1"},
			}},
		},
	}
	f.media["media-1"] = []byte("%PDF-fake-bytes")
}

func readFile(t *testing.T, root, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

func TestSyncEndToEnd(t *testing.T) {
	fake := newFakeAPI()
	fake.pageSize = 1 // force multi-page listing
	seedFake(fake)
	c, db, w := newTestConnector(t, fake, testAcct())
	ctx := context.Background()

	if err := c.Sync(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Space registered with the frozen slug.
	sp, ok, err := db.GetSpace(testSrc, "spaces/AAA")
	if err != nil || !ok {
		t.Fatalf("get space: %v %v", ok, err)
	}
	if sp.DisplaySlug != "team-platform" || sp.Type != "space" {
		t.Fatalf("space slug/type = %q/%q", sp.DisplaySlug, sp.Type)
	}

	// Cursor advanced to the max createTime.
	wantCursor := formatCursor(time.Date(2026, 8, 6, 22, 30, 0, 0, time.UTC))
	cur, ok, err := db.GetCursor(testSrc, "spaces/AAA", state.CursorMsgCreateTime)
	if err != nil || !ok || cur != wantCursor {
		t.Fatalf("cursor = %q (%v, %v), want %q", cur, ok, err, wantCursor)
	}

	// Rows bucketed across midnight in the pinned zone.
	day1, err := db.MessagesForDay(testSrc, "spaces/AAA", "2026-08-06")
	if err != nil || len(day1) != 1 || day1[0].Name != "spaces/AAA/messages/U1" {
		t.Fatalf("day1 rows: %v %v", day1, err)
	}
	day2, err := db.MessagesForDay(testSrc, "spaces/AAA", "2026-08-07")
	if err != nil || len(day2) != 1 || day2[0].Name != "spaces/AAA/messages/U2" {
		t.Fatalf("day2 rows: %v %v", day2, err)
	}

	// Attachment downloaded to its deterministic path and marked done.
	atts, err := db.AttachmentsForMessage(testSrc, "spaces/AAA/messages/U2")
	if err != nil || len(atts) != 1 {
		t.Fatalf("attachments: %v %v", atts, err)
	}
	if atts[0].Status != state.AttachmentDone {
		t.Fatalf("attachment status = %q (last error %q)", atts[0].Status, atts[0].LastError)
	}
	if got := readFile(t, w.Root, atts[0].RelPath); !bytes.Equal(got, fake.media["media-1"]) {
		t.Fatalf("attachment bytes = %q", got)
	}

	// Day files rendered: attachment link (not the unavailable marker) and
	// the resolved sender name.
	stem := naming.ChatStem(testTag, "space", "team-platform", naming.Hash8("spaces/AAA"))
	day2Rel := "2026/08/07/" + stem + ".md"
	day2MD := readFile(t, w.Root, day2Rel)
	if !bytes.Contains(day2MD, []byte("Jane Doe")) {
		t.Fatalf("day file lacks resolved sender:\n%s", day2MD)
	}
	if !bytes.Contains(day2MD, []byte("notes.pdf")) || bytes.Contains(day2MD, []byte("unavailable")) {
		t.Fatalf("day file attachment rendering wrong:\n%s", day2MD)
	}
	day1MD := readFile(t, w.Root, "2026/08/06/"+stem+".md")
	if !bytes.Contains(day1MD, []byte("first message")) {
		t.Fatalf("day1 file lacks message text:\n%s", day1MD)
	}

	// Nothing left dirty.
	dirty, err := db.DirtyDayFiles(testSrc)
	if err != nil || len(dirty) != 0 {
		t.Fatalf("dirty after sync: %v %v", dirty, err)
	}

	// Second run: rename the space upstream — the frozen slug must hold and
	// the run must be a byte-level no-op on the archive.
	fake.mu.Lock()
	fake.spaces[0].DisplayName = "Team Platform Renamed"
	fake.mu.Unlock()

	if err := c.Sync(ctx); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	sp2, _, err := db.GetSpace(testSrc, "spaces/AAA")
	if err != nil {
		t.Fatal(err)
	}
	if sp2.DisplaySlug != "team-platform" {
		t.Fatalf("slug moved on rename: %q", sp2.DisplaySlug)
	}
	if sp2.DisplayName != "Team Platform Renamed" {
		t.Fatalf("display name not refreshed: %q", sp2.DisplayName)
	}
	cur2, _, err := db.GetCursor(testSrc, "spaces/AAA", state.CursorMsgCreateTime)
	if err != nil || cur2 != wantCursor {
		t.Fatalf("cursor changed on idempotent rerun: %q", cur2)
	}
	// Day-2 rows unchanged (the overlap refetch deduped).
	day2Again, err := db.MessagesForDay(testSrc, "spaces/AAA", "2026-08-07")
	if err != nil || len(day2Again) != 1 {
		t.Fatalf("day2 after rerun: %v %v", day2Again, err)
	}

	// The second listing used the rewound filter.
	fake.mu.Lock()
	filters := append([]string(nil), fake.msgFilters["spaces/AAA"]...)
	fake.mu.Unlock()
	if len(filters) != 2 {
		t.Fatalf("filters seen: %v", filters)
	}
	if filters[0] != "" {
		t.Fatalf("first listing must be unfiltered, got %q", filters[0])
	}
	wantFilter := listFilter(time.Date(2026, 8, 6, 22, 30, 0, 0, time.UTC))
	if filters[1] != wantFilter {
		t.Fatalf("second listing filter = %q, want %q", filters[1], wantFilter)
	}
}

// TestSyncPicksUpEqualTimestampMessage is the end-to-end half of the rewind
// regression: a message sharing the stored cursor's exact createTime, seen
// only after the cursor advanced, must be ingested on the next run (the
// fake's filter uses the server's strict '>' semantics).
func TestSyncPicksUpEqualTimestampMessage(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake)
	c, db, _ := newTestConnector(t, fake, testAcct())
	ctx := context.Background()

	if err := c.Sync(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// A message with the cursor's exact timestamp arrives late.
	fake.mu.Lock()
	fake.messages["spaces/AAA"] = append(fake.messages["spaces/AAA"], &chat.Message{
		Name:       "spaces/AAA/messages/U3",
		CreateTime: "2026-08-06T22:30:00Z", // == stored cursor
		Sender:     &chat.User{Name: "users/1"},
		Text:       "same-second straggler",
	})
	fake.mu.Unlock()

	if err := c.Sync(ctx); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	day2, err := db.MessagesForDay(testSrc, "spaces/AAA", "2026-08-07")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(day2))
	for i, m := range day2 {
		names[i] = m.Name
	}
	if len(day2) != 2 {
		t.Fatalf("straggler with cursor-equal createTime was skipped (plain '>' bug): rows %v", names)
	}
}

func TestSyncEventsPass(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake)
	c, db, w := newTestConnector(t, fake, testAcct())
	ctx := context.Background()

	if err := c.Sync(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// An edit of U1 and a deletion of U2 arrive as space events.
	edited := &chat.Message{
		Name:           "spaces/AAA/messages/U1",
		CreateTime:     "2026-08-06T20:00:00Z",
		LastUpdateTime: "2026-08-07T05:00:00Z",
		Sender:         &chat.User{Name: "users/1"},
		Text:           "first message (now edited)",
	}
	fake.mu.Lock()
	fake.gets["spaces/AAA/messages/U1"] = edited
	fake.events["spaces/AAA"] = []*chat.SpaceEvent{
		{
			EventType: "google.workspace.chat.message.v1.updated",
			EventTime: "2026-08-07T05:00:01Z",
			MessageUpdatedEventData: &chat.MessageUpdatedEventData{
				Message: &chat.Message{Name: "spaces/AAA/messages/U1"},
			},
		},
		{
			EventType: "google.workspace.chat.message.v1.deleted",
			EventTime: "2026-08-07T06:00:00Z",
			MessageDeletedEventData: &chat.MessageDeletedEventData{
				Message: &chat.Message{
					Name:       "spaces/AAA/messages/U2",
					CreateTime: "2026-08-06T22:30:00Z",
				},
			},
		},
	}
	fake.mu.Unlock()

	if err := c.Sync(ctx); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	// U1: edited revision replaces raw_json, edited flag set.
	day1, err := db.MessagesForDay(testSrc, "spaces/AAA", "2026-08-06")
	if err != nil || len(day1) != 1 {
		t.Fatalf("day1: %v %v", day1, err)
	}
	if !day1[0].Edited {
		t.Fatal("U1 not flagged edited after update event")
	}
	if !strings.Contains(day1[0].RawJSON, "now edited") {
		t.Fatalf("U1 raw_json not refreshed: %s", day1[0].RawJSON)
	}

	// U2: tombstoned, but the archived content survives in raw_json.
	day2, err := db.MessagesForDay(testSrc, "spaces/AAA", "2026-08-07")
	if err != nil || len(day2) != 1 {
		t.Fatalf("day2: %v %v", day2, err)
	}
	if !day2[0].Deleted || day2[0].DeletedAt.IsZero() {
		t.Fatalf("U2 not tombstoned: %+v", day2[0])
	}
	if !strings.Contains(day2[0].RawJSON, "message with attachment") {
		t.Fatalf("U2 archived raw_json was clobbered by the tombstone: %s", day2[0].RawJSON)
	}

	// Day files re-rendered.
	stem := naming.ChatStem(testTag, "space", "team-platform", naming.Hash8("spaces/AAA"))
	day1MD := readFile(t, w.Root, "2026/08/06/"+stem+".md")
	if !bytes.Contains(day1MD, []byte("(edited)")) || !bytes.Contains(day1MD, []byte("now edited")) {
		t.Fatalf("day1 render missing edit:\n%s", day1MD)
	}
	day2MD := readFile(t, w.Root, "2026/08/07/"+stem+".md")
	if !bytes.Contains(day2MD, []byte("message deleted")) {
		t.Fatalf("day2 render missing tombstone:\n%s", day2MD)
	}

	// Events cursor recorded.
	if _, ok, err := db.GetCursor(testSrc, "spaces/AAA", cursorEventsTime); err != nil || !ok {
		t.Fatalf("events cursor missing: %v %v", ok, err)
	}
}

// TestAttachmentGonePermanentlyFails: a 404 on media.download is ItemGone —
// the row fails permanently instead of being retried forever, and the run
// keeps going.
func TestAttachmentGonePermanentlyFails(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake)
	fake.mu.Lock()
	delete(fake.media, "media-1")
	fake.mu.Unlock()

	c, db, _ := newTestConnector(t, fake, testAcct())
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync must survive a gone attachment: %v", err)
	}
	atts, err := db.AttachmentsForMessage(testSrc, "spaces/AAA/messages/U2")
	if err != nil || len(atts) != 1 {
		t.Fatalf("attachments: %v %v", atts, err)
	}
	if atts[0].Status != state.AttachmentFailed {
		t.Fatalf("status = %q, want failed", atts[0].Status)
	}
}

// TestDMSlugFallback: DMs have no display name; the frozen slug falls back
// to "dm".
func TestDMSlugFallback(t *testing.T) {
	fake := newFakeAPI()
	fake.spaces = []*chat.Space{{Name: "spaces/DM1", SpaceType: "DIRECT_MESSAGE"}}
	c, db, _ := newTestConnector(t, fake, testAcct())
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	sp, ok, err := db.GetSpace(testSrc, "spaces/DM1")
	if err != nil || !ok {
		t.Fatalf("get space: %v %v", ok, err)
	}
	if sp.Type != "dm" || sp.DisplaySlug != "dm" {
		t.Fatalf("dm space type/slug = %q/%q", sp.Type, sp.DisplaySlug)
	}
}

func TestCheckProbes(t *testing.T) {
	fake := newFakeAPI()
	c, _, _ := newTestConnector(t, fake, testAcct())
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	// Name is the INSTANCE id (the key of this account's cursors, failures
	// and runs), never the bare kind.
	if c.Name() != testSrc {
		t.Fatalf("name = %q, want the instance id %q", c.Name(), testSrc)
	}
}

// TestSlugReclaimOnEmptyDB: deleting state.db while keeping the archive is a
// documented recovery path. On the next run the identities frozen in
// existing day-file names — slug AND space type, both path components — must
// be reclaimed, not re-derived from the space's current display name, or a
// renamed space would split its history into a second file series.
func TestSlugReclaimOnEmptyDB(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake) // current display name "Team Platform" would derive "team-platform"/"space"
	c, db, w := newTestConnector(t, fake, testAcct())

	// A previous life of this archive froze a different identity: the space
	// was named differently and was a group chat when first seen.
	hash := naming.Hash8("spaces/AAA")
	stem := naming.ChatStem(testTag, "group", "old-name", hash)
	dayDir := filepath.Join(w.Root, "2026", "07", "01")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dayDir, stem+".md"), []byte("old day file"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	sp, ok, err := db.GetSpace(testSrc, "spaces/AAA")
	if err != nil || !ok {
		t.Fatalf("get space: %v %v", ok, err)
	}
	if sp.Type != "group" || sp.DisplaySlug != "old-name" {
		t.Fatalf("frozen identity = %q/%q, want group/old-name reclaimed from the on-disk stem", sp.Type, sp.DisplaySlug)
	}
	if sp.DisplayName != "Team Platform" {
		t.Fatalf("display name must still track the API: %q", sp.DisplayName)
	}
	// New day files extend the SAME file series.
	if _, err := os.Stat(filepath.Join(w.Root, "2026", "08", "06", stem+".md")); err != nil {
		t.Fatalf("new day file not under the reclaimed stem: %v", err)
	}
}

// TestSlugReclaimIgnoresOtherAccounts: the reclaim scan sees every account's
// day files, but an identity frozen in ANOTHER account's file series must
// never be reclaimed into this account's state — the two archives are
// independent, and a slug is frozen per (account, space).
func TestSlugReclaimIgnoresOtherAccounts(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake) // "Team Platform" derives team-platform/space
	personal := config.GoogleAccount{Label: "personal", Account: "you@example.net", Chat: true}
	c, db, w := newTestConnector(t, fake, personal)

	// The work account already archived this very space under a different
	// frozen identity.
	foreign := naming.ChatStem("gchat-work", "group", "old-name", naming.Hash8("spaces/AAA"))
	dayDir := filepath.Join(w.Root, "2026", "07", "01")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dayDir, foreign+".md"), []byte("work day file"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	sp, ok, err := db.GetSpace(state.InstanceID(state.SourceGChat, "personal"), "spaces/AAA")
	if err != nil || !ok {
		t.Fatalf("get space: %v %v", ok, err)
	}
	if sp.Type != "space" || sp.DisplaySlug != "team-platform" {
		t.Fatalf("identity = %q/%q, want the freshly derived space/team-platform — another account's stem was reclaimed", sp.Type, sp.DisplaySlug)
	}
}

// TestTwoAccountsShareOneSpace: two configured Google accounts can both be
// members of the SAME space, seeing the same space and message resource
// names. Each must archive its own independent copy — space, message, member,
// cursor and day-file rows keyed by its own instance id, files stemmed with
// its own tag — with nothing shared, deduplicated or overwritten.
func TestTwoAccountsShareOneSpace(t *testing.T) {
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	w := &archive.Writer{Root: filepath.Join(dir, "archive"), TZ: mustZone(t, "Europe/Amsterdam")}

	// Two accounts, two API sessions, one shared space with identical
	// resource names. Each session resolves the sender's display name from
	// its own directory, so a leaked member cache is visible in the output.
	accts := []config.GoogleAccount{
		{Label: "work", Account: "you@example.com", Chat: true},
		{Label: "personal", Account: "you@example.net", Chat: true},
	}
	names := map[string]string{"work": "Jane Doe", "personal": "J. Doe (external)"}
	srcs := map[string]string{}
	for _, a := range accts {
		srcs[a.Label] = state.InstanceID(state.SourceGChat, a.Label)
		fake := newFakeAPI()
		seedFake(fake)
		fake.members["spaces/AAA"] = []*chat.Membership{
			{Member: &chat.User{Name: "users/1", DisplayName: names[a.Label]}},
		}
		if err := connectorOn(t, fake, a, db, w).Sync(context.Background()); err != nil {
			t.Fatalf("%s sync: %v", a.Label, err)
		}
	}

	hash := naming.Hash8("spaces/AAA") // the space hash is shared by design
	for _, a := range accts {
		src := srcs[a.Label]
		sp, ok, err := db.GetSpace(src, "spaces/AAA")
		if err != nil || !ok {
			t.Fatalf("%s: get space: %v %v", a.Label, ok, err)
		}
		if sp.Source != src {
			t.Fatalf("%s: space row source = %q, want %q", a.Label, sp.Source, src)
		}
		// Messages, cursor and member cache are this account's own.
		msgs, err := db.MessagesForDay(src, "spaces/AAA", "2026-08-07")
		if err != nil || len(msgs) != 1 || msgs[0].Name != "spaces/AAA/messages/U2" {
			t.Fatalf("%s: day rows = %+v (%v)", a.Label, msgs, err)
		}
		if msgs[0].Source != src {
			t.Fatalf("%s: message row source = %q", a.Label, msgs[0].Source)
		}
		if _, ok, err := db.GetCursor(src, "spaces/AAA", state.CursorMsgCreateTime); err != nil || !ok {
			t.Fatalf("%s: cursor missing: %v %v", a.Label, ok, err)
		}
		if got, _, err := db.GetMember(src, "spaces/AAA", "users/1"); err != nil || got != names[a.Label] {
			t.Fatalf("%s: member cache = %q (%v), want %q", a.Label, got, err, names[a.Label])
		}

		// Files: same space hash, different tag — two independent series.
		stem := naming.ChatStem(state.Tag(src), "space", "team-platform", hash)
		if !strings.HasPrefix(stem, "gchat-"+a.Label+"_") {
			t.Fatalf("%s: stem = %q, want the account's file tag", a.Label, stem)
		}
		md := readFile(t, w.Root, "2026/08/07/"+stem+".md")
		if !bytes.Contains(md, []byte(names[a.Label])) {
			t.Fatalf("%s: day file lacks this account's sender name:\n%s", a.Label, md)
		}
		if !bytes.Contains(md, []byte(src)) || !bytes.Contains(md, []byte("account_label: "+a.Label)) {
			t.Fatalf("%s: day file frontmatter lacks this account's provenance:\n%s", a.Label, md)
		}

		// Attachments: each account downloaded its own copy, under its own
		// stem's sibling directory.
		atts, err := db.AttachmentsForMessage(src, "spaces/AAA/messages/U2")
		if err != nil || len(atts) != 1 {
			t.Fatalf("%s: attachments = %+v (%v)", a.Label, atts, err)
		}
		if atts[0].Status != state.AttachmentDone {
			t.Fatalf("%s: attachment status = %q (%s)", a.Label, atts[0].Status, atts[0].LastError)
		}
		if !strings.Contains(atts[0].RelPath, naming.AttachDir(stem)+"/") {
			t.Fatalf("%s: attachment rel = %q, want it under %q", a.Label, atts[0].RelPath, naming.AttachDir(stem))
		}
		if got := readFile(t, w.Root, atts[0].RelPath); !bytes.Equal(got, []byte("%PDF-fake-bytes")) {
			t.Fatalf("%s: attachment bytes = %q", a.Label, got)
		}
	}

	// The two day-file series are distinct files, and no instance id (with
	// its colon) leaked into any path.
	stemA := naming.ChatStem(state.Tag(srcs["work"]), "space", "team-platform", hash)
	stemB := naming.ChatStem(state.Tag(srcs["personal"]), "space", "team-platform", hash)
	if stemA == stemB {
		t.Fatalf("both accounts render to the same stem %q", stemA)
	}
	var mdFiles []string
	if err := filepath.WalkDir(w.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.ContainsRune(d.Name(), ':') {
			t.Fatalf("instance id leaked into a path: %s", p)
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			mdFiles = append(mdFiles, d.Name())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(mdFiles) != 4 { // 2 days × 2 accounts
		t.Fatalf("day files = %v, want two independent 2-day series", mdFiles)
	}

	// Both ledgers are clean: neither account cleared the other's dirty rows,
	// and neither left its own behind.
	dirty, err := db.AllDirtyDayFiles()
	if err != nil || len(dirty) != 0 {
		t.Fatalf("dirty day files after both syncs: %+v %v", dirty, err)
	}
}

// TestForeignBacklogDoesNotStarveDownloads: the pending-download ledger is
// shared by every configured account and is served unfiltered, so a sibling
// account mid-backfill can fill an entire due-row batch with rows this
// connector must not touch. Its own attachments must still download on this
// pass, and the sibling's rows must be left strictly alone.
func TestForeignBacklogDoesNotStarveDownloads(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake)
	work := config.GoogleAccount{Label: "work", Account: "you@example.com", Chat: true}
	c, db, w := newTestConnector(t, fake, work)

	// A full batch of personal rows is due; "gchat:personal" sorts ahead of
	// every "gchat:work" row, so an unwidened window would see nothing else.
	other := state.InstanceID(state.SourceGChat, "personal")
	for i := 0; i < attachmentBatch; i++ {
		id := fmt.Sprintf("spaces/BBB/messages/%04d", i)
		if err := db.UpsertPendingAttachment(other, id, "media-"+strconv.Itoa(i), "2026/08/07/other.d/f"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	atts, err := db.AttachmentsForMessage(state.InstanceID(state.SourceGChat, "work"), "spaces/AAA/messages/U2")
	if err != nil || len(atts) != 1 {
		t.Fatalf("attachments: %+v %v", atts, err)
	}
	if atts[0].Status != state.AttachmentDone {
		t.Fatalf("attachment status = %q: a sibling account's backlog starved this account's downloads", atts[0].Status)
	}
	if got := readFile(t, w.Root, atts[0].RelPath); !bytes.Equal(got, fake.media["media-1"]) {
		t.Fatalf("attachment bytes = %q", got)
	}
	// The sibling's rows are none of this connector's business.
	foreign, err := db.AttachmentsForMessage(other, "spaces/BBB/messages/0000")
	if err != nil || len(foreign) != 1 {
		t.Fatalf("foreign rows: %+v %v", foreign, err)
	}
	if foreign[0].Status != state.AttachmentPending || foreign[0].Attempts != 0 {
		t.Fatalf("another account's row was touched: %+v", foreign[0])
	}
}

// TestTransientListErrorAbortsRunWithoutPoison: a network/server failure
// surviving retry is environmental — it must abort the run with cursors
// unadvanced, never be recorded against the space (5 flaky runs used to
// permanently skip the whole space).
func TestTransientListErrorAbortsRunWithoutPoison(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake)
	fake.msgErr = &googleapi.Error{Code: 503, Message: "backend flake"}
	c, db, _ := newTestConnector(t, fake, testAcct())
	c.retryOpts.MaxAttempts = 1 // don't sit out retry backoff in tests

	if err := c.Sync(context.Background()); err == nil {
		t.Fatal("a Transient listing failure surviving retry must abort the run")
	}
	fails, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 0 {
		t.Fatalf("environmental trouble poisoned the failures ledger: %+v", fails)
	}
	if _, ok, err := db.GetCursor(testSrc, "spaces/AAA", state.CursorMsgCreateTime); err != nil || ok {
		t.Fatalf("cursor advanced on an aborted pass: %v %v", ok, err)
	}
}

// TestStoreErrorIsRunFatalNotPoison: state-DB failures abort the run instead
// of accruing in the failures ledger (~10 minutes of DB trouble used to
// permanently disable whole spaces).
func TestStoreErrorIsRunFatalNotPoison(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake)
	c, db, _ := newTestConnector(t, fake, testAcct())
	ctx := context.Background()

	boom := storeFatal(errors.New("disk I/O error"))
	if err := c.runItemStep(ctx, "spaces/AAA", func() error { return boom }); !errors.Is(err, errStore) {
		t.Fatalf("runItemStep absorbed a store failure as item poison: %v", err)
	}
	fails, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 0 {
		t.Fatalf("store failure recorded in the failures ledger: %+v", fails)
	}

	// And syncSpace really wraps store-layer errors that way: with the store
	// broken the space pass surfaces errStore rather than a ledger row.
	sp := spaceMeta{Name: "spaces/AAA", Type: "space", Slug: "team-platform"}
	if err := db.UpsertSpace(testSrc, sp.Name, sp.Type, "Team Platform", sp.Slug, "HISTORY_ON"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := c.syncSpace(ctx, sp); !errors.Is(err, errStore) {
		t.Fatalf("syncSpace store error = %v, want errStore", err)
	}
}

// TestPoisonMessageRetriedUntilSkipListed: a message that fails to map must
// freeze the cursor at the last good message (never be silently lost), be
// retried every run with its ledger entry growing, and past SkipThreshold be
// passed over so later messages advance the cursor again.
func TestPoisonMessageRetriedUntilSkipListed(t *testing.T) {
	fake := newFakeAPI()
	fake.spaces = []*chat.Space{{
		Name: "spaces/AAA", SpaceType: "SPACE",
		DisplayName: "Team Platform", SpaceHistoryState: "HISTORY_ON",
	}}
	fake.messages["spaces/AAA"] = []*chat.Message{
		{Name: "spaces/AAA/messages/U1", CreateTime: "2026-08-06T20:00:00Z", Sender: &chat.User{Name: "users/1"}, Text: "good"},
		{Name: "spaces/AAA/messages/BAD", CreateTime: "not-a-time"},
	}
	c, db, _ := newTestConnector(t, fake, testAcct())
	ctx := context.Background()

	wantCursor := formatCursor(time.Date(2026, 8, 6, 20, 0, 0, 0, time.UTC))
	for run := 1; run <= state.SkipThreshold; run++ {
		if err := c.Sync(ctx); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		cur, ok, err := db.GetCursor(testSrc, "spaces/AAA", state.CursorMsgCreateTime)
		if err != nil || !ok || cur != wantCursor {
			t.Fatalf("run %d: cursor = %q (%v %v), want frozen at the last good message %q", run, cur, ok, err, wantCursor)
		}
		fails, err := db.ListFailures()
		if err != nil {
			t.Fatal(err)
		}
		if len(fails) != 1 || fails[0].ID != "spaces/AAA/messages/BAD" || fails[0].Attempts != run {
			t.Fatalf("run %d: ledger = %+v, want BAD at %d attempts", run, fails, run)
		}
	}

	// Past the threshold: BAD is passed over for good and a later message
	// advances the cursor beyond it.
	fake.mu.Lock()
	fake.messages["spaces/AAA"] = append(fake.messages["spaces/AAA"], &chat.Message{
		Name: "spaces/AAA/messages/U9", CreateTime: "2026-08-06T22:30:00Z", Sender: &chat.User{Name: "users/1"}, Text: "after the poison",
	})
	fake.mu.Unlock()
	if err := c.Sync(ctx); err != nil {
		t.Fatalf("post-threshold sync: %v", err)
	}
	cur, _, err := db.GetCursor(testSrc, "spaces/AAA", state.CursorMsgCreateTime)
	if err != nil || cur != formatCursor(time.Date(2026, 8, 6, 22, 30, 0, 0, time.UTC)) {
		t.Fatalf("cursor after skip-listing = %q (%v), want advanced past the poison message", cur, err)
	}
	day, err := db.MessagesForDay(testSrc, "spaces/AAA", "2026-08-07") // 22:30Z is 00:30 local next day
	if err != nil || len(day) != 1 || day[0].Name != "spaces/AAA/messages/U9" {
		t.Fatalf("message after the skip-listed poison not ingested: %v %v", day, err)
	}
	fails, err := db.ListFailures()
	if err != nil || len(fails) != 1 || fails[0].Attempts != state.SkipThreshold {
		t.Fatalf("skip-listed message must stop accruing attempts: %+v %v", fails, err)
	}
}

// TestLateMemberFillRerendersDays: a member refresh that resolves a
// previously unknown sender must dirty every day that sender appears on, so
// files rendered with opaque user ids heal on the same run's render pass.
func TestLateMemberFillRerendersDays(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake)
	fake.members = map[string][]*chat.Membership{} // this run resolves nobody
	c, db, w := newTestConnector(t, fake, testAcct())
	ctx := context.Background()

	if err := c.Sync(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	stem := naming.ChatStem(testTag, "space", "team-platform", naming.Hash8("spaces/AAA"))
	day1 := readFile(t, w.Root, "2026/08/06/"+stem+".md")
	if !bytes.Contains(day1, []byte("users/1")) {
		t.Fatalf("day file should carry the opaque id while the cache is empty:\n%s", day1)
	}

	// The member listing recovers: the refresh alone must dirty the sender's
	// days (regression: SetMember used to fill the cache without ever
	// invalidating already-rendered files).
	fake.mu.Lock()
	fake.members["spaces/AAA"] = []*chat.Membership{
		{Member: &chat.User{Name: "users/1", DisplayName: "Jane Doe"}},
	}
	fake.mu.Unlock()
	c.refreshMembers(ctx, spaceMeta{Name: "spaces/AAA", Type: "space", Slug: "team-platform"})
	dirty, err := db.DirtyDayFiles(testSrc)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 2 {
		t.Fatalf("late cache fill dirtied %d days, want both days users/1 appears on: %+v", len(dirty), dirty)
	}

	if err := c.Sync(ctx); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	day1 = readFile(t, w.Root, "2026/08/06/"+stem+".md")
	day2 := readFile(t, w.Root, "2026/08/07/"+stem+".md")
	if !bytes.Contains(day1, []byte("Jane Doe")) || !bytes.Contains(day2, []byte("Jane Doe")) {
		t.Fatalf("late member fill did not re-render the days:\nday1: %s\nday2: %s", day1, day2)
	}
}

// TestEventsCursorTracksServerEventTime: the events cursor must advance to
// the max server eventTime actually applied — never to the local clock,
// which may run ahead of the server and would then permanently hide events —
// and the next listing must overlap it by eventsRewind.
func TestEventsCursorTracksServerEventTime(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake)
	c, db, _ := newTestConnector(t, fake, testAcct())
	ctx := context.Background()
	c.now = func() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) } // clock well ahead of every event

	// No events observed: no cursor is invented from the local clock.
	if err := c.Sync(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, ok, err := db.GetCursor(testSrc, "spaces/AAA", cursorEventsTime); err != nil || ok {
		t.Fatalf("events cursor set with no events observed: %v %v", ok, err)
	}

	// Two deletions arrive: the cursor must land on their max eventTime.
	fake.mu.Lock()
	fake.events["spaces/AAA"] = []*chat.SpaceEvent{
		{
			EventType: "google.workspace.chat.message.v1.deleted",
			EventTime: "2026-08-07T05:00:01Z",
			MessageDeletedEventData: &chat.MessageDeletedEventData{
				Message: &chat.Message{Name: "spaces/AAA/messages/U1", CreateTime: "2026-08-06T20:00:00Z"},
			},
		},
		{
			EventType: "google.workspace.chat.message.v1.deleted",
			EventTime: "2026-08-07T06:00:00Z",
			MessageDeletedEventData: &chat.MessageDeletedEventData{
				Message: &chat.Message{Name: "spaces/AAA/messages/U2", CreateTime: "2026-08-06T22:30:00Z"},
			},
		},
	}
	fake.mu.Unlock()
	if err := c.Sync(ctx); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	want := formatCursor(time.Date(2026, 8, 7, 6, 0, 0, 0, time.UTC))
	cur, ok, err := db.GetCursor(testSrc, "spaces/AAA", cursorEventsTime)
	if err != nil || !ok || cur != want {
		t.Fatalf("events cursor = %q (%v %v), want max server eventTime %q, never the local clock", cur, ok, err, want)
	}

	// Replay pass: the listing overlaps the cursor by eventsRewind and the
	// re-observed events do not move it.
	if err := c.Sync(ctx); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	cur2, _, err := db.GetCursor(testSrc, "spaces/AAA", cursorEventsTime)
	if err != nil || cur2 != want {
		t.Fatalf("cursor moved on an idempotent replay: %q (%v)", cur2, err)
	}
	fake.mu.Lock()
	filters := append([]string(nil), fake.eventFilters["spaces/AAA"]...)
	fake.mu.Unlock()
	last := filters[len(filters)-1]
	wantStart := time.Date(2026, 8, 7, 6, 0, 0, 0, time.UTC).Add(-eventsRewind).Format(time.RFC3339)
	if !strings.Contains(last, wantStart) {
		t.Fatalf("replay listing filter %q lacks the rewound start %q", last, wantStart)
	}
}

// TestContinuedThreadHeader: a thread that began on an earlier day must
// render its continuation day with the "(continued)" header carrying the
// thread's original start time (min create_time across all days).
func TestContinuedThreadHeader(t *testing.T) {
	fake := newFakeAPI()
	fake.spaces = []*chat.Space{{
		Name: "spaces/AAA", SpaceType: "SPACE",
		DisplayName: "Team Platform", SpaceHistoryState: "HISTORY_ON",
	}}
	thread := &chat.Thread{Name: "spaces/AAA/threads/T1"}
	fake.messages["spaces/AAA"] = []*chat.Message{
		{Name: "spaces/AAA/messages/T1A", CreateTime: "2026-08-05T10:00:00Z", Sender: &chat.User{Name: "users/1"}, Thread: thread, Text: "thread opens"},
		{Name: "spaces/AAA/messages/T1B", CreateTime: "2026-08-06T09:00:00Z", Sender: &chat.User{Name: "users/1"}, Thread: thread, Text: "reply next day"},
	}
	c, _, w := newTestConnector(t, fake, testAcct())
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	stem := naming.ChatStem(testTag, "space", "team-platform", naming.Hash8("spaces/AAA"))
	day2 := readFile(t, w.Root, "2026/08/06/"+stem+".md")
	if !bytes.Contains(day2, []byte("(continued)")) {
		t.Fatalf("continuation day lacks the (continued) header:\n%s", day2)
	}
	if !bytes.Contains(day2, []byte("thread started 2026-08-05 12:00")) { // 10:00Z in Europe/Amsterdam
		t.Fatalf("continuation header lacks the original start time:\n%s", day2)
	}
}

// TestSyncMirrorsDriveFiles is the end-to-end regression for
// mirror_drive_files: with the option on, DRIVE_FILE attachments are fetched
// via the Drive API — export for Google-native types, alt=media for plain
// binaries — written at their deterministic paths, and the day file links the
// local copies (Drive URL kept secondary) instead of "(not mirrored)".
func TestSyncMirrorsDriveFiles(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake)
	fake.messages["spaces/AAA"] = append(fake.messages["spaces/AAA"], &chat.Message{
		Name:       "spaces/AAA/messages/D1",
		CreateTime: "2026-08-06T23:00:00Z", // local 01:00 → day 2026-08-07
		Sender:     &chat.User{Name: "users/1"},
		Text:       "drive attachments",
		Attachment: []*chat.Attachment{
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
		},
	})
	fakeDrive := newFakeDriveAPI()
	fakeDrive.mimes["drive-doc"] = "application/vnd.google-apps.document"
	// Real magic bytes: the policy sniffs what it stores, so a fixture that
	// only claims to be a docx/jpeg is (correctly) refused as a mismatch and
	// this test would stop being about Drive mirroring.
	docx, jpeg := docxBytes(t), jpegBytes()
	fakeDrive.exports["drive-doc"] = docx
	fakeDrive.mimes["drive-bin"] = "image/jpeg"
	fakeDrive.blobs["drive-bin"] = jpeg

	c, db, w := newTestConnector(t, fake, mirrorAcct())
	c.drive = fakeDrive
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	atts, err := db.AttachmentsForMessage(testSrc, "spaces/AAA/messages/D1")
	if err != nil || len(atts) != 2 {
		t.Fatalf("attachments: %+v %v", atts, err)
	}
	for _, a := range atts {
		if a.Status != state.AttachmentDone {
			t.Fatalf("attachment %s status = %q (last error %q)", a.PartKey, a.Status, a.LastError)
		}
	}
	// Rows come back ordered by part key: drive:drive-bin, drive:drive-doc.
	binRow, docRow := atts[0], atts[1]
	if !strings.HasSuffix(docRow.RelPath, "Design spec.docx") {
		t.Fatalf("doc rel = %q, want the export extension appended", docRow.RelPath)
	}
	if got := readFile(t, w.Root, docRow.RelPath); !bytes.Equal(got, docx) {
		t.Fatalf("doc bytes = %q", got)
	}
	if got := readFile(t, w.Root, binRow.RelPath); !bytes.Equal(got, jpeg) {
		t.Fatalf("binary bytes = %q", got)
	}
	fakeDrive.mu.Lock()
	exportMime := fakeDrive.exportMimes["drive-doc"]
	fakeDrive.mu.Unlock()
	if want := "application/vnd.openxmlformats-officedocument.wordprocessingml.document"; exportMime != want {
		t.Fatalf("export mime = %q, want %q", exportMime, want)
	}

	// The day file links the local copies; the Drive URL stays secondary.
	stem := naming.ChatStem(testTag, "space", "team-platform", naming.Hash8("spaces/AAA"))
	day2 := string(readFile(t, w.Root, "2026/08/07/"+stem+".md"))
	if !strings.Contains(day2, "Design spec.docx") || strings.Contains(day2, "not mirrored") {
		t.Fatalf("day file lacks the mirrored links:\n%s", day2)
	}
	if !strings.Contains(day2, "https://drive.google.com/open?id=drive-doc") {
		t.Fatalf("day file dropped the secondary Drive URL:\n%s", day2)
	}
}

// TestDriveNotExportableFailsPermanently: a Google-native type with no binary
// export (a Form) is a deterministic per-file condition — the row fails
// permanently on the first attempt instead of burning retries, and the run
// keeps going.
func TestDriveNotExportableFailsPermanently(t *testing.T) {
	fake := newFakeAPI()
	fake.spaces = []*chat.Space{{
		Name: "spaces/AAA", SpaceType: "SPACE",
		DisplayName: "Team Platform", SpaceHistoryState: "HISTORY_ON",
	}}
	fake.messages["spaces/AAA"] = []*chat.Message{{
		Name:       "spaces/AAA/messages/D1",
		CreateTime: "2026-08-06T20:00:00Z",
		Sender:     &chat.User{Name: "users/1"},
		Attachment: []*chat.Attachment{{
			Source:       "DRIVE_FILE",
			ContentName:  "Survey",
			ContentType:  "application/vnd.google-apps.form",
			DriveDataRef: &chat.DriveDataRef{DriveFileId: "drive-form"},
		}},
	}}
	fakeDrive := newFakeDriveAPI()
	fakeDrive.mimes["drive-form"] = "application/vnd.google-apps.form"

	c, db, _ := newTestConnector(t, fake, mirrorAcct())
	c.drive = fakeDrive
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync must survive an unexportable drive file: %v", err)
	}
	atts, err := db.AttachmentsForMessage(testSrc, "spaces/AAA/messages/D1")
	if err != nil || len(atts) != 1 {
		t.Fatalf("attachments: %+v %v", atts, err)
	}
	if atts[0].Status != state.AttachmentFailed || atts[0].Attempts != 1 {
		t.Fatalf("row = %+v, want failed permanently on the first attempt", atts[0])
	}
}

// TestAttachmentWriteFailureAbortsRun: an archive-write failure is disk
// trouble, not item poison — the run aborts and the pending row keeps its
// retry attempts intact.
func TestAttachmentWriteFailureAbortsRun(t *testing.T) {
	fake := newFakeAPI()
	seedFake(fake)
	c, db, w := newTestConnector(t, fake, testAcct())
	// Every archive write fails: the root path is a regular file.
	if err := os.WriteFile(w.Root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.Sync(context.Background()); err == nil {
		t.Fatal("archive-write failure must abort the run")
	}
	atts, err := db.AttachmentsForMessage(testSrc, "spaces/AAA/messages/U2")
	if err != nil || len(atts) != 1 {
		t.Fatalf("attachments: %v %v", atts, err)
	}
	if atts[0].Status != state.AttachmentPending || atts[0].Attempts != 0 {
		t.Fatalf("write failure consumed a retry attempt: status=%q attempts=%d", atts[0].Status, atts[0].Attempts)
	}
	fails, err := db.ListFailures()
	if err != nil || len(fails) != 0 {
		t.Fatalf("write failure poisoned the ledger: %+v %v", fails, err)
	}
}
