package gchat

import (
	"context"
	"errors"
	"testing"
	"time"

	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"

	"comms/internal/outbox"
)

type fakeAPI struct {
	created   *chat.Message
	createErr error
	got       *chat.Message
	space     string
	msg       *chat.Message
	messageID string
	requestID string
	getName   string
}

func (f *fakeAPI) create(_ context.Context, space string, msg *chat.Message, messageID, requestID string) (*chat.Message, error) {
	f.space, f.msg, f.messageID, f.requestID = space, msg, messageID, requestID
	return f.created, f.createErr
}
func (f *fakeAPI) get(_ context.Context, name string) (*chat.Message, error) {
	f.getName = name
	return f.got, nil
}

func chatDraft(t *testing.T) outbox.Draft {
	t.Helper()
	d, err := outbox.Parse("chat.md", []byte("---\ntype: chat\naccount: work\nspace: spaces/AAA\nthread: spaces/AAA/threads/BBB\n---\n**hello**\n"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSendUsesDeterministicIdsAndThread(t *testing.T) {
	api := &fakeAPI{created: &chat.Message{Name: "spaces/AAA/messages/server", CreateTime: "2026-09-04T12:00:00Z"}}
	r, err := newSender(api).Send(context.Background(), chatDraft(t), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if api.space != "spaces/AAA" || api.msg.Text != "**hello**" || api.msg.MarkupSyntax != "MARKUP_SYNTAX_MARKDOWN" || api.msg.Thread.Name != "spaces/AAA/threads/BBB" {
		t.Fatalf("create = space %q msg %+v", api.space, api.msg)
	}
	if api.messageID != "client-comms-"+chatDraft(t).MessageKey[:32] || len(api.requestID) != 36 {
		t.Fatalf("ids = %q / %q", api.messageID, api.requestID)
	}
	if r.ProviderID != api.created.Name || r.SentAt.IsZero() || r.Reconciled {
		t.Fatalf("receipt = %+v", r)
	}
}

func TestSendReconcilesConflictByCustomName(t *testing.T) {
	api := &fakeAPI{
		createErr: errors.New("wrapped: " + (&googleapi.Error{Code: 409}).Error()),
		got:       &chat.Message{Name: "spaces/AAA/messages/client-existing"},
	}
	// errors.As cannot see an error flattened to text; use a real wrapped API error.
	api.createErr = &wrappedError{err: &googleapi.Error{Code: 409}}
	r, err := newSender(api).Send(context.Background(), chatDraft(t), time.Time{})
	if err != nil || !r.Reconciled || api.getName == "" {
		t.Fatalf("reconcile = %+v, %v, get=%q", r, err, api.getName)
	}
}

type wrappedError struct{ err error }

func (e *wrappedError) Error() string { return "wrapped: " + e.err.Error() }
func (e *wrappedError) Unwrap() error { return e.err }
