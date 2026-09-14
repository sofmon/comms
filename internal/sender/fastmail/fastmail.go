// Package fastmail sends plain-text email through JMAP EmailSubmission. It
// stages each message as a deterministic Message-ID draft, then moves the
// email to Sent as part of successful submission so interrupted attempts can
// be reconciled without duplicate delivery.
package fastmail

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	jmapmail "git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/emailsubmission"
	"git.sr.ht/~rockorager/go-jmap/mail/identity"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"

	"comms/internal/config"
	"comms/internal/outbox"
	"comms/internal/source"
	sourcefastmail "comms/internal/source/fastmail"
)

type api interface {
	Do(*jmap.Request) (*jmap.Response, error)
}

type connection struct {
	api       api
	accountID jmap.ID
}

// Sender sends as one configured FastMail account.
type Sender struct {
	acct    config.FastMailAccount
	tokens  sourcefastmail.TokenFunc
	connect func(context.Context) (*connection, error)
	now     func() time.Time
}

func New(acct config.FastMailAccount, tokens sourcefastmail.TokenFunc) *Sender {
	s := &Sender{acct: acct, tokens: tokens, now: time.Now}
	s.connect = s.connectLive
	return s
}

func newSender(acct config.FastMailAccount, connect func(context.Context) (*connection, error)) *Sender {
	return &Sender{acct: acct, connect: connect, now: time.Now}
}

func (s *Sender) connectLive(ctx context.Context) (*connection, error) {
	token, err := s.tokens()
	if err != nil {
		return nil, err
	}
	cl := (&jmap.Client{SessionEndpoint: sourcefastmail.SessionEndpoint}).WithAccessToken(token)
	if cl.HttpClient == nil {
		cl.HttpClient = &http.Client{}
	}
	cl.HttpClient.Transport = source.CapResponseBody(cl.HttpClient.Transport, 16<<20)
	if deadline, ok := ctx.Deadline(); ok {
		cl.HttpClient.Timeout = time.Until(deadline)
	} else {
		cl.HttpClient.Timeout = 30 * time.Second
	}
	if err := cl.Authenticate(); err != nil {
		return nil, fmt.Errorf("fastmail: authenticate: %w", err)
	}
	if _, ok := cl.Session.RawCapabilities[emailsubmission.URI]; !ok {
		return nil, fmt.Errorf("fastmail: token/session does not expose EmailSubmission; create a JMAP token with write and send access")
	}
	accountID, ok := cl.Session.PrimaryAccounts[jmapmail.URI]
	if !ok || accountID == "" {
		return nil, fmt.Errorf("fastmail: session has no primary mail account")
	}
	return &connection{api: cl, accountID: accountID}, nil
}

// Check verifies that this token exposes JMAP submission without creating or
// changing any mail.
func (s *Sender) Check(ctx context.Context) error {
	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	_, _, err = s.context(ctx, c)
	return err
}

func (s *Sender) Send(ctx context.Context, d outbox.Draft, preparedAt time.Time) (outbox.Receipt, error) {
	c, err := s.connect(ctx)
	if err != nil {
		return outbox.Receipt{}, err
	}
	boxes, ident, err := s.context(ctx, c)
	if err != nil {
		return outbox.Receipt{}, err
	}
	msgID := messageID(d.MessageKey, s.acct.Account)
	if id, err := queryOne(ctx, c, boxes.sent, msgID); err != nil {
		return outbox.Receipt{}, err
	} else if id != "" {
		return outbox.Receipt{ProviderID: string(id), SentAt: s.now(), Reconciled: true}, nil
	}

	emailID, err := queryOne(ctx, c, boxes.drafts, msgID)
	if err != nil {
		return outbox.Receipt{}, err
	}
	if emailID == "" {
		emailID, err = createDraft(ctx, c, d, s.acct.Account, boxes.drafts, msgID, preparedAt)
		if err != nil {
			return outbox.Receipt{}, err
		}
	}
	submissionID, err := submit(ctx, c, emailID, ident.ID, boxes)
	if err != nil {
		return outbox.Receipt{}, err
	}
	return outbox.Receipt{ProviderID: string(submissionID), SentAt: s.now()}, nil
}

type sendContext struct {
	drafts jmap.ID
	sent   jmap.ID
}

func (s *Sender) context(ctx context.Context, c *connection) (sendContext, *identity.Identity, error) {
	req := &jmap.Request{Context: ctx}
	mbCall := req.Invoke(&mailbox.Get{Account: c.accountID, Properties: []string{"id", "role", "myRights"}})
	idCall := req.Invoke(&identity.Get{Account: c.accountID, Properties: []string{"id", "name", "email"}})
	resp, err := c.api.Do(req)
	if err != nil {
		return sendContext{}, nil, fmt.Errorf("fastmail: load send context: %w", err)
	}
	mb, err := unpack[mailbox.GetResponse](resp, mbCall)
	if err != nil {
		return sendContext{}, nil, err
	}
	var boxes sendContext
	for _, m := range mb.List {
		if m == nil {
			continue
		}
		switch m.Role {
		case mailbox.RoleDrafts:
			boxes.drafts = m.ID
		case mailbox.RoleSent:
			boxes.sent = m.ID
		}
	}
	if boxes.drafts == "" || boxes.sent == "" {
		return sendContext{}, nil, fmt.Errorf("fastmail: account must expose both Drafts and Sent mailboxes")
	}
	ids, err := unpack[identity.GetResponse](resp, idCall)
	if err != nil {
		return sendContext{}, nil, err
	}
	for _, id := range ids.List {
		if id != nil && strings.EqualFold(id.Email, s.acct.Account) {
			return boxes, id, nil
		}
	}
	return sendContext{}, nil, fmt.Errorf("fastmail: no sending identity matches configured account %s", s.acct.Account)
}

func queryOne(ctx context.Context, c *connection, box jmap.ID, msgID string) (jmap.ID, error) {
	req := &jmap.Request{Context: ctx}
	call := req.Invoke(&email.Query{
		Account: c.accountID,
		Filter:  &email.FilterCondition{InMailbox: box, Header: []string{"Message-ID", msgID}},
		Limit:   1,
	})
	resp, err := c.api.Do(req)
	if err != nil {
		return "", fmt.Errorf("fastmail: query prior message: %w", err)
	}
	qr, err := unpack[email.QueryResponse](resp, call)
	if err != nil {
		return "", err
	}
	if len(qr.IDs) == 0 {
		return "", nil
	}
	return qr.IDs[0], nil
}

func createDraft(ctx context.Context, c *connection, d outbox.Draft, from string, drafts jmap.ID, msgID string, at time.Time) (jmap.ID, error) {
	const creationID jmap.ID = "draft"
	bodyPartID := "body"
	msg := &email.Email{
		MailboxIDs: map[jmap.ID]bool{drafts: true},
		Keywords:   map[string]bool{"$draft": true},
		MessageID:  []string{strings.Trim(msgID, "<>")},
		InReplyTo:  messageIDs(d.InReplyTo),
		References: messageIDs(d.References...),
		From:       []*jmapmail.Address{{Name: d.FromName, Email: from}},
		To:         addresses(d.To),
		CC:         addresses(d.CC),
		BCC:        addresses(d.BCC),
		Subject:    d.Subject,
		SentAt:     &at,
		BodyStructure: &email.BodyPart{
			PartID:  bodyPartID,
			Type:    "text/plain",
			Charset: "utf-8",
		},
		BodyValues: map[string]*email.BodyValue{bodyPartID: {Value: d.Body}},
	}
	if d.ReplyTo != nil {
		msg.ReplyTo = addresses([]outbox.Address{*d.ReplyTo})
	}
	req := &jmap.Request{Context: ctx}
	call := req.Invoke(&email.Set{Account: c.accountID, Create: map[jmap.ID]*email.Email{creationID: msg}})
	resp, err := c.api.Do(req)
	if err != nil {
		return "", fmt.Errorf("fastmail: create draft: %w", err)
	}
	sr, err := unpack[email.SetResponse](resp, call)
	if err != nil {
		return "", err
	}
	if setErr := sr.NotCreated[creationID]; setErr != nil {
		return "", fmt.Errorf("fastmail: create draft: %s", describeSetError(setErr))
	}
	created := sr.Created[creationID]
	if created == nil || created.ID == "" {
		return "", fmt.Errorf("fastmail: create draft response has no email id")
	}
	return created.ID, nil
}

func submit(ctx context.Context, c *connection, emailID, identityID jmap.ID, boxes sendContext) (jmap.ID, error) {
	const creationID jmap.ID = "submission"
	patch := jmap.Patch{
		"mailboxIds/" + pointerToken(string(boxes.drafts)): nil,
		"mailboxIds/" + pointerToken(string(boxes.sent)):   true,
		"keywords/$draft": nil,
		"keywords/$seen":  true,
	}
	req := &jmap.Request{Context: ctx}
	call := req.Invoke(&emailsubmission.Set{
		Account: c.accountID,
		Create: map[jmap.ID]*emailsubmission.EmailSubmission{
			creationID: {IdentityID: identityID, EmailID: emailID},
		},
		OnSuccessUpdateEmail: map[jmap.ID]jmap.Patch{"#" + creationID: patch},
	})
	resp, err := c.api.Do(req)
	if err != nil {
		return "", fmt.Errorf("fastmail: submit email: %w", err)
	}
	sr, err := unpack[emailsubmission.SetResponse](resp, call)
	if err != nil {
		return "", err
	}
	if setErr := sr.NotCreated[creationID]; setErr != nil {
		return "", fmt.Errorf("fastmail: submit email: %s", describeSetError(setErr))
	}
	created := sr.Created[creationID]
	if created == nil || created.ID == "" {
		return "", fmt.Errorf("fastmail: submission response has no id")
	}
	return created.ID, nil
}

func messageIDs(ids ...string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.Trim(id, "<>"); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func addresses(in []outbox.Address) []*jmapmail.Address {
	out := make([]*jmapmail.Address, len(in))
	for i, a := range in {
		out[i] = &jmapmail.Address{Name: a.Name, Email: a.Email}
	}
	return out
}

func messageID(key, account string) string {
	domain := "localhost"
	if _, d, ok := strings.Cut(account, "@"); ok && d != "" {
		domain = d
	}
	return fmt.Sprintf("<comms.%s@%s>", key, domain)
}

func pointerToken(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}

func describeSetError(e *jmap.SetError) string {
	if e.Description != nil && *e.Description != "" {
		return e.Type + ": " + *e.Description
	}
	return e.Type
}

func unpack[T any](resp *jmap.Response, callID string) (*T, error) {
	for _, inv := range resp.Responses {
		if inv.CallID != callID {
			continue
		}
		if methodErr, ok := inv.Args.(*jmap.MethodError); ok {
			return nil, fmt.Errorf("fastmail: %s", methodErr)
		}
		got, ok := inv.Args.(*T)
		if !ok {
			return nil, fmt.Errorf("fastmail: unexpected response type %T for call %s", inv.Args, callID)
		}
		return got, nil
	}
	return nil, fmt.Errorf("fastmail: no response for call %s", callID)
}
