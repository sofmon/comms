# comms — notes for Claude Code / contributors

Single-binary Go archiver: Gmail + Google Chat (Workspace) + FastMail (JMAP) → one
merged local Markdown tree. See `README.md` for usage; the full design spec and
operator-specific state live outside this repo — **if `../SAVE-HANDOVER.md` exists,
read it before starting any work.**

## Build & test

```sh
go build ./... && go vet ./... && go test -count=1 ./...
gofmt -l .                        # must print nothing
GOOS=linux go build ./...        # must pass (no cgo; darwin code is build-tagged)
go test -race ./internal/state/ ./internal/daemon/ ./internal/source/gchat/
```

Never run `go mod tidy`/`go get` casually: `git.sr.ht/~rockorager/go-jmap` is pinned
pre-1.0, and `modernc.org/libc` must stay at the version in `modernc.org/sqlite`'s
go.mod.

## Identifiers

- Instance id `gmail:<label>` — keys ALL state (cursors, messages, chat tables).
- File tag `gmail-<label>` — appears in EVERY filename.
- Never mix them: a colon never reaches a filename; a tag is never a state key.
- `disposition` (state.Disposition: which tree a NOTE is in) is not `archive.SkipDisposition`
  (what became of a refused ATTACHMENT's bytes); the spec chose both names, keep them apart.
- Account labels are PERMANENT (part of filenames + state); renaming one orphans that
  account's archive. Filename hashes derive from instance ids — tests must COMPUTE
  expected names via `internal/naming` (`EmailStem`/`ChatStem`/`Hash8`), never assert
  hand-copied hex.

## Invariants (do not break; each has regression tests)

1. Write ordering: files first (atomic via `<root>/.tmp` + rename), DB commit second,
   cursor last. Interruption anywhere must stay safe.
2. Deterministic naming from immutable fields; casefold+NFC collision detection (APFS);
   chat slugs and space_type frozen at first sight; slug reclaim on empty DB.
3. Chat day files are projections of SQLite rows under the `dirty_seq` protocol
   (`MarkDayRendered` clears only on a matching sequence).
4. Error taxonomy: transient/DB/write errors abort the pass with cursors unadvanced;
   only item-specific errors enter the failures ledger (skip after 5); attachment-policy
   skips are never failures.
5. Never drop silently: refused attachments are recorded in the note AND in
   `skipped_attachments` with the policy digest; `comms refetch` (explicit only)
   recovers them after a policy widening.
6. `mimetype.SetLimit(0)` (policy package init) must stay — the 4 KB default window
   misdetects real docx/xlsx as zip and would silently skip them.
7. Never decompress archives. Single documented carve-out: reading zip
   central-directory NAMES for `vbaProject.bin` (macro-Office detection).
8. Quarantine xattr = consent prompt + Office Protected View. It is NOT malware
   scanning (XProtect has no document signatures) — keep docs honest.
9. iCloud safety: the sanitizer guarantees no produced filename matches iCloud's
   silent sync-exclusion list; `verify` skips evicted (SF_DATALESS) files unless
   `--materialize`; the state DB must stay outside any synced folder; the daemon is a
   user LaunchAgent, not a LaunchDaemon.
10. Timezone is pinned (state meta + written back into config); mismatch refuses start.
11. Noise triage is a POST-PASS (`internal/triage`, `comms triage`): never call the
    classifier from a connector or the writer; sync archives everything first.
12. The DB is the source of truth for a note's location: `messages.disposition`
    (`archive`|`spam`) says which root `rel_path` is relative to, and EVERY absolute path
    built from a rel path goes through `archive.Writer.RootFor`/`NotePath`. `WriteEmail`
    accepts only the archive tree; `RewriteEmail` follows the row's disposition; `verify`
    audits both trees. `spam_root` is never inside `archive_root` nor around it.
13. Noise is moved, never deleted. `Writer.MoveNote` renames the `.md`, then the `.d/`,
    and the caller updates the row AFTER (`state.DB.SetDisposition` is the only writer
    of the triage columns; `CommitMessage` never touches them). Recovery rule for a crash
    in between: the `.md`'s location wins, the `.d/` follows, then the row. A note in
    both trees or neither is refused and reported, never guessed.
14. Undecided is never noise. Layers stop at the first decisive one (protect → headers →
    rules → llm); every outcome is a `triage_decisions` row; a model failure is undecided
    AND transient (not settled under the digest, retried next pass). The daemon never
    calls the model unless `[triage.llm] in_daemon = true`. `comms untriage` decisions
    (`disposition_rule = "manual"`) are never re-filed automatically.
15. `render_version` in email frontmatter is a layout version: bump `archive.EmailRenderVersion`
    when the frontmatter shape changes; existing notes are never rewritten for it, and
    triage treats "no headers on a v1 note" as "nobody looked", not "none present".

## Style

Match existing code. Table-driven tests with `t.TempDir()`; no real home paths, no
real email addresses or account names anywhere — examples use `you@example.com` /
`you@example.net` and labels `work` / `personal` / `fm`. Do not weaken or delete a
test to make a change pass.
