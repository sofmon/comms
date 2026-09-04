package gmail

import (
	"context"
	"encoding/base64"
	"errors"

	gmailv1 "google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"

	"comms/internal/retry"
)

// Gmail system label ids that drive scope decisions.
const (
	labelSpam  = "SPAM"
	labelTrash = "TRASH"
	labelChat  = "CHAT"
	labelDraft = "DRAFT"
)

// scopeDecision is the archive/skip verdict for a fetched message's labels.
type scopeDecision int

const (
	// decisionArchive: the message is in scope; archive it.
	decisionArchive scopeDecision = iota
	// decisionSkipSpamTrash: currently in Spam or Trash — skip WITHOUT
	// marking archived, so a later spam-rescue can still pick it up.
	decisionSkipSpamTrash
	// decisionSkipChat: legacy Hangouts artifact — always skipped (these
	// break format=raw fetching and are not email).
	decisionSkipChat
	// decisionSkipDraft: a draft while the account's include_drafts is false.
	decisionSkipDraft
)

func (d scopeDecision) String() string {
	switch d {
	case decisionArchive:
		return "archive"
	case decisionSkipSpamTrash:
		return "skip-spam-trash"
	case decisionSkipChat:
		return "skip-chat"
	case decisionSkipDraft:
		return "skip-draft"
	}
	return "unknown"
}

// decideScope applies the scope rules to a fetched message's current label
// ids. Precedence: CHAT (always out) > SPAM/TRASH (out for now, rescuable)
// > DRAFT (config-gated).
func decideScope(labelIDs []string, includeDrafts bool) scopeDecision {
	var spamTrash, draft bool
	for _, id := range labelIDs {
		switch id {
		case labelChat:
			return decisionSkipChat
		case labelSpam, labelTrash:
			spamTrash = true
		case labelDraft:
			draft = true
		}
	}
	if spamTrash {
		return decisionSkipSpamTrash
	}
	if draft && !includeDrafts {
		return decisionSkipDraft
	}
	return decisionArchive
}

// rescueCandidate reports whether a labelRemoved history record can move a
// message into scope: only losing SPAM or TRASH changes eligibility, so
// other label churn never costs a fetch.
func rescueCandidate(removedLabelIDs []string) bool {
	for _, id := range removedLabelIDs {
		if id == labelSpam || id == labelTrash {
			return true
		}
	}
	return false
}

// decodeRaw decodes Gmail's base64url message payload. The API documents
// the URL-safe alphabet, normally unpadded; some responses arrive padded,
// which RawURLEncoding rejects, so padded input falls back to URLEncoding.
func decodeRaw(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		return b, nil
	}
	if b, perr := base64.URLEncoding.DecodeString(s); perr == nil {
		return b, nil
	}
	return nil, err
}

// historyPlan is the deduplicated action set accumulated from history
// pages. History records may repeat message ids across (and within) pages;
// each id is fetched or tombstoned at most once.
type historyPlan struct {
	fetch     []string // messageAdded + spam/trash-rescue candidates, first-seen order
	deleted   []string // messageDeleted ids, first-seen order
	fetchSeen map[string]bool
	delSeen   map[string]bool
	rescues   int // fetch entries that came from labelRemoved records
}

func newHistoryPlan() *historyPlan {
	return &historyPlan{fetchSeen: make(map[string]bool), delSeen: make(map[string]bool)}
}

func (p *historyPlan) addFetch(id string) bool {
	if id == "" || p.fetchSeen[id] {
		return false
	}
	p.fetchSeen[id] = true
	p.fetch = append(p.fetch, id)
	return true
}

// addPage folds one history page into the plan.
func (p *historyPlan) addPage(records []*gmailv1.History) {
	for _, h := range records {
		if h == nil {
			continue
		}
		for _, a := range h.MessagesAdded {
			if a != nil && a.Message != nil {
				p.addFetch(a.Message.Id)
			}
		}
		for _, r := range h.LabelsRemoved {
			if r == nil || r.Message == nil || !rescueCandidate(r.LabelIds) {
				continue
			}
			if p.addFetch(r.Message.Id) {
				p.rescues++
			}
		}
		for _, d := range h.MessagesDeleted {
			if d == nil || d.Message == nil || d.Message.Id == "" || p.delSeen[d.Message.Id] {
				continue
			}
			p.delSeen[d.Message.Id] = true
			p.deleted = append(p.deleted, d.Message.Id)
		}
	}
}

// shouldPromote reports whether the backfill is complete: enumeration
// finished AND the queue fully drained. Only then may the bootstrap
// history id be promoted to the incremental cursor.
func shouldPromote(listDone bool, pending int64) bool {
	return listDone && pending == 0
}

// classifyHistoryErr reinterprets history.list failures: a 404 means the
// start history id fell out of retention (cursor gone → full re-list), not
// that an item vanished. Everything else passes through unchanged.
func classifyHistoryErr(err error) error {
	if err == nil {
		return nil
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == 404 {
		return retry.MarkCursorGone(err)
	}
	return err
}

// isBadPageToken reports whether err is the API rejecting a stale or
// invalid messages.list page token (HTTP 400). List page tokens are
// transient; a persisted one may not survive to the next run.
func isBadPageToken(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == 400
}

// itemVerdict classifies a per-message processing failure (already through
// retry.Do) into what the caller must do with it.
type itemVerdict int

const (
	// verdictGone: the message vanished (404) — treat as handled, never
	// run-fatal.
	verdictGone itemVerdict = iota
	// verdictItem: a defect of this one message (unparseable, bad
	// encoding, rejected request) — record in the failures ledger and
	// move on; the run continues.
	verdictItem
	// verdictRun: an environment failure (auth broken, quota exhausted
	// after retries, network down, shutdown) — abort the run; recording
	// it against the message would poison healthy items.
	verdictRun
)

func classifyItemErr(err error) itemVerdict {
	if errors.Is(err, context.Canceled) {
		return verdictRun
	}
	switch retry.Classify(err) {
	case retry.ItemGone:
		return verdictGone
	case retry.AuthBroken, retry.Transient, retry.RateLimited:
		return verdictRun
	default:
		return verdictItem
	}
}
