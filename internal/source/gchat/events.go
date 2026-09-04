package gchat

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	chat "google.golang.org/api/chat/v1"

	"comms/internal/naming"
	"comms/internal/retry"
	"comms/internal/state"
)

// cursorEventsTime is the per-space cursor kind for the spaceEvents pass.
const cursorEventsTime = "events_time"

type deletedStub struct {
	msg       *chat.Message // name, createTime, deletionMetadata only
	eventTime time.Time
}

// syncSpaceEvents folds edits and deletions into already-archived rows via
// spaceEvents.list: updated messages are re-fetched whole, deletions become
// tombstones (existing rows keep their archived raw_json — the content the
// server no longer has). The events cursor advances to the max server
// eventTime actually applied — never the local clock, which may run ahead of
// the server and would then permanently hide events landing in the
// difference; with no events the previous cursor is kept. The next listing
// rewinds the cursor by eventsRewind, so events still committing server-side
// during this pass are re-observed (replays are idempotent).
func (c *Connector) syncSpaceEvents(ctx context.Context, sp spaceMeta) error {
	cursor, err := c.cursorTime(sp.Name, cursorEventsTime)
	if err != nil {
		return storeFatal(err)
	}
	now := c.now()
	start, gapped := eventsListStart(cursor, now)
	if gapped {
		c.log.Warn("gchat: events cursor is older than the 28-day spaceEvents window; edits and deletions in the gap were not observed",
			"space", sp.Name, "cursor", cursor)
	}
	filter := eventsFilter(start)
	updated := make(map[string]bool)
	deleted := make(map[string]deletedStub)
	var maxEvent time.Time
	token := ""
	for {
		if err := c.limiter.Wait(ctx, sp.Name); err != nil {
			return err
		}
		var resp *chat.ListSpaceEventsResponse
		if err := retry.Do(ctx, c.retryOpts, func() error {
			r, lerr := c.api.listSpaceEvents(ctx, sp.Name, filter, token)
			if lerr != nil {
				return lerr
			}
			resp = r
			return nil
		}); err != nil {
			return fmt.Errorf("list space events: %w", err)
		}
		for _, ev := range resp.SpaceEvents {
			if t := collectEvent(ev, updated, deleted); t.After(maxEvent) {
				maxEvent = t
			}
		}
		token = resp.NextPageToken
		if token == "" {
			break
		}
	}
	if err := c.applyUpdatedMessages(ctx, sp, updated, deleted); err != nil {
		return err
	}
	if err := c.applyDeletedMessages(ctx, sp, deleted); err != nil {
		return err
	}
	next := cursor
	if maxEvent.After(next) {
		next = maxEvent
	}
	if next.IsZero() {
		return nil // nothing ever observed: the cursor stays unset
	}
	return storeFatal(c.db.SetCursor(c.src, sp.Name, cursorEventsTime, formatCursor(next)))
}

// collectEvent folds one space event into the updated/deleted sets,
// including the batch variants the server returns alongside filtered types.
// It returns the event's server eventTime (zero when unparseable) — the
// caller's cursor high-water mark.
func collectEvent(ev *chat.SpaceEvent, updated map[string]bool, deleted map[string]deletedStub) time.Time {
	if ev == nil {
		return time.Time{}
	}
	evTime, _ := time.Parse(time.RFC3339, ev.EventTime)
	addUpdated := func(m *chat.Message) {
		if m != nil && m.Name != "" {
			updated[m.Name] = true
		}
	}
	addDeleted := func(m *chat.Message) {
		if m != nil && m.Name != "" {
			deleted[m.Name] = deletedStub{msg: m, eventTime: evTime}
		}
	}
	switch {
	case ev.MessageUpdatedEventData != nil:
		addUpdated(ev.MessageUpdatedEventData.Message)
	case ev.MessageBatchUpdatedEventData != nil:
		for _, d := range ev.MessageBatchUpdatedEventData.Messages {
			if d != nil {
				addUpdated(d.Message)
			}
		}
	case ev.MessageDeletedEventData != nil:
		addDeleted(ev.MessageDeletedEventData.Message)
	case ev.MessageBatchDeletedEventData != nil:
		for _, d := range ev.MessageBatchDeletedEventData.Messages {
			if d != nil {
				addDeleted(d.Message)
			}
		}
	}
	return evTime
}

// applyUpdatedMessages re-fetches every updated message (unless a deletion
// supersedes it) and upserts the latest revision; the row's day is marked
// dirty by ApplyChatPage. The message cursor is never advanced here.
func (c *Connector) applyUpdatedMessages(ctx context.Context, sp spaceMeta, updated map[string]bool, deleted map[string]deletedStub) error {
	var msgs []*chat.Message
	for _, name := range slices.Sorted(maps.Keys(updated)) {
		if _, gone := deleted[name]; gone {
			continue // the deletion tombstone wins
		}
		if err := c.limiter.Wait(ctx, sp.Name); err != nil {
			return err
		}
		var m *chat.Message
		err := retry.Do(ctx, c.retryOpts, func() error {
			g, gerr := c.api.getMessage(ctx, name)
			if gerr != nil {
				return gerr
			}
			m = g
			return nil
		})
		if err != nil {
			if retry.Classify(err) == retry.ItemGone {
				continue // deleted since; its deletion event covers it
			}
			return fmt.Errorf("refetch %s: %w", name, err)
		}
		msgs = append(msgs, m)
	}
	if len(msgs) == 0 {
		return nil
	}
	page, skips, _, errs, _ := buildPage(c.src, sp, msgs, c.writer.TZ, time.Time{}, c.acct.MirrorDriveFiles, c.pol, nil)
	page.Cursor = "" // the message cursor tracks the list pass only
	for _, ie := range errs {
		c.recordItemFailure(ie)
	}
	// An edited message can gain an attachment, so a refetch can produce a
	// refusal that no list pass ever saw. Record it before the page, as
	// everywhere else — and record it even when the page turns out to be
	// empty, which cannot happen here (a refusal implies a message) but would
	// be a silent drop if it did.
	if err := c.recordSkips(skips); err != nil {
		return err
	}
	if len(page.Messages) == 0 && len(page.Attachments) == 0 {
		return nil
	}
	return storeFatal(c.db.ApplyChatPage(ctx, page))
}

// applyDeletedMessages tombstones deleted messages. Rows we already archived
// keep their raw_json (the deleted content is unrecoverable server-side and
// the archive's copy is the only one left); never-archived deletions are
// recorded as stub rows only when show_deleted is on.
func (c *Connector) applyDeletedMessages(ctx context.Context, sp spaceMeta, deleted map[string]deletedStub) error {
	var rows []state.ChatMessage
	for _, name := range slices.Sorted(maps.Keys(deleted)) {
		st := deleted[name]
		if st.msg == nil {
			continue
		}
		create, err := time.Parse(time.RFC3339, st.msg.CreateTime)
		if err != nil {
			c.recordItemFailure(itemError{ID: name, Err: fmt.Errorf("deleted event: bad createTime %q: %w", st.msg.CreateTime, err)})
			continue
		}
		day := naming.DayBucket(create.In(c.writer.TZ))
		if existing, ok := c.lookupRow(sp.Name, day, name); ok {
			existing.Deleted = true
			if existing.DeletedAt.IsZero() {
				existing.DeletedAt = st.eventTime
			}
			rows = append(rows, existing)
			continue
		}
		if !c.acct.ShowDeleted {
			continue
		}
		raw, err := json.Marshal(st.msg)
		if err != nil {
			c.recordItemFailure(itemError{ID: name, Err: fmt.Errorf("marshal deleted stub: %w", err)})
			continue
		}
		rows = append(rows, state.ChatMessage{
			Name:       name,
			Thread:     threadName(st.msg),
			SenderID:   senderID(st.msg),
			CreateTime: create,
			DayBucket:  day,
			RawJSON:    string(raw),
			Deleted:    true,
			DeletedAt:  st.eventTime,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	return storeFatal(c.db.ApplyChatPage(ctx, state.ChatPage{Source: c.src, Space: sp.Name, Messages: rows}))
}
