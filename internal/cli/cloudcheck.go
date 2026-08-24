package cli

// `save doctor`'s archive-storage preflight.
//
// Putting archive_root inside iCloud Drive (an Obsidian vault, say) is a
// perfectly reasonable thing to want, and it is also the configuration in
// which this program has the most ways to quietly do the wrong thing:
//
//   - the state database is a WAL-mode SQLite file; a sync agent copying it
//     and its -wal/-shm sidecars mid-transaction corrupts it;
//   - "Optimize Mac Storage" evicts archived files, so `save verify` can no
//     longer read what it wrote without pulling it back over the network;
//   - the archive counts against BOTH local disk and the iCloud storage plan;
//   - the 0600/0700 hardening is a local-filesystem property and does not
//     survive the round trip through the provider;
//   - an archive_root whose path contains an iCloud-excluded component never
//     syncs at all, silently.
//
// None of these are errors the program can fix, so all but the state-database
// one are reported as warnings and notes. The checks are silent entirely when
// archive_root is an ordinary local directory.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/adrg/xdg"

	"save/internal/naming"
)

// evictionFreeBytes is the free-space mark below which macOS starts evicting
// iCloud Drive contents to reclaim disk. It is not a documented constant;
// ~20 GiB is where the behaviour is reliably observed.
const evictionFreeBytes = 20 << 30

// deepestArchivePathBytes is the longest archive-root-relative path this
// program can generate: "YYYY/MM/DD/" + a 100-byte stem + ".d/" + a 100-byte
// attachment name, with slack. Below this much headroom, some writes will be
// refused by naming.CheckArchivePath rather than produce an unopenable path.
const deepestArchivePathBytes = 220

// cloudEnv is the injectable seam for the archive-storage preflight: the home
// directory the sync trees hang off, plus the three platform probes. Tests
// substitute all of them; production uses defaultCloudEnv.
type cloudEnv struct {
	home      string
	tracked   func(path string) (bool, error)
	optimize  func() (on bool, known bool)
	freeBytes func(path string) (uint64, error)
}

func defaultCloudEnv() cloudEnv {
	return cloudEnv{
		home:      xdg.Home,
		tracked:   trackedPath,
		optimize:  optimizeStorage,
		freeBytes: freeBytes,
	}
}

// cloudDomain describes the sync tree an archive root was found inside.
type cloudDomain struct {
	// root is the outermost directory the provider owns. Nothing under it —
	// including the state database — is safe from the provider.
	root string
	// what names it for the report.
	what string
}

// detectCloudDomain reports whether dir sits inside a macOS FileProvider sync
// tree. Two path prefixes cover every provider (~/Library/Mobile Documents is
// iCloud's container store, ~/Library/CloudStorage is where Dropbox, OneDrive,
// Google Drive and friends are mounted); the UF_TRACKED flag catches a root
// reached by some other route, e.g. through a symlink into a container.
//
// dir need not exist yet: the prefix tests are pure string work, and a failing
// stat simply means the flag test contributes nothing.
func detectCloudDomain(dir string, env cloudEnv) (cloudDomain, bool) {
	for _, c := range []cloudDomain{
		{filepath.Join(env.home, "Library", "Mobile Documents"), "iCloud Drive (~/Library/Mobile Documents)"},
		{filepath.Join(env.home, "Library", "CloudStorage"), "a cloud provider mounted at ~/Library/CloudStorage"},
	} {
		if underDir(c.root, dir) {
			return c, true
		}
	}
	if env.tracked != nil {
		if ok, err := env.tracked(dir); err == nil && ok {
			return cloudDomain{
				root: filepath.Clean(dir),
				what: "a cloud provider (the directory carries UF_TRACKED, the flag a FileProvider sets on items it owns)",
			}, true
		}
	}
	return cloudDomain{}, false
}

// underDir reports whether path is dir itself or anything beneath it. Both
// are cleaned first, so "/a/b" contains "/a/b/../b/c" and does not contain
// "/a/bc".
func underDir(dir, path string) bool {
	dir, path = filepath.Clean(dir), filepath.Clean(path)
	return dir == path || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// checkArchiveStorage reports on the medium archive_root sits on. It prints
// nothing at all for an ordinary local directory; the whole section only
// appears when the archive is inside a cloud-sync tree.
func (d *doctorReport) checkArchiveStorage(root string, env cloudEnv) {
	dom, ok := detectCloudDomain(root, env)
	if !ok {
		return
	}
	d.section("archive storage — %s", root)
	d.warn("archive_root is inside %s, so this is a synced folder, not a plain directory", dom.what)

	d.checkStateDBOutside(dom)
	d.checkOptimizeStorage(env)
	d.checkFreeSpace(root, env)
	d.checkArchivePathBudget(root)

	d.info("everything archived here leaves this machine for Apple's servers and is re-downloaded onto every device signed into the same account; on those devices it lands with the provider's own permissions (0644 files, 0755 directories), so save's local 0600/0700 hardening does not travel with it")
	d.info("an archive of tens of thousands of small files will make Obsidian's mobile app slow to open the vault and keep fileproviderd busy indexing; consider keeping the archive in its own vault, or excluding the folder from Obsidian's search")
}

// checkStateDBOutside is the one hard failure in this section. SQLite in WAL
// mode is a database file plus a -wal log and a -shm shared-memory index whose
// mutual consistency is maintained by byte-range locks the sync agent knows
// nothing about. A provider that uploads, evicts or restores any one of the
// three independently produces a corrupt database, and it will do so silently.
func (d *doctorReport) checkStateDBOutside(dom cloudDomain) {
	dbPath := stateDBPath()
	if !underDir(dom.root, dbPath) {
		d.ok("state database %s is outside the sync tree", dbPath)
		return
	}
	d.bad("Move the state directory onto local disk and re-run `save sync`:\n"+
		"  the default is ~/.local/state/save, overridden by $XDG_STATE_HOME.\n"+
		"Deleting the database is safe — the next sync re-enumerates and skips\n"+
		"everything already on disk — so if it is already damaged, delete it.",
		"state database %s is inside the sync tree %s — SQLite's WAL and shared-memory sidecars will be synced out of step with the database and corrupt it", dbPath, dom.root)
}

// checkOptimizeStorage reports iCloud's "Optimize Mac Storage" setting, which
// decides whether archived files stay on disk or become placeholders.
func (d *doctorReport) checkOptimizeStorage(env cloudEnv) {
	if env.optimize == nil {
		return
	}
	on, known := env.optimize()
	switch {
	case !known:
		d.info(`could not read iCloud Drive's "Optimize Mac Storage" setting (com.apple.bird optimize-storage); if it is on, macOS may evict archived files and 'save verify' will skip them`)
	case on:
		d.warn(`"Optimize Mac Storage" is ON — macOS may evict archived files, leaving placeholders whose bytes come back only on demand (about a second each, and only while online). Turn it off in System Settings > [your name] > iCloud > iCloud Drive, or right-click the archive folder in Finder and choose "Keep Downloaded" to pin just this tree. 'save verify' skips evicted files; 'save verify --materialize' downloads them.`)
	default:
		d.ok(`"Optimize Mac Storage" is off — archived files stay on local disk`)
	}
}

// checkFreeSpace warns near the eviction threshold and states the two-sided
// cost of a cloud archive.
func (d *doctorReport) checkFreeSpace(root string, env cloudEnv) {
	d.info("a full backfill of two Gmail accounts with attachments can run to many gigabytes, and every byte is charged twice: once to this disk and once to the iCloud storage plan")
	if env.freeBytes == nil {
		return
	}
	// The root may not exist yet; the home directory is on the same volume
	// and answers the same question.
	free, err := env.freeBytes(root)
	if err != nil {
		if free, err = env.freeBytes(env.home); err != nil {
			return
		}
	}
	if free < evictionFreeBytes {
		d.warn("only %s free on this volume — below roughly %s macOS starts evicting iCloud Drive content to reclaim space, which is exactly what turns a freshly written archive into placeholders",
			gib(free), gib(evictionFreeBytes))
		return
	}
	d.ok("%s free on the archive volume", gib(free))
}

// checkArchivePathBudget guards the two ways a deep container path can go
// wrong: busting the whole-path budget, and sitting under a component iCloud
// refuses to sync.
func (d *doctorReport) checkArchivePathBudget(root string) {
	if budget := naming.PathBudget(root); budget < deepestArchivePathBytes {
		d.warn("archive_root is %d bytes long, leaving %d for the rest of the path — the deepest file save writes needs about %d, so some attachments will be refused rather than written to a path nothing can open. Shorten the vault or folder name.",
			len(root), budget, deepestArchivePathBytes)
	} else {
		d.ok("path budget: %d bytes left under archive_root (the deepest file save writes needs about %d)", budget, deepestArchivePathBytes)
	}
	if bad := naming.FirstSyncExcluded(root); bad != "" {
		d.warn("the path component %q is on iCloud's filename exclusion list, so NOTHING under archive_root will ever be uploaded — the files stay on this Mac and the provider reports no error. Rename that folder.", bad)
	}
}

// gib renders a byte count the way the warnings talk about it.
func gib(n uint64) string {
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}
