# save

`save` archives your communication locally as Markdown: **Gmail**, **Google Chat**
(Workspace), and **FastMail** in one merged, portable folder tree you own.

> **This is a personal-use tool, not a hosted service.** It runs on your machine under
> your own credentials, and nothing is ever uploaded anywhere. Before it can fetch
> anything you need to bring:
>
> - **your own Google Cloud OAuth client** (a Desktop-app client ID with the Gmail and
>   Google Chat APIs enabled) — see [Google setup](#google-setup-once-per-account);
> - **a FastMail API token**, if you archive a FastMail account — see
>   [FastMail setup](#fastmail-setup-once-per-account);
> - **a Google Workspace account** for Chat. The Chat API returns 403 on a consumer
>   `@gmail.com` account, so set `chat = false` on such a block.
>
> The example config below uses placeholder addresses (`you@example.com`) and the
> account labels `work`, `personal` and `fm` — replace them with your own.

- **Several accounts per source.** Two Google Workspace identities plus a FastMail
  account all land in one tree; every file says which account it came from.
- One `.md` file per email, with every attachment downloaded next to it and linked
  relatively.
- One `.md` file per chat conversation per day, updated as new messages arrive.
- Everything sorted into `YYYY/MM/DD/` folders; filenames carry `<source>-<label>` so a
  mixed day directory never collides.
- Runs as a daemon (or one-shot), syncs incrementally, survives crashes and restarts,
  and re-runs are byte-identical no-ops.

```
~/Archive/2026/08/07/
├── 143205_gmail-work_re-invoice-july_a1b2c3d4.md
├── 143205_gmail-work_re-invoice-july_a1b2c3d4.d/
│   └── invoice.pdf
├── 101830_gmail-personal_weekly-report_5b4a3c2d.md
├── 091412_fastmail-fm_no-subject_0e9f8d7c.md
├── gchat-work_space_team-platform_9f8e7d6c.md
└── gchat-work_space_team-platform_9f8e7d6c.d/
    └── 091600_ab12cd34_rollout-plan.pdf
```

`work`, `personal` and `fm` above are **account labels** you choose in the config. A label
is permanent: it is part of every filename that account writes and of its sync state, so
renaming one orphans that account's archive (the next sync re-downloads everything under
the new label). Two accounts that sit in the same Chat space each archive their own copy
— there is deliberately no cross-account deduplication.

## Install

```sh
go build -o save ./cmd/save   # single static binary, macOS/Linux
```

## Quick start

```sh
save init            # creates ~/.config/save (0700) + skeleton config.toml
$EDITOR ~/.config/save/config.toml   # one block per account (see Configuration)
save auth google work       # one-time browser consent, per [[google]] account
save auth google personal   # …or `save auth google --all` to do every one in turn
save auth fastmail fm       # paste that account's FastMail API token
save sync            # first backfill — Gmail can take hours; safe to interrupt/resume
save run             # daemon; or install the launchd plist for autostart
```

The label may be omitted (`save auth google`) only when exactly one account of that kind
is configured; otherwise the command lists the configured labels and stops.

`save doctor` checks the whole setup account by account and prints the exact walkthrough
for anything missing. `save status` shows per-account cursors, counts, pending
attachments, and warnings. `save verify` audits the archive tree against the state
database. `save refetch` pulls in attachments a widened
[attachment policy](#attachment-safety) now accepts — never automatically.

### Selecting accounts

`save sync --source` accepts a source kind, an account label, or an exact instance id,
and is repeatable:

```sh
save sync --source gmail        # every Gmail account
save sync --source work         # everything of the account labelled "work"
save sync --source gmail:work   # exactly one instance
save sync --source gmail:work --source fastmail:fm
```

`--full` and `--retry-failed` apply only to the selected instances; other accounts' state
is untouched.

## Google setup (once per account)

The tool talks to the Gmail and Chat APIs with your own OAuth client:

1. [console.cloud.google.com](https://console.cloud.google.com) → create (or pick) a project.
2. **APIs & Services → Library** → enable the **Gmail API** and the **Google Chat API**.
3. **OAuth consent screen** → audience **Internal** (Workspace accounts; no verification,
   and refresh tokens don't expire). Note: a Workspace admin can block unlisted OAuth
   clients from the Chat scopes.
4. **Credentials → Create credentials → OAuth client ID** → type **Desktop app**.
5. Download the client JSON to `~/.config/save/google-client.json` and `chmod 600` it.
6. `save auth google <label>` — a browser opens for consent; that account's refresh token
   is cached at `~/.config/save/google-token-<label>.json` (0600).

One `[[google]]` block is one Google identity, and its single consent covers both its
Gmail and its Chat. The scope set follows the block: `gmail = true` adds
`gmail.readonly`, `chat = true` adds the three Chat read scopes, and
`mirror_drive_files = true` adds `drive.readonly` — change any of them and `save doctor`
tells you to re-run `save auth google <label>`.

### Two accounts in different Workspace organizations

An **Internal** OAuth client only accepts users of **its own** Workspace organization. If
your second `[[google]]` account lives in a different org (e.g. `@example.com` and
`@example.net`), it **cannot** reuse the first account's client JSON — consent will be
refused. Either:

- give that account its own Cloud project + Desktop client and point its `client_file` at
  the downloaded JSON (recommended), or
- publish the client as **External**, which brings OAuth verification and 7-day
  refresh-token expiry back.

Accounts in the *same* org can share `google-client.json`; `save doctor` prints a note
whenever two accounts point at one client file so the cross-org trap is visible.

Google Chat requires a Workspace account; on a consumer @gmail.com account the Chat
API returns 403 — set `chat = false` on that `[[google]]` block and use Google Takeout if
you need a one-shot chat export.

## FastMail setup (once per account)

1. FastMail web → **Settings → Privacy & Security → API tokens → New token**.
2. Type **JMAP**, scope **read-only**. The token is shown exactly once.
3. `save auth fastmail <label>` and paste it (or set `SAVE_FASTMAIL_TOKEN_<LABEL>`).

The token is stored at `~/.config/save/fastmail-token-<label>` (0600). API tokens are not
available on FastMail **Basic** plans.

## Configuration

`~/.config/save/config.toml` — accounts are **repeatable blocks**, one per account
(note the double brackets):

```toml
archive_root = "~/Archive"
timezone     = "local"

[daemon]
gmail_interval    = "5m"
gchat_interval    = "2m"
fastmail_interval = "5m"

# One [[google]] block is ONE Google identity; a single OAuth consent covers
# both its Gmail and its Chat.
[[google]]
label   = "work"                        # permanent; appears in filenames
account = "you@example.com"
gmail   = true
chat    = true
include_drafts     = false
mirror_drive_files = false
show_deleted       = false
# client_file = "~/.config/save/google-client-work.json"   # default: google-client.json

[[google]]
label   = "personal"
account = "you@example.net"
gmail   = true
chat    = true
client_file = "~/.config/save/google-client-personal.json"   # different Workspace org

[[fastmail]]
label   = "fm"
account = "you@fastmail.example"
```

| Key | Default | Meaning |
|---|---|---|
| `archive_root` | `~/Archive` | Root of the merged `YYYY/MM/DD` tree |
| `timezone` | `"local"` | Resolved to an IANA zone on first sync, then pinned — the tree's day boundaries never shift even if the machine travels |
| `[daemon] *_interval` | 5m / 2m / 5m | Poll intervals, global **per source kind** — every account of a kind polls on the same schedule (`gchat` short: history-off spaces retain messages only 24h) |
| `[[google]] label` | — | Required, permanent, unique across **all** accounts of all kinds; must match `^[a-z0-9][a-z0-9-]{0,19}$` |
| `[[google]] account` | — | The identity's email address; `save doctor` verifies the token really belongs to it |
| `[[google]] gmail` / `chat` | false | What this identity archives; at least one must be true |
| `[[google]] include_drafts` | false | Drafts churn message ids; off by default |
| `[[google]] mirror_drive_files` | false | Off: Drive-backed Chat attachments are linked, not downloaded (on adds the `drive.readonly` scope — re-run `save auth google <label>`) |
| `[[google]] show_deleted` | false | Keep "(message deleted)" tombstones in day files |
| `[[google]] client_file` | `google-client.json` | This account's OAuth Desktop client JSON; needed when accounts are in different Workspace orgs |
| `[[fastmail]] label` | — | Same rules as a Google label |
| `[[fastmail]] account` | — | Informational, recorded in frontmatter |

At least one account must be enabled. A `[[google]]` block with both `gmail = false` and
`chat = false` is a configuration error, as is a duplicate label. The old single-account
`[gmail]` / `[gchat]` / `[fastmail]` tables are rejected with a message naming the new
format.

### Credential files and environment

Everything lives in the config dir (all `0600`, directory `0700`):

| File | Per | Note |
|---|---|---|
| `google-client.json` | shared | Used by every `[[google]]` block without a `client_file` |
| `google-token-<label>.json` | account | Always per label — two identities can never share one |
| `fastmail-token-<label>` | account | Written by `save auth fastmail <label>` |

Env overrides: `SAVE_CONFIG_DIR`, `SAVE_ARCHIVE_ROOT`, plus per-account
`SAVE_GOOGLE_CLIENT_FILE_<LABEL>`, `SAVE_GOOGLE_TOKEN_FILE_<LABEL>`,
`SAVE_FASTMAIL_TOKEN_<LABEL>` (label uppercased, `-` → `_`; e.g.
`SAVE_FASTMAIL_TOKEN_FM`). The unsuffixed `SAVE_GOOGLE_CLIENT_FILE`,
`SAVE_GOOGLE_TOKEN_FILE` and `SAVE_FASTMAIL_TOKEN` still work, but **only** when exactly
one account of that kind is configured — with two they are ignored, so a stray variable
cannot silently point both accounts at one credential.

Scope: all mail including Sent and Archive, excluding Spam and Trash (messages later
rescued from spam are picked up). Chat: DMs, group chats, and all spaces you're a
member of. Sync state lives in `~/.local/state/save/state.db`, keyed by instance
(`gmail:work`, `gchat:personal`, `fastmail:fm`) — deleting it is safe (the next sync
re-enumerates and skips everything already on disk).

## Attachment safety

Attachments are the only bytes in this archive that an attacker chooses. Everything else
save writes is its own text. So attachments get their own rule, and it is an
**allowlist**, not a blocklist.

> An attachment is stored **iff** its normalized final extension is on the allowlist
> **AND** the content sniffed from its magic bytes is one that allowlist permits for that
> extension. Both halves must hold.

Both halves matter. `invoice.pdf.exe` keys on `.exe` and is refused — only the *last*
extension counts, after Unicode normalization, case folding and stripping trailing dots
and spaces. A `.pdf` whose bytes are actually a zip is refused too, because the extension
half alone is advisory: it only describes what the sender claimed.

The check runs on the **final** filename — the one that would land on disk, after save's
sanitizer has already rewritten anything iCloud silently refuses to sync — so the name
that is judged is the name that exists.

**Nothing is ever dropped silently.** Every refusal is recorded twice: in the note, and
in the state database with the identity needed to fetch it later. See
[Skip records](#skip-records).

### Why an allowlist at all

The threat here is not mainly you double-clicking something. macOS Spotlight parses files
at rest, with no user action, the moment save writes them into the vault — and the
installed importers cover almost exactly the formats an archive like this collects. That
passive parsing, not first open, is the real exposure, and there are documented zero-click
CVEs in that path. An allowlist is the only control that reduces it, because it decides
what is on disk at all.

The allowlist is deliberately **generous**, not narrow — Gmail and Google Chat already
refuse roughly 50 executable extensions at the transport layer, *including inside zip and
tgz archives, and including password-protected ones*, so the hostile payloads mostly
cannot arrive on two of the three sources at all. Narrowing past the common business
formats would cost real fidelity for nearly no added safety there.

**FastMail is the channel where the allowlist is genuinely load-bearing** — its
blocked-type policy is not verified — so if you want to be strict anywhere, be strict
about what arrives there.

### Defaults

Allowed: `pdf`; Word/Excel/PowerPoint modern and legacy plus their macro-free templates;
OpenDocument; `rtf`; `epub`; `txt md log csv tsv json xml yaml`; images including
`heic/heif/avif/tiff`; `ics` and `vcf`; audio and video; S/MIME and PGP parts (`p7s p7m
asc sig pgp`); `eml` and `msg`; `zip`. 57 extensions in all.

Denied: macro-enabled Office, `svg`, `7z rar tar gz tgz bz2 xz cab`, and 65
macOS-executable and Windows-payload extensions.

The executable types are a **hard deny**: `allow_extensions` cannot bring them back, and
naming one there is a config error rather than a silent no-op. `save refetch` can never
select one either, whatever the config says.

### The four contested defaults

| | Default | Why | Change it with |
|---|---|---|---|
| **zip** | **allowed** | Ubiquitous in real business mail, and save never decompresses it, so it is inert bytes on disk. But it is *opaque* — the allowlist tells you nothing about the contents, and that guarantee is absent entirely on FastMail. | `allow_containers = false` |
| **svg** | **denied** | The one image format that is a program. The archive lives in an Obsidian vault, and Obsidian is Electron: it renders SVG in-note, so it executes without anyone double-clicking anything. | `allow_svg = true` |
| **macro-Office** (`docm xlsm pptm dotm xltm potm`) | **denied** | Must be denied by *extension*: content sniffing reports them as plain `docx/xlsx/pptx`, so it offers zero protection here. save additionally reads the zip *central directory names only* (never inflating) to catch a `.docm` renamed to `.docx`. | `allow_macro_office = true` |
| **eml / msg** | **allowed** | Forwarded mail *is* the correspondence this archive exists to keep; dropping a `.eml` drops the evidence. Caveat, stated in the note: save does **not** recurse into it — the nested message's own attachments are neither extracted nor policy-checked, they ride along inside the file. | `deny_extensions = ["eml", "msg"]` |

Denying something later never deletes what is already archived; the notes simply stop
linking it. Allowing something later is recoverable via [`save refetch`](#save-refetch) —
which is what makes every one of these decisions cheap to reverse.

### Size and budget caps

```toml
[attachments]
max_size              = "50MB"    # per attachment
chat_max_size         = "50MB"    # per Chat attachment; defaults to max_size
max_message_bytes     = "100MB"   # raw RFC822 buffered before parsing
max_per_message       = "150MB"   # total stored attachment bytes for one message
max_parts_per_message = 500
run_budget            = "2GB"     # per sync pass; the next pass continues. 0 = none
free_space_floor      = "5GB"     # refuse attachments below this, keep writing notes
```

Sizes accept `kB/MB/GB/TB` (1000-based) and `KiB/MiB/GiB/TiB` (1024-based).

Gmail delivers at most ~50 MB per message, so `max_size` essentially never binds there.
**Google Chat allows 200 MB per file — that is where it actually bites**, and Chat is also
the only source where refusing saves the download, because the blob is fetched separately.
Raise `chat_max_size` if large work media is worth keeping.

`free_space_floor` is enforced for real, per write: below it, the attachment is refused
and recorded while the (tiny) note is still written, so mail keeps archiving on a full
disk instead of the sync dying. `save refetch` checks the whole plan against the floor up
front and refuses before the first byte.

`run_budget` and `free_space_floor` are the two **transient** reasons: they describe the
machine or the pass, not the attachment. `save refetch` retries them whether or not the
policy changed.

### Skip records

A refusal is written into the note twice — in the frontmatter and visibly under
`## Attachments`, so it is impossible to read the note and not notice:

```yaml
skipped_attachments:
  - name: contract.docm
    size: 84213
    declared_type: application/vnd.ms-word.document.macroEnabled.12
    sniffed_type: application/vnd.openxmlformats-officedocument.wordprocessingml.document
    reason: macro_office
    part_key: "2.1"
```

and the same refusal lands in the `skipped_attachments` table keyed by
`(source, stable_id, part_key)` with the sanitized name, the exact decoded byte count, the
declared and sniffed types, the reason, the **policy digest** that produced the decision,
the content SHA-256, and the note it belongs to.

The wording is accurate per source: on Gmail and FastMail the bytes **were downloaded and
discarded** (both fetch the whole message), while a Chat blob refused before its download
genuinely **was never fetched** — and the note says which.

`save status` totals them per account, by reason, in bytes:

```
attachments:  policy CHANGED — now 8c1a…, archive last reconsidered under fb70e01d2e46670e
              41 unresolved skip(s); 12 (18.4 MB) would be accepted by the current policy
              nothing is ever re-fetched automatically — run `save refetch --dry-run`

gmail:work — you@example.com
  refused:      41 attachment(s) by the attachment policy, 260.1 MB not stored (41 unresolved, 260.1 MB)
    not_allowlisted_extension    28 (28 unresolved)
    over_size_cap                 9 (9 unresolved)
    macro_office                  4 (4 unresolved)
```

### `save refetch`

Widening the policy does **not** retroactively fetch anything. A sync notices the digest
changed, says so, and stops there. Fetching is one explicit command:

```sh
save refetch --dry-run              # what would be fetched, and how many bytes
save refetch                        # do it
save refetch --source fastmail:fm   # same selector grammar as `save sync`
save refetch --reason svg_denied    # only what one config change unblocked
```

This is deliberate. A typo in `[attachments]` should not be able to pull gigabytes onto a
volume that is nearly full, so the plan — how many attachments, how many bytes, from which
account — is always printed before anything is fetched, and `--dry-run` stops after it.

Each selected message is re-fetched (Gmail by stable id; FastMail re-resolves the stable
id to a **current** blobId, since JMAP blobIds are not durable; Chat re-queues the blob in
the ordinary download path), re-decided over its **real bytes**, and its note re-rendered
so the archive never has a file on disk that the note still calls skipped. Then the ledger
row is dispositioned:

| Resolution | Meaning |
|---|---|
| `fetched` | The bytes are on disk and the note now links them. |
| `still_denied` | Re-decided over the real content and refused again. Stops being reconsidered every run; the next ordinary sync of that message re-opens it. |
| `source_gone` | Gone upstream, unrecoverable. The note keeps saying, honestly, that the attachment was never stored. |

If a refetched attachment comes back with a **different SHA-256** than the ledger
recorded, nothing is overwritten: the divergence is recorded in the failures ledger, the
skip stays unresolved, and the command exits non-zero. Silently replacing archived bytes
with different ones is the one outcome worse than not fetching them.

A full clean run advances the recorded policy digest, which is what stops `save status`
nagging. A `--source` or `--reason` run is partial by construction and never does.

### Quarantine tagging: what it is and is not

Stored attachments are tagged `com.apple.quarantine` (set on the temp file *before* the
rename, so the flag is never missing on a visible path — Thunderbird's failure to do
exactly this on macOS is CVE-2022-3155). Turn it off with `quarantine = false`.

**It is not a malware scan.** This was measured, not assumed: every one of XProtect's 94
signatures on a current macOS gates on app bundles, installers or executables. There are
**zero** signatures for PDFs, Office documents, zips or images — which is to say, zero for
everything on this allowlist. Quarantining an archived PDF triggers no scan of any kind.

What the tag actually buys:

- macOS shows a **consent prompt** the first time you open the file;
- Word and Excel open the file in **Protected View**.

And it is best-effort even at that: iCloud round-tripping degrades the value to flags
only, and any application that safe-saves over the file drops the tag entirely. **The
allowlist is the real boundary; the tag is a speed bump.**

If you want genuine content scanning, wire up a real scanner:

```toml
scan_command = ["clamdscan", "--fdpass", "--no-summary"]
scan_action  = "record"   # "record" stores and records the verdict; "reject" skips
```

It runs as argv (never through a shell) against the temp file *before* the rename, so a
rejected file never appears in the vault. Exit 0 is clean, 1 is flagged, and **anything
else is an error that is never treated as clean**. The default is `record` rather than
`reject` because antivirus false positives on PDFs and Office documents are common and
official corrections take days — failing closed would delete real business documents,
which the never-drop-silently rule does not permit. Point it at `clamdscan` against a
running `clamd`, not `clamscan`, which reloads a ~1 GB signature database every time.

## Storing the archive in iCloud Drive / an Obsidian vault

Pointing `archive_root` at a folder inside an Obsidian vault in iCloud Drive works, and
it is a nice way to read the archive on a phone. It also changes what the folder *is* —
it stops being a plain directory and becomes something a background daemon rewrites,
uploads and empties behind your back. Everything below is the consequence of that.

```toml
archive_root = "~/Library/Mobile Documents/iCloud~md~obsidian/Documents/MyVault/Communication"
```

Spaces and the embedded `~` in `iCloud~md~obsidian` are fine — only a **leading** `~/` is
expanded, the rest of the path is taken literally. `save doctor` prints a whole
`archive storage` section when it notices the root is inside `~/Library/Mobile Documents`
or `~/Library/CloudStorage`; read it.

**The state database must stay outside the synced tree.** This is the one hard error
`save doctor` reports. `state.db` is a WAL-mode SQLite database: three files (`state.db`,
`state.db-wal`, `state.db-shm`) whose consistency is maintained by byte-range locks that
a sync agent knows nothing about. Uploading, evicting or restoring any one of them
independently corrupts the database, silently. The default location
(`~/.local/state/save`, or `$XDG_STATE_HOME/save`) is already outside; just don't move it
in. Deleting the database is always safe — the next sync re-enumerates and skips
everything already on disk.

**"Optimize Mac Storage" evicts your archive.** With it on (System Settings → *your name*
→ iCloud → iCloud Drive), macOS reclaims disk by throwing away the *contents* of files it
has already uploaded, leaving a placeholder. `ls` still shows the right size, but reading
one costs about a second while it downloads — and fails outright if you are offline.
Either turn the setting off, or right-click the archive folder in Finder and choose
**Keep Downloaded** to pin just that tree. `save doctor` reads the current setting and
tells you which way it is.

**`save verify` skips evicted files by default.** A verify pass hashes every file it has
ever written; on an evicted archive that is a full re-download of everything, at roughly a
second per file, onto a disk the system emptied on purpose. So verify stats each file
first, skips the ones whose bytes are not local, and reports the count:

```
checked 41203 emails, 512 chat day files, 8871 completed attachments; 0 orphan(s)
1874 file(s) skipped (evicted from local storage by iCloud) — their contents were not
checked; re-run with --materialize to download and hash them
```

`save verify --materialize` does the full read-everything audit. Make sure the disk can
hold the entire archive before you use it. For belt and braces, the default pass also
pins its own process I/O policy so that an accidental read *cannot* trigger a download.

**Disk and quota.** A full backfill of two Gmail accounts with attachments runs to many
gigabytes, and every byte is charged twice: once to local disk and once to the iCloud
storage plan. Below roughly 20 GiB free, macOS starts evicting iCloud content to reclaim
space — which is exactly the state that turns a freshly written archive into placeholders.
`save doctor` warns under that mark.

**Privacy.** Everything archived here leaves the machine for Apple's servers and comes
back down on every device signed into the same account. On those devices it lands with the
provider's own permissions (0644 files, 0755 directories): save's local `0600`/`0700`
hardening protects the copy on this Mac and nothing else. Your entire mail archive in
plaintext Markdown, in someone else's datacentre, is a deliberate choice — make it
deliberately.

**Obsidian at scale.** Tens of thousands of small files will make Obsidian's mobile app
slow to open the vault and keep `fileproviderd` busy indexing. Prefer a separate vault for
the archive, or exclude the folder from Obsidian's search.

**Two things that already work correctly**, mentioned so you don't "fix" them:

- The writer's staging directory is `<archive_root>/.tmp`, and iCloud refuses to sync
  anything named `.tmp`. That is intentional: nothing half-written is ever uploaded, and
  the final rename stays on one volume, so it is atomic. Don't move or rename it.
- Every filename save generates is checked against iCloud's silent
  filename-exclusion list (`.nosync`, `.tmp`, `Dropbox`, `~$…`, `desktop.ini`, …) and
  rewritten if it matches — an attachment called `Q3 draft.tmp` is stored as
  `Q3 draft.tmp.bin`. Without that, the file would sit on disk, hash clean in `save
  verify`, and never leave the machine.

## Autostart on macOS

```sh
cp docs/launchd/com.user.save.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.user.save.plist
```

Edit the plist's binary path first; it runs `save run` with `KeepAlive`.

It must be a **user LaunchAgent** in `~/Library/LaunchAgents`, not a system LaunchDaemon
in `/Library/LaunchDaemons`. Besides needing your home directory and your OAuth tokens,
a daemon inherits the *system* dataless-file materialization policy, which is **off** — so
reading an iCloud-evicted archive file returns `EDEADLK` instead of the file. A
GUI-session agent inherits the session policy, which is on. (launchd's
`MaterializeDatalessFiles` key can force it either way; see the comments in the plist.)

## How it stays correct

- **Files first, database second, cursor last** — a crash at any point re-processes at
  most one batch, and deterministic filenames make reprocessing a no-op.
- Atomic writes (same-directory rename) — no partial files, ever.
- Gmail history expiry, FastMail state expiry, and interrupted backfills all
  self-heal by cheap re-enumeration with dedup.
- A message that keeps failing is skipped after 5 attempts and surfaced in
  `save status` instead of wedging the sync forever.
- **No attachment is ever dropped silently.** Every one the policy refuses is written into
  its note *and* into a ledger row carrying the identity needed to fetch it later, so
  widening the policy is always recoverable and never automatic
  (see [Attachment safety](#attachment-safety)).
- **Accounts are isolated in state.** Cursors, the dedup index, the Chat store, the
  backfill queue, pending downloads and the failure ledger are all keyed by instance
  (`gmail:work`), so one account's backlog or poison item never affects another's, and a
  Gmail token is refused if it does not belong to the address its block names.
- Credentials, by contrast, are checked for *every* selected account up front: if one
  account is unauthorized, `save sync` and `save run` stop with that account's re-auth
  instruction rather than running the others. Use `--source` to sync the healthy accounts
  while you sort out a broken one.

## Development

```sh
go test ./...       # unit + golden tests (no network needed)
go vet ./...
```

Package map: `internal/source/{gmail,gchat,fastmail}` connectors →
`internal/emailpipe` (MIME → Markdown) → `internal/archive` (atomic writes,
rendering) with `internal/state` (SQLite cursors/index) underneath.
`internal/policy` is the single attachment storage decision engine every one of
them shares — one implementation, one `PolicyDigest`. See the plan in the repo
history for the full design.
