// Package gchat archives Google Chat: spaces, group chats and DMs are
// discovered each run, their messages land as canonical raw-JSON rows in the
// state store, uploaded attachments download out-of-band with retry, a
// spaceEvents pass folds in edits and deletions, and the per-day Markdown
// files are re-rendered as projections of the rows.
//
// One connector serves ONE account: it is constructed with that account's
// instance id ("gchat:<label>"), every state row it reads or writes is keyed
// by that id, and every file it writes carries the matching file tag
// ("gchat-<label>"). Two accounts that are members of the same space each
// archive their own independent copy — same space resource name, separate
// rows, separate files.
//
// Write ordering follows the archive-wide law: files land before the DB rows
// that acknowledge them commit, and the per-space message cursor advances
// only inside the same transaction as the rows it covers
// (state.ApplyChatPage). The failures ledger records only deterministic
// item-specific failures (malformed message data, a vanished item);
// environmental trouble — context cancellation, broken auth, quota
// exhaustion or network/server failures surviving retries, state-store or
// archive-write errors — aborts the run with cursors intact, so a flaky
// environment can never poison an item toward the skip threshold.
package gchat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	chat "google.golang.org/api/chat/v1"
	drive "google.golang.org/api/drive/v3"

	"comms/internal/archive"
	"comms/internal/config"
	"comms/internal/naming"
	"comms/internal/policy"
	"comms/internal/ratelimit"
	"comms/internal/retry"
	"comms/internal/source"
	"comms/internal/state"
)

// limiterGlobalKey is the limiter key for calls not tied to one space
// (spaces.list); per-space calls use the space resource name.
const limiterGlobalKey = "_global"

// Connector implements source.Source for Google Chat, for exactly one
// configured account.
type Connector struct {
	src     string               // instance id "gchat:<label>": the state source key
	tag     string               // file tag "gchat-<label>": the stem's source component
	acct    config.GoogleAccount // this account's options (mirror_drive_files, show_deleted)
	db      *state.DB
	writer  *archive.Writer
	limiter *ratelimit.Keyed
	log     *slog.Logger
	api     chatAPI
	drive   driveAPI  // nil unless mirror_drive_files is on
	people  peopleAPI // nil unless resolve_chat_names is on

	// pol decides every attachment, twice: once from the message metadata
	// (avoiding the download) and once over the real bytes. Never nil.
	pol       *policy.Policy
	freeSpace func(root string) int64

	// runBytes is the attachment bytes stored by the current Sync, charged
	// against attachments.run_budget. Reset at the top of Sync; a plain field
	// because one Connector serves one account and downloadAttachments is
	// sequential.
	runBytes int64

	retryOpts retry.Options    // zero value = retry defaults; tests shrink it
	now       func() time.Time // test seam
}

var _ source.Source = (*Connector)(nil)

// New returns the Chat connector for one account. instanceID is that
// account's chat instance id (state.InstanceID(state.SourceGChat,
// acct.Label)) — it keys every state row and, as state.Tag(instanceID),
// names every file. limiter should allow ~10 req/s per space (the documented
// cap is 15/s per space, shared with other Chat apps) plus an overall
// ceiling; svc must be built on THIS account's authorized token source with
// the chat.spaces.readonly, chat.messages.readonly and
// chat.memberships.readonly scopes. driveSvc is required when the account's
// MirrorDriveFiles is on (its token then also carries drive.readonly) and
// must be nil otherwise.
// Pass WithPolicy(cfg.Policy()) so the operator's [attachments] block takes
// effect; without it the connector applies policy.Default().
func New(instanceID string, acct config.GoogleAccount, db *state.DB, writer *archive.Writer, limiter *ratelimit.Keyed, logger *slog.Logger, svc *chat.Service, driveSvc *drive.Service, opts ...Option) *Connector {
	var d driveAPI
	if driveSvc != nil {
		d = &realDriveAPI{svc: driveSvc}
	}
	return newConnector(instanceID, acct, db, writer, limiter, logger, &realAPI{svc: svc}, d, opts...)
}

func newConnector(instanceID string, acct config.GoogleAccount, db *state.DB, writer *archive.Writer, limiter *ratelimit.Keyed, logger *slog.Logger, api chatAPI, driveAPI driveAPI, opts ...Option) *Connector {
	if logger == nil {
		logger = slog.Default()
	}
	o := newOptions(opts)
	return &Connector{
		src:     instanceID,
		tag:     state.Tag(instanceID),
		acct:    acct,
		db:      db,
		writer:  writer,
		limiter: limiter,
		// Every line names the account: several Chat connectors log into the
		// same stream.
		log:       logger.With("instance", instanceID),
		api:       api,
		drive:     driveAPI,
		people:    o.people,
		pol:       o.pol,
		freeSpace: o.freeSpace,
		now:       time.Now,
	}
}

// Name implements source.Source: the instance id, which is also this
// account's key in the cursors, failures and runs tables.
func (c *Connector) Name() string { return c.src }

// Check implements source.Source: a cheap auth probe (spaces.list pageSize
// 1) that mutates nothing.
func (c *Connector) Check(ctx context.Context) error {
	if err := c.api.probeSpaces(ctx); err != nil {
		return fmt.Errorf("gchat: %w", err)
	}
	return nil
}

// Sync implements source.Source. One pass: discover spaces (frozen slugs,
// history-off warnings), per space refresh the member cache and ingest new
// messages page by page (each page one atomic commit that also advances the
// cursor), then the attachment retry pass, then the spaceEvents pass for
// edits/deletions, and finally re-render every dirty day file.
func (c *Connector) Sync(ctx context.Context) error {
	// attachments.run_budget is per pass, so the tally starts fresh here.
	c.runBytes = 0
	if _, err := c.db.SyncChatNameOverrides(c.src, c.acct.ChatNameOverrides); err != nil {
		return storeFatal(err)
	}
	spaces, err := c.discoverSpaces(ctx)
	if err != nil {
		return fmt.Errorf("gchat: discover spaces: %w", err)
	}
	for _, sp := range spaces {
		sp := sp
		if err := c.runItemStep(ctx, sp.Name, func() error { return c.syncSpace(ctx, sp) }); err != nil {
			return err
		}
	}
	if err := c.refreshSenderNames(ctx); err != nil {
		return storeFatal(err)
	}
	if err := c.downloadAttachments(ctx); err != nil {
		return err
	}
	for _, sp := range spaces {
		sp := sp
		if err := c.runItemStep(ctx, sp.Name+"/events", func() error { return c.syncSpaceEvents(ctx, sp) }); err != nil {
			return err
		}
	}
	return c.renderDirtyDays(ctx)
}

// runItemStep runs one per-item step under the poison-item protocol: ids
// past the skip threshold are not retried, failures are recorded but never
// abort the run, successes clear the ledger, and only source-level errors
// propagate.
func (c *Connector) runItemStep(ctx context.Context, id string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	skipped, err := c.db.IsSkipped(c.src, id)
	if err != nil {
		return err
	}
	if skipped {
		c.log.Warn("gchat: skipping item past the failure threshold", "id", id)
		return nil
	}
	if err := fn(); err != nil {
		if isRunFatal(ctx, err) {
			return err
		}
		attempts, rerr := c.db.RecordFailure(c.src, id, err.Error())
		if rerr != nil {
			return rerr
		}
		c.log.Error("gchat: item failed; run continues", "id", id, "attempts", attempts, "err", err)
		return nil
	}
	return c.db.ClearFailure(c.src, id)
}

// errStore marks state-store failures (SQLite I/O trouble, a full disk):
// environmental errors that must abort the run with cursors intact, never be
// recorded as item poison against a space.
var errStore = errors.New("gchat: state store failure")

// storeFatal wraps a state-store error so isRunFatal aborts the run on it;
// storeFatal(nil) is nil.
func storeFatal(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", errStore, err)
}

// isRunFatal reports whether err must abort the whole run rather than be
// absorbed as an item failure: cancellation, a state-store failure, broken
// auth, rate limiting that survived retry.Do's backoff (quota exhaustion),
// or a network/server failure that survived it too. An environment problem
// would fail every subsequent item the same way — recording it would poison
// healthy items toward the skip threshold, so the run aborts and the next
// scheduled pass resumes from the unadvanced cursors (matching gmail's
// classifyItemErr and fastmail's runFatal).
func isRunFatal(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, errStore) {
		return true
	}
	switch retry.Classify(err) {
	case retry.AuthBroken, retry.RateLimited, retry.Transient:
		return true
	}
	return false
}

// discoverSpaces pages spaces.list, upserts every space (the display slug
// and first_seen_at freeze on first sight inside UpsertSpace), warns once
// per run about history-off spaces, and returns the frozen identities in
// deterministic order. On an empty state DB over a non-empty archive the
// identities already frozen in on-disk day-file names are reclaimed (see
// reclaimStems).
func (c *Connector) discoverSpaces(ctx context.Context) ([]spaceMeta, error) {
	reclaim, err := c.reclaimStems()
	if err != nil {
		return nil, err
	}
	var out []spaceMeta
	seen := make(map[string]bool)
	token := ""
	for {
		if err := c.limiter.Wait(ctx, limiterGlobalKey); err != nil {
			return nil, err
		}
		var resp *chat.ListSpacesResponse
		if err := retry.Do(ctx, c.retryOpts, func() error {
			r, err := c.api.listSpaces(ctx, token)
			if err != nil {
				return err
			}
			resp = r
			return nil
		}); err != nil {
			return nil, err
		}
		for _, s := range resp.Spaces {
			if s == nil || s.Name == "" || seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			typ := spaceTypeOf(s.SpaceType)
			slug := naming.Slug(s.DisplayName, slugFallback(typ))
			// Only THIS account's files are reclaimable: the key carries the
			// file tag, so a space shared with another configured account
			// never hands its frozen identity across accounts.
			if info, ok := reclaim[archive.ChatStemKey{Tag: c.tag, Hash8: naming.Hash8(s.Name)}]; ok {
				// Fresh DB over an existing archive: freeze the identity the
				// on-disk day files already encode, not one derived from the
				// space's CURRENT display name — a rename since first archive
				// must not split the conversation into a second file series.
				typ, slug = info.SpaceType, info.Slug
			}
			if err := c.db.UpsertSpace(c.src, s.Name, typ, s.DisplayName, slug, s.SpaceHistoryState); err != nil {
				return nil, err
			}
			stored, ok, err := c.db.GetSpace(c.src, s.Name)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("space %q vanished after upsert", s.Name)
			}
			if s.SpaceHistoryState == "HISTORY_OFF" {
				c.log.Warn("gchat: history is off for this space — messages older than 24h are unrecoverable if the archiver falls behind",
					"space", s.Name, "display_name", s.DisplayName)
			}
			out = append(out, spaceMeta{Name: stored.Name, Type: stored.Type, Slug: stored.DisplaySlug})
		}
		token = resp.NextPageToken
		if token == "" {
			break
		}
	}
	slices.SortFunc(out, func(a, b spaceMeta) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// reclaimStems maps (file tag, naming.Hash8(space name)) to the on-disk chat
// identity when THIS account holds no chat spaces in the state DB but the
// archive holds day files — the recovery path where state.db was deleted and
// re-pinned from scratch. The slug and space type frozen in existing day-file
// names are reclaimed so re-pinned spaces keep extending the same file series
// instead of starting a second one under a freshly derived slug. With any
// space already registered for this account the DB is authoritative and no
// scan happens. The scan covers every account's files; discoverSpaces looks
// up only this account's tag.
func (c *Connector) reclaimStems() (map[archive.ChatStemKey]archive.ChatStemInfo, error) {
	has, err := c.db.HasChatSpaces(c.src)
	if err != nil {
		return nil, storeFatal(err)
	}
	if has {
		return nil, nil
	}
	stems, err := archive.ScanChatStems(c.writer.Root)
	if err != nil {
		return nil, err
	}
	return stems, nil
}

// syncSpace refreshes the member cache (tolerating failure) and ingests the
// space's new messages: createTime-ascending pages filtered from the cursor
// minus the 1s rewind, each page committed atomically with its dirty-day
// marks and cursor advance — restartable mid-space. A message that fails to
// map (and is not yet skip-listed) freezes the cursor at the last good
// message before it and ends this space's pass gracefully: the next run
// re-lists it, its ledger entry grows once per run, and past the skip
// threshold it is passed over for good.
func (c *Connector) syncSpace(ctx context.Context, sp spaceMeta) error {
	c.refreshMembers(ctx, sp)

	cursor, err := c.cursorTime(sp.Name, state.CursorMsgCreateTime)
	if err != nil {
		return storeFatal(err)
	}
	filter := listFilter(cursor)
	prev := cursor
	token := ""
	for {
		if err := c.limiter.Wait(ctx, sp.Name); err != nil {
			return err
		}
		var resp *chat.ListMessagesResponse
		if err := retry.Do(ctx, c.retryOpts, func() error {
			r, err := c.api.listMessages(ctx, sp.Name, filter, token, c.acct.ShowDeleted)
			if err != nil {
				return err
			}
			resp = r
			return nil
		}); err != nil {
			return fmt.Errorf("list messages: %w", err)
		}
		page, skips, maxCreate, errs, stopped := buildPage(c.src, sp, resp.Messages, c.writer.TZ, prev, c.acct.MirrorDriveFiles, c.pol, c.msgSkipped)
		for _, ie := range errs {
			c.recordItemFailure(ie)
		}
		if c.acct.ShowDeleted {
			c.preserveDeletedRaw(sp.Name, page.Messages)
		}
		// The refusals land BEFORE the page: ApplyChatPage advances the
		// cursor past these messages in the same transaction, so a crash
		// between the two must lose the acknowledgement (the next pass
		// re-lists and re-records, idempotently), never the record of a blob
		// that will now never be fetched. That ordering is the never-drop
		// rule applied to a cursor instead of to a file.
		if err := c.recordSkips(skips); err != nil {
			return err
		}
		if len(page.Messages) > 0 || len(page.Attachments) > 0 || page.Cursor != "" {
			if err := c.db.ApplyChatPage(ctx, page); err != nil {
				return storeFatal(err)
			}
		}
		if stopped {
			c.log.Warn("gchat: message failed to map; space pass stops at the last good message so the next run retries it",
				"space", sp.Name)
			return nil
		}
		prev = maxCreate
		token = resp.NextPageToken
		if token == "" {
			return nil
		}
	}
}

// msgSkipped reports whether a message resource name is past the poison
// threshold. A ledger read error errs on the side of retrying (not skipped):
// the message is already failing deterministically when this is consulted,
// so at worst its failure is recorded once more.
func (c *Connector) msgSkipped(id string) bool {
	skipped, err := c.db.IsSkipped(c.src, id)
	if err != nil {
		c.log.Warn("gchat: skip-list lookup failed", "id", id, "err", err)
		return false
	}
	return skipped
}

// refreshMembers repopulates the space's member-name cache, once per space
// per run. Failures are tolerated: rendering falls back to opaque user ids
// and the next run retries. A late cache fill (or a rename) marks every day
// the sender appears on dirty, so files already rendered with opaque ids
// re-render with the resolved name on this run's render pass.
func (c *Connector) refreshMembers(ctx context.Context, sp spaceMeta) {
	token := ""
	for {
		if err := c.limiter.Wait(ctx, sp.Name); err != nil {
			return
		}
		var resp *chat.ListMembershipsResponse
		if err := retry.Do(ctx, c.retryOpts, func() error {
			r, err := c.api.listMembers(ctx, sp.Name, token)
			if err != nil {
				return err
			}
			resp = r
			return nil
		}); err != nil {
			c.log.Warn("gchat: member refresh failed; senders may render as opaque ids", "space", sp.Name, "err", err)
			return
		}
		for _, m := range resp.Memberships {
			if m == nil || m.Member == nil || m.Member.Name == "" {
				continue
			}
			prev, _, gerr := c.db.GetMember(c.src, sp.Name, m.Member.Name)
			if gerr != nil {
				c.log.Warn("gchat: member cache read failed", "space", sp.Name, "user", m.Member.Name, "err", gerr)
				return
			}
			if prev != m.Member.DisplayName {
				// Dirty the days BEFORE the cache write: if marking fails,
				// the name difference is still detectable next run — the
				// reverse order would cache the name with the files stale
				// forever.
				days, derr := c.db.DaysWithSender(c.src, sp.Name, m.Member.Name)
				if derr != nil {
					c.log.Warn("gchat: sender day lookup failed", "space", sp.Name, "user", m.Member.Name, "err", derr)
					return
				}
				for _, day := range days {
					if err := c.db.MarkDayDirty(c.src, sp.Name, day); err != nil {
						c.log.Warn("gchat: day dirty mark failed", "space", sp.Name, "day", day, "err", err)
						return
					}
				}
			}
			if err := c.db.SetMember(c.src, sp.Name, m.Member.Name, m.Member.DisplayName); err != nil {
				c.log.Warn("gchat: member cache write failed", "space", sp.Name, "err", err)
				return
			}
		}
		token = resp.NextPageToken
		if token == "" {
			return
		}
	}
}

// preserveDeletedRaw keeps the archived content of messages that come back
// as deleted stubs in a showDeleted listing: a stub's contentless raw_json
// must not clobber the last content we archived.
func (c *Connector) preserveDeletedRaw(space string, rows []state.ChatMessage) {
	for i := range rows {
		if !rows[i].Deleted {
			continue
		}
		existing, ok := c.lookupRow(space, rows[i].DayBucket, rows[i].Name)
		if !ok {
			continue
		}
		rows[i].RawJSON = existing.RawJSON
		if rows[i].SenderID == "" {
			rows[i].SenderID = existing.SenderID
		}
		if rows[i].Thread == "" {
			rows[i].Thread = existing.Thread
		}
	}
}

// lookupRow finds one of this account's already-archived message rows by
// (space, day, name).
func (c *Connector) lookupRow(space, day, name string) (state.ChatMessage, bool) {
	msgs, err := c.db.MessagesForDay(c.src, space, day)
	if err != nil {
		c.log.Warn("gchat: row lookup failed", "space", space, "day", day, "err", err)
		return state.ChatMessage{}, false
	}
	for _, m := range msgs {
		if m.Name == name {
			return m, true
		}
	}
	return state.ChatMessage{}, false
}

// cursorTime loads and parses a per-space cursor; an unreadable value logs
// and falls back to zero (full re-list — primary-key dedup absorbs it).
func (c *Connector) cursorTime(space, kind string) (time.Time, error) {
	v, ok, err := c.db.GetCursor(c.src, space, kind)
	if err != nil || !ok {
		return time.Time{}, err
	}
	t, perr := parseCursor(v)
	if perr != nil {
		c.log.Warn("gchat: unreadable cursor; re-listing from scratch (dedup absorbs the replay)",
			"space", space, "kind", kind, "value", v, "err", perr)
		return time.Time{}, nil
	}
	return t, nil
}

// recordSkips makes the policy's refusals durable. A policy skip is NOT an
// item failure: it never touches the failures ledger and never holds a
// cursor. A store error IS run-fatal (storeFatal) — losing the record is the
// one outcome the never-drop rule does not allow.
func (c *Connector) recordSkips(rows []state.SkippedAttachment) error {
	if len(rows) == 0 {
		return nil
	}
	if err := c.db.UpsertSkippedBatch(rows); err != nil {
		return storeFatal(err)
	}
	for _, r := range rows {
		c.log.Warn("gchat: attachment not downloaded",
			"message", r.StableID, "part", r.PartKey, "name", r.SanitizedName,
			"declared", r.DeclaredType, "reason", r.Reason)
	}
	return nil
}

func (c *Connector) recordItemFailure(ie itemError) {
	attempts, err := c.db.RecordFailure(c.src, ie.ID, ie.Err.Error())
	if err != nil {
		c.log.Error("gchat: failure ledger write failed", "id", ie.ID, "err", err)
		return
	}
	c.log.Warn("gchat: message skipped", "id", ie.ID, "attempts", attempts, "err", ie.Err)
}
