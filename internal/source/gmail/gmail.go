// Package gmail archives a Gmail mailbox through the Gmail REST API.
//
// One connector serves ONE account: it is constructed with that account's
// instance id ("gmail:<label>"), which keys every cursor, queue row, message
// row and failure it writes, and whose file tag ("gmail-<label>") names the
// account in every file it archives. Two accounts therefore share nothing
// but the state DB and the archive tree — including message ids, which Gmail
// does not make unique across mailboxes.
//
// Backfill snapshots the mailbox historyId BEFORE enumerating (so nothing
// arriving mid-backfill falls into a cursor gap), pages messages.list into
// the persistent gmail_backfill queue, and drains it with parallel workers
// fetching format=raw; when the queue empties the snapshot is promoted to
// the incremental history_id cursor. Incremental runs replay history.list.
// Write ordering is the law everywhere: files first, DB commit second,
// cursor advance only after the batch it covers is durable. Item failures
// go to the state failures ledger and never abort a run; after
// state.SkipThreshold recorded failures an item is skipped, so a poison
// message can never wedge a cursor.
package gmail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhillyerd/enmime/v2"
	gmailv1 "google.golang.org/api/gmail/v1"

	"save/internal/archive"
	"save/internal/config"
	"save/internal/emailpipe"
	"save/internal/naming"
	"save/internal/policy"
	"save/internal/ratelimit"
	"save/internal/retry"
	"save/internal/source"
	"save/internal/state"
)

// Cursor kinds under (source=<instance id>, scope="").
const (
	// cursorBootstrap holds the getProfile historyId snapshot taken before
	// backfill enumeration; promoted to cursorHistory when the queue drains.
	cursorBootstrap = "bootstrap_history_id"
	// cursorPageToken is the persisted messages.list page token (transient
	// server-side; a rejected one is cleared and enumeration restarts).
	cursorPageToken = "backfill_page_token"
	// cursorListDone marks enumeration complete so a restart resumes
	// straight into draining the queue.
	cursorListDone = "backfill_list_done"
	// cursorHistory is the incremental history.list cursor.
	cursorHistory = "history_id"
)

// workers is the number of parallel messages.get fetchers; combined with
// the shared quota-unit limiter this sustains ~4-5 msgs/s under the
// 6,000 units/min/user cap.
const workers = 4

// backfillBatch is how many pending queue ids are claimed per drain round.
const backfillBatch = 100

// Source is one account's Gmail connector. Construct with New (live API) or
// newSource (tests).
type Source struct {
	// id is this account's instance id ("gmail:work"): the source key of
	// every row this connector writes. tag is its filename rendering
	// ("gmail-work") and is the only form allowed to reach a path.
	id      string
	tag     string
	acct    config.GoogleAccount
	db      *state.DB
	writer  *archive.Writer
	limiter *ratelimit.Units
	log     *slog.Logger
	api     api

	// pol decides every attachment; never nil (policy.Default() when the
	// caller passed none). freeSpace probes the archive volume for the
	// free-space floor.
	pol       *policy.Policy
	freeSpace func(root string) int64
}

var _ source.Source = (*Source)(nil)

// newSource builds the connector for one account. instanceID must be the
// Gmail instance id of acct — a mismatch would file this account's archive
// under another account's key, so it is rejected up front.
func newSource(instanceID string, acct config.GoogleAccount, db *state.DB, writer *archive.Writer, limiter *ratelimit.Units, logger *slog.Logger, a api, opts ...Option) (*Source, error) {
	if want := state.InstanceID(state.SourceGmail, acct.Label); instanceID != want {
		return nil, fmt.Errorf("gmail: instance id %q does not belong to account %q (want %q)", instanceID, acct.Label, want)
	}
	if _, _, ok := state.SplitInstance(instanceID); !ok {
		return nil, fmt.Errorf("gmail: %q is not an instance id — use state.InstanceID(state.SourceGmail, label)", instanceID)
	}
	o := newOptions(opts)
	return &Source{
		id:        instanceID,
		tag:       state.Tag(instanceID),
		acct:      acct,
		db:        db,
		writer:    writer,
		limiter:   limiter,
		log:       logger.With("source", instanceID),
		api:       a,
		pol:       o.pol,
		freeSpace: o.freeSpace,
	}, nil
}

// Name implements source.Source: the instance id, which is what StartRun,
// the cursors and the failures ledger are keyed by.
func (s *Source) Name() string { return s.id }

// Check probes auth with users.getProfile and verifies the token belongs
// to this account. It mutates nothing.
func (s *Source) Check(ctx context.Context) error {
	if err := s.limiter.Wait(ctx, costGetProfile); err != nil {
		return err
	}
	p, err := s.api.GetProfile(ctx)
	if err != nil {
		return fmt.Errorf("gmail: %s: getProfile: %w", s.id, err)
	}
	// Per account: a token swapped between two configured accounts would
	// silently archive one mailbox under the other's label.
	if s.acct.Account != "" && !strings.EqualFold(p.EmailAddress, s.acct.Account) {
		return fmt.Errorf("gmail: %s: token is for %s but config says %s", s.id, p.EmailAddress, s.acct.Account)
	}
	return nil
}

// Sync implements source.Source: incremental when a history cursor exists,
// backfill otherwise. A gone (or corrupt) history cursor falls back to
// backfill enumeration, which skips already-archived ids.
func (s *Source) Sync(ctx context.Context) error {
	labels, err := s.loadLabels(ctx)
	if err != nil {
		return err
	}

	hidStr, ok, err := s.db.GetCursor(s.id, "", cursorHistory)
	if err != nil {
		return err
	}
	if ok {
		hid, perr := strconv.ParseUint(hidStr, 10, 64)
		if perr == nil {
			err := s.incremental(ctx, hid, labels)
			if err == nil || retry.Classify(err) != retry.CursorGone {
				return err
			}
			s.log.Warn("history cursor expired; falling back to full re-enumeration", "start_history_id", hid, "err", err)
		} else {
			s.log.Warn("history cursor is corrupt; falling back to full re-enumeration", "value", hidStr)
		}
		if err := s.resetForBackfill(); err != nil {
			return err
		}
	}
	return s.backfill(ctx, labels)
}

// resetForBackfill clears the incremental cursor plus any stale backfill
// state so the CursorGone fallback re-enumerates from scratch. The queue
// is cleared too: stale 'done' rows from an earlier backfill would
// otherwise mask ids that must be reconsidered (INSERT OR IGNORE keeps the
// old row state).
func (s *Source) resetForBackfill() error {
	for _, kind := range []string{cursorHistory, cursorBootstrap, cursorListDone, cursorPageToken} {
		if err := s.db.DeleteCursor(s.id, "", kind); err != nil {
			return err
		}
	}
	return s.db.ClearBackfill(s.id)
}

// loadLabels fetches the label id→name map once per run. Label names go
// into frontmatter; scope decisions use the ids.
func (s *Source) loadLabels(ctx context.Context) (map[string]string, error) {
	var resp *gmailv1.ListLabelsResponse
	err := retry.Do(ctx, retry.Options{}, func() error {
		if err := s.limiter.Wait(ctx, costLabels); err != nil {
			return err
		}
		r, err := s.api.ListLabels(ctx)
		if err == nil {
			resp = r
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("gmail: labels.list: %w", err)
	}
	m := make(map[string]string, len(resp.Labels))
	for _, l := range resp.Labels {
		if l != nil && l.Id != "" {
			m[l.Id] = l.Name
		}
	}
	return m, nil
}

// labelNames maps label ids to display names, falling back to the raw id
// for labels deleted since the map was cached.
func labelNames(labels map[string]string, ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		if name, ok := labels[id]; ok && name != "" {
			out[i] = name
		} else {
			out[i] = id
		}
	}
	return out
}

// --- backfill ---

func (s *Source) backfill(ctx context.Context, labels map[string]string) error {
	// Snapshot the history id BEFORE listing; messages arriving during the
	// (possibly hours-long) backfill then overlap the first incremental run
	// instead of falling into a gap. An existing snapshot means we are
	// resuming an interrupted backfill — keep it.
	if _, ok, err := s.db.GetCursor(s.id, "", cursorBootstrap); err != nil {
		return err
	} else if !ok {
		var p *gmailv1.Profile
		err := retry.Do(ctx, retry.Options{}, func() error {
			if err := s.limiter.Wait(ctx, costGetProfile); err != nil {
				return err
			}
			r, err := s.api.GetProfile(ctx)
			if err == nil {
				p = r
			}
			return err
		})
		if err != nil {
			return fmt.Errorf("gmail: getProfile snapshot: %w", err)
		}
		if err := s.db.SetCursor(s.id, "", cursorBootstrap, strconv.FormatUint(p.HistoryId, 10)); err != nil {
			return err
		}
		s.log.Info("backfill starting", "history_id_snapshot", p.HistoryId)
	}

	if err := s.enumerate(ctx); err != nil {
		return err
	}

	stats, err := s.drain(ctx, labels)
	if err != nil {
		return err
	}
	pending, err := s.db.BackfillPendingCount(s.id)
	if err != nil {
		return err
	}
	if !shouldPromote(true, pending) {
		s.log.Warn("backfill incomplete: items failed this run and stay queued for the next",
			"pending", pending, "archived", stats.archived)
		return nil
	}

	// Promote: the snapshot becomes the incremental cursor, then backfill
	// state is torn down. Crash between the two steps is safe — with
	// history_id set the next run goes incremental and never re-reads the
	// leftovers (resetForBackfill clears them if a fallback ever recurs).
	hid, ok, err := s.db.GetCursor(s.id, "", cursorBootstrap)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("gmail: backfill drained but bootstrap history id is missing")
	}
	if err := s.db.SetCursor(s.id, "", cursorHistory, hid); err != nil {
		return err
	}
	for _, kind := range []string{cursorBootstrap, cursorListDone, cursorPageToken} {
		if err := s.db.DeleteCursor(s.id, "", kind); err != nil {
			return err
		}
	}
	if err := s.db.ClearBackfill(s.id); err != nil {
		return err
	}
	s.log.Info("backfill complete", "archived", stats.archived, "history_id", hid)
	return nil
}

// enumerate pages messages.list (default Spam/Trash exclusion) into the
// backfill queue, persisting the page token after each committed page. A
// rejected persisted token is cleared and enumeration restarts —
// INSERT OR IGNORE absorbs the replayed pages.
func (s *Source) enumerate(ctx context.Context) error {
	if _, done, err := s.db.GetCursor(s.id, "", cursorListDone); err != nil {
		return err
	} else if done {
		return nil
	}
	token, _, err := s.db.GetCursor(s.id, "", cursorPageToken)
	if err != nil {
		return err
	}
	restarted := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var resp *gmailv1.ListMessagesResponse
		listErr := retry.Do(ctx, retry.Options{}, func() error {
			if err := s.limiter.Wait(ctx, costList); err != nil {
				return err
			}
			r, err := s.api.ListMessages(ctx, token)
			if err == nil {
				resp = r
			}
			return err
		})
		if listErr != nil {
			if token != "" && !restarted && isBadPageToken(listErr) {
				s.log.Warn("persisted list page token rejected; restarting enumeration", "err", listErr)
				if err := s.db.DeleteCursor(s.id, "", cursorPageToken); err != nil {
					return err
				}
				token = ""
				restarted = true
				continue
			}
			return fmt.Errorf("gmail: messages.list: %w", listErr)
		}

		ids := make([]string, 0, len(resp.Messages))
		for _, m := range resp.Messages {
			if m == nil || m.Id == "" {
				continue
			}
			seen, err := s.db.SeenMessage(s.id, m.Id)
			if err != nil {
				return err
			}
			if !seen {
				ids = append(ids, m.Id)
			}
		}
		if err := s.db.EnqueueBackfill(s.id, ids); err != nil {
			return err
		}

		if resp.NextPageToken == "" {
			if err := s.db.SetCursor(s.id, "", cursorListDone, "1"); err != nil {
				return err
			}
			return s.db.DeleteCursor(s.id, "", cursorPageToken)
		}
		token = resp.NextPageToken
		if err := s.db.SetCursor(s.id, "", cursorPageToken, token); err != nil {
			return err
		}
	}
}

// runStats tracks per-run item accounting across workers.
type runStats struct {
	mu       sync.Mutex
	failed   map[string]bool // items that failed this run and are below the skip threshold
	archived int

	// attachBytes is the attachment bytes this pass has stored, charged
	// against attachments.run_budget. Fetch workers run in parallel, so it is
	// read and advanced under the same mutex as everything else here; the
	// budget is therefore approximate under concurrency (a worker may decide
	// against a slightly stale total) but never exceeded by more than the
	// parts in flight.
	attachBytes int64
}

func newRunStats() *runStats { return &runStats{failed: make(map[string]bool)} }

// addAttachBytes charges stored attachment bytes against the run budget and
// returns the new total.
func (r *runStats) addAttachBytes(n int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attachBytes += n
	return r.attachBytes
}

// attachBytesSoFar is the run-budget input for the next message.
func (r *runStats) attachBytesSoFar() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attachBytes
}

func (r *runStats) markFailed(id string) {
	r.mu.Lock()
	r.failed[id] = true
	r.mu.Unlock()
}

func (r *runStats) isFailed(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed[id]
}

func (r *runStats) failedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.failed)
}

func (r *runStats) addArchived() {
	r.mu.Lock()
	r.archived++
	r.mu.Unlock()
}

// drain works the backfill queue with parallel fetch workers until nothing
// pending remains except items that already failed this run.
func (s *Source) drain(ctx context.Context, labels map[string]string) (*runStats, error) {
	stats := newRunStats()
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		// Over-request by the failed count: failed items stay pending, so
		// this window always exposes fresh work when any exists.
		ids, err := s.db.NextPendingBackfill(s.id, backfillBatch+stats.failedCount())
		if err != nil {
			return stats, err
		}
		work := ids[:0:0]
		for _, id := range ids {
			if !stats.isFailed(id) {
				work = append(work, id)
			}
		}
		if len(work) == 0 {
			return stats, nil
		}
		err = s.runPool(ctx, work, func(ctx context.Context, id string) error {
			return s.processQueued(ctx, id, labels, stats)
		})
		if err != nil {
			return stats, err
		}
	}
}

// processQueued handles one backfill queue item end to end. The queue row
// is marked done in every terminal case — archived, out of scope, vanished,
// or skipped as poison — so the queue always drains.
func (s *Source) processQueued(ctx context.Context, id string, labels map[string]string, stats *runStats) error {
	skipped, err := s.db.IsSkipped(s.id, id)
	if err != nil {
		return err
	}
	if skipped {
		s.log.Warn("skipping poison message", "id", id)
		return s.db.MarkBackfillDone(s.id, id)
	}
	seen, err := s.db.SeenMessage(s.id, id)
	if err != nil {
		return err
	}
	if seen {
		return s.db.MarkBackfillDone(s.id, id)
	}
	ok, err := s.archiveOne(ctx, id, labels, stats)
	if err != nil {
		return err
	}
	if ok {
		return s.db.MarkBackfillDone(s.id, id)
	}
	// Item failure, already in the ledger. At the skip threshold the item
	// leaves the queue (surfaced by `save status`, never wedging the
	// drain); below it, it stays pending for the next run.
	nowSkipped, err := s.db.IsSkipped(s.id, id)
	if err != nil {
		return err
	}
	if nowSkipped {
		return s.db.MarkBackfillDone(s.id, id)
	}
	stats.markFailed(id)
	return nil
}

// runPool runs fn over ids with up to `workers` goroutines. fn returns
// non-nil only for run-fatal errors; the first one cancels the rest.
func (s *Source) runPool(ctx context.Context, ids []string, fn func(ctx context.Context, id string) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	n := workers
	if len(ids) < n {
		n = len(ids)
	}
	ch := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range ch {
				if ctx.Err() != nil {
					continue // keep draining the channel after cancellation
				}
				if err := fn(ctx, id); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					cancel()
				}
			}
		}()
	}
	for _, id := range ids {
		ch <- id
	}
	close(ch)
	wg.Wait()
	if firstErr == nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return firstErr
}

// --- incremental ---

// incremental replays history.list from startHid: messageAdded and
// spam/trash rescues go through the same fetch path as backfill, deletions
// become DB-only tombstones, and the cursor advances only when every item
// the batch covers is durable (or skipped as poison).
func (s *Source) incremental(ctx context.Context, startHid uint64, labels map[string]string) error {
	plan := newHistoryPlan()
	latest := startHid
	token := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var resp *gmailv1.ListHistoryResponse
		err := retry.Do(ctx, retry.Options{}, func() error {
			if err := s.limiter.Wait(ctx, costHistory); err != nil {
				return err
			}
			r, err := s.api.ListHistory(ctx, startHid, token)
			if err == nil {
				resp = r
			}
			return err
		})
		if err != nil {
			return fmt.Errorf("gmail: history.list: %w", classifyHistoryErr(err))
		}
		plan.addPage(resp.History)
		if resp.HistoryId > 0 {
			latest = resp.HistoryId
		}
		if resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
	}

	stats := newRunStats()
	if len(plan.fetch) > 0 {
		err := s.runPool(ctx, plan.fetch, func(ctx context.Context, id string) error {
			skipped, err := s.db.IsSkipped(s.id, id)
			if err != nil {
				return err
			}
			if skipped {
				return nil
			}
			seen, err := s.db.SeenMessage(s.id, id)
			if err != nil {
				return err
			}
			if seen {
				// Already archived; .md files are immutable, so label
				// churn on archived messages is a no-op.
				return nil
			}
			ok, err := s.archiveOne(ctx, id, labels, stats)
			if err != nil {
				return err
			}
			if !ok {
				nowSkipped, err := s.db.IsSkipped(s.id, id)
				if err != nil {
					return err
				}
				if !nowSkipped {
					stats.markFailed(id)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	// DB-only tombstones; the archive file is never touched. Replay after
	// a non-advanced cursor is a harmless repeat UPDATE.
	for _, id := range plan.deleted {
		if err := s.db.MarkMessageDeleted(s.id, id); err != nil {
			return err
		}
	}

	if n := stats.failedCount(); n > 0 {
		// Not durable yet: leave the cursor so the next run replays this
		// batch (dedup absorbs everything already archived). After
		// state.SkipThreshold runs the failing items are skipped and the
		// cursor moves — a poison item delays it, never wedges it.
		s.log.Warn("incremental batch incomplete; cursor not advanced",
			"failed", n, "archived", stats.archived)
		return nil
	}
	if err := s.db.SetCursor(s.id, "", cursorHistory, strconv.FormatUint(latest, 10)); err != nil {
		return err
	}
	if stats.archived > 0 || len(plan.fetch) > 0 || len(plan.deleted) > 0 {
		s.log.Info("incremental sync done",
			"archived", stats.archived, "fetched", len(plan.fetch),
			"rescue_candidates", plan.rescues, "tombstoned", len(plan.deleted),
			"history_id", latest)
	}
	return nil
}

// --- shared fetch/archive path ---

// archiveOne fetches message id (format=raw), applies the scope rules,
// renders, writes files, then commits the DB row (files first, DB second).
// It returns (true, nil) when the item is fully handled — archived, out of
// scope, or vanished; (false, nil) after recording an item failure; and a
// non-nil error only for run-fatal conditions.
func (s *Source) archiveOne(ctx context.Context, id string, labels map[string]string, stats *runStats) (bool, error) {
	var msg *gmailv1.Message
	fetchErr := retry.Do(ctx, retry.Options{}, func() error {
		if err := s.limiter.Wait(ctx, costGet); err != nil {
			return err
		}
		m, err := s.api.GetMessageRaw(ctx, id)
		if err == nil {
			msg = m
		}
		return err
	})
	if fetchErr != nil {
		switch classifyItemErr(fetchErr) {
		case verdictGone:
			s.log.Info("message vanished before fetch; skipping", "id", id)
			return true, nil
		case verdictRun:
			return false, fmt.Errorf("gmail: messages.get %s: %w", id, fetchErr)
		default:
			return s.recordItemFailure(id, fetchErr)
		}
	}

	switch d := decideScope(msg.LabelIds, s.acct.IncludeDrafts); d {
	case decisionSkipSpamTrash, decisionSkipChat, decisionSkipDraft:
		// Out of scope; deliberately NOT marked archived, so a later
		// spam/trash rescue (labelRemoved) can still pick it up.
		s.log.Debug("message out of scope", "id", id, "decision", d.String())
		return true, nil
	}

	raw, err := decodeRaw(msg.Raw)
	if err != nil {
		return s.recordItemFailure(id, fmt.Errorf("decode raw: %w", err))
	}
	// The exact cap the operator configured, over the DECODED message — the
	// transport ceiling installed in New only bounds the allocation. This is
	// an item failure, not an attachment policy skip: there is no note to
	// record a skip against, because the message cannot be parsed at all
	// without buffering it. The ledger keeps it visible in `save status` and
	// the poison protocol stops it wedging a cursor.
	if limit := s.pol.Settings().MaxMessageBytes; limit > 0 && int64(len(raw)) > limit {
		return s.recordItemFailure(id, fmt.Errorf(
			"raw message is %d bytes, over attachments.max_message_bytes (%d); raise the cap to archive it",
			len(raw), limit))
	}
	serverTime := time.UnixMilli(msg.InternalDate).UTC()
	local := serverTime.In(s.writer.TZ)
	// The attach dir must match what the writer derives from doc.Subject;
	// subjectOf uses the same enmime decode as emailpipe.Render, so the
	// two always agree. The stem carries the file tag, the hash the instance
	// id — Gmail message ids repeat across mailboxes, so two accounts that
	// hold the same id must still land on distinct paths.
	stem := naming.EmailStem(local, s.tag, subjectOf(raw), naming.Hash8(s.id+":"+id))
	doc, err := emailpipe.RenderWithOptions(raw, naming.AttachDir(stem), emailpipe.Options{
		Policy:        s.pol,
		RunBytesSoFar: stats.attachBytesSoFar(),
		FreeSpace:     s.freeSpace(s.writer.Root),
	})
	if err != nil {
		return s.recordItemFailure(id, err)
	}
	meta := archive.EmailMeta{
		Source:       s.id,
		SourceTag:    s.tag,
		Account:      s.acct.Account,
		AccountLabel: s.acct.Label,
		StableID:     id,
		ThreadID:     msg.ThreadId,
		Labels:       labelNames(labels, msg.LabelIds),
		ServerTime:   serverTime,
		// Gmail fetches format=raw: the refused bytes crossed the network and
		// were discarded, and the note must say exactly that rather than
		// implying they were never fetched.
		SkipDisposition: archive.SkipBytesDiscarded,
	}
	relPath, contentHash, err := s.writer.WriteEmail(doc, meta)
	if err != nil {
		// Write errors signal archive-wide trouble (disk, permissions),
		// not a bad item — run-fatal, matching fastmail's commitDoc.
		return false, fmt.Errorf("gmail: write %s: %w", id, err)
	}
	// The skip ledger commits BEFORE the messages row: the messages row is
	// what makes this id Seen forever, so a crash between the two must lose
	// the acknowledgement, never the record of what was not stored.
	if err := s.recordSkips(doc, meta, relPath); err != nil {
		return false, err
	}
	stats.addAttachBytes(doc.StoredBytes)

	m := state.Message{
		Source:      s.id,
		StableID:    id,
		RFC822MsgID: doc.MessageID,
		ThreadID:    msg.ThreadId,
		TS:          serverTime,
		DayBucket:   naming.DayBucket(local),
		RelPath:     relPath,
		ContentHash: contentHash,
	}
	if !doc.Date.IsZero() {
		m.OrigOffset = doc.Date.Format("-07:00")
	}
	if err := s.db.CommitMessage(m); err != nil {
		return false, err // DB failures are run-fatal, not item poison
	}
	if err := s.db.ClearFailure(s.id, id); err != nil {
		return false, err
	}
	stats.addArchived()
	return true, nil
}

// recordSkips persists every attachment the policy refused, so the note and
// the state DB tell the same story and `save refetch` can find the bytes
// again after a policy widening.
//
// A policy skip is NOT an item failure: it never touches the failures ledger
// and never holds a cursor — the message archived correctly, minus bytes the
// operator's policy declined. A DB error here IS run-fatal, exactly like
// CommitMessage: it means the store is broken, not that this message is bad.
func (s *Source) recordSkips(doc *emailpipe.EmailDoc, meta archive.EmailMeta, relPath string) error {
	rows, err := s.writer.SkippedRows(doc, meta, relPath)
	if err != nil {
		return fmt.Errorf("gmail: skipped rows %s: %w", meta.StableID, err)
	}
	if len(rows) == 0 {
		return nil
	}
	if err := s.db.UpsertSkippedBatch(rows); err != nil {
		return err
	}
	for _, r := range rows {
		s.log.Warn("gmail: attachment not stored",
			"id", meta.StableID, "part", r.PartKey, "name", r.SanitizedName,
			"bytes", r.SizeBytes, "declared", r.DeclaredType, "sniffed", r.SniffedType,
			"reason", r.Reason)
	}
	return nil
}

func (s *Source) recordItemFailure(id string, cause error) (bool, error) {
	attempts, err := s.db.RecordFailure(s.id, id, cause.Error())
	if err != nil {
		return false, err
	}
	s.log.Warn("message failed", "id", id, "attempts", attempts, "skip_threshold", state.SkipThreshold, "err", cause)
	return false, nil
}

// subjectOf extracts the Subject exactly as emailpipe.Render will (same
// enmime decode), so the attach dir computed from it always survives the
// writer's consistency check. Unparseable input degrades to "" — Render
// will produce the authoritative error.
func subjectOf(raw []byte) string {
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	return env.GetHeader("Subject")
}
