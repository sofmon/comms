package archive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"comms/internal/naming"
	"comms/internal/state"
)

// TestRootForFollowsTheDisposition pins the one place the two trees are told
// apart: the zero value and the archive disposition mean Root, spam means
// SpamRoot, and a spam disposition with no spam tree configured is an error
// rather than a fallback to Root — a fallback would quietly put a moved note
// back where triage took it from.
func TestRootForFollowsTheDisposition(t *testing.T) {
	w := &Writer{Root: "/a/archive", SpamRoot: "/a/spam", TZ: tzAms}
	for _, tc := range []struct {
		disp state.Disposition
		want string
	}{
		{"", "/a/archive"},
		{state.DispositionArchive, "/a/archive"},
		{state.DispositionSpam, "/a/spam"},
	} {
		got, err := w.RootFor(tc.disp)
		if err != nil || got != tc.want {
			t.Errorf("RootFor(%q) = %q, %v; want %q", tc.disp, got, err, tc.want)
		}
	}
	if _, err := w.RootFor("trash"); err == nil {
		t.Error("RootFor accepted an unknown disposition")
	}
	noSpam := &Writer{Root: "/a/archive", TZ: tzAms}
	if _, err := noSpam.RootFor(state.DispositionSpam); err == nil {
		t.Error("RootFor(spam) with no SpamRoot did not error")
	}

	abs, err := w.NotePath("2026/08/07/n.md", state.DispositionSpam)
	if err != nil || abs != filepath.Join("/a/spam", "2026", "08", "07", "n.md") {
		t.Errorf("NotePath = %q, %v", abs, err)
	}
	if _, err := w.NotePath("../escape.md", state.DispositionArchive); err == nil {
		t.Error("NotePath accepted a rel path that escapes the root")
	}
}

// TestWriteEmailRefusesASpamDisposition: connectors never classify, so a NEW
// note can only ever be written into the archive tree.
func TestWriteEmailRefusesASpamDisposition(t *testing.T) {
	doc, meta, _ := fullEmailFixture(t)
	w := &Writer{Root: t.TempDir(), SpamRoot: t.TempDir(), TZ: tzAms}
	meta.Disposition = state.DispositionSpam
	if _, _, err := w.WriteEmail(doc, meta); err == nil {
		t.Fatal("WriteEmail accepted a spam disposition for a new note")
	}
	if entries, _ := os.ReadDir(w.SpamRoot); len(entries) != 0 {
		t.Errorf("the refused write left files in the spam tree: %v", entries)
	}
}

// TestRewriteEmailFollowsTheDisposition: after triage has moved a note (and
// its attachment folder) to the spam tree, a re-render with the spam
// disposition lands on that copy — same rel path, other root — and a
// re-render that wrongly claims the archive disposition refuses with
// ErrNoteMissing instead of forking a second copy.
func TestRewriteEmailFollowsTheDisposition(t *testing.T) {
	doc, meta, _ := fullEmailFixture(t)
	w := &Writer{Root: t.TempDir(), SpamRoot: t.TempDir(), TZ: tzAms}
	rel, hash, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the mover: rename the note and its .d/ across roots.
	stem := strings.TrimSuffix(filepath.Base(rel), ".md")
	day := filepath.Dir(filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Join(w.SpamRoot, day), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{stem + ".md", naming.AttachDir(stem)} {
		if err := os.Rename(filepath.Join(w.Root, day, name), filepath.Join(w.SpamRoot, day, name)); err != nil {
			t.Fatal(err)
		}
	}

	meta.Disposition = state.DispositionArchive
	if _, _, err := w.RewriteEmail(doc, meta); !errors.Is(err, ErrNoteMissing) {
		t.Fatalf("RewriteEmail with the archive disposition on a moved note: err = %v, want ErrNoteMissing", err)
	}
	if _, err := os.Stat(filepath.Join(w.Root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Fatalf("the refused rewrite created a note in the archive tree (err=%v)", err)
	}

	meta.Disposition = state.DispositionSpam
	rel2, hash2, err := w.RewriteEmail(doc, meta)
	if err != nil {
		t.Fatalf("RewriteEmail with the spam disposition: %v", err)
	}
	if rel2 != rel || hash2 != hash {
		t.Errorf("rewrite = (%q, %q), want the original (%q, %q)", rel2, hash2, rel, hash)
	}
	if _, err := os.Stat(filepath.Join(w.SpamRoot, filepath.FromSlash(rel))); err != nil {
		t.Errorf("the note is not under the spam root after the rewrite: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.Root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Errorf("the rewrite forked a copy into the archive tree (err=%v)", err)
	}
	// Attachments went beside the note, in the spam tree, staged in ITS .tmp.
	for _, f := range doc.Files {
		if _, err := os.Stat(filepath.Join(w.SpamRoot, day, filepath.FromSlash(f.Rel))); err != nil {
			t.Errorf("attachment %s not under the spam root: %v", f.Rel, err)
		}
	}
	if litter := dotFiles(t, w.SpamRoot); len(litter) != 0 {
		t.Errorf("temp litter in the spam tree: %v", litter)
	}
	if _, err := os.Stat(filepath.Join(w.Root, TempDirName)); err == nil {
		if litter := dotFiles(t, w.Root); len(litter) != 0 {
			t.Errorf("a spam-tree write staged in the archive tree: %v", litter)
		}
	}
}

// TestSweepTempCoversBothTrees: crash litter can sit in either root's .tmp.
func TestSweepTempCoversBothTrees(t *testing.T) {
	w := &Writer{Root: t.TempDir(), SpamRoot: t.TempDir(), TZ: tzAms}
	for _, root := range []string{w.Root, w.SpamRoot} {
		if err := os.MkdirAll(filepath.Join(root, TempDirName), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, TempDirName, "litter"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := w.SweepTemp()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want one entry per tree", removed)
	}
	if !strings.HasPrefix(removed[1], w.SpamRoot+"/") {
		t.Errorf("the spam tree's entry is not distinguishable in the log: %v", removed)
	}
	for _, root := range []string{w.Root, w.SpamRoot} {
		if entries, _ := os.ReadDir(filepath.Join(root, TempDirName)); len(entries) != 0 {
			t.Errorf("%s/.tmp still holds %d entries", root, len(entries))
		}
	}
	// No spam tree configured: only the archive's .tmp is touched, no error.
	solo := &Writer{Root: t.TempDir(), TZ: tzAms}
	if removed, err := solo.SweepTemp(); err != nil || len(removed) != 0 {
		t.Errorf("SweepTemp without a spam root = %v, %v", removed, err)
	}
}
