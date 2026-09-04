package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorReportsEachAccountSeparately: with several accounts configured,
// doctor must open a section per account and make every complaint and hint
// name the account it belongs to. No credentials exist here, so each
// account's checks stop before any network call.
func TestDoctorReportsEachAccountSeparately(t *testing.T) {
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	personalClient := filepath.Join(cfgDir, "google-client-personal.json")
	writeFile(t, filepath.Join(cfgDir, "config.toml"), twoAccountConfig(root, personalClient))

	var out strings.Builder
	if err := runDoctor(&out); err == nil {
		t.Fatalf("doctor reported no problems despite missing credentials:\n%s", out.String())
	}
	text := out.String()
	for _, want := range []string{
		`[[google]] "work" — you@example.com (gmail, gchat)`,
		`[[google]] "personal" — you@example.net (gmail, gchat)`,
		`[[fastmail]] "fm" — you@fastmail.com`,
		filepath.Join(cfgDir, "google-client.json"), // work uses the shared default
		personalClient, // personal has its own
		filepath.Join(cfgDir, "fastmail-token-fm"),
		"comms auth google personal",
		"comms auth fastmail fm",
		// The multi-org caveat the second Workspace account depends on.
		"ITS OWN Workspace",
		"client_file",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("doctor output does not contain %q:\n%s", want, text)
		}
	}
}

// TestDoctorFlagsASharedOAuthClient: two accounts pointing at one client
// JSON only works inside a single Workspace org, so doctor says so.
func TestDoctorFlagsASharedOAuthClient(t *testing.T) {
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	writeFile(t, filepath.Join(cfgDir, "config.toml"), `archive_root = "`+root+`"
timezone = "UTC"

[[google]]
label   = "work"
account = "a@example.com"
gmail   = true

[[google]]
label   = "personal"
account = "b@example.net"
gmail   = true
`)
	// Both accounts fall back to the shared default client, which must exist
	// for the check to get as far as the sharing note.
	writeGoogleCreds(t, filepath.Join(cfgDir, "google-client.json"),
		filepath.Join(cfgDir, "google-token-unused.json"), nil)

	var out strings.Builder
	_ = runDoctor(&out) // problems are expected: neither account has a token
	text := out.String()
	if !strings.Contains(text, `shared with account "personal"`) {
		t.Errorf("doctor does not flag the shared OAuth client:\n%s", text)
	}
	if !strings.Contains(text, `shared with account "work"`) {
		t.Errorf("the sharing note is one-sided:\n%s", text)
	}
}

// fakeCloudEnv is a cloudEnv whose home is a temp directory, so a test can
// build a path that LOOKS like it is inside ~/Library/Mobile Documents without
// touching the real one. Everything is plentiful and switched off by default;
// each test overrides only what it is about.
func fakeCloudEnv(home string) cloudEnv {
	return cloudEnv{
		home:      home,
		tracked:   func(string) (bool, error) { return false, nil },
		optimize:  func() (bool, bool) { return false, true },
		freeBytes: func(string) (uint64, error) { return 500 << 30, nil },
	}
}

// TestArchiveStorageCheckOnlyFiresInsideASyncTree: the whole section is
// conditional. A plain ~/Archive must produce no output at all — a warning
// about iCloud on a machine that does not use it is noise that trains the user
// to ignore doctor.
func TestArchiveStorageCheckOnlyFiresInsideASyncTree(t *testing.T) {
	home := t.TempDir()
	env := fakeCloudEnv(home)
	mobile := filepath.Join(home, "Library", "Mobile Documents")

	for _, tc := range []struct {
		name string
		root string
		env  cloudEnv
		want bool
	}{
		{"plain home directory", filepath.Join(home, "Archive"), env, false},
		{"a sibling of the container store", filepath.Join(home, "Library", "Mobile Documents Backup"), env, false},
		{"an obsidian vault in iCloud", filepath.Join(mobile, "iCloud~md~obsidian", "Documents", "MyVault", "Communication"), env, true},
		{"the container store itself", mobile, env, true},
		{"a third-party provider", filepath.Join(home, "Library", "CloudStorage", "Dropbox", "Archive"), env, true},
		{"UF_TRACKED elsewhere", filepath.Join(home, "Elsewhere"), func() cloudEnv {
			e := fakeCloudEnv(home)
			e.tracked = func(string) (bool, error) { return true, nil }
			return e
		}(), true},
		{"an unstattable path is not assumed to be cloud", filepath.Join(home, "Elsewhere"), func() cloudEnv {
			e := fakeCloudEnv(home)
			e.tracked = func(string) (bool, error) { return false, os.ErrNotExist }
			return e
		}(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			d := &doctorReport{out: &out}
			d.checkArchiveStorage(tc.root, tc.env)
			if got := out.Len() > 0; got != tc.want {
				t.Fatalf("section printed = %v, want %v; output:\n%s", got, tc.want, out.String())
			}
			if d.problems != 0 {
				t.Errorf("the storage section counted %d problem(s) with the state DB outside the tree:\n%s", d.problems, out.String())
			}
		})
	}
}

// TestArchiveStorageWarnsAboutEvictionAndCost: inside a sync tree with
// "Optimize Mac Storage" on and a nearly full disk, doctor must say every
// thing the user needs to decide — and still exit 0, because none of it is
// something the program can call wrong.
func TestArchiveStorageWarnsAboutEvictionAndCost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state")) // outside the tree
	env := fakeCloudEnv(home)
	env.optimize = func() (bool, bool) { return true, true }
	env.freeBytes = func(string) (uint64, error) { return 27 << 30, nil }
	root := filepath.Join(home, "Library", "Mobile Documents", "iCloud~md~obsidian", "Documents", "MyVault", "Communication")

	var out strings.Builder
	d := &doctorReport{out: &out}
	d.checkArchiveStorage(root, env)
	text := out.String()

	for _, want := range []string{
		"iCloud Drive (~/Library/Mobile Documents)",
		`"Optimize Mac Storage" is ON`,
		"Keep Downloaded",
		"--materialize",
		"27.0 GiB free",       // above the eviction mark, reported as ok
		"iCloud storage plan", // the double charge
		"0644 files, 0755 directories",
		"fileproviderd",
		"is outside the sync tree", // the state DB
	} {
		if !strings.Contains(text, want) {
			t.Errorf("archive storage section does not mention %q:\n%s", want, text)
		}
	}
	if d.problems != 0 {
		t.Errorf("warnings were counted as %d problem(s):\n%s", d.problems, text)
	}
}

// TestArchiveStorageWarnsBelowTheEvictionThreshold pins the disk-space
// warning to the ~20 GiB mark where macOS starts evicting.
func TestArchiveStorageWarnsBelowTheEvictionThreshold(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	env := fakeCloudEnv(home)
	env.freeBytes = func(string) (uint64, error) { return 8 << 30, nil }

	var out strings.Builder
	d := &doctorReport{out: &out}
	d.checkArchiveStorage(filepath.Join(home, "Library", "Mobile Documents", "V", "Comms"), env)
	if !strings.Contains(out.String(), "only 8.0 GiB free") {
		t.Errorf("low disk space not warned about:\n%s", out.String())
	}
	if d.problems != 0 {
		t.Errorf("a low-disk warning was counted as a problem:\n%s", out.String())
	}
}

// TestArchiveStorageFallsBackToHomeForFreeSpace: archive_root usually does not
// exist yet at doctor time, and statfs on a missing path fails. The volume is
// the same either way, so the check must not silently disappear.
func TestArchiveStorageFallsBackToHomeForFreeSpace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	root := filepath.Join(home, "Library", "Mobile Documents", "V", "Comms")
	env := fakeCloudEnv(home)
	env.freeBytes = func(p string) (uint64, error) {
		if p == root {
			return 0, os.ErrNotExist
		}
		return 4 << 30, nil
	}

	var out strings.Builder
	d := &doctorReport{out: &out}
	d.checkArchiveStorage(root, env)
	if !strings.Contains(out.String(), "only 4.0 GiB free") {
		t.Errorf("free space not reported from the fallback path:\n%s", out.String())
	}
}

// TestArchiveStorageRejectsAStateDBInsideTheSyncTree is the one hard failure
// in this section: SQLite's WAL and shared-memory sidecars are synced out of
// step with the database and corrupt it. This goes through the whole doctor
// run so the wiring — not just the check — is covered.
func TestArchiveStorageRejectsAStateDBInsideTheSyncTree(t *testing.T) {
	home := t.TempDir()
	cfgDir := t.TempDir()
	mobile := filepath.Join(home, "Library", "Mobile Documents")
	root := filepath.Join(mobile, "iCloud~md~obsidian", "Documents", "MyVault", "Communication")
	// The state directory is inside the very same synced container.
	setTestEnv(t, cfgDir, filepath.Join(root, "state"))
	writeTestConfig(t, filepath.Join(cfgDir, "config.toml"), root, "UTC")

	var out strings.Builder
	err := runDoctorWith(&out, fakeCloudEnv(home))
	if err == nil {
		t.Fatalf("doctor accepted a state database inside the sync tree:\n%s", out.String())
	}
	text := out.String()
	if !strings.Contains(text, "PROBLEM") || !strings.Contains(text, "is inside the sync tree") {
		t.Errorf("the state-DB placement is not reported as a PROBLEM:\n%s", text)
	}
	if !strings.Contains(text, "XDG_STATE_HOME") {
		t.Errorf("the fix is not spelled out:\n%s", text)
	}
	// Moving it out clears exactly that complaint.
	before := strings.Count(text, "PROBLEM")
	setTestEnv(t, cfgDir, filepath.Join(home, ".local", "state"))
	var out2 strings.Builder
	_ = runDoctorWith(&out2, fakeCloudEnv(home)) // still fails: no credentials
	if strings.Contains(out2.String(), "is inside the sync tree") {
		t.Errorf("the complaint survived moving the state dir out:\n%s", out2.String())
	}
	if after := strings.Count(out2.String(), "PROBLEM"); after != before-1 {
		t.Errorf("PROBLEM count %d -> %d; moving the state DB should have cleared exactly one", before, after)
	}
}

// TestArchiveStorageWarnsAboutAnUnsyncableRoot: an archive_root whose path
// contains a component on iCloud's exclusion list never uploads anything, and
// the provider reports no error at all — the worst failure this program has.
func TestArchiveStorageWarnsAboutAnUnsyncableRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	root := filepath.Join(home, "Library", "Mobile Documents", "iCloud~md~obsidian", "Documents", "tmp", "Comms")

	var out strings.Builder
	d := &doctorReport{out: &out}
	d.checkArchiveStorage(root, fakeCloudEnv(home))
	if !strings.Contains(out.String(), `"tmp" is on iCloud's filename exclusion list`) {
		t.Errorf("an unsyncable archive_root is not flagged:\n%s", out.String())
	}
}

// TestArchiveStorageWarnsWhenThePathBudgetIsTight: a very long container path
// leaves no room for YYYY/MM/DD/<stem>.d/<attachment>, and the writer refuses
// those rather than creating a path nothing can open.
func TestArchiveStorageWarnsWhenThePathBudgetIsTight(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	deep := filepath.Join(home, "Library", "Mobile Documents")
	for len(deep) < 900 {
		deep = filepath.Join(deep, strings.Repeat("d", 60))
	}

	var out strings.Builder
	d := &doctorReport{out: &out}
	d.checkArchiveStorage(deep, fakeCloudEnv(home))
	if !strings.Contains(out.String(), "leaving") || !strings.Contains(out.String(), "for the rest of the path") {
		t.Errorf("a busted path budget is not warned about:\n%s", out.String())
	}
	if d.problems != 0 {
		t.Errorf("the path-budget warning was counted as a problem:\n%s", out.String())
	}
}

// TestUnderDirIsComponentWise: the sync-tree test is a path-prefix test, and a
// naive strings.HasPrefix would put "~/Library/Mobile Documents Backup" inside
// "~/Library/Mobile Documents".
func TestUnderDirIsComponentWise(t *testing.T) {
	for _, tc := range []struct {
		dir, path string
		want      bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/b/c/d", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a/b-backup/c", false},
		{"/a/b", "/a", false},
		{"/a/b", "/a/b/../b/c", true},
		{"/a/b/", "/a/b/c", true},
	} {
		if got := underDir(tc.dir, tc.path); got != tc.want {
			t.Errorf("underDir(%q, %q) = %v, want %v", tc.dir, tc.path, got, tc.want)
		}
	}
}

// TestCloudProbesAreHarmlessOffTheirPlatform: the portable fallbacks must be
// no-ops that never fail a run. datalessFile answering "no" everywhere keeps
// verify's behaviour identical to before on Linux, and setNoMaterialize must
// succeed vacuously so verify does not print a warning on every run.
func TestCloudProbesAreHarmlessOffTheirPlatform(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "probe")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if dl, err := datalessFile(f.Name()); err != nil || dl {
		t.Errorf("datalessFile(a freshly written local file) = %v, %v; want false, nil", dl, err)
	}
	if _, err := datalessFile(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("datalessFile on a missing path lost its fs.ErrNotExist identity: %v", err)
	}
	if err := setNoMaterialize(); err != nil {
		t.Errorf("setNoMaterialize: %v", err)
	}
	if tracked, err := trackedPath(f.Name()); err != nil || tracked {
		t.Errorf("trackedPath(a plain temp file) = %v, %v; want false, nil", tracked, err)
	}
}
