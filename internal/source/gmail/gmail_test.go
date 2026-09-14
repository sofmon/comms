package gmail

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gmailv1 "google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"

	"comms/internal/archive"
	"comms/internal/config"
	"comms/internal/naming"
	"comms/internal/ratelimit"
	"comms/internal/state"
)

// fakeAPI implements api with pluggable functions and per-id fetch counts.
type fakeAPI struct {
	profileFn func() (*gmailv1.Profile, error)
	listFn    func(pageToken string) (*gmailv1.ListMessagesResponse, error)
	getFn     func(id string) (*gmailv1.Message, error)
	historyFn func(hid uint64, token string) (*gmailv1.ListHistoryResponse, error)
	labelsFn  func() (*gmailv1.ListLabelsResponse, error)

	mu   sync.Mutex
	gets map[string]int
}

func (f *fakeAPI) GetProfile(context.Context) (*gmailv1.Profile, error) { return f.profileFn() }
func (f *fakeAPI) ListMessages(_ context.Context, tok string) (*gmailv1.ListMessagesResponse, error) {
	return f.listFn(tok)
}
func (f *fakeAPI) GetMessageRaw(_ context.Context, id string) (*gmailv1.Message, error) {
	f.mu.Lock()
	if f.gets == nil {
		f.gets = make(map[string]int)
	}
	f.gets[id]++
	f.mu.Unlock()
	return f.getFn(id)
}
func (f *fakeAPI) ListHistory(_ context.Context, hid uint64, tok string) (*gmailv1.ListHistoryResponse, error) {
	return f.historyFn(hid, tok)
}
func (f *fakeAPI) ListLabels(context.Context) (*gmailv1.ListLabelsResponse, error) {
	if f.labelsFn != nil {
		return f.labelsFn()
	}
	return &gmailv1.ListLabelsResponse{Labels: []*gmailv1.Label{
		{Id: "INBOX", Name: "INBOX"},
		{Id: "SPAM", Name: "SPAM"},
		{Id: "Label_1", Name: "Receipts"},
	}}, nil
}

func (f *fakeAPI) getCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets[id]
}

// listPages wires listFn from a token->response map ("" is the first page).
func listPages(pages map[string]*gmailv1.ListMessagesResponse) func(string) (*gmailv1.ListMessagesResponse, error) {
	return func(tok string) (*gmailv1.ListMessagesResponse, error) {
		p, ok := pages[tok]
		if !ok {
			return nil, fmt.Errorf("fake: unexpected list page token %q", tok)
		}
		return p, nil
	}
}

func listIDs(ids ...string) []*gmailv1.Message {
	out := make([]*gmailv1.Message, len(ids))
	for i, id := range ids {
		out[i] = &gmailv1.Message{Id: id, ThreadId: "t-" + id}
	}
	return out
}

var testTime = time.Date(2026, 8, 7, 9, 30, 0, 0, time.UTC)

func rawB64(subject string) string {
	raw := "From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Fri, 07 Aug 2026 11:30:00 +0200\r\n" +
		"Message-ID: <" + subject + "@example.com>\r\n" +
		"\r\n" +
		"Hello about " + subject + ".\r\n"
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func testMsg(id, subject string, labelIDs ...string) *gmailv1.Message {
	return &gmailv1.Message{
		Id:           id,
		ThreadId:     "t-" + id,
		InternalDate: testTime.UnixMilli(),
		LabelIds:     labelIDs,
		Raw:          rawB64(subject),
	}
}

// msgSet wires getFn from a fixed message set.
func msgSet(msgs ...*gmailv1.Message) func(string) (*gmailv1.Message, error) {
	byID := make(map[string]*gmailv1.Message, len(msgs))
	for _, m := range msgs {
		byID[m.Id] = m
	}
	return func(id string) (*gmailv1.Message, error) {
		m, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("fake: unexpected messages.get %q", id)
		}
		return m, nil
	}
}

func emptyHistory(currentHid uint64) func(uint64, string) (*gmailv1.ListHistoryResponse, error) {
	return func(uint64, string) (*gmailv1.ListHistoryResponse, error) {
		return &gmailv1.ListHistoryResponse{HistoryId: currentHid}, nil
	}
}

// testLabel is the account label every single-account test uses; testSource
// is the instance id its rows are keyed by.
const testLabel = "work"

var testSourceID = state.InstanceID(state.SourceGmail, testLabel)

// testAccount builds a [[google]] account block with the default test label.
func testAccount(address string) config.GoogleAccount {
	return config.GoogleAccount{Label: testLabel, Account: address, Gmail: true}
}

func newTestSource(t *testing.T, a api, ga config.GoogleAccount, opts ...Option) (*Source, *state.DB, string) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	root := t.TempDir()
	return newTestSourceIn(t, db, root, a, ga, opts...), db, root
}

// newTestSourceIn builds a connector on a caller-owned DB and archive root,
// so several accounts can share both.
func newTestSourceIn(t *testing.T, db *state.DB, root string, a api, ga config.GoogleAccount, opts ...Option) *Source {
	t.Helper()
	w := &archive.Writer{Root: root, TZ: time.UTC}
	lim := ratelimit.NewUnits(1e9, 1000)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Tests run on whatever volume TempDir lives on, which is usually far
	// above the 5 GB floor but need not be; pinning the probe keeps the
	// default-policy tests independent of the machine they run on.
	opts = append([]Option{WithFreeSpace(func(string) int64 { return 0 })}, opts...)
	s, err := newSource(state.InstanceID(state.SourceGmail, ga.Label), ga, db, w, lim, logger, a, opts...)
	if err != nil {
		t.Fatalf("newSource(%s): %v", ga.Label, err)
	}
	return s
}

func countMD(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return n
}

// cursor reads a cursor of the default test instance.
func cursor(t *testing.T, db *state.DB, kind string) (string, bool) {
	t.Helper()
	return cursorOf(t, db, testSourceID, kind)
}

func cursorOf(t *testing.T, db *state.DB, source, kind string) (string, bool) {
	t.Helper()
	v, ok, err := db.GetCursor(source, "", kind)
	if err != nil {
		t.Fatalf("get cursor %s/%s: %v", source, kind, err)
	}
	return v, ok
}

func TestBackfillArchivesInScopeAndPromotes(t *testing.T) {
	fake := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) {
			return &gmailv1.Profile{EmailAddress: "me@example.com", HistoryId: 1000}, nil
		},
		listFn: listPages(map[string]*gmailv1.ListMessagesResponse{
			"":   {Messages: listIDs("m1", "m2", "m3"), NextPageToken: "p2"},
			"p2": {Messages: listIDs("m4")},
		}),
		getFn: msgSet(
			testMsg("m1", "invoice july", "INBOX", "Label_1"),
			testMsg("m2", "old hangout", "CHAT"),
			testMsg("m3", "unsent draft", "DRAFT"),
			testMsg("m4", "sent message", "SENT"),
		),
		historyFn: emptyHistory(1500),
	}
	s, db, root := newTestSource(t, fake, testAccount("me@example.com"))
	ctx := context.Background()

	if err := s.Sync(ctx); err != nil {
		t.Fatalf("backfill sync: %v", err)
	}

	if got := countMD(t, root); got != 2 {
		t.Errorf("archived %d .md files, want 2 (CHAT and DRAFT skipped)", got)
	}
	if v, ok := cursor(t, db, cursorHistory); !ok || v != "1000" {
		t.Errorf("history cursor = %q,%v; want the pre-listing snapshot 1000", v, ok)
	}
	for _, kind := range []string{cursorBootstrap, cursorListDone, cursorPageToken} {
		if _, ok := cursor(t, db, kind); ok {
			t.Errorf("backfill cursor %s not cleared after promotion", kind)
		}
	}
	if n, _ := db.BackfillPendingCount(testSourceID); n != 0 {
		t.Errorf("backfill queue pending = %d, want 0", n)
	}
	counts, err := db.MessageCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts[testSourceID].Total != 2 {
		t.Errorf("DB total = %d, want 2", counts[testSourceID].Total)
	}
	// Skipped-not-archived rule: CHAT/DRAFT must not be marked seen.
	for _, id := range []string{"m2", "m3"} {
		if seen, _ := db.SeenMessage(testSourceID, id); seen {
			t.Errorf("%s was marked archived; skips must not mark archived", id)
		}
	}

	// Second run: incremental with an empty history — nothing rewritten,
	// cursor advances to the current mailbox history id.
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("incremental sync: %v", err)
	}
	if got := countMD(t, root); got != 2 {
		t.Errorf("second sync wrote files: %d .md, want 2", got)
	}
	if v, _ := cursor(t, db, cursorHistory); v != "1500" {
		t.Errorf("history cursor after empty incremental = %q, want 1500", v)
	}
	if fake.getCount("m1") != 1 {
		t.Errorf("m1 fetched %d times, want 1", fake.getCount("m1"))
	}
}

func TestBackfillRestartsOnRejectedPageToken(t *testing.T) {
	fake := &fakeAPI{
		listFn: func(tok string) (*gmailv1.ListMessagesResponse, error) {
			switch tok {
			case "stale":
				return nil, &googleapi.Error{Code: 400, Message: "Invalid pageToken"}
			case "":
				return &gmailv1.ListMessagesResponse{Messages: listIDs("m1")}, nil
			}
			return nil, fmt.Errorf("fake: unexpected token %q", tok)
		},
		getFn: msgSet(testMsg("m1", "hello", "INBOX")),
	}
	s, db, root := newTestSource(t, fake, testAccount("me@example.com"))
	// Simulate a resumed backfill whose persisted page token has gone stale.
	if err := db.SetCursor(testSourceID, "", cursorBootstrap, "500"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCursor(testSourceID, "", cursorPageToken, "stale"); err != nil {
		t.Fatal(err)
	}

	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := countMD(t, root); got != 1 {
		t.Errorf("archived %d files, want 1", got)
	}
	// The kept snapshot (not a fresh one) must have been promoted.
	if v, _ := cursor(t, db, cursorHistory); v != "500" {
		t.Errorf("history cursor = %q, want the preserved bootstrap 500", v)
	}
}

func TestIncrementalAddRescueDeleteAndDedupe(t *testing.T) {
	fake := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) {
			return &gmailv1.Profile{HistoryId: 1000}, nil
		},
		listFn: listPages(map[string]*gmailv1.ListMessagesResponse{
			"": {Messages: listIDs("m1")},
		}),
		getFn: msgSet(
			testMsg("m1", "first", "INBOX"),
			testMsg("m2", "sent second", "SENT"),
			testMsg("m3", "rescued from spam", "INBOX"), // current labels: no longer SPAM
			testMsg("m7", "still junk", "SPAM"),         // rescue candidate that is still spam
		),
		historyFn: func(hid uint64, tok string) (*gmailv1.ListHistoryResponse, error) {
			if hid != 1000 {
				return nil, fmt.Errorf("fake: unexpected startHistoryId %d", hid)
			}
			switch tok {
			case "":
				return &gmailv1.ListHistoryResponse{
					History: []*gmailv1.History{
						added("m2"),
						labelRemoved("m3", "SPAM"),
						labelRemoved("m7", "TRASH"),
						labelRemoved("m6", "STARRED"), // must never be fetched
						deleted("m1"),
					},
					HistoryId:     2000,
					NextPageToken: "h2",
				}, nil
			case "h2":
				return &gmailv1.ListHistoryResponse{
					History:   []*gmailv1.History{added("m2"), deleted("m1")}, // repeats across pages
					HistoryId: 2000,
				}, nil
			}
			return nil, fmt.Errorf("fake: unexpected history token %q", tok)
		},
	}
	s, db, root := newTestSource(t, fake, testAccount("me@example.com"))
	ctx := context.Background()

	if err := s.Sync(ctx); err != nil { // backfill m1, promote at 1000
		t.Fatalf("backfill: %v", err)
	}
	if err := s.Sync(ctx); err != nil { // incremental
		t.Fatalf("incremental: %v", err)
	}

	if got := countMD(t, root); got != 3 {
		t.Errorf("archive holds %d .md, want 3 (m1, m2, rescued m3)", got)
	}
	if seen, _ := db.SeenMessage(testSourceID, "m3"); !seen {
		t.Error("spam-rescue: m3 not archived after losing SPAM")
	}
	if seen, _ := db.SeenMessage(testSourceID, "m7"); seen {
		t.Error("m7 is still SPAM and must not be archived")
	}
	counts, err := db.MessageCounts()
	if err != nil {
		t.Fatal(err)
	}
	if c := counts[testSourceID]; c.Total != 3 || c.Deleted != 1 {
		t.Errorf("counts = %+v, want Total 3, Deleted 1 (m1 tombstoned)", c)
	}
	if v, _ := cursor(t, db, cursorHistory); v != "2000" {
		t.Errorf("history cursor = %q, want 2000", v)
	}
	if n := fake.getCount("m2"); n != 1 {
		t.Errorf("m2 fetched %d times, want 1 (ids must be deduped across pages)", n)
	}
	if n := fake.getCount("m6"); n != 0 {
		t.Errorf("m6 fetched %d times, want 0 (non-spam/trash label churn)", n)
	}
}

func TestHistory404FallsBackToBackfill(t *testing.T) {
	profileHid := uint64(1000)
	fake := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) {
			return &gmailv1.Profile{HistoryId: profileHid}, nil
		},
		listFn: listPages(map[string]*gmailv1.ListMessagesResponse{
			"": {Messages: listIDs("m1", "m5")},
		}),
		getFn: msgSet(
			testMsg("m1", "first", "INBOX"),
			testMsg("m5", "arrived while cursor expired", "INBOX"),
		),
		historyFn: func(uint64, string) (*gmailv1.ListHistoryResponse, error) {
			return nil, &googleapi.Error{Code: 404, Message: "startHistoryId is too old"}
		},
	}
	s, db, root := newTestSource(t, fake, testAccount("me@example.com"))
	ctx := context.Background()

	// Seed the archive with m1 only.
	fake.listFn = listPages(map[string]*gmailv1.ListMessagesResponse{
		"": {Messages: listIDs("m1")},
	})
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("seed backfill: %v", err)
	}

	// Now the cursor 404s; the full mailbox is m1 + m5.
	profileHid = 3000
	fake.listFn = listPages(map[string]*gmailv1.ListMessagesResponse{
		"": {Messages: listIDs("m1", "m5")},
	})
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("fallback sync: %v", err)
	}

	if got := countMD(t, root); got != 2 {
		t.Errorf("archive holds %d .md, want 2", got)
	}
	if n := fake.getCount("m1"); n != 1 {
		t.Errorf("m1 fetched %d times, want 1 (already-archived ids are skipped on re-list)", n)
	}
	if v, _ := cursor(t, db, cursorHistory); v != "3000" {
		t.Errorf("history cursor = %q, want the fresh snapshot 3000", v)
	}
}

func TestFetch404IsSkippedNotFatal(t *testing.T) {
	fake := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) {
			return &gmailv1.Profile{HistoryId: 100}, nil
		},
		listFn: listPages(map[string]*gmailv1.ListMessagesResponse{
			"": {Messages: listIDs("m1", "mgone")},
		}),
		getFn: func(id string) (*gmailv1.Message, error) {
			if id == "mgone" {
				return nil, &googleapi.Error{Code: 404}
			}
			return testMsg("m1", "survivor", "INBOX"), nil
		},
	}
	s, db, root := newTestSource(t, fake, testAccount("me@example.com"))

	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := countMD(t, root); got != 1 {
		t.Errorf("archived %d files, want 1", got)
	}
	if v, ok := cursor(t, db, cursorHistory); !ok || v != "100" {
		t.Errorf("history cursor = %q,%v; a vanished item must not block promotion", v, ok)
	}
	failures, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Errorf("failures ledger = %+v, want empty (404 is gone, not poison)", failures)
	}
}

func TestPoisonItemSkippedAfterThreshold(t *testing.T) {
	bad := testMsg("mbad", "poison", "INBOX")
	bad.Raw = "!!!not-base64!!!"
	fake := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) {
			return &gmailv1.Profile{HistoryId: 100}, nil
		},
		listFn: listPages(map[string]*gmailv1.ListMessagesResponse{
			"": {Messages: listIDs("mbad", "mok")},
		}),
		getFn: msgSet(bad, testMsg("mok", "fine", "INBOX")),
	}
	s, db, root := newTestSource(t, fake, testAccount("me@example.com"))
	ctx := context.Background()

	for run := 1; run < state.SkipThreshold; run++ {
		if err := s.Sync(ctx); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if _, ok := cursor(t, db, cursorHistory); ok {
			t.Fatalf("run %d: cursor promoted while a failing item is still retryable", run)
		}
		if n, _ := db.BackfillPendingCount(testSourceID); n != 1 {
			t.Fatalf("run %d: pending = %d, want 1 (the poison item)", run, n)
		}
	}

	// The threshold run: the item is skipped, the queue drains, promotion
	// proceeds — the poison item cannot wedge the cursor.
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("threshold run: %v", err)
	}
	if v, ok := cursor(t, db, cursorHistory); !ok || v != "100" {
		t.Errorf("history cursor = %q,%v; want promotion at 100 after poison skip", v, ok)
	}
	if n, _ := db.BackfillPendingCount(testSourceID); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
	if got := countMD(t, root); got != 1 {
		t.Errorf("archived %d files, want 1 (only mok)", got)
	}
	failures, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || failures[0].ID != "mbad" || failures[0].Attempts != state.SkipThreshold {
		t.Errorf("failures = %+v, want mbad at %d attempts", failures, state.SkipThreshold)
	}
}

// TestArchiveWriteFailureIsRunFatalNotPoison: a failing archive write is
// environmental (disk, permissions), not a bad item — it must abort the run
// without a failures-ledger row, leaving the queue position unadvanced so
// nothing drifts toward the skip threshold.
func TestArchiveWriteFailureIsRunFatalNotPoison(t *testing.T) {
	fake := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) {
			return &gmailv1.Profile{HistoryId: 100}, nil
		},
		listFn: listPages(map[string]*gmailv1.ListMessagesResponse{
			"": {Messages: listIDs("m1")},
		}),
		getFn: msgSet(testMsg("m1", "healthy message", "INBOX")),
	}
	s, db, _ := newTestSource(t, fake, testAccount("me@example.com"))
	// Simulate an unavailable archive volume: the root is a regular file, so
	// every write fails identically for every message.
	badRoot := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(badRoot, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.writer.Root = badRoot

	if err := s.Sync(context.Background()); err == nil {
		t.Fatal("Sync succeeded; want a run-fatal archive write error")
	}
	failures, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Errorf("failures ledger = %+v, want empty (write trouble is not item poison)", failures)
	}
	if n, _ := db.BackfillPendingCount(testSourceID); n != 1 {
		t.Errorf("backfill pending = %d, want 1 (queue position must not advance)", n)
	}
	if _, ok := cursor(t, db, cursorHistory); ok {
		t.Error("history cursor promoted despite the aborted run")
	}
	if seen, _ := db.SeenMessage(testSourceID, "m1"); seen {
		t.Error("m1 marked archived despite the failed write")
	}
}

func TestCorruptHistoryCursorHeals(t *testing.T) {
	fake := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) {
			return &gmailv1.Profile{HistoryId: 700}, nil
		},
		listFn: listPages(map[string]*gmailv1.ListMessagesResponse{
			"": {Messages: listIDs("m1")},
		}),
		getFn: msgSet(testMsg("m1", "hello", "INBOX")),
	}
	s, db, root := newTestSource(t, fake, testAccount("me@example.com"))
	if err := db.SetCursor(testSourceID, "", cursorHistory, "not-a-number"); err != nil {
		t.Fatal(err)
	}

	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := countMD(t, root); got != 1 {
		t.Errorf("archived %d files, want 1", got)
	}
	if v, _ := cursor(t, db, cursorHistory); v != "700" {
		t.Errorf("history cursor = %q, want the fresh snapshot 700", v)
	}
}

func TestCheckVerifiesAccount(t *testing.T) {
	fake := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) {
			return &gmailv1.Profile{EmailAddress: "Me@Example.com", HistoryId: 1}, nil
		},
	}
	s, _, _ := newTestSource(t, fake, testAccount("me@example.com"))
	if err := s.Check(context.Background()); err != nil {
		t.Errorf("Check with case-insensitive account match: %v", err)
	}

	s2, _, _ := newTestSource(t, fake, testAccount("other@example.com"))
	if err := s2.Check(context.Background()); err == nil {
		t.Error("Check must fail when the token belongs to a different account")
	}
}

func TestLabelNamesInFrontmatter(t *testing.T) {
	fake := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) {
			return &gmailv1.Profile{HistoryId: 10}, nil
		},
		listFn: listPages(map[string]*gmailv1.ListMessagesResponse{
			"": {Messages: listIDs("m1")},
		}),
		getFn: msgSet(testMsg("m1", "labeled", "INBOX", "Label_1", "Label_gone")),
	}
	s, _, root := newTestSource(t, fake, testAccount("me@example.com"))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(root, "2026", "08", "07", "*.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("glob: %v, matches %v", err, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	content := string(b)
	if !strings.Contains(content, "Receipts") {
		t.Errorf("frontmatter should carry the label NAME Receipts, got:\n%s", content)
	}
	if !strings.Contains(content, "Label_gone") {
		t.Errorf("a deleted label should fall back to its id, got:\n%s", content)
	}
	// Provenance is per account: the instance id keys the row, the label
	// names the human account, the address is the mailbox.
	for _, want := range []string{
		"source: " + testSourceID,
		"account_label: " + testLabel,
		"account: me@example.com",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("frontmatter missing %q, got:\n%s", want, content)
		}
	}
	if base := filepath.Base(matches[0]); !strings.Contains(base, "_gmail-"+testLabel+"_") {
		t.Errorf("filename %q must carry the account file tag gmail-%s", base, testLabel)
	}
}

// --- multi-account ---

// sharedFixture is one state DB plus one archive tree that several accounts
// use at once, exactly as the real deployment does.
func sharedFixture(t *testing.T) (*state.DB, string) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, t.TempDir()
}

// TestTwoAccountsKeepSeparateStateAndFiles: Gmail message ids are unique only
// within one mailbox, so "m1" below exists in BOTH accounts with different
// content. Each account must fetch, archive and key its own copy — separate
// cursors, separate archived-message rows, separate files.
func TestTwoAccountsKeepSeparateStateAndFiles(t *testing.T) {
	db, root := sharedFixture(t)
	ctx := context.Background()

	work := config.GoogleAccount{Label: "work", Account: "you@example.com", Gmail: true}
	personal := config.GoogleAccount{Label: "personal", Account: "you@example.net", Gmail: true}
	workID := state.InstanceID(state.SourceGmail, work.Label)
	personalID := state.InstanceID(state.SourceGmail, personal.Label)

	// Same id "m1", same subject: only the account differs, so the file tag
	// and the instance-salted hash are all that keep the two copies apart.
	fakeA := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) { return &gmailv1.Profile{HistoryId: 1000}, nil },
		listFn: listPages(map[string]*gmailv1.ListMessagesResponse{
			"": {Messages: listIDs("m1", "m2")},
		}),
		getFn: msgSet(testMsg("m1", "shared subject", "INBOX"), testMsg("m2", "only in work", "INBOX")),
	}
	fakeB := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) { return &gmailv1.Profile{HistoryId: 2000}, nil },
		listFn: listPages(map[string]*gmailv1.ListMessagesResponse{
			"": {Messages: listIDs("m1", "m3")},
		}),
		getFn: msgSet(testMsg("m1", "shared subject", "INBOX"), testMsg("m3", "only in personal", "INBOX")),
	}
	a := newTestSourceIn(t, db, root, fakeA, work)
	b := newTestSourceIn(t, db, root, fakeB, personal)

	if a.Name() != workID || b.Name() != personalID {
		t.Fatalf("Name() = %q / %q; want the instance ids %q / %q", a.Name(), b.Name(), workID, personalID)
	}
	if err := a.Sync(ctx); err != nil {
		t.Fatalf("work sync: %v", err)
	}
	if err := b.Sync(ctx); err != nil {
		t.Fatalf("personal sync: %v", err)
	}

	if got := countMD(t, root); got != 4 {
		t.Errorf("archive holds %d .md, want 4 (2 per account, including both copies of m1)", got)
	}
	// Both copies of m1 exist under their own tag AND their own hash: the
	// hash is salted with the instance id, so it differs even though the
	// message id and the rendered subject do not.
	for _, tc := range []struct{ source, tag string }{
		{workID, "gmail-work"},
		{personalID, "gmail-personal"},
	} {
		stem := naming.EmailStem(testTime, tc.tag, "shared subject", naming.Hash8(tc.source+":m1"))
		p := filepath.Join(root, "2026", "08", "07", stem+".md")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s's copy of m1 at %s: %v", tc.source, stem+".md", err)
		}
	}

	// State rows are per instance, not per kind.
	counts, err := db.MessageCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts[workID].Total != 2 || counts[personalID].Total != 2 {
		t.Errorf("counts = %+v, want 2 for each of %s and %s", counts, workID, personalID)
	}
	if _, ok := counts[state.SourceGmail]; ok {
		t.Errorf("counts hold a row keyed by the bare kind %q: %+v", state.SourceGmail, counts)
	}
	if seen, _ := db.SeenMessage(personalID, "m2"); seen {
		t.Error("personal sees work's m2: the messages table is not account-scoped")
	}
	if seen, _ := db.SeenMessage(workID, "m3"); seen {
		t.Error("work sees personal's m3: the messages table is not account-scoped")
	}
	if n := fakeB.getCount("m1"); n != 1 {
		t.Errorf("personal fetched m1 %d times, want 1 (work's m1 row must not mask it)", n)
	}

	// Cursors advance independently, each to its own mailbox's snapshot.
	if v, ok := cursorOf(t, db, workID, cursorHistory); !ok || v != "1000" {
		t.Errorf("work history cursor = %q,%v; want 1000", v, ok)
	}
	if v, ok := cursorOf(t, db, personalID, cursorHistory); !ok || v != "2000" {
		t.Errorf("personal history cursor = %q,%v; want 2000", v, ok)
	}
}

// TestBackfillQueueAndLedgerAreAccountScoped: one account stuck on a poison
// message must not hold back the other. Their backfill queues, cursors and
// failure ledgers live in the same tables, keyed only by instance id.
func TestBackfillQueueAndLedgerAreAccountScoped(t *testing.T) {
	db, root := sharedFixture(t)
	ctx := context.Background()

	work := config.GoogleAccount{Label: "work", Account: "you@example.com", Gmail: true}
	personal := config.GoogleAccount{Label: "personal", Account: "you@example.net", Gmail: true}
	workID := state.InstanceID(state.SourceGmail, work.Label)
	personalID := state.InstanceID(state.SourceGmail, personal.Label)

	poison := testMsg("m1", "poison", "INBOX")
	poison.Raw = "!!!not-base64!!!"
	fakeA := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) { return &gmailv1.Profile{HistoryId: 1000}, nil },
		listFn: listPages(map[string]*gmailv1.ListMessagesResponse{
			"": {Messages: listIDs("m1")},
		}),
		getFn: msgSet(poison),
	}
	// The healthy account holds a message with the SAME id.
	fakeB := &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) { return &gmailv1.Profile{HistoryId: 2000}, nil },
		listFn: listPages(map[string]*gmailv1.ListMessagesResponse{
			"": {Messages: listIDs("m1")},
		}),
		getFn: msgSet(testMsg("m1", "perfectly fine", "INBOX")),
	}
	a := newTestSourceIn(t, db, root, fakeA, work)
	b := newTestSourceIn(t, db, root, fakeB, personal)

	if err := a.Sync(ctx); err != nil {
		t.Fatalf("work sync: %v", err)
	}
	if err := b.Sync(ctx); err != nil {
		t.Fatalf("personal sync: %v", err)
	}

	// work is stuck: its queue keeps the item and its cursor stays put.
	if n, _ := db.BackfillPendingCount(workID); n != 1 {
		t.Errorf("work pending = %d, want 1 (the poison item)", n)
	}
	if _, ok := cursorOf(t, db, workID, cursorHistory); ok {
		t.Error("work promoted its cursor with an item still queued")
	}
	// personal is unaffected: its own m1 archived, queue drained, cursor promoted.
	if n, _ := db.BackfillPendingCount(personalID); n != 0 {
		t.Errorf("personal pending = %d, want 0 (work's stuck queue is not shared)", n)
	}
	if v, ok := cursorOf(t, db, personalID, cursorHistory); !ok || v != "2000" {
		t.Errorf("personal history cursor = %q,%v; want 2000", v, ok)
	}
	if got := countMD(t, root); got != 1 {
		t.Errorf("archive holds %d .md, want 1 (only personal's m1)", got)
	}

	// The ledger row belongs to work alone; personal's identically-named item
	// must not inherit an attempt count (and so must never be skipped).
	failures, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || failures[0].Source != workID || failures[0].ID != "m1" {
		t.Fatalf("failures = %+v, want exactly one row for %s/m1", failures, workID)
	}
	if skipped, _ := db.IsSkipped(personalID, "m1"); skipped {
		t.Error("personal's m1 is skipped because of work's failures on the same id")
	}
}

// TestNewSourceRejectsForeignInstanceID: the instance id and the account it
// serves must agree, or the connector would file one mailbox under another
// account's key and file tag.
func TestNewSourceRejectsForeignInstanceID(t *testing.T) {
	db, root := sharedFixture(t)
	w := &archive.Writer{Root: root, TZ: time.UTC}
	lim := ratelimit.NewUnits(1e9, 1000)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ga := config.GoogleAccount{Label: "work", Account: "you@example.com", Gmail: true}

	for _, id := range []string{
		state.InstanceID(state.SourceGmail, "personal"), // another account's id
		state.InstanceID(state.SourceGChat, "work"),     // right account, wrong kind
		state.SourceGmail, // bare kind: shared by every account
		"",
	} {
		if _, err := newSource(id, ga, db, w, lim, logger, &fakeAPI{}); err == nil {
			t.Errorf("newSource(%q) succeeded; want a rejection", id)
		}
	}
}
