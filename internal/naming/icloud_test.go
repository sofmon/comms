package naming

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// excludedCorpus covers every verified exclusion rule, with case variants.
var excludedCorpus = []string{
	// contains ".nosync" / ".weakpkg", anywhere, any case
	"file.nosync",
	".nosync",
	"plan.NoSync.pdf",
	"PLAN.NOSYNC",
	"tmpdir.nosync",
	"bundle.weakpkg",
	"Bundle.WeakPkg.zip",
	// extension "tmp"
	"draft.tmp",
	"Draft.TMP",
	"a.b.c.tmp",
	// component named "tmp" / ".tmp"
	"tmp",
	"TMP",
	".tmp",
	".TMP",
	// exact names
	".DS_Store",
	".ds_store",
	".ubd",
	"desktop.ini",
	"DESKTOP.INI",
	"$RECYCLE.BIN",
	"$recycle.bin",
	"Dropbox",
	"DROPBOX",
	"dropbox",
	".dropbox",
	".dropbox.attr",
	"OneDrive",
	"onedrive",
	"IDrive-Sync",
	"idrive-sync",
	"Microsoft User Data",
	"microsoft user data",
	"icon\r",
	"ICON\r",
	// prefixes
	"~$q3.docx",
	"~$",
	"(A Document Being Saved By Word)",
	"(a document being saved by TextEdit 2)",
	// photo-library extensions
	"Photos.photoslibrary",
	"Old.PhotoLibrary",
	"Shoot.aplibrary",
	"x.migratedphotolibrary",
	"y.MigratedAperturelibrary",
	"backup.ubd",
}

// syncableCorpus are names that must NOT be flagged: the rules match a whole
// component, not a substring of one.
var syncableCorpus = []string{
	"invoice.pdf",
	"dropbox-export.csv",
	"Dropbox.pdf",
	"my dropbox notes.md",
	"onedrive_backup.zip",
	"tmpfile",
	"tmp.pdf",
	"temp",
	"nosync.txt",
	"a_nosync.pdf",
	"desktop.ini.pdf",
	"~q3.docx",
	"$RECYCLE.pdf",
	"143205_ab12cd34_notes.pdf",
	"143205_gmail-work_re-invoice-july_a1b2c3d4",
	"143205_gmail-work_re-invoice-july_a1b2c3d4.d",
	".obsidian",
	"Разговор.md",
	"emoji✅.md",
	"",
}

func TestSyncExcluded(t *testing.T) {
	for _, name := range excludedCorpus {
		if !SyncExcluded(name) {
			t.Errorf("SyncExcluded(%q) = false, want true", name)
		}
	}
	for _, name := range syncableCorpus {
		if SyncExcluded(name) {
			t.Errorf("SyncExcluded(%q) = true, want false", name)
		}
	}
}

// The archiver's staging directory must stay excluded: that is what keeps
// partially written files off the wire. If this ever flips, writer.go's
// TempDirName has to change with it.
func TestStagingDirStaysExcluded(t *testing.T) {
	if !SyncExcluded(".tmp") {
		t.Fatal(`SyncExcluded(".tmp") = false: the archiver relies on <root>/.tmp being sync-excluded`)
	}
}

// A multi-byte rune that lowercases to ASCII (U+212A KELVIN SIGN -> 'k')
// must still match, and the rewrite must not corrupt the string.
func TestSyncExcludedUnicodeFold(t *testing.T) {
	name := "bundle.weaKpkg"
	if !SyncExcluded(name) {
		t.Fatalf("SyncExcluded(%q) = false: Kelvin sign folds to 'k'", name)
	}
	got := ensureSyncable(name)
	if SyncExcluded(got) {
		t.Fatalf("ensureSyncable(%q) = %q, still excluded", name, got)
	}
	if !strings.HasSuffix(got, "weaKpkg") {
		t.Fatalf("ensureSyncable(%q) = %q, mangled the multi-byte rune", name, got)
	}
}

func TestEnsureSyncableRewrite(t *testing.T) {
	cases := []struct{ in, want string }{
		// extension rule: extension demoted into the stem, ".bin" added
		{"draft.tmp", "draft.tmp.bin"},
		{"Draft.TMP", "Draft.TMP.bin"},
		{"backup.ubd", "backup.ubd.bin"},
		{"Photos.photoslibrary", "Photos.photoslibrary.bin"},
		// contains rule: the token's dot becomes an underscore, case kept
		{"plan.NoSync.pdf", "plan_NoSync.pdf"},
		{"file.nosync", "file_nosync"},
		{".nosync", "_nosync"},
		{"a.nosync.b.nosync.c", "a_nosync.b_nosync.c"},
		{"Bundle.WeakPkg.zip", "Bundle_WeakPkg.zip"},
		// whole-name rule: marker in front, extension untouched
		{"Dropbox", "_Dropbox"},
		{"DROPBOX", "_DROPBOX"},
		{"desktop.ini", "_desktop.ini"},
		{"$RECYCLE.BIN", "_$RECYCLE.BIN"},
		{"Microsoft User Data", "_Microsoft User Data"},
		{".DS_Store", "_.DS_Store"},
		{"tmp", "_tmp"},
		// both extension and whole-name rules
		{".tmp", ".tmp.bin"},
		// prefix rule
		{"~$q3.docx", "_~$q3.docx"},
		{"(A Document Being Saved By Word)", "_(A Document Being Saved By Word)"},
		// combined: contains + extension + prefix
		{"~$plan.nosync.tmp", "_~$plan_nosync.tmp.bin"},
		// untouched
		{"invoice.pdf", "invoice.pdf"},
		{"", ""},
	}
	for _, c := range cases {
		got := ensureSyncable(c.in)
		if got != c.want {
			t.Errorf("ensureSyncable(%q) = %q, want %q", c.in, got, c.want)
		}
		if got != "" && SyncExcluded(got) {
			t.Errorf("ensureSyncable(%q) = %q, still excluded", c.in, got)
		}
		if again := ensureSyncable(got); again != got {
			t.Errorf("ensureSyncable not idempotent: %q -> %q -> %q", c.in, got, again)
		}
	}
}

func TestSanitizeFilenameNeverExcluded(t *testing.T) {
	cases := []struct{ in, want string }{
		{"draft.tmp", "draft.tmp.bin"},
		{"Draft.TMP", "Draft.TMP.bin"},
		{"Dropbox", "_Dropbox"},
		{"DROPBOX", "_DROPBOX"},
		{"IDrive-Sync", "_IDrive-Sync"},
		{"desktop.ini", "_desktop.ini"},
		{"~$x.docx", "_~$x.docx"},
		{"quarterly.nosync.xlsx", "quarterly_nosync.xlsx"},
		{"a.NOSYNC.pdf", "a_NOSYNC.pdf"},
		{"$RECYCLE.BIN", "_$RECYCLE.BIN"},
		{"Photos.photoslibrary", "Photos.photoslibrary.bin"},
		// control characters go first, so "icon\r" collapses to a safe name
		{"icon\r", "icon"},
		// "con" is a reserved device AND ".tmp" is excluded: both guards fire
		{"con.tmp", "con-x.tmp.bin"},
	}
	for _, c := range cases {
		got := SanitizeFilename(c.in, "attachment-1.bin")
		if got != c.want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeFilenameIdempotent(t *testing.T) {
	var corpus []string
	corpus = append(corpus, excludedCorpus...)
	corpus = append(corpus, syncableCorpus...)
	corpus = append(corpus,
		"weird/na:me*?.pdf",
		"trailing. . .",
		"...",
		"CON.txt",
		"café.pdf",
		strings.Repeat("x", 300)+".tmp",
		strings.Repeat("é", 80)+".nosync.pdf",
		strings.Repeat("a", 96)+".tmp",
		strings.Repeat("a", 100),
		"Dropbox"+strings.Repeat("z", 200),
	)
	for _, in := range corpus {
		once := SanitizeFilename(in, "attachment-1.bin")
		twice := SanitizeFilename(once, "attachment-1.bin")
		if once != twice {
			t.Errorf("SanitizeFilename not idempotent for %q: %q -> %q", in, once, twice)
		}
	}
}

// Property: whatever goes in, the output is syncable, within the 100-byte
// cap, on a rune boundary, and non-empty.
func TestSanitizeFilenamePropertyNeverExcluded(t *testing.T) {
	var corpus []string
	corpus = append(corpus, excludedCorpus...)
	corpus = append(corpus, syncableCorpus...)

	// Cross every excluded token with prefixes, suffixes and padding that
	// exercise the length path at the same time.
	pads := []string{"", "x", strings.Repeat("y", 90), strings.Repeat("z", 250), strings.Repeat("é", 60)}
	for _, base := range excludedCorpus {
		for _, pad := range pads {
			corpus = append(corpus, pad+base, base+pad, pad+base+pad)
		}
	}
	// Nasty extras: forbidden characters, controls, dots, spaces, NFD.
	corpus = append(corpus,
		"a/b\\c:d*e?f\"g<h>i|j.tmp",
		"\x00\x01\x02.nosync",
		". . . .tmp . . .",
		"   Dropbox   ",
		"café́.tmp",
		strings.Repeat(".", 50)+"tmp",
		strings.Repeat("~$", 60)+".weakpkg",
		"\r\n\t",
	)

	for _, in := range corpus {
		got := SanitizeFilename(in, "attachment-1.bin")
		if got == "" {
			t.Errorf("SanitizeFilename(%q) returned empty", in)
			continue
		}
		if SyncExcluded(got) {
			t.Errorf("SanitizeFilename(%q) = %q, which iCloud would silently skip", in, got)
		}
		if len(got) > filenameMaxBytes {
			t.Errorf("SanitizeFilename(%q) = %q: %d bytes exceeds the %d-byte cap", in, got, len(got), filenameMaxBytes)
		}
		if !isValidUTF8Boundary(got) {
			t.Errorf("SanitizeFilename(%q) = %q: cut mid-rune", in, got)
		}
		if strings.ContainsAny(got, `/\:*?"<>|`) {
			t.Errorf("SanitizeFilename(%q) = %q: forbidden character survived", in, got)
		}
	}
}

func isValidUTF8Boundary(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

// The 100-byte cap must survive the rewrite: the guard's additions come out
// of the stem, not out of the budget.
func TestSanitizeFilenameCapHoldsAfterRewrite(t *testing.T) {
	cases := []string{
		strings.Repeat("a", 96) + ".tmp",  // exactly 100 before the rewrite
		strings.Repeat("a", 300) + ".tmp", // truncated, then rewritten
		strings.Repeat("a", 99) + ".tmp",
		strings.Repeat("é", 60) + ".nosync.tmp",
		"Dropbox" + strings.Repeat("z", 300),
		strings.Repeat("a", 300),
	}
	for _, in := range cases {
		got := SanitizeFilename(in, "attachment-1.bin")
		if len(got) > filenameMaxBytes {
			t.Errorf("SanitizeFilename(%q…) = %q: %d bytes exceeds %d", in[:min(20, len(in))], got, len(got), filenameMaxBytes)
		}
		if SyncExcluded(got) {
			t.Errorf("SanitizeFilename(%q…) = %q, still excluded", in[:min(20, len(in))], got)
		}
	}
	// The real extension must still be reachable after a truncate+rewrite.
	if got := SanitizeFilename(strings.Repeat("a", 300)+".tmp", "f"); !strings.HasSuffix(got, ".tmp.bin") {
		t.Errorf("truncate+rewrite lost the original extension: %q", got)
	}
}

func TestSlugNeverExcluded(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Dropbox", "dropbox-x"},
		{"DROPBOX", "dropbox-x"},
		{"OneDrive", "onedrive-x"},
		{"tmp", "tmp-x"},
		{"TMP", "tmp-x"},
		{"IDrive Sync", "idrive-sync-x"},
		{"IDrive-Sync", "idrive-sync-x"},
		// not excluded: the rule matches a whole component
		{"Dropbox migration", "dropbox-migration"},
		{"Microsoft User Data", "microsoft-user-data"},
		{"desktop.ini", "desktop-ini"},
		{"CON", "con-x"},
	}
	for _, c := range cases {
		got := Slug(c.in, "no-subject")
		if got != c.want {
			t.Errorf("Slug(%q) = %q, want %q", c.in, got, c.want)
		}
		if SyncExcluded(got) {
			t.Errorf("Slug(%q) = %q, which iCloud would silently skip", c.in, got)
		}
		if again := Slug(got, "no-subject"); again != got {
			t.Errorf("Slug not idempotent: %q -> %q -> %q", c.in, got, again)
		}
		if len(got) > slugMaxBytes {
			t.Errorf("Slug(%q) = %q: %d bytes exceeds %d", c.in, got, len(got), slugMaxBytes)
		}
	}
}

// Every generated name — stem, attachment dir, chat attachment — must be
// syncable, including when the free-text field is an exclusion-list word.
func TestGeneratedNamesNeverExcluded(t *testing.T) {
	ts := time.Date(2026, 8, 7, 14, 32, 5, 0, time.UTC)
	subjects := []string{"Dropbox", "tmp", "OneDrive", "IDrive Sync", "~$draft", "Re: Invoice", ""}
	for _, subj := range subjects {
		stem := EmailStem(ts, "gmail-work", subj, "a1b2c3d4")
		checkSyncable(t, "EmailStem("+subj+")", stem)
		checkSyncable(t, "AttachDir(EmailStem("+subj+"))", AttachDir(stem))

		cs := ChatStem("gchat-work", "space", Slug(subj, "untitled"), "9f8e7d6c")
		checkSyncable(t, "ChatStem("+subj+")", cs)
		checkSyncable(t, "AttachDir(ChatStem("+subj+"))", AttachDir(cs))
	}
	// A slug frozen by an older build (before the Slug guard existed) still
	// yields a syncable stem, because iCloud matches the whole component.
	checkSyncable(t, "ChatStem(frozen dropbox slug)", ChatStem("gchat-work", "space", "dropbox", "9f8e7d6c"))

	// Chat attachment names: the tail is the exposed surface.
	for _, tail := range []string{"notes.pdf", "draft.tmp", "plan.nosync.pdf", "Dropbox"} {
		checkSyncable(t, "ChatAttachmentName("+tail+")", ChatAttachmentName(ts, "ab12cd34", tail))
	}
	// The sanitized tail is the normal path and must not be disturbed: the
	// "HHMMSS_hash8_" prefix chatrender strips has to survive verbatim.
	got := ChatAttachmentName(ts, "ab12cd34", SanitizeFilename("draft.tmp", "f"))
	if got != "143205_ab12cd34_draft.tmp.bin" {
		t.Errorf("ChatAttachmentName = %q", got)
	}
}

func checkSyncable(t *testing.T, what, name string) {
	t.Helper()
	if SyncExcluded(name) {
		t.Errorf("%s = %q, which iCloud would silently skip", what, name)
	}
}

func TestUniqueKeepsNamesSyncable(t *testing.T) {
	taken := map[string]bool{}
	// Defensive: an unsanitized name handed straight to Unique.
	if got := Unique(taken, "draft.tmp"); got != "draft.tmp.bin" {
		t.Errorf("Unique(%q) = %q, want %q", "draft.tmp", got, "draft.tmp.bin")
	}
	if got := Unique(taken, "draft.tmp"); got != "draft.tmp_2.bin" {
		t.Errorf("second Unique(%q) = %q, want %q", "draft.tmp", got, "draft.tmp_2.bin")
	}
	// Suffixing a safe name never produces an excluded one.
	taken2 := map[string]bool{}
	for _, n := range append(append([]string{}, excludedCorpus...), syncableCorpus...) {
		if n == "" {
			continue
		}
		for i := 0; i < 3; i++ {
			if got := Unique(taken2, SanitizeFilename(n, "attachment-1.bin")); SyncExcluded(got) {
				t.Errorf("Unique produced excluded name %q from %q", got, n)
			}
		}
	}
}

func TestFirstSyncExcluded(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026/08/07/143205_gmail-work_x_a1b2c3d4.md", ""},
		{"2026/08/07/draft.tmp", "draft.tmp"},
		{"2026/08/07/stem.d/plan.nosync.pdf", "plan.nosync.pdf"},
		{".tmp/staged-123", ".tmp"},
		{filepath.Join("a", "Dropbox", "b.pdf"), "Dropbox"},
		{"", ""},
	}
	for _, c := range cases {
		if got := FirstSyncExcluded(c.in); got != c.want {
			t.Errorf("FirstSyncExcluded(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCheckComponentAndPath(t *testing.T) {
	if err := CheckComponent(strings.Repeat("a", MaxComponentBytes)); err != nil {
		t.Errorf("255-byte component rejected: %v", err)
	}
	err := CheckComponent(strings.Repeat("a", MaxComponentBytes+1))
	if !errors.Is(err, ErrComponentTooLong) {
		t.Errorf("256-byte component: err = %v, want ErrComponentTooLong", err)
	}

	if err := CheckPath("/Users/x/archive/2026/08/07/note.md"); err != nil {
		t.Errorf("short path rejected: %v", err)
	}
	// Whole-path budget.
	long := "/" + strings.Repeat(strings.Repeat("a", 100)+"/", 11)
	if err := CheckPath(long); !errors.Is(err, ErrPathTooLong) {
		t.Errorf("%d-byte path: err = %v, want ErrPathTooLong", len(long), err)
	}
	// A path inside the budget but with an oversized component.
	if err := CheckPath("/root/" + strings.Repeat("b", 300) + "/x.md"); !errors.Is(err, ErrComponentTooLong) {
		t.Errorf("oversized component in a short path: err = %v, want ErrComponentTooLong", err)
	}
}

func TestCheckArchivePathAndBudget(t *testing.T) {
	const root = "/Users/you/Library/Mobile Documents/iCloud~md~obsidian/Documents/MyVault/Communication"
	if err := CheckArchivePath(root, "2026/08/07/143205_gmail-work_re-invoice-july_a1b2c3d4.d/report.pdf"); err != nil {
		t.Errorf("realistic archive path rejected: %v", err)
	}
	// The real container root must still leave room for the deepest path the
	// archiver can generate: "YYYY/MM/DD/" + 100-byte stem + ".d/" + 100-byte
	// attachment name.
	const deepest = len("2026/08/07/") + filenameMaxBytes + len(".d/") + filenameMaxBytes
	if got := PathBudget(root); got < deepest {
		t.Errorf("PathBudget(%q) = %d, need at least %d", root, got, deepest)
	}

	deep := strings.Repeat("d/", 20) + strings.Repeat("e", 90) + ".md"
	if err := CheckArchivePath("/"+strings.Repeat("r", 900), deep); !errors.Is(err, ErrPathTooLong) {
		t.Errorf("path past the budget: err = %v, want ErrPathTooLong", err)
	}
	if got := PathBudget("/" + strings.Repeat("r", MaxPathBytes)); got >= 0 {
		t.Errorf("PathBudget for an over-budget root = %d, want negative", got)
	}
}

func TestTruncateComponent(t *testing.T) {
	short := "invoice.pdf"
	if got := TruncateComponent(short, "attachment-1.bin"); got != short {
		t.Errorf("TruncateComponent(%q) = %q, want unchanged", short, got)
	}
	long := strings.Repeat("a", 400) + ".pdf"
	got := TruncateComponent(long, "attachment-1.bin")
	if len(got) > MaxComponentBytes {
		t.Errorf("TruncateComponent: %d bytes exceeds %d", len(got), MaxComponentBytes)
	}
	if !strings.HasSuffix(got, ".pdf") {
		t.Errorf("TruncateComponent lost the extension: %q", got)
	}
	if again := TruncateComponent(got, "attachment-1.bin"); again != got {
		t.Errorf("TruncateComponent not idempotent: %q -> %q", got, again)
	}
	// Guards apply at the 255-byte ceiling too.
	if got := TruncateComponent("draft.tmp", "f"); got != "draft.tmp.bin" {
		t.Errorf("TruncateComponent(%q) = %q, want %q", "draft.tmp", got, "draft.tmp.bin")
	}
	if got := TruncateComponent(strings.Repeat("é", 200)+".tmp", "f"); SyncExcluded(got) || len(got) > MaxComponentBytes {
		t.Errorf("TruncateComponent = %q (%d bytes, excluded=%v)", got, len(got), SyncExcluded(got))
	}
	// Nothing survives: an over-long run of dots trims away to nothing.
	if got := TruncateComponent(strings.Repeat(".", 300), "attachment-1.bin"); got != "attachment-1.bin" {
		t.Errorf("TruncateComponent(300 dots) = %q, want the fallback", got)
	}
	// A short name that is already legal is returned verbatim: this helper
	// caps length and guards sync, it does not sanitize the charset.
	if got := TruncateComponent("...", "attachment-1.bin"); got != "..." {
		t.Errorf("TruncateComponent(%q) = %q, want it unchanged", "...", got)
	}
}
