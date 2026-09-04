//go:build darwin

package archive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// getXattr reads one extended attribute, reporting whether it is present at
// all. Everything here runs against the real macOS syscalls — the point of
// these tests is that the attribute is genuinely on the file, not that a seam
// was called.
func getXattr(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	size, err := unix.Getxattr(path, name, nil)
	if err != nil {
		if errors.Is(err, unix.ENOATTR) {
			return "", false
		}
		t.Fatalf("getxattr %s on %s: %v", name, path, err)
	}
	buf := make([]byte, size)
	if _, err := unix.Getxattr(path, name, buf); err != nil {
		t.Fatalf("getxattr %s on %s: %v", name, path, err)
	}
	return string(buf), true
}

// TestQuarantineXattrOnWrittenAttachment: the attribute must be on the
// attachment as it sits in the vault, having survived renameio's rename —
// which is why it is set on the pending temp file rather than afterwards.
// The .md note must not carry it.
func TestQuarantineXattrOnWrittenAttachment(t *testing.T) {
	doc, meta, _ := fullEmailFixture(t)
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	rel, _, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatalf("WriteEmail: %v", err)
	}

	for _, f := range doc.Files {
		p := filepath.Join(w.Root, "2026/08/07", filepath.FromSlash(f.Rel))
		got, ok := getXattr(t, p, xattrQuarantine)
		if !ok {
			t.Fatalf("%s carries no %s", f.Rel, xattrQuarantine)
		}
		if !quarantineRE.MatchString(got) {
			t.Errorf("%s quarantine value %q does not match the verified format", f.Rel, got)
		}

		// Provenance: Finder's "Where from", as a binary plist array.
		wf, ok := getXattr(t, p, xattrWhereFroms)
		if !ok {
			t.Fatalf("%s carries no %s", f.Rel, xattrWhereFroms)
		}
		want := string(bplistStrings([]string{"gmail:work", "<m1@example.com>"}))
		if wf != want {
			t.Errorf("%s where-froms = %x, want %x", f.Rel, wf, want)
		}
	}

	// The note is comms's own text; tagging it would claim it was downloaded.
	if _, ok := getXattr(t, filepath.Join(w.Root, filepath.FromSlash(rel)), xattrQuarantine); ok {
		t.Error("the .md note was quarantine-tagged")
	}
	// Files stay 0600 regardless.
	for _, f := range doc.Files {
		p := filepath.Join(w.Root, "2026/08/07", filepath.FromSlash(f.Rel))
		if mode := fileMode(t, p); mode != 0o600 {
			t.Errorf("%s mode = %o after tagging, want 0600", f.Rel, mode)
		}
	}
}

// TestQuarantineXattrAbsentWhenDisabled: attachments.quarantine = false must
// reach the filesystem, not merely the config struct.
func TestQuarantineXattrAbsentWhenDisabled(t *testing.T) {
	doc, meta, _ := fullEmailFixture(t)
	w := &Writer{Root: t.TempDir(), TZ: tzAms, Quarantine: QuarantineOff}
	if _, _, err := w.WriteEmail(doc, meta); err != nil {
		t.Fatalf("WriteEmail: %v", err)
	}
	for _, f := range doc.Files {
		p := filepath.Join(w.Root, "2026/08/07", filepath.FromSlash(f.Rel))
		if v, ok := getXattr(t, p, xattrQuarantine); ok {
			t.Errorf("%s is quarantined (%q) with tagging disabled", f.Rel, v)
		}
		if _, ok := getXattr(t, p, xattrWhereFroms); ok {
			t.Errorf("%s carries where-froms with tagging disabled", f.Rel)
		}
	}
}

// TestQuarantineSurvivesRename is the invariant the whole ordering rests on:
// set the attribute on the temp file, rename, and it is still there. If this
// ever stops holding, tagging must move after the rename and accept the gap.
func TestQuarantineSurvivesRename(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, ".staged")
	if err := os.WriteFile(tmp, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tagOrigin(tmp, Origin{Source: "gmail:work", Ref: "<x@example.com>"}); err != nil {
		t.Fatalf("tagOrigin: %v", err)
	}
	before, _ := getXattr(t, tmp, xattrQuarantine)
	final := filepath.Join(dir, "final.pdf")
	if err := os.Rename(tmp, final); err != nil {
		t.Fatal(err)
	}
	after, ok := getXattr(t, final, xattrQuarantine)
	if !ok || after != before {
		t.Errorf("quarantine did not survive the rename: %q -> %q (present=%v)", before, after, ok)
	}
}

// TestWhereFromsOmittedWithoutOrigin: an attachment with nothing to say about
// its provenance still gets quarantined, but no empty plist is written.
func TestWhereFromsOmittedWithoutOrigin(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.pdf")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tagOrigin(p, Origin{}); err != nil {
		t.Fatalf("tagOrigin: %v", err)
	}
	if _, ok := getXattr(t, p, xattrQuarantine); !ok {
		t.Error("no quarantine attribute")
	}
	if _, ok := getXattr(t, p, xattrWhereFroms); ok {
		t.Error("an empty Origin still wrote where-froms")
	}
}

// TestQuarantineIsOnTheTempFileBeforeTheRename closes the one gap the other
// tests leave open.
//
// TestQuarantineXattrOnWrittenAttachment proves the visible file carries the
// tag, and TestQuarantineSurvivesRename proves a rename preserves it — but
// together they still admit an implementation that tags AFTER
// CloseAtomicallyReplace, which is exactly the untagged window that makes up
// CVE-2022-3155. This observes the attribute on the renameio pending file
// during a real WriteEmail, at the moment tagging has run and the rename has
// not.
//
// Writer.Dataless is the observation point: writeAtomic calls it on the
// staged temp file immediately after tagPending and immediately before
// CloseAtomicallyReplace. Nothing about the tagging is stubbed — this reads
// the attribute the production setxattr just wrote.
func TestQuarantineIsOnTheTempFileBeforeTheRename(t *testing.T) {
	doc, meta, _ := fullEmailFixture(t)
	root := t.TempDir()
	tmpPrefix := filepath.Join(root, TempDirName) + string(filepath.Separator)

	staged := make(map[string]string) // temp path -> quarantine value ("" = absent)
	w := &Writer{Root: root, TZ: tzAms}
	w.Dataless = func(p string) (bool, error) {
		if !strings.HasPrefix(p, tmpPrefix) {
			return false, nil
		}
		if _, seen := staged[p]; seen {
			return false, nil
		}
		size, err := unix.Getxattr(p, xattrQuarantine, nil)
		if err != nil {
			staged[p] = "" // absent, or unreadable — either way, not tagged
			return false, nil
		}
		buf := make([]byte, size)
		if _, err := unix.Getxattr(p, xattrQuarantine, buf); err != nil {
			staged[p] = ""
			return false, nil
		}
		staged[p] = string(buf)
		return false, nil
	}

	rel, _, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatalf("WriteEmail: %v", err)
	}
	if len(staged) == 0 {
		t.Fatal("the dataless hook never saw a staged temp file; the ordering could not be observed")
	}

	// Every attachment's temp file must already have carried a well-formed
	// tag. The note's must not have one at all.
	noteTemps, taggedTemps := 0, 0
	for p, v := range staged {
		switch {
		case strings.Contains(p, filepath.Base(rel)):
			noteTemps++
			if v != "" {
				t.Errorf("the staged .md note %s was quarantine-tagged (%q)", filepath.Base(p), v)
			}
		case v == "":
			t.Errorf("staged attachment temp file %s carried NO quarantine tag before the rename — "+
				"tagging must happen on the pending file, never after CloseAtomicallyReplace", filepath.Base(p))
		default:
			taggedTemps++
			if !quarantineRE.MatchString(v) {
				t.Errorf("staged temp file %s quarantine value %q does not match the verified format",
					filepath.Base(p), v)
			}
		}
	}
	if taggedTemps != len(doc.Files) {
		t.Errorf("observed %d tagged staged attachment(s), want %d", taggedTemps, len(doc.Files))
	}
	if noteTemps == 0 {
		t.Error("the .md note's staged temp file was never observed")
	}
}
