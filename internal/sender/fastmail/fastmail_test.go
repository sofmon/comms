package fastmail

import (
	"context"
	"testing"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/emailsubmission"
	"git.sr.ht/~rockorager/go-jmap/mail/identity"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"

	"comms/internal/config"
	"comms/internal/outbox"
)

type fakeAPI struct {
	queryCalls  int
	alreadySent bool
	created     *email.Email
	submission  *emailsubmission.Set
}

func (f *fakeAPI) Do(req *jmap.Request) (*jmap.Response, error) {
	resp := &jmap.Response{}
	for _, call := range req.Calls {
		var args any
		switch call.Name {
		case "Mailbox/get":
			args = &mailbox.GetResponse{List: []*mailbox.Mailbox{
				{ID: "drafts", Role: mailbox.RoleDrafts},
				{ID: "sent", Role: mailbox.RoleSent},
			}}
		case "Identity/get":
			args = &identity.GetResponse{List: []*identity.Identity{{ID: "identity", Email: "you@fastmail.example"}}}
		case "Email/query":
			f.queryCalls++
			ids := []jmap.ID(nil)
			if f.alreadySent && f.queryCalls == 1 {
				ids = []jmap.ID{"existing-email"}
			}
			args = &email.QueryResponse{IDs: ids}
		case "Email/set":
			set := call.Args.(*email.Set)
			f.created = set.Create["draft"]
			args = &email.SetResponse{Created: map[jmap.ID]*email.Email{"draft": {ID: "email-1"}}}
		case "EmailSubmission/set":
			f.submission = call.Args.(*emailsubmission.Set)
			args = &emailsubmission.SetResponse{Created: map[jmap.ID]*emailsubmission.EmailSubmission{"submission": {ID: "submission-1"}}}
		default:
			panic("unexpected method " + call.Name)
		}
		resp.Responses = append(resp.Responses, &jmap.Invocation{Name: call.Name, Args: args, CallID: call.CallID})
	}
	return resp, nil
}

func fastmailDraft(t *testing.T) outbox.Draft {
	t.Helper()
	d, err := outbox.Parse("mail.md", []byte("---\ntype: email\naccount: fm\nto: Jane <jane@example.com>\nsubject: Hello\nin_reply_to: <original@example.net>\nreferences: [<older@example.net>, <original@example.net>]\n---\nPlain text\n"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSendCreatesDraftAndSubmission(t *testing.T) {
	api := &fakeAPI{}
	s := newSender(config.FastMailAccount{Account: "you@fastmail.example"}, func(context.Context) (*connection, error) {
		return &connection{api: api, accountID: "account"}, nil
	})
	s.now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
	r, err := s.Send(context.Background(), fastmailDraft(t), s.now())
	if err != nil {
		t.Fatal(err)
	}
	if r.ProviderID != "submission-1" || api.created == nil || api.submission == nil {
		t.Fatalf("receipt=%+v created=%+v submission=%+v", r, api.created, api.submission)
	}
	if api.created.Subject != "Hello" || api.created.From[0].Email != "you@fastmail.example" || api.created.To[0].Email != "jane@example.com" {
		t.Fatalf("created email = %+v", api.created)
	}
	if api.created.BodyValues["body"].Value != "Plain text" || !api.created.MailboxIDs["drafts"] {
		t.Fatalf("created body/mailbox = %+v", api.created)
	}
	if len(api.created.InReplyTo) != 1 || api.created.InReplyTo[0] != "original@example.net" ||
		len(api.created.References) != 2 || api.created.References[0] != "older@example.net" {
		t.Fatalf("created reply headers = %+v", api.created)
	}
	sub := api.submission.Create["submission"]
	if sub.EmailID != "email-1" || sub.IdentityID != "identity" || api.submission.OnSuccessUpdateEmail["#submission"] == nil {
		t.Fatalf("submission = %+v", api.submission)
	}
}

func TestSendReconcilesExistingSentEmail(t *testing.T) {
	api := &fakeAPI{alreadySent: true}
	s := newSender(config.FastMailAccount{Account: "you@fastmail.example"}, func(context.Context) (*connection, error) {
		return &connection{api: api, accountID: "account"}, nil
	})
	r, err := s.Send(context.Background(), fastmailDraft(t), time.Now())
	if err != nil || !r.Reconciled || r.ProviderID != "existing-email" || api.created != nil || api.submission != nil {
		t.Fatalf("reconcile = %+v, %v fake=%+v", r, err, api)
	}
}
