package gmail

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	gmailv1 "google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"

	"save/internal/retry"
)

func TestDecideScope(t *testing.T) {
	tests := []struct {
		name          string
		labels        []string
		includeDrafts bool
		want          scopeDecision
	}{
		{"inbox", []string{"INBOX", "UNREAD"}, false, decisionArchive},
		{"no labels", nil, false, decisionArchive},
		{"sent", []string{"SENT"}, false, decisionArchive},
		{"user label only", []string{"Label_7"}, false, decisionArchive},
		{"spam", []string{"SPAM", "UNREAD"}, false, decisionSkipSpamTrash},
		{"trash", []string{"TRASH"}, false, decisionSkipSpamTrash},
		{"chat", []string{"CHAT"}, false, decisionSkipChat},
		{"chat wins over spam", []string{"SPAM", "CHAT"}, false, decisionSkipChat},
		{"chat wins even with drafts on", []string{"CHAT", "DRAFT"}, true, decisionSkipChat},
		{"draft excluded by default", []string{"DRAFT"}, false, decisionSkipDraft},
		{"draft included when configured", []string{"DRAFT"}, true, decisionArchive},
		{"trashed draft is spam-trash skip", []string{"DRAFT", "TRASH"}, true, decisionSkipSpamTrash},
		{"spam wins over draft", []string{"DRAFT", "SPAM"}, false, decisionSkipSpamTrash},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideScope(tt.labels, tt.includeDrafts); got != tt.want {
				t.Errorf("decideScope(%v, %v) = %v, want %v", tt.labels, tt.includeDrafts, got, tt.want)
			}
		})
	}
}

func TestRescueCandidate(t *testing.T) {
	tests := []struct {
		name    string
		removed []string
		want    bool
	}{
		{"spam removed", []string{"SPAM"}, true},
		{"trash removed", []string{"TRASH"}, true},
		{"spam among others", []string{"UNREAD", "SPAM"}, true},
		{"unrelated label churn", []string{"STARRED", "Label_3"}, false},
		{"inbox removed", []string{"INBOX"}, false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rescueCandidate(tt.removed); got != tt.want {
				t.Errorf("rescueCandidate(%v) = %v, want %v", tt.removed, got, tt.want)
			}
		})
	}
}

func TestDecodeRaw(t *testing.T) {
	payload := []byte("Subject: hi\r\n\r\nb\xfbody?>~\r\n") // includes a non-ASCII byte
	tests := []struct {
		name    string
		input   string
		want    []byte
		wantErr bool
	}{
		{"unpadded url-safe", base64.RawURLEncoding.EncodeToString(payload), payload, false},
		{"padded url-safe", base64.URLEncoding.EncodeToString(payload), payload, false},
		{"empty", "", []byte{}, false},
		{"standard alphabet rejected", "a+/c", nil, true},
		{"garbage", "!!!", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeRaw(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decodeRaw(%q) succeeded, want error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeRaw(%q): %v", tt.input, err)
			}
			if string(got) != string(tt.want) {
				t.Errorf("decodeRaw(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
	// Sanity: the padded form must actually exercise the fallback branch.
	padded := base64.URLEncoding.EncodeToString(payload)
	if _, err := base64.RawURLEncoding.DecodeString(padded); err == nil {
		t.Fatalf("test setup: %q should not decode with RawURLEncoding", padded)
	}
}

func added(id string) *gmailv1.History {
	return &gmailv1.History{MessagesAdded: []*gmailv1.HistoryMessageAdded{{Message: &gmailv1.Message{Id: id}}}}
}

func labelRemoved(id string, labels ...string) *gmailv1.History {
	return &gmailv1.History{LabelsRemoved: []*gmailv1.HistoryLabelRemoved{{LabelIds: labels, Message: &gmailv1.Message{Id: id}}}}
}

func deleted(id string) *gmailv1.History {
	return &gmailv1.History{MessagesDeleted: []*gmailv1.HistoryMessageDeleted{{Message: &gmailv1.Message{Id: id}}}}
}

func TestHistoryPlanDedupe(t *testing.T) {
	plan := newHistoryPlan()
	// Page 1: m1 added twice, m2 spam-rescue, m3 deleted.
	plan.addPage([]*gmailv1.History{
		added("m1"),
		added("m1"),
		labelRemoved("m2", "SPAM"),
		labelRemoved("m9", "STARRED"), // not a rescue: no SPAM/TRASH removed
		deleted("m3"),
		nil, // hole in the response must not panic
	})
	// Page 2 repeats everything and adds m4; m2 also re-added.
	plan.addPage([]*gmailv1.History{
		added("m1"),
		added("m2"),
		labelRemoved("m2", "TRASH"),
		deleted("m3"),
		deleted("m3"),
		added("m4"),
		{MessagesAdded: []*gmailv1.HistoryMessageAdded{{Message: nil}, nil}},
	})

	wantFetch := []string{"m1", "m2", "m4"}
	if len(plan.fetch) != len(wantFetch) {
		t.Fatalf("fetch = %v, want %v", plan.fetch, wantFetch)
	}
	for i, id := range wantFetch {
		if plan.fetch[i] != id {
			t.Errorf("fetch[%d] = %q, want %q (order must be first-seen)", i, plan.fetch[i], id)
		}
	}
	if len(plan.deleted) != 1 || plan.deleted[0] != "m3" {
		t.Errorf("deleted = %v, want [m3]", plan.deleted)
	}
	if plan.rescues != 1 {
		t.Errorf("rescues = %d, want 1 (m2 first entered via labelRemoved)", plan.rescues)
	}
}

func TestShouldPromote(t *testing.T) {
	tests := []struct {
		listDone bool
		pending  int64
		want     bool
	}{
		{true, 0, true},
		{true, 3, false},
		{false, 0, false},
		{false, 12, false},
	}
	for _, tt := range tests {
		if got := shouldPromote(tt.listDone, tt.pending); got != tt.want {
			t.Errorf("shouldPromote(%v, %d) = %v, want %v", tt.listDone, tt.pending, got, tt.want)
		}
	}
}

func TestClassifyHistoryErr(t *testing.T) {
	if classifyHistoryErr(nil) != nil {
		t.Error("classifyHistoryErr(nil) should be nil")
	}
	err404 := fmt.Errorf("wrapped: %w", &googleapi.Error{Code: 404})
	if got := retry.Classify(classifyHistoryErr(err404)); got != retry.CursorGone {
		t.Errorf("history 404 classified %v, want CursorGone (expired cursor -> full re-list)", got)
	}
	err500 := &googleapi.Error{Code: 500}
	if got := retry.Classify(classifyHistoryErr(err500)); got != retry.Transient {
		t.Errorf("history 500 classified %v, want Transient", got)
	}
	plain := errors.New("boom")
	if got := classifyHistoryErr(plain); got != plain {
		t.Errorf("non-API error must pass through unchanged, got %v", got)
	}
}

func TestIsBadPageToken(t *testing.T) {
	if !isBadPageToken(fmt.Errorf("list: %w", &googleapi.Error{Code: 400})) {
		t.Error("400 should be a bad page token")
	}
	if isBadPageToken(&googleapi.Error{Code: 404}) {
		t.Error("404 is not a bad page token")
	}
	if isBadPageToken(errors.New("plain")) {
		t.Error("non-API error is not a bad page token")
	}
}

func TestClassifyItemErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want itemVerdict
	}{
		{"404 item gone", &googleapi.Error{Code: 404}, verdictGone},
		{"401 auth aborts run", &googleapi.Error{Code: 401}, verdictRun},
		{"429 quota aborts run", &googleapi.Error{Code: 429}, verdictRun},
		{"500 transient aborts run", &googleapi.Error{Code: 500}, verdictRun},
		{"400 is this item's defect", &googleapi.Error{Code: 400}, verdictItem},
		{"cancellation aborts run", context.Canceled, verdictRun},
		{"local parse failure is item-scoped", errors.New("emailpipe: parse message: bad"), verdictItem},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyItemErr(tt.err); got != tt.want {
				t.Errorf("classifyItemErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
