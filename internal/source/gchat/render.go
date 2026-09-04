package gchat

import (
	"context"
	"fmt"
	"time"

	"comms/internal/archive"
	"comms/internal/state"
)

// renderDirtyDays regenerates every dirty day file OF THIS ACCOUNT as a pure
// projection of the canonical rows: whole-file render, atomic replace, then
// the ledger row is marked clean. Another account's dirty days belong to its
// own connector (the CLI's startup healing pass covers every instance). A
// failed render is logged and left dirty for the next run — it never aborts
// the run.
func (c *Connector) renderDirtyDays(ctx context.Context) error {
	files, err := c.db.DirtyDayFiles(c.src)
	if err != nil {
		return err
	}
	memberCache := make(map[string]map[string]string)
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.renderDay(f, memberCache); err != nil {
			c.log.Warn("gchat: day render failed; left dirty for the next run",
				"space", f.Space, "day", f.DayBucket, "err", err)
		}
	}
	return nil
}

func (c *Connector) renderDay(f state.DayFile, memberCache map[string]map[string]string) error {
	sp, ok, err := c.db.GetSpace(c.src, f.Space)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("space %q not registered", f.Space)
	}
	msgs, err := c.db.MessagesForDay(c.src, f.Space, f.DayBucket)
	if err != nil {
		return err
	}
	atts := make(map[string][]state.Attachment)
	for _, m := range msgs {
		rows, aerr := c.db.AttachmentsForMessage(c.src, m.Name)
		if aerr != nil {
			return aerr
		}
		if len(rows) > 0 {
			atts[m.Name] = rows
		}
	}
	cache := memberCache[f.Space]
	if cache == nil {
		cache = make(map[string]string)
		memberCache[f.Space] = cache
	}
	memberName := func(userID string) string {
		if name, hit := cache[userID]; hit {
			return name
		}
		name, _, merr := c.db.GetMember(c.src, f.Space, userID)
		if merr != nil {
			c.log.Warn("gchat: member lookup failed", "space", f.Space, "user", userID, "err", merr)
			name = ""
		}
		cache[userID] = name
		return name
	}
	// threadStart feeds the "(continued)" header for threads that began on
	// an earlier day; a failed lookup degrades to the plain header (zero
	// time means unknown) rather than aborting the render.
	threadStarts := make(map[string]time.Time)
	threadStart := func(thread string) time.Time {
		if ts, hit := threadStarts[thread]; hit {
			return ts
		}
		ts, _, terr := c.db.ThreadFirstCreateTime(c.src, f.Space, thread)
		if terr != nil {
			c.log.Warn("gchat: thread start lookup failed", "space", f.Space, "thread", thread, "err", terr)
			ts = time.Time{}
		}
		threadStarts[thread] = ts
		return ts
	}
	skips, err := c.db.SkippedForNote(f.RelPath)
	if err != nil {
		return err
	}
	content := archive.RenderChatDayFor(archive.ChatDay{
		Space:       sp,
		Day:         f.DayBucket,
		Messages:    msgs,
		Attachments: atts,
		Skipped:     skips,
		Disposition: skipDisposition(skips),
		MemberName:  memberName,
		ThreadStart: threadStart,
		TZ:          c.writer.TZ,
	})
	hash, err := c.writer.WriteChatDay(f.RelPath, content)
	if err != nil {
		return err
	}
	return c.db.MarkDayRendered(c.src, f.Space, f.DayBucket, f.DirtySeq, hash)
}

// skipDisposition words a day's refusals truthfully.
//
// Chat's designed path refuses from the message metadata, before the blob is
// ever requested, so the normal answer is SkipBytesNotFetched. But the
// content half of the allowlist can only be applied to bytes in hand, so a
// day may also contain a refusal decided AFTER a download — for those the
// bytes did cross the network and were discarded.
//
// A skip carries a sniffed type exactly when its bytes were fetched, which is
// what distinguishes the two. archive.ChatDay has one disposition for the
// whole file, so a day holding both kinds falls back to
// SkipDispositionUnspecified, which claims nothing about the bytes rather
// than making a claim that is wrong for half the entries. Being vague is
// allowed; being wrong is not.
func skipDisposition(skips []state.SkippedAttachment) archive.SkipDisposition {
	fetched, notFetched := false, false
	for _, s := range skips {
		if s.Resolution == state.SkipResolutionFetched {
			continue // rendered as a link, not as a skip
		}
		if s.SniffedType != "" {
			fetched = true
		} else {
			notFetched = true
		}
	}
	switch {
	case fetched && notFetched:
		return archive.SkipDispositionUnspecified
	case fetched:
		return archive.SkipBytesDiscarded
	default:
		return archive.SkipBytesNotFetched
	}
}
