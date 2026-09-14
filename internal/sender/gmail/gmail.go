// Package gmail sends file-backed email through Gmail drafts. A deterministic
// Message-ID lets a retry find either the already-sent message or the draft
// created by an interrupted attempt before doing anything new.
package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"

	gmailv1 "google.golang.org/api/gmail/v1"

	"comms/internal/config"
	"comms/internal/outbox"
)

// Scope manages drafts and sends mail; gmail.send alone cannot create the
// recoverable draft used by this sender's idempotency protocol.
const Scope = gmailv1.GmailComposeScope

const userID = "me"

type api interface {
	findSent(context.Context, string) (*gmailv1.Message, error)
	findDraft(context.Context, string) (*gmailv1.Draft, error)
	createDraft(context.Context, *gmailv1.Draft) (*gmailv1.Draft, error)
	sendDraft(context.Context, *gmailv1.Draft) (*gmailv1.Message, error)
}

type liveAPI struct{ svc *gmailv1.Service }

func (a *liveAPI) findSent(ctx context.Context, msgID string) (*gmailv1.Message, error) {
	resp, err := a.svc.Users.Messages.List(userID).
		Q("in:sent rfc822msgid:" + strings.Trim(msgID, "<>")).
		IncludeSpamTrash(true).
		MaxResults(1).
		Context(ctx).Do()
	if err != nil || len(resp.Messages) == 0 {
		return nil, err
	}
	return resp.Messages[0], nil
}

func (a *liveAPI) findDraft(ctx context.Context, msgID string) (*gmailv1.Draft, error) {
	resp, err := a.svc.Users.Drafts.List(userID).
		Q("rfc822msgid:" + strings.Trim(msgID, "<>")).
		IncludeSpamTrash(true).
		MaxResults(1).
		Context(ctx).Do()
	if err != nil || len(resp.Drafts) == 0 {
		return nil, err
	}
	return resp.Drafts[0], nil
}

func (a *liveAPI) createDraft(ctx context.Context, d *gmailv1.Draft) (*gmailv1.Draft, error) {
	return a.svc.Users.Drafts.Create(userID, d).Context(ctx).Do()
}

func (a *liveAPI) sendDraft(ctx context.Context, d *gmailv1.Draft) (*gmailv1.Message, error) {
	return a.svc.Users.Drafts.Send(userID, d).Context(ctx).Do()
}

// Sender sends as one configured Google account.
type Sender struct {
	acct config.GoogleAccount
	api  api
	now  func() time.Time
}

func New(acct config.GoogleAccount, svc *gmailv1.Service) *Sender {
	return &Sender{acct: acct, api: &liveAPI{svc: svc}, now: time.Now}
}

func newSender(acct config.GoogleAccount, a api) *Sender {
	return &Sender{acct: acct, api: a, now: time.Now}
}

// Send first reconciles by the deterministic RFC Message-ID. If an earlier
// run created only a Gmail draft it reuses that draft; if it already reached
// Sent it returns without another delivery.
func (s *Sender) Send(ctx context.Context, d outbox.Draft, preparedAt time.Time) (outbox.Receipt, error) {
	msgID := messageID(d.MessageKey, s.acct.Account)
	if sent, err := s.api.findSent(ctx, msgID); err != nil {
		return outbox.Receipt{}, fmt.Errorf("gmail: find prior send: %w", err)
	} else if sent != nil {
		return outbox.Receipt{ProviderID: sent.Id, SentAt: internalDate(sent, s.now()), Reconciled: true}, nil
	}

	draft, err := s.api.findDraft(ctx, msgID)
	if err != nil {
		return outbox.Receipt{}, fmt.Errorf("gmail: find prior draft: %w", err)
	}
	if draft == nil {
		raw, err := renderMIME(d, s.acct.Account, msgID, preparedAt)
		if err != nil {
			return outbox.Receipt{}, err
		}
		draft, err = s.api.createDraft(ctx, &gmailv1.Draft{Message: &gmailv1.Message{
			Raw:      base64.RawURLEncoding.EncodeToString(raw),
			ThreadId: d.ThreadID,
		}})
		if err != nil {
			return outbox.Receipt{}, fmt.Errorf("gmail: create draft: %w", err)
		}
	}
	if draft.Id == "" {
		return outbox.Receipt{}, fmt.Errorf("gmail: draft response has no id")
	}
	sent, err := s.api.sendDraft(ctx, &gmailv1.Draft{Id: draft.Id})
	if err != nil {
		return outbox.Receipt{}, fmt.Errorf("gmail: send draft %s: %w", draft.Id, err)
	}
	if sent == nil || sent.Id == "" {
		return outbox.Receipt{}, fmt.Errorf("gmail: send response has no message id")
	}
	return outbox.Receipt{ProviderID: sent.Id, SentAt: internalDate(sent, s.now())}, nil
}

func renderMIME(d outbox.Draft, from, msgID string, at time.Time) ([]byte, error) {
	parsedFrom, err := mail.ParseAddress(from)
	if err != nil || parsedFrom.Address != from {
		return nil, fmt.Errorf("gmail: configured account %q is not a plain email address", from)
	}
	fromAddr := outbox.Address{Name: d.FromName, Email: parsedFrom.Address}.String()
	var b bytes.Buffer
	writeHeader := func(name, value string) {
		fmt.Fprintf(&b, "%s: %s\r\n", name, value)
	}
	writeHeader("From", fromAddr)
	if len(d.To) > 0 {
		writeHeader("To", joinAddresses(d.To))
	}
	if len(d.CC) > 0 {
		writeHeader("Cc", joinAddresses(d.CC))
	}
	if len(d.BCC) > 0 {
		writeHeader("Bcc", joinAddresses(d.BCC))
	}
	if d.ReplyTo != nil {
		writeHeader("Reply-To", d.ReplyTo.String())
	}
	writeHeader("Subject", mime.QEncoding.Encode("utf-8", d.Subject))
	if d.InReplyTo != "" {
		writeHeader("In-Reply-To", d.InReplyTo)
	}
	if len(d.References) > 0 {
		writeHeader("References", strings.Join(d.References, " "))
	}
	writeHeader("Date", at.Format(time.RFC1123Z))
	writeHeader("Message-ID", msgID)
	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", `text/plain; charset="UTF-8"`)
	writeHeader("Content-Transfer-Encoding", "8bit")
	b.WriteString("\r\n")
	b.WriteString(toCRLF(d.Body))
	b.WriteString("\r\n")
	return b.Bytes(), nil
}

func joinAddresses(in []outbox.Address) string {
	out := make([]string, len(in))
	for i, a := range in {
		out[i] = (&mail.Address{Name: a.Name, Address: a.Email}).String()
	}
	return strings.Join(out, ", ")
}

func toCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func messageID(key, account string) string {
	domain := "localhost"
	if _, d, ok := strings.Cut(account, "@"); ok && d != "" {
		domain = d
	}
	return fmt.Sprintf("<comms.%s@%s>", key, domain)
}

func internalDate(m *gmailv1.Message, fallback time.Time) time.Time {
	if m != nil && m.InternalDate > 0 {
		return time.UnixMilli(m.InternalDate)
	}
	return fallback
}
