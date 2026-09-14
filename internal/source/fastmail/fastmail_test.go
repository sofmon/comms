package fastmail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"

	"comms/internal/archive"
	"comms/internal/config"
	"comms/internal/naming"
	"comms/internal/retry"
	"comms/internal/state"
)

var testReceived = time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)

const rawPlain = "From: Alice <alice@example.com>\r\n" +
	"To: Bob <bob@example.com>\r\n" +
	"Subject: Hello World\r\n" +
	"Date: Mon, 04 Aug 2026 10:00:00 +0200\r\n" +
	"Message-Id: <m1@example.com>\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"Hi there.\r\n"

const rawWithAttach = "From: Alice <alice@example.com>\r\n" +
	"To: Bob <bob@example.com>\r\n" +
	"Subject: Hello World\r\n" +
	"Date: Mon, 04 Aug 2026 10:00:00 +0200\r\n" +
	"Message-Id: <m2@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"BB\"\r\n" +
	"\r\n" +
	"--BB\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"Hi with attach.\r\n" +
	"--BB\r\n" +
	"Content-Type: application/octet-stream; name=\"note.txt\"\r\n" +
	"Content-Disposition: attachment; filename=\"note.txt\"\r\n" +
	"\r\n" +
	"payload\r\n" +
	"--BB--\r\n"

// rawBroken makes emailpipe.Render fail: multipart with no boundary param.
const rawBroken = "Content-Type: multipart/mixed\r\n\r\nbody"

// --- fake JMAP transport ---

type fakeAPI struct {
	t         *testing.T
	handle    func(req *jmap.Request) (*jmap.Response, error)
	blobs     map[jmap.ID][]byte
	downloads []jmap.ID
}

func (f *fakeAPI) Do(req *jmap.Request) (*jmap.Response, error) { return f.handle(req) }

func (f *fakeAPI) DownloadWithContext(_ context.Context, _ jmap.ID, blobID jmap.ID) (io.ReadCloser, error) {
	f.downloads = append(f.downloads, blobID)
	b, ok := f.blobs[blobID]
	if !ok {
		return nil, &jmap.RequestError{Type: "about:blank", Status: 404, Detail: "no such blob"}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// scripted returns a fake whose Do consumes steps in order; extra requests
// fail the test.
func scripted(t *testing.T, steps ...func(req *jmap.Request) (*jmap.Response, error)) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, blobs: make(map[jmap.ID][]byte)}
	i := 0
	f.handle = func(req *jmap.Request) (*jmap.Response, error) {
		if i >= len(steps) {
			t.Fatalf("unexpected JMAP request #%d: %s", i+1, req.Calls[0].Name)
		}
		step := steps[i]
		i++
		return step(req)
	}
	t.Cleanup(func() {
		if i != len(steps) {
			t.Errorf("only %d of %d scripted JMAP requests were made", i, len(steps))
		}
	})
	return f
}

// reply pairs each call of req, in order, with args[i]; a *jmap.MethodError
// arg becomes an "error" invocation, as on the wire.
func reply(req *jmap.Request, args ...any) (*jmap.Response, error) {
	if len(args) != len(req.Calls) {
		return nil, fmt.Errorf("fake: %d reply args for %d calls", len(args), len(req.Calls))
	}
	resp := &jmap.Response{}
	for i, call := range req.Calls {
		name := call.Name
		if _, ok := args[i].(*jmap.MethodError); ok {
			name = "error"
		}
		resp.Responses = append(resp.Responses, &jmap.Invocation{
			Name: name, Args: args[i], CallID: call.CallID,
		})
	}
	return resp, nil
}

// callArgs returns the first call of the wanted method type in req.
func callArgs[T any](t *testing.T, req *jmap.Request) *T {
	t.Helper()
	for _, call := range req.Calls {
		if v, ok := call.Args.(*T); ok {
			return v
		}
	}
	t.Fatalf("request has no %T call", (*T)(nil))
	return nil
}

// --- fixtures ---

// The default fixture account. Every helper below is keyed by its instance
// id, never by the bare state.SourceFastmail kind.
const testLabel = "fm"

var (
	testInstance = state.InstanceID(state.SourceFastmail, testLabel) // "fastmail:fm"
	testTag      = state.Tag(testInstance)                           // "fastmail-fm"
)

func newTestSource(t *testing.T) (*Source, *state.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	root := filepath.Join(dir, "archive")
	return newSourceFor(t, testLabel, db, root), db, root
}

// newSourceFor builds the connector for one account label against an
// existing DB and archive root, so several accounts can share both.
func newSourceFor(t *testing.T, label string, db *state.DB, root string) *Source {
	t.Helper()
	return New(
		state.InstanceID(state.SourceFastmail, label),
		config.FastMailAccount{Label: label, Account: label + "@fastmail.example"},
		db, &archive.Writer{Root: root, TZ: time.UTC}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() (string, error) { return "test-token", nil },
		// Tests must not depend on how full the machine's disk is: "unknown"
		// disables the free-space floor. The floor tests override this.
		WithFreeSpace(func(string) int64 { return 0 }),
	)
}

func testConn(f *fakeAPI) *conn { return &conn{api: f, accountID: "acct", maxCalls: 16} }

func testBoxes() *mailboxes {
	return &mailboxes{
		names: map[jmap.ID]string{
			"mb-in": "Inbox", "mb-junk": "Junk Mail", "mb-trash": "Trash", "mb-arc": "Archive", "mb-sent": "Sent",
		},
		junkTrash: map[jmap.ID]bool{"mb-junk": true, "mb-trash": true},
		exclude:   []jmap.ID{"mb-junk", "mb-trash"},
	}
}

func testEmail(id, blob string, boxes ...jmap.ID) *email.Email {
	mb := make(map[jmap.ID]bool, len(boxes))
	for _, b := range boxes {
		mb[b] = true
	}
	recv := testReceived
	return &email.Email{
		ID: jmap.ID(id), BlobID: jmap.ID(blob), ThreadID: jmap.ID("T-" + id),
		MailboxIDs: mb, ReceivedAt: &recv, Subject: "Hello World",
	}
}

// preArchive plants a messages row so an id counts as Seen.
func preArchive(t *testing.T, db *state.DB, id string) {
	t.Helper()
	err := db.CommitMessage(state.Message{
		Source: testInstance, StableID: id, TS: testReceived,
		DayBucket: "2026-08-04", RelPath: "pre/" + id + ".md", ContentHash: "h",
	})
	if err != nil {
		t.Fatalf("pre-archive %s: %v", id, err)
	}
}

// mdFiles returns root-relative .md path → content.
func mdFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && os.IsNotExist(err) {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() || filepath.Ext(p) != ".md" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk archive: %v", err)
	}
	return out
}

func getCursor(t *testing.T, db *state.DB, kind string) (string, bool) {
	t.Helper()
	v, ok, err := db.GetCursor(testInstance, "", kind)
	if err != nil {
		t.Fatalf("get cursor %s: %v", kind, err)
	}
	return v, ok
}

func mustSeen(t *testing.T, db *state.DB, id string, want bool) {
	t.Helper()
	seen, err := db.SeenMessage(testInstance, id)
	if err != nil {
		t.Fatalf("seen %s: %v", id, err)
	}
	if seen != want {
		t.Errorf("SeenMessage(%s) = %v, want %v", id, seen, want)
	}
}

func setPageLimit(t *testing.T, n int) {
	t.Helper()
	old := backfillPageLimit
	backfillPageLimit = n
	t.Cleanup(func() { backfillPageLimit = old })
}

func setRetryOpts(t *testing.T, o retry.Options) {
	t.Helper()
	old := retryOpts
	retryOpts = o
	t.Cleanup(func() { retryOpts = old })
}

// --- tests ---

// TestTwoAccountsAreIsolated runs two FastMail accounts against one database
// and one archive root. Both see an email with the SAME JMAP id — ids are
// unique per account, not globally — so every keyed thing must stay separate:
// the Email/changes state cursor, the message rows, the failures ledger, and
// the archived files.
func TestTwoAccountsAreIsolated(t *testing.T) {
	setPageLimit(t, 2)
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	root := filepath.Join(dir, "archive")

	instA := state.InstanceID(state.SourceFastmail, "acme")
	instB := state.InstanceID(state.SourceFastmail, "beta")
	sa := newSourceFor(t, "acme", db, root)
	sb := newSourceFor(t, "beta", db, root)

	mailboxStep := func(req *jmap.Request) (*jmap.Response, error) {
		callArgs[mailbox.Get](t, req)
		return reply(req, &mailbox.GetResponse{List: []*mailbox.Mailbox{
			{ID: "mb-in", Name: "Inbox", Role: mailbox.RoleInbox},
			{ID: "mb-junk", Name: "Spam", Role: mailbox.RoleJunk},
		}})
	}
	// Each account backfills one page holding the same email id. A stray
	// Email/get here (e.g. a retryFailed pass that picked up the *other*
	// account's failure) fails the callArgs lookup.
	page := func(objState string) func(*jmap.Request) (*jmap.Response, error) {
		return func(req *jmap.Request) (*jmap.Response, error) {
			if q := callArgs[email.Query](t, req); q.Anchor != "" {
				t.Errorf("fresh backfill anchored at %q, want no anchor", q.Anchor)
			}
			return reply(req,
				&email.QueryResponse{IDs: []jmap.ID{"shared-1"}},
				&email.GetResponse{State: objState, List: []*email.Email{testEmail("shared-1", "blob-1", "mb-in")}},
			)
		}
	}

	fa := scripted(t, mailboxStep, page("A1"))
	fa.blobs["blob-1"] = []byte(rawPlain)
	sa.dial = func(context.Context) (*conn, error) { return testConn(fa), nil }
	if err := sa.Sync(context.Background()); err != nil {
		t.Fatalf("acme sync: %v", err)
	}

	// A pending failure on acme must be invisible to beta's retry pass.
	if _, err := db.RecordFailure(instA, "ghost", "acme-only failure"); err != nil {
		t.Fatal(err)
	}

	fb := scripted(t, mailboxStep, page("B1"))
	fb.blobs["blob-1"] = []byte(rawPlain)
	sb.dial = func(context.Context) (*conn, error) { return testConn(fb), nil }
	if err := sb.Sync(context.Background()); err != nil {
		t.Fatalf("beta sync: %v", err)
	}

	// Cursors: one Email/changes state per account, neither overwritten.
	for _, tc := range []struct{ inst, want string }{{instA, "A1"}, {instB, "B1"}} {
		v, ok, err := db.GetCursor(tc.inst, "", cursorEmailState)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || v != tc.want {
			t.Errorf("email_state[%s] = %q (%v), want %q", tc.inst, v, ok, tc.want)
		}
	}
	if _, ok, err := db.GetCursor(state.SourceFastmail, "", cursorEmailState); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("a cursor was written under the bare kind \"fastmail\"")
	}

	// Rows: each account archived its own copy; no cross-account dedup.
	for _, inst := range []string{instA, instB} {
		seen, err := db.SeenMessage(inst, "shared-1")
		if err != nil {
			t.Fatal(err)
		}
		if !seen {
			t.Errorf("SeenMessage(%s, shared-1) = false, want true", inst)
		}
	}
	if !slices.Equal(fb.downloads, []jmap.ID{"blob-1"}) {
		t.Errorf("beta downloads = %v, want blob-1 (acme's copy must not dedup it away)", fb.downloads)
	}
	counts, err := db.MessageCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts[instA].Total != 1 || counts[instB].Total != 1 {
		t.Errorf("counts = %+v, want one message per instance", counts)
	}

	// Failures: beta's pass left acme's entry alone.
	fails, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 1 || fails[0].Source != instA || fails[0].ID != "ghost" {
		t.Errorf("failures = %+v, want acme's ghost entry untouched", fails)
	}

	// Files: distinct stems — the tag names the account and the hash is
	// salted with the instance id, so the same JMAP id cannot collide.
	hashA, hashB := naming.Hash8(instA+":shared-1"), naming.Hash8(instB+":shared-1")
	if hashA == hashB {
		t.Fatalf("hash8 %q is identical for both accounts", hashA)
	}
	mds := mdFiles(t, root)
	if len(mds) != 2 {
		t.Fatalf("archived %d files, want 2: %v", len(mds), mds)
	}
	for _, want := range []struct{ tag, hash, source, account string }{
		{"fastmail-acme", hashA, instA, "acme@fastmail.example"},
		{"fastmail-beta", hashB, instB, "beta@fastmail.example"},
	} {
		var content string
		for rel, c := range mds {
			base := filepath.Base(rel)
			if strings.Contains(base, "_"+want.tag+"_") && strings.Contains(base, want.hash) {
				content = c
			}
			if strings.Contains(base, ":") {
				t.Errorf("filename %q contains an instance-id colon", rel)
			}
		}
		if content == "" {
			t.Fatalf("no archived file for tag %s hash %s: %v", want.tag, want.hash, mds)
		}
		for _, frag := range []string{
			"source: " + want.source + "\n",
			"account: " + want.account + "\n",
			"account_label: " + strings.TrimPrefix(want.tag, "fastmail-") + "\n",
		} {
			if !strings.Contains(content, frag) {
				t.Errorf("%s file missing %q in frontmatter:\n%s", want.tag, frag, content)
			}
		}
	}
}

// TestInstanceIDGuard: a connector whose source key is not its own account's
// instance id must refuse to run. A bare "fastmail" kind would have every
// account sharing one set of cursors and rows, and a label mismatch would
// file one account's mail under another's name.
func TestInstanceIDGuard(t *testing.T) {
	_, db, root := newTestSource(t)
	build := func(inst, label string) *Source {
		s := New(inst, config.FastMailAccount{Label: label, Account: "me@fastmail.example"},
			db, &archive.Writer{Root: root, TZ: time.UTC}, nil,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
			func() (string, error) { return "test-token", nil })
		s.dial = func(context.Context) (*conn, error) {
			return nil, errors.New("dialed: the instance guard did not fire")
		}
		return s
	}
	tests := []struct {
		name  string
		inst  string
		label string
	}{
		{name: "bare kind", inst: state.SourceFastmail, label: "fm"},
		{name: "empty label", inst: state.InstanceID(state.SourceFastmail, ""), label: ""},
		{name: "other account's label", inst: state.InstanceID(state.SourceFastmail, "other"), label: "fm"},
		{name: "wrong kind", inst: state.InstanceID(state.SourceGmail, "fm"), label: "fm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := build(tt.inst, tt.label)
			for op, err := range map[string]error{
				"Check": s.Check(context.Background()),
				"Sync":  s.Sync(context.Background()),
			} {
				if err == nil || !strings.Contains(err.Error(), "instance id") {
					t.Errorf("%s error = %v, want an instance-id complaint", op, err)
				}
			}
		})
	}
	// The healthy shape reports the instance id as its name.
	if got := build(testInstance, testLabel).Name(); got != testInstance {
		t.Errorf("Name() = %q, want %q", got, testInstance)
	}
}

func TestFileTokenSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("  sekrit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		literal string
		path    string
		want    string
		wantErr bool
	}{
		{name: "literal wins", literal: "env-token", path: path, want: "env-token"},
		{name: "file trimmed", path: path, want: "sekrit"},
		{name: "missing file", path: filepath.Join(dir, "nope"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FileTokenSource(tt.literal, tt.path)()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("token = %q, want %q", got, tt.want)
			}
		})
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileTokenSource("", empty)(); err == nil {
		t.Error("empty token file: want error")
	}
}

func TestInScope(t *testing.T) {
	junkTrash := map[jmap.ID]bool{"junk": true, "trash": true}
	tests := []struct {
		name string
		ids  map[jmap.ID]bool
		want bool
	}{
		{name: "inbox only", ids: map[jmap.ID]bool{"in": true}, want: true},
		{name: "sent only", ids: map[jmap.ID]bool{"sent": true}, want: true},
		{name: "junk only", ids: map[jmap.ID]bool{"junk": true}, want: false},
		{name: "trash only", ids: map[jmap.ID]bool{"trash": true}, want: false},
		{name: "junk and trash", ids: map[jmap.ID]bool{"junk": true, "trash": true}, want: false},
		{name: "junk plus inbox", ids: map[jmap.ID]bool{"junk": true, "in": true}, want: true},
		{name: "trash plus archive", ids: map[jmap.ID]bool{"trash": true, "arc": true}, want: true},
		{name: "empty", ids: map[jmap.ID]bool{}, want: false},
		{name: "nil", ids: nil, want: false},
		{name: "false-valued inbox", ids: map[jmap.ID]bool{"in": false, "junk": true}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inScope(tt.ids, junkTrash); got != tt.want {
				t.Errorf("inScope = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLabels(t *testing.T) {
	boxes := testBoxes()
	got := boxes.labels(map[jmap.ID]bool{"mb-in": true, "mb-arc": true, "mb-mystery": true, "mb-junk": false})
	want := []string{"Archive", "Inbox", "mb-mystery"}
	if !slices.Equal(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

// TestBackfillPagingAndPromotion drives a two-page backfill: anchor math,
// mid-run anchor persistence, bootstrap-state capture from the first
// response, promotion on completion, and the subject-guess re-render (e2's
// JMAP subject disagrees with the raw message but its attachment must still
// land in the writer-derived .d directory).
func TestBackfillPagingAndPromotion(t *testing.T) {
	setPageLimit(t, 2)
	s, db, root := newTestSource(t)
	boxes := testBoxes()

	e1 := testEmail("e1", "b1", "mb-in")
	// Sent mail is deliberately part of the backfill, not just Inbox mail.
	e2 := testEmail("e2", "b2", "mb-sent")
	e2.Subject = "Totally Different Guess"
	e3 := testEmail("e3", "b3", "mb-in")

	fake := scripted(t,
		func(req *jmap.Request) (*jmap.Response, error) {
			q := callArgs[email.Query](t, req)
			if q.Anchor != "" || q.AnchorOffset != 0 {
				t.Errorf("first page: anchor = %q offset %d, want none", q.Anchor, q.AnchorOffset)
			}
			if q.Limit != 2 {
				t.Errorf("limit = %d, want 2", q.Limit)
			}
			fc, ok := q.Filter.(*email.FilterCondition)
			if !ok || !slices.Equal(fc.InMailboxOtherThan, []jmap.ID{"mb-junk", "mb-trash"}) {
				t.Errorf("filter = %+v, want inMailboxOtherThan [mb-junk mb-trash]", q.Filter)
			}
			if len(q.Sort) != 1 || q.Sort[0].Property != "receivedAt" || !q.Sort[0].IsAscending {
				t.Errorf("sort = %+v, want receivedAt asc", q.Sort)
			}
			g := callArgs[email.Get](t, req)
			if g.ReferenceIDs == nil || g.ReferenceIDs.Path != "/ids" || g.ReferenceIDs.Name != "Email/query" {
				t.Errorf("get back-reference = %+v", g.ReferenceIDs)
			}
			return reply(req,
				&email.QueryResponse{IDs: []jmap.ID{"e1", "e2"}},
				&email.GetResponse{State: "S1", List: []*email.Email{e1, e2}},
			)
		},
		func(req *jmap.Request) (*jmap.Response, error) {
			q := callArgs[email.Query](t, req)
			if q.Anchor != "e2" || q.AnchorOffset != 1 {
				t.Errorf("second page: anchor = %q offset %d, want e2/1", q.Anchor, q.AnchorOffset)
			}
			// The anchor cursor must be durable after page one.
			if v, ok := getCursor(t, db, cursorBackfillAnchor); !ok || v != "e2" {
				t.Errorf("mid-run backfill_anchor = %q (%v), want e2", v, ok)
			}
			if v, ok := getCursor(t, db, cursorBootstrapState); !ok || v != "S1" {
				t.Errorf("mid-run bootstrap_email_state = %q (%v), want S1", v, ok)
			}
			return reply(req,
				&email.QueryResponse{IDs: []jmap.ID{"e3"}},
				&email.GetResponse{State: "S9", List: []*email.Email{e3}},
			)
		},
	)
	fake.blobs["b1"] = []byte(rawPlain)
	fake.blobs["b2"] = []byte(rawWithAttach)
	fake.blobs["b3"] = []byte(rawPlain)

	if err := s.backfill(context.Background(), testConn(fake), boxes); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	if v, ok := getCursor(t, db, cursorEmailState); !ok || v != "S1" {
		t.Errorf("email_state = %q (%v), want S1 (bootstrap, not the later S9)", v, ok)
	}
	if _, ok := getCursor(t, db, cursorBackfillAnchor); ok {
		t.Error("backfill_anchor not cleared after promotion")
	}
	if _, ok := getCursor(t, db, cursorBootstrapState); ok {
		t.Error("bootstrap_email_state not cleared after promotion")
	}
	for _, id := range []string{"e1", "e2", "e3"} {
		mustSeen(t, db, id, true)
	}
	mds := mdFiles(t, root)
	if len(mds) != 3 {
		t.Fatalf("archived %d .md files, want 3: %v", len(mds), mds)
	}
	// e2: attachment written under the stem derived from the *rendered*
	// subject ("Hello World"), not the wrong JMAP guess.
	stem := naming.EmailStem(testReceived.In(time.UTC), testTag, "Hello World", naming.Hash8(testInstance+":e2"))
	attach := filepath.Join(root, "2026", "08", "04", naming.AttachDir(stem), "note.txt")
	if b, err := os.ReadFile(attach); err != nil {
		t.Errorf("attachment missing after subject-guess correction: %v", err)
	} else if !strings.Contains(string(b), "payload") {
		t.Errorf("attachment content = %q", b)
	}
	for rel, content := range mds {
		if !strings.Contains(content, "source: "+testInstance+"\n") {
			t.Errorf("%s: missing %q source in frontmatter", rel, testInstance)
		}
		if !strings.Contains(content, "account_label: "+testLabel+"\n") {
			t.Errorf("%s: missing account_label in frontmatter", rel)
		}
		if !strings.Contains(filepath.Base(rel), "_"+testTag+"_") {
			t.Errorf("%s: filename does not carry the account tag %q", rel, testTag)
		}
	}
}

// TestBackfillResumeKeepsBootstrap resumes a half-done backfill: the query
// must anchor at the persisted id, archived ids must not be re-downloaded,
// and the ORIGINAL bootstrap state (not this run's fresher one) must be
// promoted.
func TestBackfillResumeKeepsBootstrap(t *testing.T) {
	setPageLimit(t, 2)
	s, db, _ := newTestSource(t)
	boxes := testBoxes()

	preArchive(t, db, "e1")
	preArchive(t, db, "e2")
	if err := db.SetCursor(testInstance, "", cursorBackfillAnchor, "e2"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCursor(testInstance, "", cursorBootstrapState, "S0"); err != nil {
		t.Fatal(err)
	}

	fake := scripted(t,
		func(req *jmap.Request) (*jmap.Response, error) {
			q := callArgs[email.Query](t, req)
			if q.Anchor != "e2" || q.AnchorOffset != 1 {
				t.Errorf("resume anchor = %q offset %d, want e2/1", q.Anchor, q.AnchorOffset)
			}
			return reply(req,
				&email.QueryResponse{IDs: []jmap.ID{"e3"}},
				&email.GetResponse{State: "S5", List: []*email.Email{testEmail("e3", "b3", "mb-in")}},
			)
		},
	)
	fake.blobs["b3"] = []byte(rawPlain)

	if err := s.backfill(context.Background(), testConn(fake), boxes); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if v, ok := getCursor(t, db, cursorEmailState); !ok || v != "S0" {
		t.Errorf("email_state = %q (%v), want original bootstrap S0", v, ok)
	}
	if got := fake.downloads; !slices.Equal(got, []jmap.ID{"b3"}) {
		t.Errorf("downloads = %v, want only b3", got)
	}
	mustSeen(t, db, "e3", true)
}

// TestBackfillAnchorDestroyed: the persisted anchor no longer exists on the
// server; the connector must clear it, restart the enumeration, and rely on
// Seen dedup for already-archived ids.
func TestBackfillAnchorDestroyed(t *testing.T) {
	setPageLimit(t, 2)
	s, db, _ := newTestSource(t)
	boxes := testBoxes()

	preArchive(t, db, "e1")
	if err := db.SetCursor(testInstance, "", cursorBackfillAnchor, "gone"); err != nil {
		t.Fatal(err)
	}

	fake := scripted(t,
		func(req *jmap.Request) (*jmap.Response, error) {
			if q := callArgs[email.Query](t, req); q.Anchor != "gone" {
				t.Errorf("anchor = %q, want gone", q.Anchor)
			}
			return reply(req,
				&jmap.MethodError{Type: "anchorNotFound"},
				&jmap.MethodError{Type: "invalidResultReference"},
			)
		},
		func(req *jmap.Request) (*jmap.Response, error) {
			if q := callArgs[email.Query](t, req); q.Anchor != "" {
				t.Errorf("restarted enumeration still anchored at %q", q.Anchor)
			}
			return reply(req,
				&email.QueryResponse{IDs: []jmap.ID{"e1", "e2"}},
				&email.GetResponse{State: "S2", List: []*email.Email{
					testEmail("e1", "b1", "mb-in"), testEmail("e2", "b2", "mb-in"),
				}},
			)
		},
		func(req *jmap.Request) (*jmap.Response, error) {
			if q := callArgs[email.Query](t, req); q.Anchor != "e2" {
				t.Errorf("anchor = %q, want e2", q.Anchor)
			}
			return reply(req,
				&email.QueryResponse{IDs: []jmap.ID{}},
				&email.GetResponse{State: "S3"},
			)
		},
	)
	fake.blobs["b1"] = []byte(rawPlain)
	fake.blobs["b2"] = []byte(rawPlain)

	if err := s.backfill(context.Background(), testConn(fake), boxes); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if v, ok := getCursor(t, db, cursorEmailState); !ok || v != "S2" {
		t.Errorf("email_state = %q (%v), want S2", v, ok)
	}
	// e1 was already archived: only e2's blob may be downloaded.
	if got := fake.downloads; !slices.Equal(got, []jmap.ID{"b2"}) {
		t.Errorf("downloads = %v, want only b2", got)
	}
}

// TestIncrementalScopeAndJunkRescue covers the three change shapes: a
// created email only in junk stays unarchived; a later update moving it to
// the inbox archives it (junk rescue); an update on an archived id is not
// even fetched; destroy tombstones the DB row only.
func TestIncrementalScopeAndJunkRescue(t *testing.T) {
	s, db, root := newTestSource(t)
	boxes := testBoxes()
	ctx := context.Background()

	fake := scripted(t,
		// Batch 1: two created emails.
		func(req *jmap.Request) (*jmap.Response, error) {
			ch := callArgs[email.Changes](t, req)
			if ch.SinceState != "S0" {
				t.Errorf("sinceState = %q, want S0", ch.SinceState)
			}
			if ch.MaxChanges != changesBatchLimit {
				t.Errorf("maxChanges = %d, want %d", ch.MaxChanges, changesBatchLimit)
			}
			return reply(req, &email.ChangesResponse{
				NewState: "S1", Created: []jmap.ID{"c-in", "c-junk"},
			})
		},
		func(req *jmap.Request) (*jmap.Response, error) {
			g := callArgs[email.Get](t, req)
			if !slices.Equal(g.IDs, []jmap.ID{"c-in", "c-junk"}) {
				t.Errorf("get ids = %v, want [c-in c-junk]", g.IDs)
			}
			return reply(req, &email.GetResponse{State: "S1", List: []*email.Email{
				testEmail("c-in", "b-ci", "mb-in"),
				testEmail("c-junk", "b-cj", "mb-junk"), // junk-only: out of scope
			}})
		},
		// Batch 2: c-junk moved to the inbox (rescue); c-in updated but
		// already archived — it must NOT be in the Email/get.
		func(req *jmap.Request) (*jmap.Response, error) {
			if ch := callArgs[email.Changes](t, req); ch.SinceState != "S1" {
				t.Errorf("sinceState = %q, want S1", ch.SinceState)
			}
			return reply(req, &email.ChangesResponse{
				NewState: "S2", Updated: []jmap.ID{"c-junk", "c-in"},
			})
		},
		func(req *jmap.Request) (*jmap.Response, error) {
			g := callArgs[email.Get](t, req)
			if !slices.Equal(g.IDs, []jmap.ID{"c-junk"}) {
				t.Errorf("rescue get ids = %v, want [c-junk] only", g.IDs)
			}
			return reply(req, &email.GetResponse{State: "S2", List: []*email.Email{
				testEmail("c-junk", "b-cj", "mb-in"),
			}})
		},
		// Batch 3: destroy of an archived id.
		func(req *jmap.Request) (*jmap.Response, error) {
			return reply(req, &email.ChangesResponse{
				NewState: "S3", Destroyed: []jmap.ID{"c-in"},
			})
		},
	)
	fake.blobs["b-ci"] = []byte(rawPlain)
	fake.blobs["b-cj"] = []byte(rawPlain)
	c := testConn(fake)

	if err := s.incremental(ctx, c, boxes, "S0"); err != nil {
		t.Fatalf("incremental batch 1: %v", err)
	}
	mustSeen(t, db, "c-in", true)
	mustSeen(t, db, "c-junk", false) // junk-only: deliberately unarchived
	if fails, err := db.ListFailures(); err != nil || len(fails) != 0 {
		t.Errorf("junk-only skip must not record a failure: %v %v", fails, err)
	}

	if err := s.incremental(ctx, c, boxes, "S1"); err != nil {
		t.Fatalf("incremental batch 2: %v", err)
	}
	mustSeen(t, db, "c-junk", true)
	if v, _ := getCursor(t, db, cursorEmailState); v != "S2" {
		t.Errorf("email_state = %q, want S2", v)
	}
	var rescued string
	for rel, content := range mdFiles(t, root) {
		if strings.Contains(rel, naming.Hash8(testInstance+":c-junk")) {
			rescued = content
		}
	}
	if rescued == "" {
		t.Fatal("no .md written for rescued c-junk")
	}
	if !strings.Contains(rescued, "- Inbox") {
		t.Errorf("rescued email labels missing Inbox:\n%s", rescued)
	}
	if !strings.Contains(rescued, "Hi there.") {
		t.Errorf("rescued email body missing:\n%s", rescued)
	}

	if err := s.incremental(ctx, c, boxes, "S2"); err != nil {
		t.Fatalf("incremental batch 3: %v", err)
	}
	counts, err := db.MessageCounts()
	if err != nil {
		t.Fatal(err)
	}
	got := counts[testInstance]
	if got.Total != 2 || got.Deleted != 1 {
		t.Errorf("counts = %+v, want Total 2 Deleted 1", got)
	}
	if v, _ := getCursor(t, db, cursorEmailState); v != "S3" {
		t.Errorf("email_state = %q, want S3", v)
	}
}

// TestChangesLoopBatching: hasMoreChanges loops; the state cursor is
// persisted after each fully-processed batch, so a crash mid-loop resumes
// from the last durable batch.
func TestChangesLoopBatching(t *testing.T) {
	s, db, _ := newTestSource(t)
	boxes := testBoxes()
	ctx := context.Background()

	fake := scripted(t,
		func(req *jmap.Request) (*jmap.Response, error) {
			return reply(req, &email.ChangesResponse{
				NewState: "S1", HasMoreChanges: true, Created: []jmap.ID{"x"},
			})
		},
		func(req *jmap.Request) (*jmap.Response, error) {
			return reply(req, &email.GetResponse{State: "S1", List: []*email.Email{testEmail("x", "bx", "mb-in")}})
		},
		// The loop continues from S1 and the transport dies.
		func(req *jmap.Request) (*jmap.Response, error) {
			if ch := callArgs[email.Changes](t, req); ch.SinceState != "S1" {
				t.Errorf("sinceState = %q, want S1", ch.SinceState)
			}
			return nil, errors.New("boom")
		},
		// Recovery run resumes from the persisted S1.
		func(req *jmap.Request) (*jmap.Response, error) {
			if ch := callArgs[email.Changes](t, req); ch.SinceState != "S1" {
				t.Errorf("recovery sinceState = %q, want S1", ch.SinceState)
			}
			return reply(req, &email.ChangesResponse{
				NewState: "S2", Created: []jmap.ID{"y"},
			})
		},
		func(req *jmap.Request) (*jmap.Response, error) {
			return reply(req, &email.GetResponse{State: "S2", List: []*email.Email{testEmail("y", "by", "mb-in")}})
		},
	)
	fake.blobs["bx"] = []byte(rawPlain)
	fake.blobs["by"] = []byte(rawPlain)
	c := testConn(fake)

	err := s.incremental(ctx, c, boxes, "S0")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("incremental error = %v, want boom", err)
	}
	if v, _ := getCursor(t, db, cursorEmailState); v != "S1" {
		t.Errorf("after crash email_state = %q, want S1 (batch 1 was durable)", v)
	}
	mustSeen(t, db, "x", true)

	cur, _ := getCursor(t, db, cursorEmailState)
	if err := s.incremental(ctx, c, boxes, cur); err != nil {
		t.Fatalf("recovery incremental: %v", err)
	}
	if v, _ := getCursor(t, db, cursorEmailState); v != "S2" {
		t.Errorf("email_state = %q, want S2", v)
	}
	mustSeen(t, db, "y", true)
}

// TestCannotCalculateChanges: an expired state falls back to a full
// requery; Seen dedup keeps archived ids untouched and the fresh bootstrap
// state is promoted.
func TestCannotCalculateChanges(t *testing.T) {
	setPageLimit(t, 10)
	s, db, _ := newTestSource(t)
	boxes := testBoxes()

	preArchive(t, db, "e1")
	if err := db.SetCursor(testInstance, "", cursorEmailState, "SX"); err != nil {
		t.Fatal(err)
	}

	fake := scripted(t,
		func(req *jmap.Request) (*jmap.Response, error) {
			return reply(req, &jmap.MethodError{Type: "cannotCalculateChanges"})
		},
		func(req *jmap.Request) (*jmap.Response, error) {
			if q := callArgs[email.Query](t, req); q.Anchor != "" {
				t.Errorf("requery anchored at %q, want none", q.Anchor)
			}
			return reply(req,
				&email.QueryResponse{IDs: []jmap.ID{"e1", "e2"}},
				&email.GetResponse{State: "SN", List: []*email.Email{
					testEmail("e1", "b1", "mb-in"), testEmail("e2", "b2", "mb-in"),
				}},
			)
		},
	)
	fake.blobs["b1"] = []byte(rawPlain)
	fake.blobs["b2"] = []byte(rawPlain)
	c := testConn(fake)

	cur, _ := getCursor(t, db, cursorEmailState)
	if err := s.incremental(context.Background(), c, boxes, cur); err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if v, ok := getCursor(t, db, cursorEmailState); !ok || v != "SN" {
		t.Errorf("email_state = %q (%v), want SN", v, ok)
	}
	if got := fake.downloads; !slices.Equal(got, []jmap.ID{"b2"}) {
		t.Errorf("downloads = %v, want only b2 (e1 deduped)", got)
	}
	mustSeen(t, db, "e2", true)
}

// TestSyncRetriesFailedItems drives a whole Sync: mailbox snapshot parsing
// (names + roles), the failed-item retry pass (one recoverable, one gone
// from the server), and an empty incremental batch.
func TestSyncRetriesFailedItems(t *testing.T) {
	s, db, root := newTestSource(t)

	if err := db.SetCursor(testInstance, "", cursorEmailState, "S0"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordFailure(testInstance, "f1", "transient blob failure"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordFailure(testInstance, "f2", "poof"); err != nil {
		t.Fatal(err)
	}

	fake := scripted(t,
		func(req *jmap.Request) (*jmap.Response, error) {
			g := callArgs[mailbox.Get](t, req)
			if g.Account != "acct" {
				t.Errorf("mailbox get account = %q", g.Account)
			}
			return reply(req, &mailbox.GetResponse{List: []*mailbox.Mailbox{
				{ID: "mb-in", Name: "Inbox", Role: mailbox.RoleInbox},
				{ID: "mb-junk", Name: "Spam", Role: mailbox.RoleJunk},
				{ID: "mb-trash", Name: "Trash", Role: mailbox.RoleTrash},
			}})
		},
		func(req *jmap.Request) (*jmap.Response, error) {
			g := callArgs[email.Get](t, req)
			if !slices.Equal(g.IDs, []jmap.ID{"f1", "f2"}) {
				t.Errorf("retry get ids = %v, want [f1 f2]", g.IDs)
			}
			return reply(req, &email.GetResponse{
				State:    "S0",
				List:     []*email.Email{testEmail("f1", "b-f1", "mb-in")},
				NotFound: []jmap.ID{"f2"},
			})
		},
		func(req *jmap.Request) (*jmap.Response, error) {
			return reply(req, &email.ChangesResponse{NewState: "S0"})
		},
	)
	fake.blobs["b-f1"] = []byte(rawPlain)
	s.dial = func(ctx context.Context) (*conn, error) { return testConn(fake), nil }

	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	mustSeen(t, db, "f1", true)
	fails, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 0 {
		t.Errorf("failures after sync = %+v, want none", fails)
	}
	found := false
	for _, content := range mdFiles(t, root) {
		if strings.Contains(content, naming.Hash8(testInstance+":f1")) || strings.Contains(content, "Hello World") {
			found = true
		}
	}
	if !found {
		t.Error("no archived .md for retried f1")
	}
}

// TestParseFallbackMinimalDoc: a blob that repeatedly fails to parse hits
// the skip threshold and is archived from the server's bodyValues instead,
// with a warning, clearing its failure entry.
func TestParseFallbackMinimalDoc(t *testing.T) {
	s, db, root := newTestSource(t)
	boxes := testBoxes()

	for range state.SkipThreshold - 1 {
		if _, err := db.RecordFailure(testInstance, "p1", "parse"); err != nil {
			t.Fatal(err)
		}
	}

	sent := time.Date(2026, 8, 4, 9, 55, 0, 0, time.FixedZone("", 2*3600))
	recv := testReceived
	fake := scripted(t,
		func(req *jmap.Request) (*jmap.Response, error) {
			g := callArgs[email.Get](t, req)
			if !g.FetchTextBodyValues {
				t.Error("fallback get must set fetchTextBodyValues")
			}
			if !slices.Equal(g.IDs, []jmap.ID{"p1"}) {
				t.Errorf("fallback get ids = %v", g.IDs)
			}
			return reply(req, &email.GetResponse{State: "S1", List: []*email.Email{{
				ID: "p1", BlobID: "b-p1", ThreadID: "T-p1",
				MailboxIDs: map[jmap.ID]bool{"mb-in": true},
				ReceivedAt: &recv, SentAt: &sent,
				Subject:   "Broken One",
				From:      []*mail.Address{{Name: "Al", Email: "al@example.com"}},
				To:        []*mail.Address{{Email: "bob@example.com"}},
				MessageID: []string{"mid-p1@example.com"},
				TextBody:  []*email.BodyPart{{PartID: "1"}},
				BodyValues: map[string]*email.BodyValue{
					"1": {Value: "plain text body", IsTruncated: true},
				},
			}}})
		},
	)
	fake.blobs["b-p1"] = []byte(rawBroken)

	em := testEmail("p1", "b-p1", "mb-in")
	if err := s.processEmail(context.Background(), testConn(fake), boxes, em); err != nil {
		t.Fatalf("processEmail: %v", err)
	}

	mustSeen(t, db, "p1", true)
	if fails, err := db.ListFailures(); err != nil || len(fails) != 0 {
		t.Errorf("failures = %+v (%v), want cleared", fails, err)
	}
	mds := mdFiles(t, root)
	if len(mds) != 1 {
		t.Fatalf("archived %d files, want 1", len(mds))
	}
	for rel, content := range mds {
		for _, want := range []string{
			"plain text body",
			"body recovered from JMAP bodyValues",
			"Broken One",
			"<mid-p1@example.com>",
			"Al <al@example.com>",
			"body text truncated by the server",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("%s: missing %q in:\n%s", rel, want, content)
			}
		}
	}
}

// TestClassifyTransport covers both error shapes go-jmap emits: the typed
// *jmap.RequestError (Content-Type exactly "application/json") and the bare
// "HTTP <code> <status>" error every other non-2xx becomes.
func TestClassifyTransport(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want retry.Class
	}{
		{name: "plain 503", err: errors.New("HTTP 503 503 Service Unavailable"), want: retry.Transient},
		{name: "plain 429", err: errors.New("HTTP 429 429 Too Many Requests"), want: retry.Transient},
		{name: "plain 401", err: errors.New("HTTP 401 401 Unauthorized"), want: retry.AuthBroken},
		{name: "plain 403", err: errors.New("HTTP 403 403 Forbidden"), want: retry.AuthBroken},
		{name: "plain 404 stays item-level", err: errors.New("HTTP 404 404 Not Found"), want: retry.Fatal},
		{name: "typed 503", err: &jmap.RequestError{Type: "about:blank", Status: 503}, want: retry.Transient},
		{name: "typed 429", err: &jmap.RequestError{Type: "about:blank", Status: 429}, want: retry.Transient},
		{name: "typed 401", err: &jmap.RequestError{Type: "about:blank", Status: 401}, want: retry.AuthBroken},
		{name: "typed 404 stays item-level", err: &jmap.RequestError{Type: "about:blank", Status: 404}, want: retry.Fatal},
		{name: "unrelated error untouched", err: errors.New("boom"), want: retry.Fatal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retry.Classify(classifyTransport(tt.err)); got != tt.want {
				t.Errorf("Classify(classifyTransport(%v)) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestGoJMAPPlainErrorFormatPinned drives the real go-jmap client against a
// non-JSON 503 response and asserts classifyTransport still recognizes the
// resulting error as Transient. A go-jmap upgrade that changes its
// decodeHttpError message format must fail here loudly.
func TestGoJMAPPlainErrorFormatPinned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	cl := &jmap.Client{
		HttpClient:      srv.Client(),
		SessionEndpoint: srv.URL + "/session",
		Session:         &jmap.Session{DownloadURL: srv.URL + "/blob/{blobId}/{type}/{name}?accountId={accountId}"},
	}
	_, err := cl.DownloadWithContext(context.Background(), "acct", "b1")
	if err == nil {
		t.Fatal("download succeeded, want an HTTP 503 error")
	}
	if got := retry.Classify(classifyTransport(err)); got != retry.Transient {
		t.Errorf("Classify(classifyTransport(%v)) = %v, want Transient — go-jmap's plain error format may have changed", err, got)
	}
}

// outageAPI simulates a blob-host outage: every download fails with the
// plain non-JSON error shape go-jmap produces for non-2xx responses.
type outageAPI struct{ downloads int }

func (o *outageAPI) Do(*jmap.Request) (*jmap.Response, error) {
	return nil, errors.New("unexpected JMAP request during outage test")
}

func (o *outageAPI) DownloadWithContext(context.Context, jmap.ID, jmap.ID) (io.ReadCloser, error) {
	o.downloads++
	return nil, errors.New("HTTP 503 503 Service Unavailable")
}

// TestTransientDownloadFailureAbortsRun: a 5xx from the blob host is
// environmental, not a bad item — after in-run retries it must abort the
// run and leave the failures ledger untouched, so an outage can never walk
// healthy emails toward the skip threshold.
func TestTransientDownloadFailureAbortsRun(t *testing.T) {
	s, db, _ := newTestSource(t)
	boxes := testBoxes()
	setRetryOpts(t, retry.Options{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Budget: time.Second})
	fake := &outageAPI{}

	em := testEmail("d2", "b-d2", "mb-in")
	err := s.processEmail(context.Background(), &conn{api: fake, accountID: "acct", maxCalls: 16}, boxes, em)
	if err == nil {
		t.Fatal("processEmail returned nil; a transient outage must abort the run")
	}
	if fake.downloads != 2 {
		t.Errorf("download attempted %d times, want 2 (retried before aborting)", fake.downloads)
	}
	fails, ferr := db.ListFailures()
	if ferr != nil {
		t.Fatal(ferr)
	}
	if len(fails) != 0 {
		t.Errorf("failures ledger = %+v, want empty (outage is not item poison)", fails)
	}
	mustSeen(t, db, "d2", false)
}

// TestDownloadFailureRecordsItem: a per-item 404 on the blob is recorded in
// the failures ledger and does not abort the run or advance Seen.
func TestDownloadFailureRecordsItem(t *testing.T) {
	s, db, _ := newTestSource(t)
	boxes := testBoxes()
	fake := &fakeAPI{t: t, blobs: map[jmap.ID][]byte{}} // no blob: download 404s

	em := testEmail("d1", "b-d1", "mb-in")
	if err := s.processEmail(context.Background(), testConn(fake), boxes, em); err != nil {
		t.Fatalf("processEmail: %v", err)
	}
	mustSeen(t, db, "d1", false)
	fails, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 1 || fails[0].ID != "d1" || fails[0].Attempts != 1 {
		t.Errorf("failures = %+v, want one d1 attempt", fails)
	}
}
