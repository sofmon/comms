package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"comms/internal/archive"
	"comms/internal/paths"
	"comms/internal/state"
)

// TestVerifyChatDayHashMismatch pins two things: a genuine mismatch on a
// clean chat day survives the live-daemon TOCTOU re-check and is still
// reported, and the writer's .tmp scratch directory is never flagged as
// orphans.
func TestVerifyChatDayHashMismatch(t *testing.T) {
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	writeTestConfig(t, filepath.Join(cfgDir, "config.toml"), root, "UTC")

	if err := paths.EnsureDir(paths.StateDir()); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(stateDBPath())
	if err != nil {
		t.Fatal(err)
	}
	const space = "spaces/AAA"
	src := state.InstanceID(state.SourceGChat, "work")
	if err := db.UpsertSpace(src, space, "space", "Team", "team", ""); err != nil {
		t.Fatal(err)
	}
	msg := state.ChatMessage{
		Source:     src,
		Name:       space + "/messages/m1",
		SenderID:   "users/1",
		CreateTime: time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC),
		DayBucket:  "2026-08-07",
		RawJSON:    `{"text":"hi"}`,
	}
	if err := db.ApplyChatPage(context.Background(), state.ChatPage{Source: src, Space: space, Messages: []state.ChatMessage{msg}}); err != nil {
		t.Fatal(err)
	}
	dirty, err := db.DirtyDayFiles(src)
	if err != nil || len(dirty) != 1 {
		t.Fatalf("dirty = %d, %v; want 1", len(dirty), err)
	}
	f := dirty[0]

	// The file on disk disagrees with the recorded hash — real corruption,
	// not a daemon race, since nothing rewrites it during this test.
	w := &archive.Writer{Root: root, TZ: time.UTC}
	if _, err := w.WriteChatDay(f.RelPath, []byte("actual content\n")); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDayRendered(src, space, "2026-08-07", f.DirtySeq, "recorded-but-wrong"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Stray litter in .tmp must not show up as an orphan finding.
	tmp := filepath.Join(root, archive.TempDirName)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".x_ab12cd34.md42"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A file with a tagged stem but no DB row is an orphan; an arbitrary .md
	// a user parked in a day directory is not.
	writeFile(t, filepath.Join(root, "2026", "08", "07", "143000_gmail-work_stray_deadbeef.md"), "x")
	writeFile(t, filepath.Join(root, "2026", "08", "07", "my-notes.md"), "x")

	var out strings.Builder
	err = runVerify(&out)
	if err == nil {
		t.Fatalf("verify reported clean despite a hash mismatch:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "chat day file content hash mismatch") {
		t.Errorf("mismatch not reported:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "143000_gmail-work_stray_deadbeef.md") {
		t.Errorf("tagged orphan stem not reported:\n%s", out.String())
	}
	if strings.Contains(out.String(), "my-notes.md") {
		t.Errorf("unrelated .md reported as orphan:\n%s", out.String())
	}
	if strings.Contains(out.String(), "orphan") && strings.Contains(out.String(), archive.TempDirName) {
		t.Errorf(".tmp litter reported as orphan:\n%s", out.String())
	}
}

// oneEmailArchive sets up a config, a state DB and one committed email whose
// .md file on disk deliberately DISAGREES with the recorded content hash. Any
// verify pass that actually reads the file must report a mismatch; a pass that
// skips it must not. It returns the archive root and the file's rel path.
func oneEmailArchive(t *testing.T) (root, rel string) {
	t.Helper()
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	writeTestConfig(t, filepath.Join(cfgDir, "config.toml"), root, "UTC")

	if err := paths.EnsureDir(paths.StateDir()); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(stateDBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rel = "2026/08/07/143000_gmail-work_hello_1a2b3c4d.md"
	writeFile(t, filepath.Join(root, filepath.FromSlash(rel)), "on-disk bytes\n")
	if err := db.CommitMessage(state.Message{
		Source:      state.InstanceID(state.SourceGmail, "work"),
		StableID:    "m1",
		TS:          time.Date(2026, 8, 7, 14, 30, 0, 0, time.UTC),
		DayBucket:   "2026-08-07",
		RelPath:     rel,
		ContentHash: "recorded-but-wrong",
	}); err != nil {
		t.Fatal(err)
	}
	return root, rel
}

// TestVerifySkipsEvictedFiles: a file iCloud has evicted has no bytes on this
// machine, so verify must count it and move on rather than force a download —
// and must NOT report the hash mismatch it would have found had it read the
// file. It must also pin the process materialization policy off first.
func TestVerifySkipsEvictedFiles(t *testing.T) {
	root, rel := oneEmailArchive(t)
	abs := filepath.Join(root, filepath.FromSlash(rel))

	var probed []string
	pinned := 0
	var out bytes.Buffer
	err := runVerifyWith(&out, verifyOpts{
		dataless: func(p string) (bool, error) {
			probed = append(probed, p)
			return p == abs, nil
		},
		noMaterialize: func() error { pinned++; return nil },
	})
	if err != nil {
		t.Fatalf("verify failed on an archive whose only discrepancy was skipped: %v\n%s", err, out.String())
	}
	if pinned != 1 {
		t.Errorf("materialization policy pinned %d times, want exactly 1", pinned)
	}
	if len(probed) != 1 || probed[0] != abs {
		t.Errorf("dataless probe saw %v, want exactly [%s]", probed, abs)
	}
	text := out.String()
	if !strings.Contains(text, "1 file(s) skipped (evicted from local storage by iCloud)") {
		t.Errorf("skip count not reported:\n%s", text)
	}
	if !strings.Contains(text, "--materialize") {
		t.Errorf("skip line does not point at --materialize:\n%s", text)
	}
	if strings.Contains(text, "hash mismatch") {
		t.Errorf("verify hashed a file it reported as evicted:\n%s", text)
	}
}

// TestVerifyMaterializeReadsEverything: --materialize is the opt-in that
// restores the old read-everything behaviour. The dataless probe must not run
// at all (it would only slow things down), the policy must NOT be pinned off
// (that would make the reads fail), and the mismatch must surface.
func TestVerifyMaterializeReadsEverything(t *testing.T) {
	oneEmailArchive(t)

	probes, pins := 0, 0
	var out bytes.Buffer
	err := runVerifyWith(&out, verifyOpts{
		materialize:   true,
		dataless:      func(string) (bool, error) { probes++; return true, nil },
		noMaterialize: func() error { pins++; return nil },
	})
	if err == nil {
		t.Fatalf("verify reported clean despite a hash mismatch:\n%s", out.String())
	}
	if probes != 0 {
		t.Errorf("--materialize still ran the dataless probe %d time(s)", probes)
	}
	if pins != 0 {
		t.Errorf("--materialize still disabled materialization %d time(s)", pins)
	}
	if !strings.Contains(out.String(), "content hash mismatch") {
		t.Errorf("mismatch not reported under --materialize:\n%s", out.String())
	}
	if strings.Contains(out.String(), "skipped (evicted") {
		t.Errorf("--materialize reported skips:\n%s", out.String())
	}
}

// TestVerifyTreatsEvictedReadErrorsAsSkips: the dataless probe can lose a race
// (the file is evicted between the stat and the read), and a materialization
// the provider never satisfies times out. Both surface as read errors that
// mean "not here", not "corrupt", so they must be counted as skips rather than
// reported as unreadable.
func TestVerifyTreatsEvictedReadErrorsAsSkips(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EDEADLK, syscall.ETIMEDOUT} {
		t.Run(errno.Error(), func(t *testing.T) {
			if !evictedRead(&os.PathError{Op: "read", Path: "x", Err: errno}) {
				t.Fatalf("evictedRead does not recognize %v", errno)
			}
		})
	}
	// A genuine I/O error still counts as a discrepancy.
	if evictedRead(&os.PathError{Op: "read", Path: "x", Err: syscall.EIO}) {
		t.Error("evictedRead treats EIO as an eviction")
	}
}

// TestVerifyStillHashesLocalFiles: the eviction machinery must not weaken the
// default audit. With nothing evicted, the corrupted file is still caught.
func TestVerifyStillHashesLocalFiles(t *testing.T) {
	oneEmailArchive(t)

	// Sanity: the on-disk bytes really do hash to something other than the
	// recorded value, so the mismatch below is the file and not the plumbing.
	sum := sha256.Sum256([]byte("on-disk bytes\n"))
	if hex.EncodeToString(sum[:]) == "recorded-but-wrong" {
		t.Fatal("test fixture is degenerate")
	}

	var out bytes.Buffer
	err := runVerifyWith(&out, verifyOpts{
		dataless:      func(string) (bool, error) { return false, nil },
		noMaterialize: func() error { return nil },
	})
	if err == nil {
		t.Fatalf("verify reported clean despite a hash mismatch:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "content hash mismatch") {
		t.Errorf("mismatch not reported:\n%s", out.String())
	}
	if strings.Contains(out.String(), "skipped (evicted") {
		t.Errorf("nothing was evicted, yet skips were reported:\n%s", out.String())
	}
}

// TestVerifyReportsAnUnpinnableMaterializationPolicy: failing to pin the
// policy is a weakened guarantee, not a failure — verify says so and carries
// on with the per-file probe.
func TestVerifyReportsAnUnpinnableMaterializationPolicy(t *testing.T) {
	oneEmailArchive(t)

	var out bytes.Buffer
	err := runVerifyWith(&out, verifyOpts{
		dataless:      func(string) (bool, error) { return true, nil },
		noMaterialize: func() error { return syscall.EPERM },
	})
	if err != nil {
		t.Fatalf("verify failed because the policy could not be pinned: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "could not disable cloud materialization") {
		t.Errorf("the weakened guarantee is not reported:\n%s", out.String())
	}
}

// TestVerifyMissingFileStillReportedWhenEvictionAware: the dataless probe
// stats the file, so it is the probe — not hashFile — that first meets a
// missing file. Its error must keep its fs.ErrNotExist identity or "missing"
// silently degrades into "unreadable".
func TestVerifyMissingFileStillReportedWhenEvictionAware(t *testing.T) {
	root, rel := oneEmailArchive(t)
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	// The real probe: it stats, so it produces the real fs.ErrNotExist.
	if err := runVerifyWith(&out, verifyOpts{noMaterialize: func() error { return nil }}); err == nil {
		t.Fatalf("verify reported clean despite a missing file:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "missing file: "+rel) {
		t.Errorf("missing file not reported as missing:\n%s", out.String())
	}
}

// TestArchiveMDNameMatchesTaggedStems pins the orphan scanner's basename
// pattern to the stems the writer actually produces: every one carries the
// owning instance's file tag ("<kind>-<label>") as its source component.
func TestArchiveMDNameMatchesTaggedStems(t *testing.T) {
	for _, name := range []string{
		"143000_gmail-work_quarterly-review_1a2b3c4d.md",
		"090512_gmail-personal_no-subject_00000000.md",
		"235959_fastmail-fm_hello-there_deadbeef.md",
		"gchat-work_space_team-platform_1a2b3c4d.md",
		"gchat-personal_dm_jane-doe_abcdef01.md",
		"gchat-a_group_x_00112233.md",
	} {
		if !archiveMDName.MatchString(name) {
			t.Errorf("archiveMDName does not match the archive stem %q", name)
		}
	}
	for _, name := range []string{
		"143000_gmail_quarterly-review_1a2b3c4d.md", // untagged: never written
		"143000_imap-work_subject_1a2b3c4d.md",      // unknown source kind
		"gchat-WORK_space_team_1a2b3c4d.md",         // labels are lowercase
		"143000_gmail-work_subject_1A2B3C4D.md",     // hash8 is lowercase hex
		"gchat-work_space_team_1a2b3c4.md",          // 7 hex digits
		"my-notes.md",
		"README.md",
	} {
		if archiveMDName.MatchString(name) {
			t.Errorf("archiveMDName matches %q, which this program never writes", name)
		}
	}
}
