package gmail

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	gmailv1 "google.golang.org/api/gmail/v1"

	"comms/internal/config"
	"comms/internal/outbox"
)

type fakeAPI struct {
	sent          *gmailv1.Message
	draft         *gmailv1.Draft
	createdRaw    string
	createdThread string
	createCalls   int
	sendCalls     int
	err           error
}

func (f *fakeAPI) findSent(context.Context, string) (*gmailv1.Message, error) {
	return f.sent, f.err
}
func (f *fakeAPI) findDraft(context.Context, string) (*gmailv1.Draft, error) { return f.draft, nil }
func (f *fakeAPI) createDraft(_ context.Context, d *gmailv1.Draft) (*gmailv1.Draft, error) {
	f.createCalls++
	f.createdRaw = d.Message.Raw
	f.createdThread = d.Message.ThreadId
	return &gmailv1.Draft{Id: "draft-1"}, nil
}
func (f *fakeAPI) sendDraft(_ context.Context, d *gmailv1.Draft) (*gmailv1.Message, error) {
	f.sendCalls++
	if d.Id == "" {
		return nil, errors.New("missing draft id")
	}
	return &gmailv1.Message{Id: "message-1", InternalDate: 1_800_000_000_000}, nil
}

func emailDraft(t *testing.T) outbox.Draft {
	t.Helper()
	d, err := outbox.Parse("hello.md", []byte(`---
type: email
account: work
to: Jane Example <jane@example.com>
cc: boss@example.net
subject: Héllo
from_name: Sender Name
in_reply_to: <original@example.net>
references:
  - <older@example.net>
  - <original@example.net>
thread_id: gmail-thread-123
---
Hello,

World.
`))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSendCreatesRecoverableDraftThenSends(t *testing.T) {
	api := &fakeAPI{}
	s := newSender(config.GoogleAccount{Account: "you@example.com"}, api)
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	receipt, err := s.Send(context.Background(), emailDraft(t), at)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ProviderID != "message-1" || api.createCalls != 1 || api.sendCalls != 1 || api.createdThread != "gmail-thread-123" {
		t.Fatalf("receipt=%+v creates=%d sends=%d", receipt, api.createCalls, api.sendCalls)
	}
	raw, err := base64.RawURLEncoding.DecodeString(api.createdRaw)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		"From: \"Sender Name\" <you@example.com>\r\n",
		"To: \"Jane Example\" <jane@example.com>\r\n",
		"Cc: <boss@example.net>\r\n",
		"Subject: =?utf-8?q?H=C3=A9llo?=\r\n",
		"In-Reply-To: <original@example.net>\r\n",
		"References: <older@example.net> <original@example.net>\r\n",
		"Message-ID: <comms.",
		"Hello,\r\n\r\nWorld.\r\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("raw message lacks %q:\n%s", want, text)
		}
	}
}

func TestSendReconcilesSentOrExistingDraft(t *testing.T) {
	d := emailDraft(t)
	at := time.Now()

	already := &fakeAPI{sent: &gmailv1.Message{Id: "already", InternalDate: 1234}}
	r, err := newSender(config.GoogleAccount{Account: "you@example.com"}, already).Send(context.Background(), d, at)
	if err != nil || !r.Reconciled || r.ProviderID != "already" || already.createCalls != 0 || already.sendCalls != 0 {
		t.Fatalf("sent reconciliation = %+v, %v, fake=%+v", r, err, already)
	}

	staged := &fakeAPI{draft: &gmailv1.Draft{Id: "existing"}}
	r, err = newSender(config.GoogleAccount{Account: "you@example.com"}, staged).Send(context.Background(), d, at)
	if err != nil || staged.createCalls != 0 || staged.sendCalls != 1 || r.ProviderID != "message-1" {
		t.Fatalf("draft recovery = %+v, %v, fake=%+v", r, err, staged)
	}
}
