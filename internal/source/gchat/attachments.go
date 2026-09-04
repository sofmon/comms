package gchat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"comms/internal/archive"
	"comms/internal/policy"
	"comms/internal/retry"
	"comms/internal/state"
)

const (
	attachmentBatch       = 200
	maxAttachmentAttempts = 10
	attachmentBaseDelay   = time.Minute
	attachmentMaxDelay    = 6 * time.Hour
)

// attachmentRetryDelay is the backoff schedule for attachment downloads:
// exponential from 1m, doubling per recorded attempt, capped at 6h.
func attachmentRetryDelay(attempts int) time.Duration {
	d := attachmentBaseDelay
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= attachmentMaxDelay {
			return attachmentMaxDelay
		}
	}
	return d
}

// downloadAttachments drains this account's due pending downloads registered
// by message ingestion. Each blob is fetched via media.download and written
// atomically before its row is marked done; a completed download marks its
// day dirty so the day file's "[unavailable]" marker becomes a link on this
// run's render pass. Per-blob failures reschedule with backoff (or fail
// permanently); only source-level errors abort.
func (c *Connector) downloadAttachments(ctx context.Context) error {
	for {
		rows, err := c.dueAttachments()
		if err != nil {
			return err
		}
		processed := 0
		for _, a := range rows {
			if c.drive == nil && strings.HasPrefix(a.PartKey, archive.DrivePartKeyPrefix) {
				// Row registered while mirror_drive_files was on, option now
				// off: left pending, resumed when mirroring is re-enabled.
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			processed++
			if err := c.downloadOne(ctx, a); err != nil {
				return err
			}
		}
		if processed == 0 || len(rows) < attachmentBatch {
			return nil
		}
	}
}

// dueAttachments returns up to attachmentBatch of THIS account's due
// attachment rows. The pending-download ledger is shared by every configured
// account, so the query is scoped to this instance: a sibling account
// mid-backfill must never fill the window with rows this connector may not
// touch, which would starve this account's downloads.
func (c *Connector) dueAttachments() ([]state.Attachment, error) {
	return c.db.DueAttachments(c.src, c.now(), attachmentBatch)
}

// downloadOne fetches one blob. It returns an error only for source-level
// failures; per-item outcomes are recorded on the attachment row.
func (c *Connector) downloadOne(ctx context.Context, a state.Attachment) error {
	space := spaceOfMessage(a.StableID)
	if err := c.limiter.Wait(ctx, space); err != nil {
		return err
	}
	var data []byte
	err := retry.Do(ctx, c.retryOpts, func() error {
		b, ferr := c.fetchAttachmentData(ctx, a)
		if ferr != nil {
			return ferr
		}
		data = b
		return nil
	})
	if err == nil {
		// The authoritative decision, over the real bytes. PreCheck passing
		// before the download was explicitly NOT an authorization to store:
		// it could not see the content, and it could not see the size either
		// (Chat publishes neither before the fetch). This is where the
		// content half of the allowlist and attachments.chat_max_size are
		// actually applied.
		stored, derr := c.admitDownloaded(a, data)
		if derr != nil {
			return derr
		}
		if !stored {
			return nil // recorded as a skip; the row is terminal, not retried
		}
		// File first; the done row commits only after the rename landed. An
		// archive-write failure is environmental (disk trouble), never item
		// poison: it aborts the pass without consuming a retry attempt, and
		// the still-pending row is retried next run.
		//
		// WriteChatAttachment, not WriteChatDay: these bytes arrived from
		// outside and carry the macOS provenance tags (a consent prompt and
		// Office Protected View — not a malware scan).
		if _, werr := c.writer.WriteChatAttachment(a.RelPath, data, archive.Origin{
			Source: c.src,
			Ref:    a.StableID,
		}); werr != nil {
			return werr
		}
		c.runBytes += int64(len(data))
	}
	if err != nil {
		if isRunFatal(ctx, err) {
			return err
		}
		attempts := a.Attempts + 1
		if errors.Is(err, errDriveNotExportable) || retry.Classify(err) == retry.ItemGone || attempts >= maxAttachmentAttempts {
			c.log.Error("gchat: attachment permanently failed", "message", a.StableID, "rel", a.RelPath, "attempts", attempts, "err", err)
			return c.db.MarkAttachmentFailed(a.Source, a.StableID, a.PartKey, err.Error(), attempts)
		}
		next := c.now().Add(attachmentRetryDelay(attempts))
		c.log.Warn("gchat: attachment download failed; retry scheduled", "message", a.StableID, "rel", a.RelPath, "attempts", attempts, "next_retry", next, "err", err)
		return c.db.MarkAttachmentRetry(a.Source, a.StableID, a.PartKey, err.Error(), attempts, next)
	}
	// Done mark and day dirtying commit as one transaction: a crash between
	// them could otherwise leave the day file showing "[unavailable]" for a
	// blob that is on disk, with nothing ever re-rendering it.
	return c.db.MarkAttachmentDoneAndDirtyDay(a.Source, a.StableID, a.PartKey, int64(len(data)))
}

// admitDownloaded puts a freshly downloaded blob to the policy and, when it
// is refused, records the refusal and closes the ledger row. It reports
// whether the bytes may be written.
//
// The pre-download PreCheck could only apply the extension allowlist: Chat
// exposes no size and, of course, no content before the fetch. Everything
// else is decided here — the sniffed content type, attachments.chat_max_size,
// the run budget and the free-space floor — over bytes that really exist.
//
// A refusal produces two writes, in this order:
//
//  1. the skipped_attachments row, which is the durable record and the thing
//     `comms refetch` re-decides later. Unlike a pre-download refusal it
//     carries the sniffed type AND the content hash, because the bytes were
//     in hand;
//  2. the ledger row marked failed, which stops the download being retried
//     forever and (via the dirty day) makes the day file re-render.
//
// The row is marked failed rather than deleted or marked done because those
// are the only three states the ledger has, and the other two would lie: the
// blob is not on disk. Its last_error names the policy, so `comms status` does
// not read as a network problem. The day file shows both the ledger row's
// "attachment unavailable" line and the skip's full explanation — redundant,
// but never contradictory, and the explanation is the one that matters.
func (c *Connector) admitDownloaded(a state.Attachment, data []byte) (stored bool, err error) {
	v := c.pol.Decide(policy.Input{
		Name:          path.Base(a.RelPath),
		Content:       data,
		Chat:          true,
		RunBytesSoFar: c.runBytes,
		FreeSpace:     c.freeSpace(c.writer.Root),
	})
	if v.Store {
		if v.Warned() {
			c.log.Warn("gchat: attachment stored under protest",
				"message", a.StableID, "rel", a.RelPath, "reason", v.Reason, "detail", v.Detail)
		}
		return true, nil
	}

	space := spaceOfMessage(a.StableID)
	day, ok := dayFromRelPath(a.RelPath)
	if !ok {
		// The rel path is generated by this package and always carries the
		// day directory; if it somehow does not, the skip has no note to
		// belong to, which is precisely the silent drop the design forbids.
		return false, storeFatal(fmt.Errorf("gchat: attachment %s/%s: cannot derive the day bucket from rel path %q, so its refusal has no note to record against",
			a.StableID, a.PartKey, a.RelPath))
	}
	note, nerr := c.dayNote(space, day)
	if nerr != nil {
		return false, nerr
	}
	sum := sha256.Sum256(data)
	base := path.Base(a.RelPath)
	row := state.SkippedAttachment{
		Source:        c.src,
		StableID:      a.StableID,
		PartKey:       a.PartKey,
		OrigName:      chatAttachmentOrigName(a.RelPath),
		SanitizedName: base,
		SizeBytes:     int64(len(data)),
		// The extension the decision keyed on — from the basename, which is
		// the name that would have landed on disk.
		DeclaredExt:   policy.NormalizeExt(base),
		SniffedType:   v.SniffedType,
		Reason:        v.Reason,
		PolicyDigest:  c.pol.PolicyDigest(),
		ContentSHA256: hex.EncodeToString(sum[:]),
		NoteRelPath:   note,
		DayBucket:     day,
	}
	if err := c.recordSkips([]state.SkippedAttachment{row}); err != nil {
		return false, err
	}
	c.log.Warn("gchat: downloaded attachment refused by policy; bytes discarded",
		"message", a.StableID, "rel", a.RelPath, "bytes", len(data),
		"sniffed", v.SniffedType, "reason", v.Reason, "detail", v.Detail)
	if err := c.db.MarkAttachmentFailed(a.Source, a.StableID, a.PartKey,
		"refused by the attachment policy: "+v.Reason+" — "+v.Detail, a.Attempts+1); err != nil {
		return false, storeFatal(err)
	}
	// MarkAttachmentFailed does not dirty the day (a failed download changes
	// no rendered content on its own), but a new skip does — without this the
	// entry would not appear until something else touched the day.
	if err := c.db.MarkDayDirty(c.src, space, day); err != nil {
		return false, storeFatal(err)
	}
	return false, nil
}

// dayNote resolves the conversation-day file a skip belongs to, from the
// space's FROZEN identity in the state DB — never from the API's current
// display name, which a rename would have moved.
func (c *Connector) dayNote(space, day string) (string, error) {
	sp, ok, err := c.db.GetSpace(c.src, space)
	if err != nil {
		return "", storeFatal(err)
	}
	if !ok {
		return "", storeFatal(fmt.Errorf("gchat: space %q is not registered for %s", space, c.src))
	}
	local, err := time.ParseInLocation("2006-01-02", day, c.writer.TZ)
	if err != nil {
		return "", fmt.Errorf("gchat: bad day bucket %q: %w", day, err)
	}
	return dayNoteRel(c.tag, sp.Type, sp.DisplaySlug, space, local), nil
}

// chatAttachmentOrigName recovers the sender's filename from a chat
// attachment rel path by stripping the deterministic "HHMMSS_msghash8_"
// prefix — the same transformation the renderer applies. It is a
// reconstruction, not the original header: by download time the API message
// is no longer in hand.
func chatAttachmentOrigName(rel string) string {
	base := path.Base(rel)
	if m := chatAttPrefixRe.FindString(base); m != "" && len(base) > len(m) {
		return base[len(m):]
	}
	return base
}

// chatAttPrefixRe matches the "HHMMSS_msghash8_" prefix that
// naming.ChatAttachmentName puts on every chat attachment file.
var chatAttPrefixRe = regexp.MustCompile(`^[0-9]{6}_[0-9a-f]{8}_`)

// fetchAttachmentData fetches one attachment's bytes: "drive:" part keys go
// to the Drive API (see fetchDriveFile), everything else to Chat
// media.download.
func (c *Connector) fetchAttachmentData(ctx context.Context, a state.Attachment) ([]byte, error) {
	if fileID, ok := strings.CutPrefix(a.PartKey, archive.DrivePartKeyPrefix); ok {
		return c.fetchDriveFile(ctx, fileID)
	}
	return readAll(c.api.downloadMedia(ctx, a.PartKey))
}
