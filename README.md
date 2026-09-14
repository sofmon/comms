# comms

`comms` archives your communication locally as Markdown and sends file-backed
outgoing messages through **Gmail**, **Google Chat** (Workspace), and **FastMail**.

> **This is a personal-use tool, not a hosted service.** It runs on your machine under
> your own credentials. Archive data stays local; only an explicit `comms send`
> transmits a draft to its configured mail or Chat provider. Before use, bring:
>
> - **your own Google Cloud OAuth client** (a Desktop-app client ID with the Gmail and
>   Google Chat APIs enabled, plus People API when resolving Chat names) — see
>   [Google setup](#google-setup-once-per-account);
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
- Sends only on an explicit `comms send`: valid Markdown drafts move atomically from
  `send/` to `archived/`; failures stay put and the daemon never sends.

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
go build -o comms ./cmd/comms   # single static binary, macOS/Linux
```

Existing local `save` installations should follow the
[migration guide](docs/migrate-save-to-comms.md) before starting the renamed daemon.

## Quick start

```sh
comms init            # creates ~/.config/comms (0700) + skeleton config.toml
$EDITOR ~/.config/comms/config.toml   # one block per account (see Configuration)
comms auth google work       # one-time browser consent, per [[google]] account
comms auth google personal   # …or `comms auth google --all` to do every one in turn
comms auth fastmail fm       # paste that account's FastMail API token
comms sync            # first backfill — Gmail can take hours; safe to interrupt/resume
comms new gmail:work   # create a provider-correct outgoing draft to edit
comms send --dry-run   # validate outgoing Markdown files without sending
comms send             # explicitly send all valid drafts and archive successes
comms run             # daemon; or install the launchd plist for autostart
```

The label may be omitted (`comms auth google`) only when exactly one account of that kind
is configured; otherwise the command lists the configured labels and stops.

`comms doctor` checks the whole setup account by account and prints the exact walkthrough
for anything missing. `comms status` shows per-account cursors, counts, pending
attachments, and warnings. `comms verify` audits the archive tree — and the spam tree —
against the state database. `comms refetch` pulls in attachments a widened
[attachment policy](#attachment-safety) now accepts — never automatically.
`comms triage` files newsletters, notifications and other noise under a separate
`spam_root`, never deleting anything and never touching sync; see
[Noise triage](#noise-triage). Start with `comms triage --dry-run`.

### Selecting accounts

`comms sync --source` accepts a source kind, an account label, or an exact instance id,
and is repeatable:

```sh
comms sync --source gmail        # every Gmail account
comms sync --source work         # everything of the account labelled "work"
comms sync --source gmail:work   # exactly one instance
comms sync --source gmail:work --source fastmail:fm
```

`--full` and `--retry-failed` apply only to the selected instances; other accounts' state
is untouched.

## Filesystem outbox

Sending is deliberately opt-in and never part of `comms run`. Set the outbound
capability on each account you want to use:

```toml
[sending]
root = "~/Comms"               # contains ~/Comms/send and ~/Comms/archived

[[google]]
label = "work"
account = "you@example.com"
gmail = true
chat = true
send_email = true
send_chat = true

[[fastmail]]
label = "fm"
account = "you@fastmail.example"
send_email = true
```

If `[sending].root` is omitted, it defaults to `archive_root + "-outbox"`; with
`archive_root = "~/Archive"`, drafts go in `~/Archive-outbox/send`. The environment
override is `COMMS_OUTBOX_ROOT`. The outbox must be a separate sibling tree, not
inside the archive or spam tree.

Create a draft from the configured account instead of copying a template by hand:

```sh
comms new gmail:work       # email through the Google account labelled work
comms new fastmail:fm      # email through the FastMail account labelled fm
comms new gchat:work       # Google Chat message
```

The command creates a collision-safe, timestamped `0600` file under `send/` and prints
its full path. Required delivery fields and the body start empty, so an untouched template
is deliberately rejected by `comms send`; edit it, run `comms send --dry-run`, then send.
Only explicitly send-enabled instances are accepted.

To reply to an incoming archived email, pass its Markdown file:

```sh
comms new gmail:work --reply ~/Archive/2026/09/05/received-message.md
comms new fastmail:fm --reply ~/Archive/2026/09/05/received-message.md
```

The reply draft prefills the first usable `from` address, adds `Re:` when needed, and
sets `in_reply_to` plus `references` from the archived `message_id`. For a Gmail note
archived by the same Gmail instance, it also carries the Gmail `thread_id`. The original
body is not copied; add the reply body and review every prefilled field before sending.
If an older or malformed archive note has no usable Message-ID, the draft is still created
and the command warns that provider conversation grouping may be unavailable. `--reply`
is for email instances; Google Chat replies still use the `thread` field shown below.

An email draft is Markdown with strict YAML frontmatter:

```markdown
---
type: email
account: work
to:
  - Jane Example <jane@example.com>
cc: boss@example.net
bcc: audit@example.net
subject: Project update
from_name: Your Name
reply_to: replies@example.com
---
Hello Jane,

The project is **ready**.
```

`to`, `cc`, and `bcc` each accept one address or a YAML list; at least one recipient
and a non-empty subject are required. `in_reply_to`, `references`, and `thread_id` are
optional reply metadata normally written by `comms new --reply`. Email bodies are sent as
UTF-8 plain text, so the Markdown source remains readable but is not converted to HTML.
Attachments are not yet supported in the outbound format.

A Google Chat draft names an existing space resource. `thread` is optional; include its
full resource name to reply to an existing thread:

```markdown
---
type: chat
account: work
space: spaces/AAAA123
thread: spaces/AAAA123/threads/BBBB456
---
**Deployment complete.** Please check the dashboard.
```

Run `comms send --dry-run` first, then `comms send`. The command recursively processes
regular `.md` files under `send/` in path order. Invalid or failed files remain there;
each success moves the exact source file to `archived/YYYY/MM/DD/` (UTC), adding a short
id suffix to prevent filename collisions. A persistent ledger plus provider-specific
identities reconciles interrupted attempts: rerunning `comms send` resumes a Gmail or
FastMail draft, finds an already-sent message, or reuses Google Chat's idempotent request
ID instead of blindly delivering it twice. Keep a draft's filename stable while it is in
flight; use a different filename or content when intentionally sending a similar message
again.

Sending and incoming synchronization are separate operations. `comms send` immediately
moves the exact draft into `[sending].root/archived`; the next `comms sync` also downloads
the provider copy from Sent into the main `archive_root` for both Gmail and FastMail. Seeing
both files is expected: one is the submitted source draft and the other is the message as
stored by the mail provider. Running sync again is safe and does not duplicate that copy.

### Sending credentials

- Gmail sending adds `gmail.compose`, because the crash-recovery protocol creates a
  Gmail draft before sending it, and uses `gmail.readonly` to reconcile Sent mail.
- Google Chat sending adds `chat.messages.create` and uses `chat.messages.readonly`
  for conflict reconciliation.
- After enabling either Google flag, run `comms auth google <label>` again and approve
  the added permission.
- FastMail sending requires a JMAP token with write/send access; replace a read-only
  archive token by running `comms auth fastmail <label>` and pasting the new token.

## Google setup (once per account)

The tool talks to the Gmail and Chat APIs with your own OAuth client:

1. [console.cloud.google.com](https://console.cloud.google.com) → create (or pick) a project.
2. **APIs & Services → Library** → enable the **Gmail API** and the **Google Chat API**.
   Also enable the **People API** when `resolve_chat_names = true`.
3. **OAuth consent screen** → audience **Internal** (Workspace accounts; no verification,
   and refresh tokens don't expire). Note: a Workspace admin can block unlisted OAuth
   clients from the Chat scopes.
4. **Credentials → Create credentials → OAuth client ID** → type **Desktop app**.
5. Download the client JSON to `~/.config/comms/google-client.json` and `chmod 600` it.
6. `comms auth google <label>` — a browser opens for consent; that account's refresh token
   is cached at `~/.config/comms/google-token-<label>.json` (0600).

One `[[google]]` block is one Google identity, and its single consent covers both its
Gmail and its Chat. The scope set follows the block: `gmail = true` adds
`gmail.readonly`, `chat = true` adds the three Chat read scopes, `send_email = true`
adds `gmail.compose`, `send_chat = true` adds `chat.messages.create`,
`resolve_chat_names = true` adds `directory.readonly`, and `mirror_drive_files = true`
adds `drive.readonly` — change any of them and `comms doctor` tells you to re-run
`comms auth google <label>`.

### Chat display names and message anchors

Google Chat user-authentication responses can contain only an opaque sender such as
`users/123456789`. To enrich those senders with Workspace profile display
names, enable the People API in the Cloud project for that account and opt in:

```toml
[[google]]
label = "work"
account = "you@example.com"
chat = true
resolve_chat_names = true

# Optional fallbacks for external, deleted, or private profiles:
chat_name_overrides = { "users/123456789" = "Jane Doe" }
```

Then authorize the added scope and sync normally:

```sh
comms auth google work
comms doctor
comms sync --source gchat:work
```

Name lookup is best-effort: a People API failure never stops Chat archival. Comms keeps
resolved and unresolved results in an account-scoped cache, refreshes them periodically,
and falls back to an existing Chat membership name or the original `users/{id}`. A local
override always wins for its account.

Every archived message header also carries a stable block identifier derived from the
immutable Google message resource:

```markdown
**Jane Doe** (09:15) ^gchat-u1-4d4afa86f12d1565
```

In Obsidian, copy that identifier into a block link such as
`[[gchat-work_space_team-platform_9f8e7d6c#^gchat-u1-4d4afa86f12d1565]]`. The readable
part may vary with the Google token, while the hash suffix prevents normalized tokens
from colliding. The identifier stays unchanged when a display name or message body changes.

On upgrade, the state migration marks existing Chat day projections dirty. The next normal
`comms sync` re-renders them from local state with anchors and any resolved names; no
`--full` re-download is required.

### Two accounts in different Workspace organizations

An **Internal** OAuth client only accepts users of **its own** Workspace organization. If
your second `[[google]]` account lives in a different org (e.g. `@example.com` and
`@example.net`), it **cannot** reuse the first account's client JSON — consent will be
refused. Either:

- give that account its own Cloud project + Desktop client and point its `client_file` at
  the downloaded JSON (recommended), or
- publish the client as **External**, which brings OAuth verification and 7-day
  refresh-token expiry back.

Accounts in the *same* org can share `google-client.json`; `comms doctor` prints a note
whenever two accounts point at one client file so the cross-org trap is visible.

Google Chat requires a Workspace account; on a consumer @gmail.com account the Chat
API returns 403 — set `chat = false` on that `[[google]]` block and use Google Takeout if
you need a one-shot chat export.

## FastMail setup (once per account)

1. FastMail web → **Settings → Privacy & Security → API tokens → New token**.
2. Type **JMAP**. Choose **read-only** for archiving alone; enable write/send access
   when `send_email = true`. The token is shown exactly once.
3. `comms auth fastmail <label>` and paste it (or set `COMMS_FASTMAIL_TOKEN_<LABEL>`).

The token is stored at `~/.config/comms/fastmail-token-<label>` (0600). API tokens are not
available on FastMail **Basic** plans.

## Configuration

`~/.config/comms/config.toml` — accounts are **repeatable blocks**, one per account
(note the double brackets):

```toml
archive_root = "~/Archive"
# spam_root  = "~/spam"          # where noise triage files notes; default: beside archive_root
timezone     = "local"

[sending]
# root = "~/Comms"              # default: archive_root + "-outbox"

[daemon]
gmail_interval    = "5m"
gchat_interval    = "2m"
fastmail_interval = "5m"

[triage]                         # see "Noise triage"; all optional
# after_sync        = false      # run the rules after each successful mail sync
# protect_subject   = ["*invoice*", "*factuur*", "*receipt*", "*security alert*", "*new sign-in*"]
[triage.llm]
# enabled           = false      # a local model, consulted last and only for the undecided

# One [[google]] block is ONE Google identity; a single OAuth consent covers
# both its Gmail and its Chat.
[[google]]
label   = "work"                        # permanent; appears in filenames
account = "you@example.com"
gmail   = true
chat    = true
send_email = false
send_chat  = false
resolve_chat_names = false
# chat_name_overrides = { "users/123456789" = "Jane Doe" }
include_drafts     = false
mirror_drive_files = false
show_deleted       = false
# client_file = "~/.config/comms/google-client-work.json"   # default: google-client.json

[[google]]
label   = "personal"
account = "you@example.net"
gmail   = true
chat    = true
client_file = "~/.config/comms/google-client-personal.json"   # different Workspace org

[[fastmail]]
label   = "fm"
account = "you@fastmail.example"
send_email = false
```

| Key | Default | Meaning |
|---|---|---|
| `archive_root` | `~/Archive` | Root of the merged `YYYY/MM/DD` tree |
| `spam_root` | a `spam` directory beside `archive_root` | Where [noise triage](#noise-triage) files notes, at the same `YYYY/MM/DD/<name>` path. Must sit **beside** the archive (never inside it, nor around it) and on the same volume — a move is an atomic rename |
| `[sending] root` | `archive_root + "-outbox"` | Dedicated root containing `send/` and `archived/`; must not contain or sit inside either archive tree |
| `timezone` | `"local"` | Resolved to an IANA zone on first sync, then pinned — the tree's day boundaries never shift even if the machine travels |
| `[daemon] *_interval` | 5m / 2m / 5m | Poll intervals, global **per source kind** — every account of a kind polls on the same schedule (`gchat` short: history-off spaces retain messages only 24h) |
| `[[google]] label` | — | Required, permanent, unique across **all** accounts of all kinds; must match `^[a-z0-9][a-z0-9-]{0,19}$` |
| `[[google]] account` | — | The identity's email address; `comms doctor` verifies the token really belongs to it |
| `[[google]] gmail` / `chat` | false | What this identity archives |
| `[[google]] send_email` / `send_chat` | false | Explicit outbound opt-ins; add OAuth scopes and require `comms auth google <label>` again |
| `[[google]] resolve_chat_names` | false | Resolve opaque Chat sender ids to display names through People API; adds `directory.readonly`, so enable People API and re-authorize |
| `[[google]] chat_name_overrides` | `{}` | Account-scoped `users/{id}` to display-name fallbacks; requires `chat = true` |
| `[[google]] include_drafts` | false | Drafts churn message ids; off by default |
| `[[google]] mirror_drive_files` | false | Off: Drive-backed Chat attachments are linked, not downloaded (on adds the `drive.readonly` scope — re-run `comms auth google <label>`) |
| `[[google]] show_deleted` | false | Keep "(message deleted)" tombstones in day files |
| `[[google]] client_file` | `google-client.json` | This account's OAuth Desktop client JSON; needed when accounts are in different Workspace orgs |
| `[[fastmail]] label` | — | Same rules as a Google label |
| `[[fastmail]] account` | — | Informational, recorded in frontmatter — and one of the "own addresses" triage never files |
| `[[fastmail]] send_email` | false | Enable JMAP submission; requires a token with write/send access |
| `[triage] after_sync` | false | Run the rules-only triage layers after each successful mail sync, in `comms sync` and in the daemon |
| `[triage] header_heuristics` | true | Let the bulk-mail headers captured at archive time decide (Precedence: bulk/junk, Auto-Submitted, X-Auto-Response-Suppress, Gmail Promotions/Social) |
| `[triage] rules_file` | `~/.config/comms/triage.toml` | The `[[keep]]` / `[[noise]]` rules; `comms init` writes a starter |
| `[triage] protect_from` / `protect_subject` | `[]` / invoices, receipts, security alerts, new sign-ins | Globs that are signal before any other layer runs; a stored PDF/Office attachment and your own addresses are protected always |
| `[triage.llm] enabled` | false | Ask a local model about the notes the rules left undecided |
| `[triage.llm] in_daemon` | false | Let the `after_sync` pass ask it too (off: a stopped endpoint costs the archiver nothing) |
| `[triage.llm] base_url` / `model` | `http://127.0.0.1:1234/v1` / — | An OpenAI-compatible chat endpoint (LM Studio, Ollama, …); `model` is required when enabled |
| `[triage.llm] timeout` / `max_body_chars` | 20s / 4000 | Per request; body excerpt sent with the header fields |

At least one archive or send capability must be enabled. A `[[google]]` block with all of
`gmail`, `chat`, `send_email`, and `send_chat` false is a configuration error, as is a
duplicate label. Chat name resolution and overrides require `chat = true`. The old single-account
`[gmail]` / `[gchat]` / `[fastmail]` tables are rejected with a message naming the new
format.

### Credential files and environment

Everything lives in the config dir (all `0600`, directory `0700`):

| File | Per | Note |
|---|---|---|
| `google-client.json` | shared | Used by every `[[google]]` block without a `client_file` |
| `google-token-<label>.json` | account | Always per label — two identities can never share one |
| `fastmail-token-<label>` | account | Written by `comms auth fastmail <label>` |

Env overrides: `COMMS_CONFIG_DIR`, `COMMS_ARCHIVE_ROOT`, `COMMS_SPAM_ROOT`,
`COMMS_OUTBOX_ROOT`, plus per-account
`COMMS_GOOGLE_CLIENT_FILE_<LABEL>`, `COMMS_GOOGLE_TOKEN_FILE_<LABEL>`,
`COMMS_FASTMAIL_TOKEN_<LABEL>` (label uppercased, `-` → `_`; e.g.
`COMMS_FASTMAIL_TOKEN_FM`). The unsuffixed `COMMS_GOOGLE_CLIENT_FILE`,
`COMMS_GOOGLE_TOKEN_FILE` and `COMMS_FASTMAIL_TOKEN` still work, but **only** when exactly
one account of that kind is configured — with two they are ignored, so a stray variable
cannot silently point both accounts at one credential.

Scope: all mail including Sent and Archive, excluding Spam and Trash (messages later
rescued from spam are picked up). Chat: DMs, group chats, and all spaces you're a
member of. Sync state lives in `~/.local/state/comms/state.db`, keyed by instance
(`gmail:work`, `gchat:personal`, `fastmail:fm`) — deleting it is safe (the next sync
re-enumerates and skips everything already on disk).

## Attachment safety

Attachments are the only bytes in this archive that an attacker chooses. Everything else
comms writes is its own text. So attachments get their own rule, and it is an
**allowlist**, not a blocklist.

> An attachment is stored **iff** its normalized final extension is on the allowlist
> **AND** the content sniffed from its magic bytes is one that allowlist permits for that
> extension. Both halves must hold.

Both halves matter. `invoice.pdf.exe` keys on `.exe` and is refused — only the *last*
extension counts, after Unicode normalization, case folding and stripping trailing dots
and spaces. A `.pdf` whose bytes are actually a zip is refused too, because the extension
half alone is advisory: it only describes what the sender claimed.

The check runs on the **final** filename — the one that would land on disk, after comms's
sanitizer has already rewritten anything iCloud silently refuses to sync — so the name
that is judged is the name that exists.

**Nothing is ever dropped silently.** Every refusal is recorded twice: in the note, and
in the state database with the identity needed to fetch it later. See
[Skip records](#skip-records).

### Why an allowlist at all

The threat here is not mainly you double-clicking something. macOS Spotlight parses files
at rest, with no user action, the moment comms writes them into the vault — and the
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
naming one there is a config error rather than a silent no-op. `comms refetch` can never
select one either, whatever the config says.

### The four contested defaults

| | Default | Why | Change it with |
|---|---|---|---|
| **zip** | **allowed** | Ubiquitous in real business mail, and comms never decompresses it, so it is inert bytes on disk. But it is *opaque* — the allowlist tells you nothing about the contents, and that guarantee is absent entirely on FastMail. | `allow_containers = false` |
| **svg** | **denied** | The one image format that is a program. The archive lives in an Obsidian vault, and Obsidian is Electron: it renders SVG in-note, so it executes without anyone double-clicking anything. | `allow_svg = true` |
| **macro-Office** (`docm xlsm pptm dotm xltm potm`) | **denied** | Must be denied by *extension*: content sniffing reports them as plain `docx/xlsx/pptx`, so it offers zero protection here. comms additionally reads the zip *central directory names only* (never inflating) to catch a `.docm` renamed to `.docx`. | `allow_macro_office = true` |
| **eml / msg** | **allowed** | Forwarded mail *is* the correspondence this archive exists to keep; dropping a `.eml` drops the evidence. Caveat, stated in the note: comms does **not** recurse into it — the nested message's own attachments are neither extracted nor policy-checked, they ride along inside the file. | `deny_extensions = ["eml", "msg"]` |

Denying something later never deletes what is already archived; the notes simply stop
linking it. Allowing something later is recoverable via [`comms refetch`](#comms-refetch) —
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
disk instead of the sync dying. `comms refetch` checks the whole plan against the floor up
front and refuses before the first byte.

`run_budget` and `free_space_floor` are the two **transient** reasons: they describe the
machine or the pass, not the attachment. `comms refetch` retries them whether or not the
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

`comms status` totals them per account, by reason, in bytes:

```
attachments:  policy CHANGED — now 8c1a…, archive last reconsidered under fb70e01d2e46670e
              41 unresolved skip(s); 12 (18.4 MB) would be accepted by the current policy
              nothing is ever re-fetched automatically — run `comms refetch --dry-run`

gmail:work — you@example.com
  refused:      41 attachment(s) by the attachment policy, 260.1 MB not stored (41 unresolved, 260.1 MB)
    not_allowlisted_extension    28 (28 unresolved)
    over_size_cap                 9 (9 unresolved)
    macro_office                  4 (4 unresolved)
```

### `comms refetch`

Widening the policy does **not** retroactively fetch anything. A sync notices the digest
changed, says so, and stops there. Fetching is one explicit command:

```sh
comms refetch --dry-run              # what would be fetched, and how many bytes
comms refetch                        # do it
comms refetch --source fastmail:fm   # same selector grammar as `comms sync`
comms refetch --reason svg_denied    # only what one config change unblocked
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

A full clean run advances the recorded policy digest, which is what stops `comms status`
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

## Noise triage

Most of what lands in a mailbox was written by nobody: notifications, digests,
promotions, monitoring alerts. Sync archives all of it, because deciding at archive time
what is worth keeping is exactly the kind of decision that is wrong once and lost forever.
So triage is a **separate pass**, over notes already on disk, and it has one rule of its
own:

> Noise is **moved**, never deleted. A note triage files as noise goes to `spam_root` at
> the identical `YYYY/MM/DD/<name>` path it had in the archive, with its attachment
> folder, and `comms untriage` brings it back. Sync is untouched.

The state database is the source of truth for where a note is. Every command that
resolves a note's path — `verify`, `refetch`, `untriage`, the daemon — asks the row which
tree it lives in, so a filed note is found under `spam_root`, never reported missing.

### What moves, what never moves

Classification runs in layers and stops at the first decisive one:

| Layer | What it looks at | Decides |
|---|---|---|
| 0 protect | A stored PDF/Office attachment; mail from one of your own addresses; the `[triage] protect_from` / `protect_subject` globs | signal, always |
| 1 headers | `Precedence: bulk`/`junk`, `Auto-Submitted`, `X-Auto-Response-Suppress`, Gmail's Promotions/Social categories | noise |
| 2 rules | `triage.toml`: every `[[keep]]` rule, then every `[[noise]]` rule, in file order | signal or noise |
| 3 llm | A local model, only if enabled, only for what layers 0–2 left undecided | signal or noise — or undecided |

**Undecided is never noise.** A note no layer decides stays exactly where it is; the
decision is recorded so `comms status` can say how much of the archive the rules do not
cover. A note is only ever in `spam_root` because a rule put it there, so a rules change
that no longer calls it noise brings it back on the next pass.

Layer 1 is deliberately narrow: `List-Id` and `List-Unsubscribe` are recorded but never
decisive on their own — a work mailing list carries both — and Gmail's Updates category is
where receipts land, so it decides nothing either. Those headers are available to the
rules (`list_id`) and to the model.

Rules match when **every** field a rule sets has a glob matching one of the note's values.
Globs are case-insensitive over the whole value: `*` matches anything, `?` one character,
and a pattern with no wildcard is an exact match. Keep rules are consulted before noise
rules, so your "keep" always beats your "noise"; the protect list sits above both, since a
`[[keep]]` rule cannot override the header layer but `protect_*` can.

```toml
[[keep]]
name    = "billing"
subject = ["*invoice*", "*receipt*"]

[[noise]]
name = "github-notifications"
from = ["notifications@github.com"]

[[noise]]
name          = "personal-newsletters"
subject       = ["*newsletter*"]
account_label = ["personal"]
```

Fields: `from`, `to` (also matches Cc), `subject`, `list_id`, `labels`, `account_label`.

### `comms triage`

```sh
comms triage --dry-run                     # every move, file by file, with rule and reason; moves nothing
comms triage                               # do it
comms triage --day 2026-08-07              # one day (in the archive's pinned zone); --since for a range
comms triage --source gmail:work           # same selector grammar as `comms sync`
comms triage --reclassify                  # re-evaluate notes already decided under these rules
comms triage --reclassify --only llm       # ...but only the model's decisions
comms triage --explain <path-to-note.md>   # what each layer made of one note
comms untriage <path-or-id>                # bring a note back; it is yours from then on
```

A pass looks at the notes not yet **settled** under the current *triage digest* — a hash
over everything that can change a verdict: the protect list, the header switch, every
rule in order, the model's identity. Edit a rule and the digest moves, so the next pass
re-evaluates everything; leave the rules alone and a pass over a triaged archive is a
no-op. `--reclassify` re-evaluates regardless.

Each move is two atomic renames — the `.md`, then its `.d/` attachment folder — followed
by the database update, in that order, so the database never claims a location the files
have not reached. A crash between the two leaves a state the next pass recognises and
finishes: the `.md`'s location wins, the folder follows it, then the row. `comms verify`
audits both trees and reports a note found in both, or in neither, or in the wrong one.

A note you bring back with `comms untriage` is recorded as kept **by hand** and is never
filed again by a later pass, whatever the rules say, until you ask for exactly that with
`comms triage --reclassify --only manual`.

### The model is last and optional

`[triage.llm]` is off by default, and when it is on it sees only what layers 0–2 could
not decide. It is given the header fields and a bounded excerpt of the body, between
markers the prompt declares untrusted data — the email cannot give it instructions, and
any marker inside the email is neutralised so the fence cannot be forged — and it must
answer with one strict JSON object. A timeout, a refused connection, prose instead of
JSON, anything that is not a verdict, counts as **undecided**, never as noise. Its
decisions carry the model name and prompt version, so a prompt change re-opens them and
`--reclassify --only llm` finds exactly them.

The daemon never calls it unless `in_daemon = true`. A stopped LM Studio cannot stall
the archiver.

### What sync persists for it

Only the archiver ever sees the raw message, so the headers triage keys on are captured
at archive time into the note's frontmatter (`headers:`), alongside the labels that were
always there. That changed the bytes of every note written since, so notes carry a
`render_version`: `2` means the headers were captured (and none present means the
message had none); a note without the key predates the capture, and the header layer
says nothing about it — the other layers decide. Existing notes are never rewritten for
this.

## Storing the archive in iCloud Drive / an Obsidian vault

Pointing `archive_root` at a folder inside an Obsidian vault in iCloud Drive works, and
it is a nice way to read the archive on a phone. It also changes what the folder *is* —
it stops being a plain directory and becomes something a background daemon rewrites,
uploads and empties behind your back. Everything below is the consequence of that.

```toml
archive_root = "~/Library/Mobile Documents/iCloud~md~obsidian/Documents/MyVault/Communication"
```

Spaces and the embedded `~` in `iCloud~md~obsidian` are fine — only a **leading** `~/` is
expanded, the rest of the path is taken literally. `comms doctor` prints a whole
`archive storage` section when it notices the root is inside `~/Library/Mobile Documents`
or `~/Library/CloudStorage`; read it.

Everything here applies to `spam_root` too — with one difference that is the point of
having it: it does not have to be in the vault at all. The default puts it beside
`archive_root` (a vault at `…/MyVault/Communication` gets `…/MyVault/spam`, which
Obsidian will index); setting `spam_root` to a plain folder on the same volume keeps the
noise out of the vault, out of iCloud and off your phone, while `comms untriage` still
brings any note back. Filenames are unchanged by a move, so the sanitizer's guarantees
hold in both trees.

**The state database must stay outside the synced tree.** This is the one hard error
`comms doctor` reports. `state.db` is a WAL-mode SQLite database: three files (`state.db`,
`state.db-wal`, `state.db-shm`) whose consistency is maintained by byte-range locks that
a sync agent knows nothing about. Uploading, evicting or restoring any one of them
independently corrupts the database, silently. The default location
(`~/.local/state/comms`, or `$XDG_STATE_HOME/comms`) is already outside; just don't move it
in. Deleting the database is always safe — the next sync re-enumerates and skips
everything already on disk.

**"Optimize Mac Storage" evicts your archive.** With it on (System Settings → *your name*
→ iCloud → iCloud Drive), macOS reclaims disk by throwing away the *contents* of files it
has already uploaded, leaving a placeholder. `ls` still shows the right size, but reading
one costs about a second while it downloads — and fails outright if you are offline.
Either turn the setting off, or right-click the archive folder in Finder and choose
**Keep Downloaded** to pin just that tree. `comms doctor` reads the current setting and
tells you which way it is.

**`comms verify` skips evicted files by default.** A verify pass hashes every file it has
ever written; on an evicted archive that is a full re-download of everything, at roughly a
second per file, onto a disk the system emptied on purpose. So verify stats each file
first, skips the ones whose bytes are not local, and reports the count:

```
checked 41203 emails, 512 chat day files, 8871 completed attachments; 0 orphan(s)
1874 file(s) skipped (evicted from local storage by iCloud) — their contents were not
checked; re-run with --materialize to download and hash them
```

`comms verify --materialize` does the full read-everything audit. Make sure the disk can
hold the entire archive before you use it. For belt and braces, the default pass also
pins its own process I/O policy so that an accidental read *cannot* trigger a download.

**Disk and quota.** A full backfill of two Gmail accounts with attachments runs to many
gigabytes, and every byte is charged twice: once to local disk and once to the iCloud
storage plan. Below roughly 20 GiB free, macOS starts evicting iCloud content to reclaim
space — which is exactly the state that turns a freshly written archive into placeholders.
`comms doctor` warns under that mark.

**Privacy.** Everything archived here leaves the machine for Apple's servers and comes
back down on every device signed into the same account. On those devices it lands with the
provider's own permissions (0644 files, 0755 directories): comms's local `0600`/`0700`
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
- Every filename comms generates is checked against iCloud's silent
  filename-exclusion list (`.nosync`, `.tmp`, `Dropbox`, `~$…`, `desktop.ini`, …) and
  rewritten if it matches — an attachment called `Q3 draft.tmp` is stored as
  `Q3 draft.tmp.bin`. Without that, the file would sit on disk, hash clean in `comms
  verify`, and never leave the machine.

## Autostart on macOS

```sh
cp docs/launchd/com.user.comms.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.user.comms.plist
```

Edit the plist's binary path first; it runs `comms run` with `KeepAlive`.

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
  `comms status` instead of wedging the sync forever.
- **No attachment is ever dropped silently.** Every one the policy refuses is written into
  its note *and* into a ledger row carrying the identity needed to fetch it later, so
  widening the policy is always recoverable and never automatic
  (see [Attachment safety](#attachment-safety)).
- **Noise is moved, never deleted, and never by sync.** Triage is a separate pass; a note
  it files keeps its path under `spam_root` and its attachment folder, the database says
  which tree every note is in, each move is atomic per note with a documented recovery
  for a crash between the renames and the row, and every decision — signal and undecided
  included — is recorded (see [Noise triage](#noise-triage)).
- **Accounts are isolated in state.** Cursors, the dedup index, the Chat store, the
  backfill queue, pending downloads and the failure ledger are all keyed by instance
  (`gmail:work`), so one account's backlog or poison item never affects another's, and a
  Gmail token is refused if it does not belong to the address its block names.
- Credentials, by contrast, are checked for *every* selected account up front: if one
  account is unauthorized, `comms sync` and `comms run` stop with that account's re-auth
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
them shares — one implementation, one `PolicyDigest`. `internal/triage` is the
noise classifier (rules, headers, the optional model) that `comms triage` runs as
a post-pass; the mover lives in `internal/archive`. See the plan in the repo
history for the full design.
