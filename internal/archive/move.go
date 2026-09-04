package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"comms/internal/naming"
	"comms/internal/state"
)

// Sentinel errors for MoveNote. Callers classify with errors.Is.
var (
	// ErrNoteInBothTrees means the note exists under both roots. Nothing is
	// touched: the mover cannot know which copy is the real one, and
	// `comms verify` reports it for a person to resolve.
	ErrNoteInBothTrees = errors.New("note exists in both the archive and the spam tree")

	// ErrCrossDevice means the two roots are on different filesystems, so a
	// rename — the only atomic move — is impossible. Nothing is copied.
	ErrCrossDevice = errors.New("archive_root and spam_root are on different volumes; a note cannot be moved atomically between them")
)

// MoveResult says what MoveNote did.
type MoveResult struct {
	// From is the tree the note was found in, To the tree it is in now.
	From, To state.Disposition

	// AlreadyThere reports that the note was already under To when the call
	// came — a previous move was interrupted after the .md rename — and
	// only the attachment directory, if it had been left behind, moved.
	AlreadyThere bool

	// MovedAttachDir reports that a <stem>.d/ directory was renamed too.
	MovedAttachDir bool
}

// MoveNote moves an email note — its .md and its <stem>.d/ attachment
// directory, if any — to the same rel path under the tree `to` selects,
// by rename. It is the only thing that relocates a note, and it is the
// filesystem half of a triage decision: the caller records the new
// disposition in the state database AFTER this returns, never before
// (see state.DB.SetDisposition), so the database never claims a location
// the files have not reached.
//
// The order is .md first, then .d/, and each rename is atomic, so a crash
// leaves one of three states, all recoverable by calling MoveNote again
// with the same arguments — which is what the next triage pass does:
//
//   - nothing moved: the move simply happens;
//   - .md moved, .d/ not yet: the .md is found under `to`, the .d/ is moved
//     to follow it, and AlreadyThere is reported;
//   - both moved, row not updated: found under `to`, nothing to do,
//     AlreadyThere.
//
// The .md's location is the truth a recovery reads. What MoveNote refuses:
// a note present under BOTH roots (ErrNoteInBothTrees — a person decides),
// a note under neither (ErrNoteMissing), an attachment directory already
// present under `to` while the note is still under `from` (the same
// ambiguity), a .md whose bytes are evicted to iCloud (ErrDataless: a
// placeholder must not be touched), and roots on different volumes
// (ErrCrossDevice). Renaming a directory moves its entries without reading
// them, so the attachment files inside are not probed for eviction.
func (w *Writer) MoveNote(rel string, to state.Disposition) (MoveResult, error) {
	var res MoveResult
	if err := w.check(); err != nil {
		return res, err
	}
	if err := checkRel(rel); err != nil {
		return res, fmt.Errorf("archive: move note: %w", err)
	}
	if !strings.HasSuffix(rel, ".md") {
		return res, fmt.Errorf("archive: move note %s: not a note (.md)", rel)
	}
	if !state.ValidDisposition(to) {
		return res, fmt.Errorf("archive: move note %s: %q is not a disposition", rel, to)
	}
	from := state.DispositionArchive
	if to == state.DispositionArchive {
		from = state.DispositionSpam
	}
	fromRoot, err := w.RootFor(from)
	if err != nil {
		return res, fmt.Errorf("archive: move note %s: %w", rel, err)
	}
	toRoot, err := w.RootFor(to)
	if err != nil {
		return res, fmt.Errorf("archive: move note %s: %w", rel, err)
	}
	if err := w.checkDest(toRoot, rel); err != nil {
		return res, fmt.Errorf("archive: move note %s: %w", rel, err)
	}
	res.From, res.To = from, to

	stat := os.Lstat
	if w.Stat != nil {
		stat = w.Stat
	}
	exists := func(p string) (bool, error) {
		_, err := stat(p)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, fs.ErrNotExist):
			return false, nil
		}
		return false, err
	}
	sub := filepath.FromSlash(rel)
	attachRel := path.Join(path.Dir(rel), naming.AttachDir(strings.TrimSuffix(path.Base(rel), ".md")))
	attachSub := filepath.FromSlash(attachRel)
	mdFrom, mdTo := filepath.Join(fromRoot, sub), filepath.Join(toRoot, sub)
	dirFrom, dirTo := filepath.Join(fromRoot, attachSub), filepath.Join(toRoot, attachSub)

	inFrom, err := exists(mdFrom)
	if err != nil {
		return res, fmt.Errorf("archive: move note %s: %w", rel, err)
	}
	inTo, err := exists(mdTo)
	if err != nil {
		return res, fmt.Errorf("archive: move note %s: %w", rel, err)
	}
	switch {
	case inFrom && inTo:
		return res, fmt.Errorf("archive: move note %s: %w", rel, ErrNoteInBothTrees)
	case !inFrom && !inTo:
		return res, fmt.Errorf("archive: move note %s: %w (neither %s nor %s)", rel, ErrNoteMissing, mdFrom, mdTo)
	case inTo:
		// The .md already crossed; only the attachment directory may lag.
		res.AlreadyThere = true
		if err := w.moveAttachDir(rel, dirFrom, dirTo, exists, &res); err != nil {
			return res, err
		}
		return res, nil
	}

	// The normal case: the note is under `from`. Refuse the ambiguous shape
	// before touching anything.
	if dirInTo, err := exists(dirTo); err != nil {
		return res, fmt.Errorf("archive: move note %s: %w", rel, err)
	} else if dirInTo {
		return res, fmt.Errorf("archive: move note %s: %w: the attachment directory %s already exists under the destination while the note is still under %s",
			rel, ErrNoteInBothTrees, attachRel, fromRoot)
	}
	if err := w.assertNotDataless(mdFrom, "note"); err != nil {
		return res, fmt.Errorf("archive: move note %s: %w", rel, err)
	}
	if err := os.MkdirAll(filepath.Dir(mdTo), 0o700); err != nil {
		return res, fmt.Errorf("archive: move note %s: %w", rel, err)
	}
	if err := os.Rename(mdFrom, mdTo); err != nil {
		return res, fmt.Errorf("archive: move note %s: %w", rel, classifyRename(err))
	}
	fi, err := stat(mdTo)
	if err != nil || !fi.Mode().IsRegular() {
		return res, fmt.Errorf("archive: %w for %s: the rename reported success but the destination did not stat as a regular file (%v)", ErrRenameUnconfirmed, rel, err)
	}
	if err := w.moveAttachDir(rel, dirFrom, dirTo, exists, &res); err != nil {
		return res, err
	}
	return res, nil
}

// moveAttachDir renames the note's attachment directory to follow the .md,
// when there is one left behind.
func (w *Writer) moveAttachDir(rel, dirFrom, dirTo string, exists func(string) (bool, error), res *MoveResult) error {
	dirInFrom, err := exists(dirFrom)
	if err != nil {
		return fmt.Errorf("archive: move note %s: %w", rel, err)
	}
	if !dirInFrom {
		return nil
	}
	dirInTo, err := exists(dirTo)
	if err != nil {
		return fmt.Errorf("archive: move note %s: %w", rel, err)
	}
	if dirInTo {
		return fmt.Errorf("archive: move note %s: %w: the attachment directory exists under both roots", rel, ErrNoteInBothTrees)
	}
	if err := os.Rename(dirFrom, dirTo); err != nil {
		return fmt.Errorf("archive: move note %s: attachment directory: %w", rel, classifyRename(err))
	}
	res.MovedAttachDir = true
	return nil
}

// classifyRename maps a cross-device rename onto its sentinel so the
// caller can say what to fix; every other error passes through.
func classifyRename(err error) error {
	if errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("%w: %w", ErrCrossDevice, err)
	}
	return err
}
