package cli

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"save/internal/archive"
	"save/internal/config"
	"save/internal/state"
)

const verifyLong = `Audit the state database against the archive tree (read-only).

Every archived email and rendered chat day is re-hashed and compared with the
hash recorded when it was written; every completed attachment is checked for
existence and size; and any file matching the archive naming pattern with no
database row is reported as an orphan.

Cloud-synced archives: when archive_root lives in iCloud Drive (or any other
macOS FileProvider), "Optimize Mac Storage" may have EVICTED a file — the
directory entry and size are still local but the bytes are not. Reading such a
file forces a download that costs a second or more each and can refill a disk
the system deliberately emptied, so verify detects those files (SF_DATALESS)
and SKIPS them, reporting the count. It also pins this process's
materialization policy off, so an accidental read fails instead of quietly
fetching gigabytes.

--materialize opts back into the old behaviour: hash everything, downloading
whatever has been evicted. Make sure there is disk space for the whole archive
before using it.`

func newVerifyCmd() *cobra.Command {
	var materialize bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Audit the state database against the archive tree (read-only)",
		Long:  verifyLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerifyWith(cmd.OutOrStdout(), verifyOpts{materialize: materialize})
		},
	}
	cmd.Flags().BoolVar(&materialize, "materialize", false,
		"hash cloud-evicted files too, downloading them first (slow, and it consumes disk space)")
	return cmd
}

// verifyOpts carries verify's flags and the filesystem seams it goes through.
// The function fields are nil in production — the real implementations from
// cloudfs_{darwin,other}.go are used — and injected by tests.
type verifyOpts struct {
	// materialize opts back into reading every file, letting the cloud
	// provider download whatever it has evicted.
	materialize bool

	// dataless reports whether a path is an evicted cloud placeholder.
	dataless func(path string) (bool, error)

	// noMaterialize pins this process's materialization policy off.
	noMaterialize func() error
}

func (o verifyOpts) isDataless(path string) (bool, error) {
	if o.dataless != nil {
		return o.dataless(path)
	}
	return datalessFile(path)
}

func (o verifyOpts) pinNoMaterialize() error {
	if o.noMaterialize != nil {
		return o.noMaterialize()
	}
	return setNoMaterialize()
}

// evictedRead reports whether err is the kernel refusing to fetch bytes that
// live only in the cloud. With materialization disabled a dataless read fails
// with EDEADLK; a materialization the provider never satisfies fails with
// ETIMEDOUT. Neither means the archive is corrupt — it means the file is not
// here to check.
func evictedRead(err error) bool {
	return errors.Is(err, syscall.EDEADLK) || errors.Is(err, syscall.ETIMEDOUT)
}

// labelInName is config.LabelPattern with its anchors stripped, so the
// account-label grammar can be embedded in the basename pattern below
// instead of being spelled out a second time.
var labelInName = strings.TrimSuffix(strings.TrimPrefix(config.LabelPattern, "^"), "$")

// archiveMDName matches the day-directory .md basenames this program writes.
// Every stem carries the owning instance's FILE TAG ("<kind>-<label>") as
// its source component:
//
//	email: HHMMSS_gmail-work_<subject-slug>_<hash8>.md
//	chat:  gchat-work_<space-type>_<space-slug>_<hash8>.md
//
// Matching the tag rather than just the "_<hash8>.md" tail keeps the orphan
// scan from claiming unrelated files a user parked in a day directory.
var archiveMDName = regexp.MustCompile(
	`^(?:\d{6}_(?:` + state.SourceGmail + `|` + state.SourceFastmail + `)|` + state.SourceGChat + `)-` +
		labelInName + `_.+_[0-9a-f]{8}\.md$`)

// dayRelPath matches an archive-root-relative path directly inside a day
// directory: YYYY/MM/DD/<basename>.
var dayRelPath = regexp.MustCompile(`^\d{4}/\d{2}/\d{2}/[^/]+$`)

// runVerify audits the archive with the default (eviction-aware) options.
func runVerify(out io.Writer) error {
	return runVerifyWith(out, verifyOpts{})
}

func runVerifyWith(out io.Writer, o verifyOpts) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	ro, err := openRO(stateDBPath())
	if err != nil {
		return err
	}
	defer ro.Close()

	// Belt and braces: before touching the tree, forbid this process from
	// materializing cloud placeholders at all. Then even a code path that
	// forgets the dataless probe fails fast instead of downloading the
	// archive. Skipped under --materialize, which wants exactly that download.
	if !o.materialize {
		if err := o.pinNoMaterialize(); err != nil {
			fmt.Fprintf(out, "note: could not disable cloud materialization for this process (%v);\n"+
				"      an accidental read may still trigger an iCloud download\n", err)
		}
	}

	root := cfg.ArchiveRoot
	var problems int
	report := func(format string, args ...any) {
		problems++
		fmt.Fprintf(out, "  %s\n", fmt.Sprintf(format, args...))
	}
	// evicted counts files whose bytes are not on local disk. They are not
	// discrepancies — there is simply nothing to hash without a download.
	var evicted int
	// hashOrSkip returns the file's sha256, or skipped=true when the file is
	// an evicted cloud placeholder. Errors keep their fs.ErrNotExist identity
	// so the callers can still tell "missing" from "unreadable".
	hashOrSkip := func(path string) (sum string, skipped bool, err error) {
		if !o.materialize {
			switch dl, derr := o.isDataless(path); {
			case derr != nil:
				return "", false, derr
			case dl:
				evicted++
				return "", true, nil
			}
		}
		sum, err = hashFile(path)
		if err != nil && evictedRead(err) {
			evicted++
			return "", true, nil
		}
		return sum, false, err
	}
	// known collects every rel path a DB row claims, for the orphan pass.
	known := make(map[string]bool)

	// Every archived email's file must exist with the recorded content hash
	// (email .md files are immutable after write).
	var emails int
	err = forEachRow(ro, `SELECT source, stable_id, rel_path, content_hash FROM messages`,
		func(scan func(...any) error) error {
			var source, id, rel, hash string
			if err := scan(&source, &id, &rel, &hash); err != nil {
				return err
			}
			emails++
			known[rel] = true
			sum, skipped, err := hashOrSkip(filepath.Join(root, filepath.FromSlash(rel)))
			switch {
			case skipped:
				// Evicted to the cloud; counted, not checked.
			case errors.Is(err, fs.ErrNotExist):
				report("missing file: %s (%s/%s)", rel, source, id)
			case err != nil:
				report("unreadable file: %s: %v", rel, err)
			case sum != hash:
				report("content hash mismatch: %s (%s/%s)", rel, source, id)
			}
			return nil
		})
	if err != nil {
		return err
	}

	// Every rendered chat day file must exist; clean (non-dirty) ones must
	// match their recorded hash. Dirty rows are pending re-render, so only
	// existence is checked; never-rendered rows (NULL hash) own no file yet.
	// A hash mismatch is not reported immediately: a live daemon can
	// legitimately re-render a clean day between this scan's DB snapshot and
	// the file hash (TOCTOU), so suspects are re-checked afterwards against
	// a fresh row and only a persistent mismatch is reported.
	type chatDaySuspect struct {
		source, space, day, rel string
	}
	var chatDays int
	var suspects []chatDaySuspect
	err = forEachRow(ro, `SELECT source, space_name, day_bucket, rel_path, dirty, COALESCE(content_hash, '') FROM chat_day_files`,
		func(scan func(...any) error) error {
			var source, space, day, rel string
			var dirty int
			var hash string
			if err := scan(&source, &space, &day, &rel, &dirty, &hash); err != nil {
				return err
			}
			chatDays++
			known[rel] = true
			if hash == "" {
				return nil
			}
			sum, skipped, err := hashOrSkip(filepath.Join(root, filepath.FromSlash(rel)))
			switch {
			case skipped:
				// Evicted to the cloud; counted, not checked.
			case errors.Is(err, fs.ErrNotExist):
				report("missing chat day file: %s (%s %s %s)", rel, source, space, day)
			case err != nil:
				report("unreadable chat day file: %s: %v", rel, err)
			case dirty == 0 && sum != hash:
				suspects = append(suspects, chatDaySuspect{source, space, day, rel})
			}
			return nil
		})
	if err != nil {
		return err
	}
	// The re-check must run after the scan's rows are closed: openRO allows
	// a single connection, so a nested query inside forEachRow would block.
	// It re-reads the full primary key — two accounts can hold rows for the
	// very same (space, day).
	for _, s := range suspects {
		var dirty int
		var hash string
		err := ro.QueryRow(`
			SELECT dirty, COALESCE(content_hash, '') FROM chat_day_files
			WHERE source = ? AND space_name = ? AND day_bucket = ?`, s.source, s.space, s.day).Scan(&dirty, &hash)
		if errors.Is(err, sql.ErrNoRows) {
			continue // row vanished mid-scan; nothing stable to report
		}
		if err != nil {
			return err
		}
		if dirty != 0 || hash == "" {
			continue // re-render pending: the mismatch was the daemon at work
		}
		sum, skipped, err := hashOrSkip(filepath.Join(root, filepath.FromSlash(s.rel)))
		switch {
		case skipped:
			// Evicted between the two passes; counted, not checked.
		case errors.Is(err, fs.ErrNotExist):
			report("missing chat day file: %s (%s %s %s)", s.rel, s.source, s.space, s.day)
		case err != nil:
			report("unreadable chat day file: %s: %v", s.rel, err)
		case sum != hash:
			report("chat day file content hash mismatch: %s (%s %s %s)", s.rel, s.source, s.space, s.day)
		}
	}

	// Every attachment marked done must exist with the recorded size. This
	// pass needs no eviction handling: a dataless placeholder still reports
	// the file's true st_size, and stat never materializes anything.
	var attsDone int
	err = forEachRow(ro, `SELECT source, stable_id, part_key, rel_path, COALESCE(bytes, 0) FROM attachments WHERE status = 'done'`,
		func(scan func(...any) error) error {
			var source, id, part, rel string
			var size int64
			if err := scan(&source, &id, &part, &rel, &size); err != nil {
				return err
			}
			attsDone++
			known[rel] = true
			info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
			switch {
			case errors.Is(err, fs.ErrNotExist):
				report("missing attachment: %s (%s/%s)", rel, source, id)
			case err != nil:
				report("unreadable attachment: %s: %v", rel, err)
			case size > 0 && info.Size() != size:
				report("attachment size mismatch: %s (have %d bytes, recorded %d)", rel, info.Size(), size)
			}
			return nil
		})
	if err != nil {
		return err
	}

	// Orphans: day-dir .md files matching the naming pattern with no row.
	// (Email attachment files inside .d dirs are not DB-tracked — they are
	// listed in each .md's frontmatter — so the orphan scan covers .md only.)
	var orphans int
	walkErr := filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll // nothing archived yet
			}
			return err
		}
		if de.IsDir() {
			if de.Name() == archive.TempDirName && filepath.Dir(p) == root {
				return fs.SkipDir // writer scratch dir; swept, never archived
			}
			return nil
		}
		if !de.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if !dayRelPath.MatchString(rel) || !archiveMDName.MatchString(de.Name()) {
			return nil
		}
		if !known[rel] {
			orphans++
			report("orphan file (matches the archive naming pattern but has no DB row): %s", rel)
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("scan archive tree: %w", walkErr)
	}

	fmt.Fprintf(out, "checked %d emails, %d chat day files, %d completed attachments; %d orphan(s)\n",
		emails, chatDays, attsDone, orphans)
	if evicted > 0 {
		fmt.Fprintf(out, "%d file(s) skipped (evicted from local storage by iCloud) — "+
			"their contents were not checked; re-run with --materialize to download and hash them\n", evicted)
	}
	if problems > 0 {
		return fmt.Errorf("verify found %d discrepanc(ies)", problems)
	}
	fmt.Fprintln(out, "verify: OK — state database and archive tree agree")
	return nil
}

// forEachRow runs query and invokes fn once per row with that row's Scan.
func forEachRow(db *sql.DB, query string, fn func(scan func(...any) error) error) error {
	rows, err := db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

// hashFile returns the sha256 hex of the file's bytes.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
