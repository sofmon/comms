package fastmail

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"

	"save/internal/archive"
	"save/internal/emailpipe"
	"save/internal/naming"
	"save/internal/retry"
	"save/internal/state"
)

// emailProps are the Email/get properties for sync. The plan's set is
// id/blobId/threadId/mailboxIds/receivedAt; "subject" is added so the
// attachment-directory name can be guessed before rendering (the writer
// derives it from the parsed subject — see processEmail).
var emailProps = []string{"id", "blobId", "threadId", "mailboxIds", "receivedAt", "subject"}

// fallbackProps additionally pull the parsed headers and text body for the
// bodyValues fallback document.
var fallbackProps = []string{
	"id", "blobId", "threadId", "mailboxIds", "receivedAt", "subject",
	"from", "to", "cc", "messageId", "sentAt", "textBody", "bodyValues",
}

// mailboxes is one run's mailbox snapshot.
type mailboxes struct {
	names     map[jmap.ID]string
	junkTrash map[jmap.ID]bool
	exclude   []jmap.ID // junk+trash ids, sorted, for inMailboxOtherThan
}

// labels returns the sorted mailbox names for an email's mailboxIds;
// unknown ids (a mailbox created after this run's snapshot) fall back to
// the raw id so the frontmatter stays complete.
func (m *mailboxes) labels(ids map[jmap.ID]bool) []string {
	var out []string
	for id, in := range ids {
		if !in {
			continue
		}
		if name, ok := m.names[id]; ok && name != "" {
			out = append(out, name)
		} else {
			out = append(out, string(id))
		}
	}
	slices.Sort(out)
	return out
}

// inScope reports whether an email belongs in the archive: member of at
// least one mailbox that is neither junk nor trash. Junk/trash-only emails
// stay unarchived — not tombstoned — so a later move out of junk (a junk
// rescue) archives them via the updated path.
func inScope(ids map[jmap.ID]bool, junkTrash map[jmap.ID]bool) bool {
	for id, in := range ids {
		if in && !junkTrash[id] {
			return true
		}
	}
	return false
}

// Sync implements source.Source: backfill when no email_state cursor
// exists, incremental otherwise. Interruptible anywhere thanks to the
// files-row-cursor write ordering.
func (s *Source) Sync(ctx context.Context) error {
	if err := s.checkInstance(); err != nil {
		return err
	}
	// attachments.run_budget is per pass, so the tally starts fresh here.
	s.runBytes = 0
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	boxes, err := s.fetchMailboxes(ctx, c)
	if err != nil {
		return err
	}
	if err := s.retryFailed(ctx, c, boxes); err != nil {
		return err
	}
	cur, ok, err := s.db.GetCursor(s.inst, "", cursorEmailState)
	if err != nil {
		return err
	}
	if !ok {
		return s.backfill(ctx, c, boxes)
	}
	return s.incremental(ctx, c, boxes, cur)
}

// fetchMailboxes gets every mailbox and builds the id→name map plus the
// junk/trash id set.
func (s *Source) fetchMailboxes(ctx context.Context, c *conn) (*mailboxes, error) {
	req := &jmap.Request{}
	gID := req.Invoke(&mailbox.Get{
		Account:    c.accountID,
		Properties: []string{"id", "name", "role"},
	})
	resp, err := s.doReq(ctx, c, req)
	if err != nil {
		return nil, err
	}
	gr, err := unpack[mailbox.GetResponse](resp, gID)
	if err != nil {
		return nil, err
	}
	m := &mailboxes{
		names:     make(map[jmap.ID]string, len(gr.List)),
		junkTrash: make(map[jmap.ID]bool, 2),
	}
	for _, mb := range gr.List {
		if mb == nil || mb.ID == "" {
			continue
		}
		m.names[mb.ID] = mb.Name
		if mb.Role == mailbox.RoleJunk || mb.Role == mailbox.RoleTrash {
			m.junkTrash[mb.ID] = true
			m.exclude = append(m.exclude, mb.ID)
		}
	}
	slices.Sort(m.exclude)
	return m, nil
}

// backfill enumerates the account oldest-first with anchor paging,
// archiving every unseen in-scope email, then promotes the bootstrap state
// to the incremental cursor.
func (s *Source) backfill(ctx context.Context, c *conn, boxes *mailboxes) error {
	anchor, _, err := s.db.GetCursor(s.inst, "", cursorBackfillAnchor)
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		qr, gr, err := s.queryPage(ctx, c, boxes, anchor)
		if err != nil {
			if errors.Is(err, retry.ErrCursorGone) && anchor != "" {
				// The anchor email was destroyed server-side. Restart the
				// enumeration from the top; Seen dedup absorbs the replay.
				s.log.Warn("fastmail: backfill anchor destroyed; restarting enumeration")
				if derr := s.db.DeleteCursor(s.inst, "", cursorBackfillAnchor); derr != nil {
					return derr
				}
				anchor = ""
				continue
			}
			return err
		}
		if gr != nil && gr.State != "" {
			// Capture the state of the FIRST response of the whole
			// backfill. Once promoted, Email/changes replays everything
			// after this point, so a resumed run must keep the original
			// bootstrap state rather than overwrite it with a newer one.
			_, have, err := s.db.GetCursor(s.inst, "", cursorBootstrapState)
			if err != nil {
				return err
			}
			if !have {
				if err := s.db.SetCursor(s.inst, "", cursorBootstrapState, gr.State); err != nil {
					return err
				}
			}
		}
		if gr != nil {
			for _, em := range gr.List {
				if err := s.processEmail(ctx, c, boxes, em); err != nil {
					return err
				}
			}
		}
		if len(qr.IDs) == 0 {
			break
		}
		anchor = string(qr.IDs[len(qr.IDs)-1])
		if err := s.db.SetCursor(s.inst, "", cursorBackfillAnchor, anchor); err != nil {
			return err
		}
		// A short page means the enumeration is complete. The server may
		// have clamped our limit (echoed in qr.Limit); honor the clamp so
		// a full-but-clamped page keeps paging.
		effLimit := backfillPageLimit
		if qr.Limit > 0 && int(qr.Limit) < effLimit {
			effLimit = int(qr.Limit)
		}
		if len(qr.IDs) < effLimit {
			break
		}
	}
	return s.promoteBootstrap()
}

// promoteBootstrap commits the bootstrap state as the incremental cursor
// and clears the backfill cursors — the backfill is complete.
func (s *Source) promoteBootstrap() error {
	boot, ok, err := s.db.GetCursor(s.inst, "", cursorBootstrapState)
	if err != nil {
		return err
	}
	if !ok || boot == "" {
		// Only reachable when no page yielded a state (split-request path
		// on an empty account). Re-enumerating next run is cheap and safe.
		return errors.New("fastmail: backfill finished without capturing an Email state; will re-enumerate next run")
	}
	if err := s.db.SetCursor(s.inst, "", cursorEmailState, boot); err != nil {
		return err
	}
	if err := s.db.DeleteCursor(s.inst, "", cursorBackfillAnchor); err != nil {
		return err
	}
	return s.db.DeleteCursor(s.inst, "", cursorBootstrapState)
}

// queryPage runs one Email/query page with the junk/trash-excluding filter,
// chaining Email/get via back-reference when the session allows two calls
// per request, else splitting into two sequential requests.
func (s *Source) queryPage(ctx context.Context, c *conn, boxes *mailboxes, anchor string) (*email.QueryResponse, *email.GetResponse, error) {
	q := &email.Query{
		Account: c.accountID,
		Filter:  &email.FilterCondition{InMailboxOtherThan: boxes.exclude},
		Sort:    []*email.SortComparator{{Property: "receivedAt", IsAscending: true}},
		Limit:   uint64(backfillPageLimit),
	}
	if anchor != "" {
		q.Anchor = jmap.ID(anchor)
		q.AnchorOffset = 1
	}
	if c.maxCalls >= 2 {
		req := &jmap.Request{}
		qID := req.Invoke(q)
		g := &email.Get{
			Account:      c.accountID,
			ReferenceIDs: &jmap.ResultReference{ResultOf: qID, Name: "Email/query", Path: "/ids"},
			Properties:   emailProps,
		}
		gID := req.Invoke(g)
		resp, err := s.doReq(ctx, c, req)
		if err != nil {
			return nil, nil, err
		}
		qr, err := unpack[email.QueryResponse](resp, qID)
		if err != nil {
			return nil, nil, err
		}
		gr, err := unpack[email.GetResponse](resp, gID)
		if err != nil {
			return nil, nil, err
		}
		return qr, gr, nil
	}

	req := &jmap.Request{}
	qID := req.Invoke(q)
	resp, err := s.doReq(ctx, c, req)
	if err != nil {
		return nil, nil, err
	}
	qr, err := unpack[email.QueryResponse](resp, qID)
	if err != nil {
		return nil, nil, err
	}
	if len(qr.IDs) == 0 {
		return qr, nil, nil
	}
	req2 := &jmap.Request{}
	gID := req2.Invoke(&email.Get{Account: c.accountID, IDs: qr.IDs, Properties: emailProps})
	resp2, err := s.doReq(ctx, c, req2)
	if err != nil {
		return nil, nil, err
	}
	gr, err := unpack[email.GetResponse](resp2, gID)
	if err != nil {
		return nil, nil, err
	}
	return qr, gr, nil
}

// incremental walks Email/changes from since, persisting the new state
// after each fully-processed batch.
func (s *Source) incremental(ctx context.Context, c *conn, boxes *mailboxes, since string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		req := &jmap.Request{}
		chID := req.Invoke(&email.Changes{
			Account:    c.accountID,
			SinceState: since,
			MaxChanges: changesBatchLimit,
		})
		resp, err := s.doReq(ctx, c, req)
		if err != nil {
			return err
		}
		ch, err := unpack[email.ChangesResponse](resp, chID)
		if err != nil {
			if errors.Is(err, retry.ErrCursorGone) {
				// The server expired our state: fall back to a full
				// requery. Seen dedup makes it re-download nothing.
				s.log.Warn("fastmail: server cannot calculate changes; re-enumerating with dedup")
				for _, kind := range []string{cursorEmailState, cursorBackfillAnchor, cursorBootstrapState} {
					if derr := s.db.DeleteCursor(s.inst, "", kind); derr != nil {
						return derr
					}
				}
				return s.backfill(ctx, c, boxes)
			}
			return err
		}
		if ch.NewState == "" {
			return errors.New("fastmail: Email/changes returned an empty newState")
		}

		fetch := make([]jmap.ID, 0, len(ch.Created)+len(ch.Updated))
		fetch = append(fetch, ch.Created...)
		for _, id := range ch.Updated {
			seen, err := s.db.SeenMessage(s.inst, string(id))
			if err != nil {
				return err
			}
			// An update on an UNarchived id may be a move out of
			// junk/trash — re-evaluate its mailboxes (junk rescue).
			// Updates on archived ids are DB-only, and no mutable label
			// state is kept, so there is nothing to do for them.
			if !seen {
				fetch = append(fetch, id)
			}
		}
		if _, err := s.fetchAndProcess(ctx, c, boxes, fetch); err != nil {
			return err
		}
		for _, id := range ch.Destroyed {
			// DB-only tombstone; the file (if any) is never touched.
			// Unknown ids are a no-op.
			if err := s.db.MarkMessageDeleted(s.inst, string(id)); err != nil {
				return err
			}
		}
		if err := s.db.SetCursor(s.inst, "", cursorEmailState, ch.NewState); err != nil {
			return err
		}
		if !ch.HasMoreChanges {
			return nil
		}
		if ch.NewState == since {
			return fmt.Errorf("fastmail: Email/changes reports more changes but did not advance from state %q", since)
		}
		since = ch.NewState
	}
}

// fetchAndProcess gets the given emails in chunks and processes each; ids
// the server no longer knows are returned in notFound.
func (s *Source) fetchAndProcess(ctx context.Context, c *conn, boxes *mailboxes, ids []jmap.ID) (notFound []jmap.ID, err error) {
	for start := 0; start < len(ids); start += getChunkSize {
		end := min(start+getChunkSize, len(ids))
		req := &jmap.Request{}
		gID := req.Invoke(&email.Get{Account: c.accountID, IDs: ids[start:end], Properties: emailProps})
		resp, err := s.doReq(ctx, c, req)
		if err != nil {
			return notFound, err
		}
		gr, err := unpack[email.GetResponse](resp, gID)
		if err != nil {
			return notFound, err
		}
		for _, em := range gr.List {
			if err := s.processEmail(ctx, c, boxes, em); err != nil {
				return notFound, err
			}
		}
		notFound = append(notFound, gr.NotFound...)
	}
	return notFound, nil
}

// retryFailed re-drives item failures that have not yet hit the skip
// threshold. Incremental sync never revisits an id once its change is
// consumed, so without this pass a transiently failed item would stay
// unarchived until the next full re-enumeration.
func (s *Source) retryFailed(ctx context.Context, c *conn, boxes *mailboxes) error {
	fails, err := s.db.ListFailures()
	if err != nil {
		return err
	}
	var ids []jmap.ID
	for _, f := range fails {
		if f.Source != s.inst || f.Attempts >= state.SkipThreshold {
			continue
		}
		seen, err := s.db.SeenMessage(s.inst, f.ID)
		if err != nil {
			return err
		}
		if seen {
			// Archived by a later replay (e.g. crash between the write
			// and the clear): the failure entry is stale.
			if err := s.db.ClearFailure(s.inst, f.ID); err != nil {
				return err
			}
			continue
		}
		ids = append(ids, jmap.ID(f.ID))
	}
	if len(ids) == 0 {
		return nil
	}
	notFound, err := s.fetchAndProcess(ctx, c, boxes, ids)
	if err != nil {
		return err
	}
	for _, id := range notFound {
		// The email no longer exists; stop tracking the failure.
		if err := s.db.ClearFailure(s.inst, string(id)); err != nil {
			return err
		}
	}
	return nil
}

// processEmail archives one email if it is unseen and in scope. Item-level
// problems are recorded in the failures ledger and never returned; the
// returned error is always run-fatal (cancellation, auth, store, or
// network-after-retries failures).
func (s *Source) processEmail(ctx context.Context, c *conn, boxes *mailboxes, em *email.Email) error {
	if em == nil || em.ID == "" {
		return nil
	}
	id := string(em.ID)
	if skipped, err := s.db.IsSkipped(s.inst, id); err != nil {
		return err
	} else if skipped {
		return nil
	}
	if seen, err := s.db.SeenMessage(s.inst, id); err != nil {
		return err
	} else if seen {
		return nil
	}
	if !inScope(em.MailboxIDs, boxes.junkTrash) {
		return nil
	}
	if em.ReceivedAt == nil || em.BlobID == "" {
		return s.itemFailure(id, errors.New("email lacks receivedAt or blobId"))
	}

	raw, err := s.download(ctx, c, em.BlobID)
	if err != nil {
		if runFatal(ctx, err) {
			return fmt.Errorf("fastmail: download blob for %s: %w", id, err)
		}
		return s.itemFailure(id, fmt.Errorf("download blob: %w", err))
	}

	// The writer derives the attachment directory from the *rendered*
	// subject; guess it from the JMAP subject, and re-render in the rare
	// case the two decode differently enough to change the slug.
	//
	// The hash is salted with the instance id (not the bare kind) so two
	// accounts holding the same JMAP id land on different stems; the stem's
	// source component is the file tag, which never contains a colon.
	local := em.ReceivedAt.In(s.writer.TZ)
	hash8 := naming.Hash8(s.inst + ":" + id)
	attachDir := naming.AttachDir(naming.EmailStem(local, s.tag, em.Subject, hash8))
	// One free-space probe per message, shared by every part of it: the
	// policy re-checks the floor for each attachment against this value, and
	// a statfs per part would buy nothing.
	ropts := emailpipe.Options{
		Policy:        s.pol,
		RunBytesSoFar: s.runBytes,
		FreeSpace:     s.freeSpace(s.writer.Root),
	}
	doc, rerr := emailpipe.RenderWithOptions(raw, attachDir, ropts)
	if rerr == nil {
		if want := naming.AttachDir(naming.EmailStem(local, s.tag, doc.Subject, hash8)); want != attachDir {
			doc, rerr = emailpipe.RenderWithOptions(raw, want, ropts)
		}
	}
	if rerr != nil {
		attempts, ferr := s.db.RecordFailure(s.inst, id, rerr.Error())
		if ferr != nil {
			return ferr
		}
		s.log.Warn("fastmail: render failed", "id", id, "attempts", attempts, "error", rerr)
		if attempts >= state.SkipThreshold {
			// Retries are exhausted: archive a minimal document from the
			// server-parsed body rather than lose the message entirely.
			if err := s.archiveFallback(ctx, c, boxes, em, rerr); err != nil {
				if runFatal(ctx, err) {
					return err
				}
				s.log.Error("fastmail: bodyValues fallback failed; item skipped", "id", id, "error", err)
			}
		}
		return nil
	}
	return s.commitDoc(doc, em, boxes)
}

// commitDoc writes the rendered document (files first) and then commits
// the DB row — the write-ordering law. Write errors are run-fatal: they
// signal archive-wide trouble (disk, permissions), not a bad item.
func (s *Source) commitDoc(doc *emailpipe.EmailDoc, em *email.Email, boxes *mailboxes) error {
	id := string(em.ID)
	meta := archive.EmailMeta{
		Source:       s.inst,
		SourceTag:    s.tag,
		Account:      s.acct.Account,
		AccountLabel: s.acct.Label,
		StableID:     id,
		ThreadID:     string(em.ThreadID),
		Labels:       boxes.labels(em.MailboxIDs),
		ServerTime:   em.ReceivedAt.UTC(),
		// The whole RFC 5322 blob is downloaded before anything is decided,
		// so refused bytes were fetched and discarded — not never fetched.
		SkipDisposition: archive.SkipBytesDiscarded,
	}
	rel, hash, err := s.writer.WriteEmail(doc, meta)
	if err != nil {
		return err
	}
	// Skips commit BEFORE the messages row: that row is what makes this id
	// Seen forever, so a crash between the two must lose the acknowledgement,
	// never the record of what was not stored.
	if err := s.recordSkips(doc, meta, rel); err != nil {
		return err
	}
	s.runBytes += doc.StoredBytes
	var offset string
	if !doc.Date.IsZero() {
		offset = doc.Date.Format("-07:00")
	}
	local := em.ReceivedAt.In(s.writer.TZ)
	if err := s.db.CommitMessage(state.Message{
		Source:      s.inst,
		StableID:    id,
		RFC822MsgID: doc.MessageID,
		ThreadID:    string(em.ThreadID),
		TS:          *em.ReceivedAt,
		OrigOffset:  offset,
		DayBucket:   naming.DayBucket(local),
		RelPath:     rel,
		ContentHash: hash,
	}); err != nil {
		return err
	}
	return s.db.ClearFailure(s.inst, id)
}

// archiveFallback archives a minimal document built from the server's own
// parse (Email/get bodyValues) after the raw blob repeatedly failed to
// parse locally: losing MIME detail beats losing the message.
func (s *Source) archiveFallback(ctx context.Context, c *conn, boxes *mailboxes, em *email.Email, renderErr error) error {
	req := &jmap.Request{}
	gID := req.Invoke(&email.Get{
		Account:             c.accountID,
		IDs:                 []jmap.ID{em.ID},
		Properties:          fallbackProps,
		FetchTextBodyValues: true,
		MaxBodyValueBytes:   fallbackBodyCap,
	})
	resp, err := s.doReq(ctx, c, req)
	if err != nil {
		return err
	}
	gr, err := unpack[email.GetResponse](resp, gID)
	if err != nil {
		return err
	}
	if len(gr.List) == 0 || gr.List[0] == nil {
		return fmt.Errorf("email %s not found for bodyValues fallback", em.ID)
	}
	full := gr.List[0]
	if full.ReceivedAt == nil {
		full.ReceivedAt = em.ReceivedAt
	}
	if full.ReceivedAt == nil {
		return fmt.Errorf("email %s has no receivedAt", em.ID)
	}
	if !inScope(full.MailboxIDs, boxes.junkTrash) {
		// Moved to junk/trash since we last looked: no longer archivable.
		return nil
	}

	var body strings.Builder
	truncated := false
	for _, p := range full.TextBody {
		if p == nil {
			continue
		}
		bv, ok := full.BodyValues[p.PartID]
		if !ok || bv == nil {
			continue
		}
		if body.Len() > 0 {
			body.WriteString("\n\n")
		}
		body.WriteString(bv.Value)
		truncated = truncated || bv.IsTruncated
	}
	doc := &emailpipe.EmailDoc{
		Subject: full.Subject,
		From:    addressStrings(full.From),
		To:      addressStrings(full.To),
		Cc:      addressStrings(full.CC),
		BodyMD:  body.String(),
		// The policy never saw this document: the raw message could not be
		// parsed, so no part was ever offered to it. The digest is recorded
		// anyway (it costs nothing and keeps the field meaningful), and the
		// warning below is what tells a reader the attachments are missing
		// because the MIME parse failed, not because the policy refused them.
		PolicyDigest: s.pol.PolicyDigest(),
		Warnings: []string{
			fmt.Sprintf("body recovered from JMAP bodyValues; raw message failed to parse: %v", renderErr),
			"attachments were NOT extracted from this message: the MIME parse failed, so no part could be offered to the attachment policy",
		},
	}
	if len(full.MessageID) > 0 && full.MessageID[0] != "" {
		doc.MessageID = "<" + full.MessageID[0] + ">" // JMAP strips the angle brackets
	}
	if full.SentAt != nil {
		doc.Date = *full.SentAt
	}
	if truncated {
		doc.Warnings = append(doc.Warnings, "body text truncated by the server")
	}
	return s.commitDoc(doc, full, boxes)
}

// recordSkips persists every attachment the policy refused, so the note and
// the state DB tell the same story and `save refetch` can find the bytes
// again after a policy widening.
//
// A policy skip is NOT an item failure: it never touches the failures ledger
// and never holds a cursor — the message archived correctly, minus bytes the
// operator's policy declined. A DB error here IS run-fatal, exactly like
// CommitMessage.
func (s *Source) recordSkips(doc *emailpipe.EmailDoc, meta archive.EmailMeta, relPath string) error {
	rows, err := s.writer.SkippedRows(doc, meta, relPath)
	if err != nil {
		return fmt.Errorf("fastmail: skipped rows %s: %w", meta.StableID, err)
	}
	if len(rows) == 0 {
		return nil
	}
	if err := s.db.UpsertSkippedBatch(rows); err != nil {
		return err
	}
	for _, r := range rows {
		s.log.Warn("fastmail: attachment not stored",
			"id", meta.StableID, "part", r.PartKey, "name", r.SanitizedName,
			"bytes", r.SizeBytes, "declared", r.DeclaredType, "sniffed", r.SniffedType,
			"reason", r.Reason)
	}
	return nil
}

// itemFailure records one item's processing failure; only a store error is
// returned (run-fatal).
func (s *Source) itemFailure(id string, ierr error) error {
	attempts, err := s.db.RecordFailure(s.inst, id, ierr.Error())
	if err != nil {
		return err
	}
	s.log.Warn("fastmail: item failed", "id", id, "attempts", attempts, "error", ierr)
	return nil
}

// addressStrings formats JMAP addresses as "Name <addr>" strings.
func addressStrings(addrs []*mail.Address) []string {
	if len(addrs) == 0 {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a == nil {
			continue
		}
		out = append(out, a.String())
	}
	return out
}
