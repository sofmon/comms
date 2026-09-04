// Package gchat sends Markdown text through Google Chat using both the API's
// requestId deduplication and a deterministic client-assigned message name.
package gchat

import (
	"context"
	"errors"
	"fmt"
	"time"

	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"

	"comms/internal/outbox"
)

const Scope = chat.ChatMessagesCreateScope

type api interface {
	create(context.Context, string, *chat.Message, string, string) (*chat.Message, error)
	get(context.Context, string) (*chat.Message, error)
}

type liveAPI struct{ svc *chat.Service }

func (a *liveAPI) create(ctx context.Context, space string, msg *chat.Message, messageID, requestID string) (*chat.Message, error) {
	call := a.svc.Spaces.Messages.Create(space, msg).
		MessageId(messageID).
		RequestId(requestID).
		Context(ctx)
	if msg.Thread != nil && msg.Thread.Name != "" {
		call = call.MessageReplyOption("REPLY_MESSAGE_OR_FAIL")
	}
	return call.Do()
}

func (a *liveAPI) get(ctx context.Context, name string) (*chat.Message, error) {
	return a.svc.Spaces.Messages.Get(name).Context(ctx).Do()
}

// Sender sends as the user represented by its Google token.
type Sender struct {
	api api
	now func() time.Time
}

func New(svc *chat.Service) *Sender { return &Sender{api: &liveAPI{svc: svc}, now: time.Now} }

func newSender(a api) *Sender { return &Sender{api: a, now: time.Now} }

func (s *Sender) Send(ctx context.Context, d outbox.Draft, _ time.Time) (outbox.Receipt, error) {
	clientID := "client-comms-" + d.MessageKey[:32]
	requestID := uuidFromKey(d.MessageKey)
	msg := &chat.Message{Text: d.Body, MarkupSyntax: "MARKUP_SYNTAX_MARKDOWN"}
	if d.Thread != "" {
		msg.Thread = &chat.Thread{Name: d.Thread}
	}
	got, err := s.api.create(ctx, d.Space, msg, clientID, requestID)
	if err != nil {
		// requestId normally makes a replay return the original message. The
		// deterministic custom name provides a second reconciliation path for
		// deployments that answer an already-created replay with 409.
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != 409 {
			return outbox.Receipt{}, fmt.Errorf("gchat: create message in %s: %w", d.Space, err)
		}
		got, err = s.api.get(ctx, d.Space+"/messages/"+clientID)
		if err != nil {
			return outbox.Receipt{}, fmt.Errorf("gchat: reconcile message in %s: %w", d.Space, err)
		}
		return receipt(got, s.now(), true)
	}
	return receipt(got, s.now(), false)
}

func receipt(m *chat.Message, fallback time.Time, reconciled bool) (outbox.Receipt, error) {
	if m == nil || m.Name == "" {
		return outbox.Receipt{}, fmt.Errorf("gchat: create response has no message name")
	}
	at := fallback
	if m.CreateTime != "" {
		parsed, err := time.Parse(time.RFC3339Nano, m.CreateTime)
		if err != nil {
			return outbox.Receipt{}, fmt.Errorf("gchat: invalid create time %q: %w", m.CreateTime, err)
		}
		at = parsed
	}
	return outbox.Receipt{ProviderID: m.Name, SentAt: at, Reconciled: reconciled}, nil
}

func uuidFromKey(key string) string {
	// Message keys are SHA-256 hex. This UUID-shaped rendering satisfies APIs
	// that validate requestId as a UUID while remaining deterministic.
	return key[0:8] + "-" + key[8:12] + "-5" + key[13:16] + "-a" + key[17:20] + "-" + key[20:32]
}
