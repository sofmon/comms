package archive

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"save/internal/emailpipe"
	"save/internal/naming"
	"save/internal/state"
)

// --- helpers ---------------------------------------------------------------

// fakeInfo is a minimal os.FileInfo for injecting Writer.Stat results.
type fakeInfo struct {
	size int64
	mode fs.FileMode
}

func (f fakeInfo) Name() string       { return "fake" }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

// mdLinkTargets extracts the destinations of every "[text](<dest>)" link in
// md, in order — the exact form mdLinkDest writes.
var mdLinkRe = regexp.MustCompile(`\]\(<([^>]*)>\)`)

func mdLinkTargets(md string) []string {
	var out []string
	for _, m := range mdLinkRe.FindAllStringSubmatch(md, -1) {
		out = append(out, m[1])
	}
	return out
}

// tmpEntries lists the names staged in <root>/.tmp (renameio litter).
func tmpEntries(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, TempDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read .tmp: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// --- rename confirmation ---------------------------------------------------

// TestConfirmRenameFailurePaths: a rename that reports success but does not
// leave a regular file of exactly the written size behind must be an error,
// so the caller never commits a state-DB row for it. The FileProvider
// reconciles renames inside an iCloud container asynchronously, which is
// exactly the case this guards.
func TestConfirmRenameFailurePaths(t *testing.T) {
	content := []byte("day file\n")
	tests := []struct {
		name    string
		stat    func(string) (os.FileInfo, error)
		wantMsg string
	}{
		{
			name:    "destination vanished",
			stat:    func(string) (os.FileInfo, error) { return nil, fs.ErrNotExist },
			wantMsg: "could not be stat'ed",
		},
		{
			name: "destination is not a regular file",
			stat: func(string) (os.FileInfo, error) {
				return fakeInfo{size: int64(len(content)), mode: fs.ModeDir | 0o700}, nil
			},
			wantMsg: "not a regular file",
		},
		{
			name:    "destination truncated",
			stat:    func(string) (os.FileInfo, error) { return fakeInfo{size: 3}, nil },
			wantMsg: "wrote 9 bytes but the destination holds 3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &Writer{Root: t.TempDir(), TZ: tzAms, Stat: tt.stat}
			_, err := w.WriteChatDay("2026/08/07/gchat-work_space_team_ab12cd34.md", content)
			if err == nil {
				t.Fatal("WriteChatDay succeeded, want an unconfirmed-rename error")
			}
			if !errors.Is(err, ErrRenameUnconfirmed) {
				t.Errorf("error %v is not ErrRenameUnconfirmed", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tt.wantMsg)
			}
			// The staging dir must not keep litter even on the failure path.
			if litter := tmpEntries(t, w.Root); len(litter) != 0 {
				t.Errorf("temp litter after failed confirmation: %v", litter)
			}
		})
	}

	// WriteEmail must surface the same failure rather than returning a path
	// and hash the caller would then commit.
	w := &Writer{Root: t.TempDir(), TZ: tzAms,
		Stat: func(string) (os.FileInfo, error) { return nil, fs.ErrNotExist }}
	doc, meta, _ := fullEmailFixture(t)
	rel, hash, err := w.WriteEmail(doc, meta)
	if err == nil {
		t.Fatalf("WriteEmail succeeded (rel=%q hash=%q), want an unconfirmed-rename error", rel, hash)
	}
	if !errors.Is(err, ErrRenameUnconfirmed) {
		t.Errorf("WriteEmail error %v is not ErrRenameUnconfirmed", err)
	}
	if rel != "" || hash != "" {
		t.Errorf("failed WriteEmail returned rel=%q hash=%q, want empty", rel, hash)
	}
}

// TestConfirmRenameAcceptsRealWrites: the default (os.Lstat) confirmation
// must pass for every real write, including a zero-byte file and a replace.
func TestConfirmRenameAcceptsRealWrites(t *testing.T) {
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	rel := "2026/08/07/gchat-work_space_team_ab12cd34.md"
	for _, content := range [][]byte{[]byte("first\n"), {}, []byte("third, longer\n")} {
		if _, err := w.WriteChatDay(rel, content); err != nil {
			t.Fatalf("WriteChatDay(%d bytes): %v", len(content), err)
		}
		got, err := os.ReadFile(filepath.Join(w.Root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(content) {
			t.Errorf("on disk %q, want %q", got, content)
		}
	}
	doc, meta, _ := fullEmailFixture(t)
	if _, _, err := w.WriteEmail(doc, meta); err != nil {
		t.Errorf("WriteEmail with the real confirmation: %v", err)
	}
}

// TestStatHookIsOnlyTheConfirmation guards against the injected Stat being
// used for anything but the confirmation (it must not, for instance, decide
// whether a destination exists): a hook that always reports the right size
// still produces a correct archive.
func TestStatHookIsOnlyTheConfirmation(t *testing.T) {
	var statted []string
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	w.Stat = func(p string) (os.FileInfo, error) {
		statted = append(statted, p)
		return os.Lstat(p)
	}
	doc, meta, stem := fullEmailFixture(t)
	rel, _, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatalf("WriteEmail: %v", err)
	}
	// One confirmation per written file: two attachments plus the .md.
	if len(statted) != len(doc.Files)+1 {
		t.Errorf("confirmed %d paths (%v), want %d", len(statted), statted, len(doc.Files)+1)
	}
	wantLast := filepath.Join(w.Root, filepath.FromSlash(rel))
	if statted[len(statted)-1] != wantLast {
		t.Errorf("last confirmation was %q, want the .md %q", statted[len(statted)-1], wantLast)
	}
	// Files-first: every attachment is confirmed before the .md.
	for _, s := range statted[:len(statted)-1] {
		if !strings.Contains(s, stem+".d") {
			t.Errorf("unexpected confirmation before the .md: %q", s)
		}
	}
}

// --- dataless guard --------------------------------------------------------

// TestDatalessGuardNeverClobbersEvictedFile: when the destination's contents
// have been evicted to iCloud, the write must fail loudly and leave the
// placeholder exactly as it was — renaming over it would destroy the only
// local trace of a file whose bytes are in the cloud.
func TestDatalessGuardNeverClobbersEvictedFile(t *testing.T) {
	const rel = "2026/08/07/gchat-work_space_team_ab12cd34.md"

	tests := []struct {
		name string
		// hook is built per subtest around that subtest's own destination;
		// calls counts invocations so a hook can flip mid-write.
		hook func(abs string, calls *int) func(string) (bool, error)
		want string
		// wantCalls pins which of the three guard points fired, and
		// wantStaged whether any bytes were written to <root>/.tmp first.
		wantCalls  int
		wantStaged bool
	}{
		{
			// Evicted before the write even starts: the destination is
			// checked before anything is staged, so a 50 MB attachment is
			// not written out just to be thrown away.
			name: "destination dataless up front",
			hook: func(abs string, calls *int) func(string) (bool, error) {
				return func(p string) (bool, error) { *calls++; return p == abs, nil }
			},
			want:       "destination",
			wantCalls:  1,
			wantStaged: false,
		},
		{
			// Evicted while the bytes were being staged: caught by the
			// re-check immediately before the rename.
			name: "destination evicted mid-write",
			hook: func(abs string, calls *int) func(string) (bool, error) {
				return func(p string) (bool, error) {
					*calls++
					return p == abs && *calls > 1, nil
				}
			},
			want:       "destination",
			wantCalls:  3, // destination, staged temp file, destination again
			wantStaged: true,
		},
		{
			// The staged temp file itself is a placeholder: renaming it into
			// place would publish a file with no contents.
			name: "staged temp file dataless",
			hook: func(_ string, calls *int) func(string) (bool, error) {
				return func(p string) (bool, error) {
					*calls++
					return strings.Contains(p, string(filepath.Separator)+TempDirName+string(filepath.Separator)), nil
				}
			},
			want:       "staged temp file",
			wantCalls:  2, // destination, then the temp file
			wantStaged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A fresh root per subtest: one guard failing to fire must not
			// change what the next subtest observes on disk.
			root := t.TempDir()
			abs := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(abs, []byte("evicted placeholder\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			w := &Writer{Root: root, TZ: tzAms, Dataless: tt.hook(abs, &calls)}
			_, err := w.WriteChatDay(rel, []byte("replacement\n"))
			if err == nil {
				t.Fatal("WriteChatDay succeeded, want a dataless error")
			}
			if !errors.Is(err, ErrDataless) {
				t.Errorf("error %v is not ErrDataless", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name the %s", err, tt.want)
			}
			got, rerr := os.ReadFile(abs)
			if rerr != nil {
				t.Fatalf("destination gone after a refused write: %v", rerr)
			}
			if string(got) != "evicted placeholder\n" {
				t.Errorf("destination was modified: %q", got)
			}
			if litter := tmpEntries(t, root); len(litter) != 0 {
				t.Errorf("temp litter after a refused write: %v", litter)
			}
			if calls != tt.wantCalls {
				t.Errorf("dataless check ran %d times, want %d", calls, tt.wantCalls)
			}
			// Staging only ever happens after the up-front destination
			// check has passed.
			_, serr := os.Stat(filepath.Join(root, TempDirName))
			if staged := serr == nil; staged != tt.wantStaged {
				t.Errorf("staged = %v, want %v (stat %s: %v)", staged, tt.wantStaged, TempDirName, serr)
			}
		})
	}
}

// TestDatalessCheckErrorFailsTheWrite: an unreadable flag is not proof the
// file is safe to clobber, so the error propagates.
func TestDatalessCheckErrorFailsTheWrite(t *testing.T) {
	boom := errors.New("lstat: permission denied")
	w := &Writer{Root: t.TempDir(), TZ: tzAms,
		Dataless: func(string) (bool, error) { return false, boom }}
	_, err := w.WriteChatDay("2026/08/07/x.md", []byte("x"))
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap %v", err, boom)
	}
}

// TestDatalessGuardBlocksEmailBeforeAnyFileLands: the guard must also stop an
// email write, and the failure must be reported instead of a path+hash.
func TestDatalessGuardBlocksEmailBeforeAnyFileLands(t *testing.T) {
	w := &Writer{Root: t.TempDir(), TZ: tzAms,
		Dataless: func(string) (bool, error) { return true, nil }}
	doc, meta, _ := fullEmailFixture(t)
	rel, hash, err := w.WriteEmail(doc, meta)
	if !errors.Is(err, ErrDataless) {
		t.Fatalf("WriteEmail error = %v, want ErrDataless", err)
	}
	if rel != "" || hash != "" {
		t.Errorf("returned rel=%q hash=%q, want empty", rel, hash)
	}
	var found []string
	_ = filepath.WalkDir(w.Root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("refused email left files: %v", found)
	}
}

// TestIsDatalessDefault exercises the real platform helper: on macOS it runs
// the SF_DATALESS lstat, elsewhere the portable no-op. Ordinary files and
// missing paths are never dataless on either.
func TestIsDatalessDefault(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain.bin")
	if err := os.WriteFile(file, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		file,
		dir,
		filepath.Join(dir, "missing"),
		filepath.Join(file, "not-a-dir"), // ENOTDIR
	} {
		ok, err := isDataless(p)
		if err != nil {
			t.Errorf("isDataless(%s): %v", p, err)
		}
		if ok {
			t.Errorf("isDataless(%s) = true, want false", p)
		}
	}
	// A writer with no hook uses it and writes normally.
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	if _, err := w.WriteChatDay("2026/08/07/x.md", []byte("x")); err != nil {
		t.Errorf("WriteChatDay with the default dataless check: %v", err)
	}
}

// --- path budget -----------------------------------------------------------

// TestPathBudgetFailsLoudly: an over-long destination must fail with an
// actionable error that names the archive root, not produce a file nothing
// can open.
func TestPathBudgetFailsLoudly(t *testing.T) {
	// A root that leaves too little room for "YYYY/MM/DD/<stem>.md". Grown
	// in 41-byte steps so it stays under PATH_MAX (the point is the
	// archiver's own 1000-byte budget, not the kernel's limit).
	base := t.TempDir()
	longRoot := base
	for naming.PathBudget(longRoot) > 40 {
		longRoot = filepath.Join(longRoot, strings.Repeat("d", 40))
	}
	w := &Writer{Root: longRoot, TZ: tzAms}
	doc, meta, _ := fullEmailFixture(t)
	_, _, err := w.WriteEmail(doc, meta)
	if err == nil {
		t.Fatal("WriteEmail succeeded, want a path-budget error")
	}
	if !errors.Is(err, naming.ErrPathTooLong) {
		t.Errorf("error %v is not naming.ErrPathTooLong", err)
	}
	for _, want := range []string{"archive root", longRoot, "shorter path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// Pre-validated: not one directory of the doomed path is created.
	if entries, rerr := os.ReadDir(base); rerr != nil || len(entries) != 0 {
		t.Errorf("over-budget write created %v under the root (err=%v)", entries, rerr)
	}

	// A single over-long component is reported as such (a connector can hand
	// the writer a name the sanitizer never produced).
	w2 := &Writer{Root: t.TempDir(), TZ: tzAms}
	long := strings.Repeat("n", naming.MaxComponentBytes+1) + ".md"
	if _, err := w2.WriteChatDay("2026/08/07/"+long, []byte("x")); !errors.Is(err, naming.ErrComponentTooLong) {
		t.Errorf("WriteChatDay(over-long component) error = %v, want naming.ErrComponentTooLong", err)
	}

	// A path that fits is written, so the budget is not merely paranoid: the
	// real container root leaves ~905 bytes and the deepest path needs ~214.
	w3 := &Writer{Root: t.TempDir(), TZ: tzAms}
	shortDoc, shortMeta, _ := fullEmailFixture(t)
	if _, _, err := w3.WriteEmail(shortDoc, shortMeta); err != nil {
		t.Errorf("WriteEmail under a short root: %v", err)
	}
}

// --- iCloud sync exclusion -------------------------------------------------

// TestWriterRejectsSyncExcludedDestinations: a name iCloud silently refuses
// to sync would produce a file that is written, hashed, and verified locally
// but never uploaded — the worst failure this archiver can have. The writer
// cannot rename it (the .md's links already point at it), so it fails loudly.
func TestWriterRejectsSyncExcludedDestinations(t *testing.T) {
	for _, rel := range []string{
		"2026/08/07/x.d/draft.tmp",       // excluded extension
		"2026/08/07/x.d/Dropbox",         // excluded whole name
		"2026/08/07/x.d/notes.nosync.md", // excluded substring
		"2026/08/07/x.d/~$plan.docx",     // excluded prefix
		"2026/08/07/tmp/report.pdf",      // excluded directory component
		"2026/08/07/x.d/report.PhotosLibrary",
	} {
		w := &Writer{Root: t.TempDir(), TZ: tzAms}
		_, err := w.WriteChatDay(rel, []byte("x"))
		if err == nil {
			t.Errorf("WriteChatDay(%q) succeeded, want ErrSyncExcluded", rel)
			continue
		}
		if !errors.Is(err, ErrSyncExcluded) {
			t.Errorf("WriteChatDay(%q) error %v is not ErrSyncExcluded", rel, err)
		}
		if _, serr := os.Stat(filepath.Join(w.Root, filepath.FromSlash(rel))); !os.IsNotExist(serr) {
			t.Errorf("excluded destination %q was created (err=%v)", rel, serr)
		}
	}

	// Same for an email attachment: the whole message fails, and — because
	// the check runs in the pre-validation pass — no sibling attachment is
	// left stranded on disk.
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	doc, meta, stem := fullEmailFixture(t)
	doc.Files = append(doc.Files, emailpipe.File{Rel: stem + ".d/draft.tmp", Content: []byte("x")})
	if _, _, err := w.WriteEmail(doc, meta); !errors.Is(err, ErrSyncExcluded) {
		t.Errorf("WriteEmail error = %v, want ErrSyncExcluded", err)
	}
	var found []string
	_ = filepath.WalkDir(w.Root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("rejected email left files: %v", found)
	}

	// The staging directory is excluded ON PURPOSE and must keep working:
	// it is where every write is staged.
	if !naming.SyncExcluded(TempDirName) {
		t.Errorf("%q is expected to be sync-excluded (that is why partial writes never upload)", TempDirName)
	}
	ok := &Writer{Root: t.TempDir(), TZ: tzAms}
	if _, err := ok.WriteChatDay("2026/08/07/gchat-work_space_team_ab12cd34.md", []byte("x")); err != nil {
		t.Errorf("normal write through the excluded staging dir failed: %v", err)
	}
}

// TestEmailAttachmentExcludedNameIsSyncableAndLinked is the end-to-end shape
// of the hazard: a sender attaches a file whose name iCloud would refuse to
// sync. The pipeline must land a syncable file on disk and the .md's link
// must resolve to that exact file.
//
// The fixture carries both shapes of the hazard:
//
//   - "plan.NoSync.pdf" is excluded by a SUBSTRING rule, which the sanitizer
//     breaks without touching the extension — so the attachment policy still
//     sees a .pdf and the file is stored and linked.
//   - "draft.tmp" is excluded by its EXTENSION, which the sanitizer demotes
//     into the stem by appending ".bin". Since the allowlist runs on the
//     FINAL sanitized name, that file now keys on .bin and is refused. It is
//     recorded in doc.Skipped rather than dropped, and nothing for it lands
//     on disk. Any attachment whose extension iCloud excludes is unstorable
//     for this reason; the honest skip record is what makes it recoverable.
func TestEmailAttachmentExcludedNameIsSyncableAndLinked(t *testing.T) {
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	server := time.Date(2026, 8, 7, 12, 32, 5, 0, time.UTC)
	meta := EmailMeta{
		Source: "gmail:work", SourceTag: "gmail-work", Account: "u@example.com",
		AccountLabel: "work", StableID: "abc123", ServerTime: server,
	}
	// The connector derives the attach dir exactly as the writer does.
	stem := naming.EmailStem(server.In(tzAms), meta.SourceTag, "Draft", naming.Hash8(meta.Source+":"+meta.StableID))
	attachDir := naming.AttachDir(stem)

	raw := strings.ReplaceAll(`From: Ann <ann@example.com>
To: Bob <bob@example.com>
Subject: Draft
Date: Fri, 07 Aug 2026 15:32:05 +0300
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="B"

--B
Content-Type: text/plain; charset=utf-8

See attached.

--B
Content-Type: application/pdf; name="plan.NoSync.pdf"
Content-Disposition: attachment; filename="plan.NoSync.pdf"

%PDF-1.7
DRAFT-BYTES
--B
Content-Type: application/octet-stream; name="draft.tmp"
Content-Disposition: attachment; filename="draft.tmp"

MORE-DRAFT-BYTES
--B--
`, "\n", "\r\n")

	doc, err := emailpipe.Render([]byte(raw), attachDir)
	if err != nil {
		t.Fatalf("emailpipe.Render: %v", err)
	}
	if len(doc.Files) != 1 {
		t.Fatalf("rendered %d files, want 1: %v", len(doc.Files), doc.Files)
	}
	// The sanitizer breaks the excluded substring without moving the
	// extension, so the allowlist still sees a .pdf.
	if got, want := path.Base(doc.Files[0].Rel), "plan_NoSync.pdf"; got != want {
		t.Fatalf("attachment name = %q, want %q", got, want)
	}
	// The excluded EXTENSION becomes .bin, which is not on the allowlist:
	// recorded, never written, never silent.
	if len(doc.Skipped) != 1 {
		t.Fatalf("skipped %d files, want 1: %v", len(doc.Skipped), doc.Skipped)
	}
	if got, want := doc.Skipped[0].Name, "draft.tmp.bin"; got != want {
		t.Errorf("skipped name = %q, want %q", got, want)
	}
	if doc.Skipped[0].Rel != "" || doc.Skipped[0].Content != nil {
		t.Errorf("skipped file carries a path or bytes: %+v", doc.Skipped[0])
	}

	rel, _, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatalf("WriteEmail: %v", err)
	}

	// Nothing in the archive-relative path may be sync-excluded.
	attRel := path.Join(path.Dir(rel), doc.Files[0].Rel)
	for _, p := range []string{rel, attRel} {
		if c := naming.FirstSyncExcluded(p); c != "" {
			t.Errorf("path %q has sync-excluded component %q", p, c)
		}
	}

	// The link in the .md must resolve, from the .md's own directory, to the
	// file that was actually written.
	absMD := filepath.Join(w.Root, filepath.FromSlash(rel))
	md, err := os.ReadFile(absMD)
	if err != nil {
		t.Fatal(err)
	}
	links := mdLinkTargets(string(md))
	if len(links) != 1 {
		t.Fatalf("found %d links in the .md, want 1: %v\n%s", len(links), links, md)
	}
	if links[0] != attachDir+"/plan_NoSync.pdf" {
		t.Errorf("link = %q, want %q", links[0], attachDir+"/plan_NoSync.pdf")
	}
	target := filepath.Join(filepath.Dir(absMD), filepath.FromSlash(links[0]))
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the .md links to a file that does not exist: %v", err)
	}
	if !strings.Contains(string(got), "DRAFT-BYTES") {
		t.Errorf("linked file holds %q, want the attachment bytes", got)
	}
	// The frontmatter list and the link must agree.
	if !strings.Contains(string(md), "    - "+attachDir+"/plan_NoSync.pdf\n") {
		t.Errorf("frontmatter does not list the written attachment:\n%s", md)
	}
	// And no leftover "draft.tmp" anywhere on disk.
	_ = filepath.WalkDir(w.Root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && naming.SyncExcluded(d.Name()) && filepath.Join(w.Root, TempDirName) != p {
			t.Errorf("sync-excluded item on disk: %s", p)
		}
		return nil
	})
}

// TestChatAttachmentDraftTmpIsSyncableAndLinked is the chat-side twin: the
// gchat connector writes attachment blobs through WriteChatDay, and the day
// file's link must point at the file that landed.
func TestChatAttachmentDraftTmpIsSyncableAndLinked(t *testing.T) {
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	const spaceName = "spaces/AAAA"
	space := state.Space{
		Source: chatInstance, Name: spaceName, Type: "space",
		DisplayName: "Team Platform", DisplaySlug: "team-platform",
	}
	stem := naming.ChatStem(state.Tag(chatInstance), space.Type, space.DisplaySlug, naming.Hash8(spaceName))
	dayRel := "2026/08/07/" + stem + ".md"
	local := time.Date(2026, 8, 7, 14, 32, 5, 0, tzAms)

	// Exactly the derivation in gchat's mapping.go.
	name := naming.ChatAttachmentName(local, "ab12cd34", naming.SanitizeFilename("draft.tmp", "attachment-1"))
	if want := "143205_ab12cd34_draft.tmp.bin"; name != want {
		t.Fatalf("chat attachment name = %q, want %q", name, want)
	}
	attRel := path.Join("2026/08/07", stem+".d", name)
	if c := naming.FirstSyncExcluded(attRel); c != "" {
		t.Fatalf("chat attachment path %q has sync-excluded component %q", attRel, c)
	}

	blob := []byte("CHAT-DRAFT-BYTES")
	if _, err := w.WriteChatDay(attRel, blob); err != nil {
		t.Fatalf("write attachment blob: %v", err)
	}

	msg := state.ChatMessage{
		Name: spaceName + "/messages/M1", Space: spaceName, SenderID: "users/111",
		CreateTime: local.UTC(), DayBucket: "2026-08-07", RawJSON: `{"text":"Draft attached."}`,
	}
	atts := map[string][]state.Attachment{
		msg.Name: {{
			Source: chatInstance, StableID: msg.Name, PartKey: "p1",
			RelPath: attRel, Status: state.AttachmentDone,
		}},
	}
	content := RenderChatDay(space, "2026-08-07", []state.ChatMessage{msg}, atts, nil, nil, tzAms)
	if _, err := w.WriteChatDay(dayRel, content); err != nil {
		t.Fatalf("WriteChatDay: %v", err)
	}

	// The rendered display name drops the deterministic prefix but keeps the
	// syncable file name.
	if got := chatAttachmentDisplayName(attRel); got != "draft.tmp.bin" {
		t.Errorf("display name = %q, want %q", got, "draft.tmp.bin")
	}
	links := mdLinkTargets(string(content))
	if len(links) != 1 {
		t.Fatalf("found %d links, want 1: %v\n%s", len(links), links, content)
	}
	absDay := filepath.Join(w.Root, filepath.FromSlash(dayRel))
	target := filepath.Join(filepath.Dir(absDay), filepath.FromSlash(links[0]))
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the day file links to a file that does not exist (%q): %v", links[0], err)
	}
	if string(got) != string(blob) {
		t.Errorf("linked file holds %q, want %q", got, blob)
	}
}

// TestWriterGuardsAreDeterministic: the new guards must not disturb the
// idempotency invariant — the same inputs still produce the same bytes at the
// same path on a re-run over an existing archive.
func TestWriterGuardsAreDeterministic(t *testing.T) {
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	doc, meta, _ := fullEmailFixture(t)
	rel1, hash1, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatalf("first WriteEmail: %v", err)
	}
	rel2, hash2, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatalf("second WriteEmail (over an existing file): %v", err)
	}
	if rel1 != rel2 || hash1 != hash2 {
		t.Errorf("re-write not stable: (%q,%q) vs (%q,%q)", rel1, hash1, rel2, hash2)
	}
	if litter := tmpEntries(t, w.Root); len(litter) != 0 {
		t.Errorf("temp litter: %v", litter)
	}
}

// Compile-time proof that the injection seams keep the platform signatures,
// so a test hook is always substitutable for the real implementation.
var (
	_ = func(w *Writer) { w.Dataless = isDataless }
	_ = func(w *Writer) { w.Stat = os.Lstat }
)
