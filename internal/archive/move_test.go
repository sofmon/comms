package archive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"save/internal/naming"
	"save/internal/state"
)

// movedNote writes one email note with two attachments into a writer that
// has both trees, and returns its rel path and attach-dir rel path.
func movedNote(t *testing.T) (*Writer, string, string) {
	t.Helper()
	doc, meta, stem := fullEmailFixture(t)
	w := &Writer{Root: t.TempDir(), SpamRoot: t.TempDir(), TZ: tzAms}
	rel, _, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatal(err)
	}
	return w, rel, "2026/08/07/" + naming.AttachDir(stem)
}

func mustExistAt(t *testing.T, root, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Errorf("%s not under %s: %v", rel, root, err)
	}
}

func mustBeAbsent(t *testing.T, root, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Errorf("%s still under %s (err=%v)", rel, root, err)
	}
}

// TestMoveNoteThereAndBack: the .md and its .d/ cross together, the day
// directory is created 0700 on the far side, bytes are untouched, and the
// reverse move restores everything.
func TestMoveNoteThereAndBack(t *testing.T) {
	w, rel, dir := movedNote(t)
	before, _ := os.ReadFile(filepath.Join(w.Root, filepath.FromSlash(rel)))

	res, err := w.MoveNote(rel, state.DispositionSpam)
	if err != nil {
		t.Fatalf("MoveNote to spam: %v", err)
	}
	if res.From != state.DispositionArchive || res.To != state.DispositionSpam || res.AlreadyThere || !res.MovedAttachDir {
		t.Errorf("result = %+v", res)
	}
	mustExistAt(t, w.SpamRoot, rel)
	mustExistAt(t, w.SpamRoot, dir+"/invoice.pdf")
	mustBeAbsent(t, w.Root, rel)
	mustBeAbsent(t, w.Root, dir)
	after, _ := os.ReadFile(filepath.Join(w.SpamRoot, filepath.FromSlash(rel)))
	if string(before) != string(after) {
		t.Error("the note's bytes changed in the move")
	}
	if info, err := os.Stat(filepath.Join(w.SpamRoot, "2026", "08", "07")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("spam day dir: %v, mode %v", err, info.Mode())
	}

	back, err := w.MoveNote(rel, state.DispositionArchive)
	if err != nil {
		t.Fatalf("MoveNote back: %v", err)
	}
	if back.From != state.DispositionSpam || back.To != state.DispositionArchive || !back.MovedAttachDir {
		t.Errorf("result = %+v", back)
	}
	mustExistAt(t, w.Root, rel)
	mustExistAt(t, w.Root, dir+"/photo 1.png")
	mustBeAbsent(t, w.SpamRoot, rel)
	mustBeAbsent(t, w.SpamRoot, dir)
}

// TestMoveNoteWithoutAttachments: a note with no .d/ moves alone.
func TestMoveNoteWithoutAttachments(t *testing.T) {
	w := &Writer{Root: t.TempDir(), SpamRoot: t.TempDir(), TZ: tzAms}
	rel := "2026/08/07/100000_gmail-work_x_ab12cd34.md"
	if err := os.MkdirAll(filepath.Join(w.Root, "2026", "08", "07"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Root, filepath.FromSlash(rel)), []byte("n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := w.MoveNote(rel, state.DispositionSpam)
	if err != nil || res.MovedAttachDir || res.AlreadyThere {
		t.Fatalf("MoveNote = %+v, %v", res, err)
	}
	mustExistAt(t, w.SpamRoot, rel)
}

// TestMoveNoteRecoversAnInterruptedMove replays the two crash points. After
// the .md crossed but the .d/ did not, a second call finishes the job and
// says the note was already there; after both crossed, it is a no-op that
// still says so — the caller then records the disposition it never got to.
func TestMoveNoteRecoversAnInterruptedMove(t *testing.T) {
	w, rel, dir := movedNote(t)
	day := filepath.Join("2026", "08", "07")
	if err := os.MkdirAll(filepath.Join(w.SpamRoot, day), 0o700); err != nil {
		t.Fatal(err)
	}
	// Crash point 1: only the .md moved.
	if err := os.Rename(filepath.Join(w.Root, filepath.FromSlash(rel)), filepath.Join(w.SpamRoot, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
	res, err := w.MoveNote(rel, state.DispositionSpam)
	if err != nil {
		t.Fatalf("recovery after the .md rename: %v", err)
	}
	if !res.AlreadyThere || !res.MovedAttachDir {
		t.Errorf("result = %+v, want AlreadyThere with the .d/ moved", res)
	}
	mustExistAt(t, w.SpamRoot, dir+"/invoice.pdf")
	mustBeAbsent(t, w.Root, dir)

	// Crash point 2: both moved, row never updated. Idempotent.
	res, err = w.MoveNote(rel, state.DispositionSpam)
	if err != nil || !res.AlreadyThere || res.MovedAttachDir {
		t.Errorf("second recovery = %+v, %v", res, err)
	}
}

// TestMoveNoteRefusesAmbiguity: a note under both roots, an attachment
// directory already at the destination, and a note under neither are all
// refused without touching anything.
func TestMoveNoteRefusesAmbiguity(t *testing.T) {
	w, rel, dir := movedNote(t)
	day := filepath.Join("2026", "08", "07")
	if err := os.MkdirAll(filepath.Join(w.SpamRoot, day), 0o700); err != nil {
		t.Fatal(err)
	}
	// A stray copy of the .md on the far side.
	if err := os.WriteFile(filepath.Join(w.SpamRoot, filepath.FromSlash(rel)), []byte("stray"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.MoveNote(rel, state.DispositionSpam); !errors.Is(err, ErrNoteInBothTrees) {
		t.Errorf("both trees: err = %v", err)
	}
	mustExistAt(t, w.Root, rel)
	mustExistAt(t, w.Root, dir)
	if err := os.Remove(filepath.Join(w.SpamRoot, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}

	// A stray attachment directory on the far side.
	if err := os.MkdirAll(filepath.Join(w.SpamRoot, filepath.FromSlash(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := w.MoveNote(rel, state.DispositionSpam); !errors.Is(err, ErrNoteInBothTrees) {
		t.Errorf("stray .d/: err = %v", err)
	}
	mustExistAt(t, w.Root, rel)

	// Under neither.
	if _, err := w.MoveNote("2026/08/07/000000_gmail-work_nope_00000000.md", state.DispositionSpam); !errors.Is(err, ErrNoteMissing) {
		t.Errorf("missing: err = %v", err)
	}

	// Input validation.
	for _, tc := range []struct {
		rel  string
		disp state.Disposition
	}{
		{"../x.md", state.DispositionSpam},
		{"2026/08/07/not-a-note.txt", state.DispositionSpam},
		{rel, "trash"},
	} {
		if _, err := w.MoveNote(tc.rel, tc.disp); err == nil {
			t.Errorf("MoveNote(%q, %q) accepted", tc.rel, tc.disp)
		}
	}
	solo := &Writer{Root: w.Root, TZ: tzAms}
	if _, err := solo.MoveNote(rel, state.DispositionSpam); err == nil || !strings.Contains(err.Error(), "spam_root") {
		t.Errorf("no spam root configured: err = %v", err)
	}
}

// TestMoveNoteRefusesAnEvictedNote: a placeholder whose bytes live only in
// iCloud is never renamed — the dataless seam says so and nothing moves.
func TestMoveNoteRefusesAnEvictedNote(t *testing.T) {
	w, rel, dir := movedNote(t)
	noteAbs := filepath.Join(w.Root, filepath.FromSlash(rel))
	w.Dataless = func(p string) (bool, error) { return p == noteAbs, nil }
	if _, err := w.MoveNote(rel, state.DispositionSpam); !errors.Is(err, ErrDataless) {
		t.Errorf("evicted note: err = %v", err)
	}
	mustExistAt(t, w.Root, rel)
	mustExistAt(t, w.Root, dir)
	mustBeAbsent(t, w.SpamRoot, rel)
}
