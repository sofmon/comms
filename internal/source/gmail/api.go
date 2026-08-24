package gmail

import (
	"context"
	"log/slog"
	"net/http"

	gmailv1 "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"save/internal/archive"
	"save/internal/config"
	"save/internal/ratelimit"
	"save/internal/source"
	"save/internal/state"

	"golang.org/x/oauth2"
)

// Scope is the OAuth scope the connector needs. gmail.metadata is not
// enough: it cannot fetch RAW bodies or attachments.
const Scope = "https://www.googleapis.com/auth/gmail.readonly"

// Suggested quota-unit limiter parameters for callers constructing the
// ratelimit.Units this connector shares: a safety-margined ~83 units/s
// under the 6,000 units/min/user cap, with burst comfortably above the
// most expensive single call (messages.get, 20 units).
const (
	UnitsPerSecond = 83
	UnitsBurst     = 100
)

// Documented quota-unit costs per call.
const (
	costGetProfile = 1
	costLabels     = 1
	costHistory    = 2
	costList       = 5
	costGet        = 20
)

// api is the minimal Gmail API surface the connector needs, split out so
// the sync logic is testable without the network.
type api interface {
	GetProfile(ctx context.Context) (*gmailv1.Profile, error)
	// ListMessages pages message ids (500/page) with the API's default
	// Spam/Trash exclusion. pageToken "" starts from the beginning.
	ListMessages(ctx context.Context, pageToken string) (*gmailv1.ListMessagesResponse, error)
	// GetMessageRaw fetches one message with format=raw.
	GetMessageRaw(ctx context.Context, id string) (*gmailv1.Message, error)
	// ListHistory pages history records after startHistoryID, restricted
	// to messageAdded, labelRemoved, and messageDeleted.
	ListHistory(ctx context.Context, startHistoryID uint64, pageToken string) (*gmailv1.ListHistoryResponse, error)
	ListLabels(ctx context.Context) (*gmailv1.ListLabelsResponse, error)
}

// liveAPI implements api against the real Gmail service.
type liveAPI struct {
	svc *gmailv1.Service
}

const userID = "me"

func (a *liveAPI) GetProfile(ctx context.Context) (*gmailv1.Profile, error) {
	return a.svc.Users.GetProfile(userID).Context(ctx).Do()
}

func (a *liveAPI) ListMessages(ctx context.Context, pageToken string) (*gmailv1.ListMessagesResponse, error) {
	call := a.svc.Users.Messages.List(userID).MaxResults(500)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	return call.Context(ctx).Do()
}

func (a *liveAPI) GetMessageRaw(ctx context.Context, id string) (*gmailv1.Message, error) {
	return a.svc.Users.Messages.Get(userID, id).Format("raw").Context(ctx).Do()
}

func (a *liveAPI) ListHistory(ctx context.Context, startHistoryID uint64, pageToken string) (*gmailv1.ListHistoryResponse, error) {
	call := a.svc.Users.History.List(userID).
		StartHistoryId(startHistoryID).
		HistoryTypes("messageAdded", "labelRemoved", "messageDeleted").
		MaxResults(500)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	return call.Context(ctx).Do()
}

func (a *liveAPI) ListLabels(ctx context.Context) (*gmailv1.ListLabelsResponse, error) {
	return a.svc.Users.Labels.List(userID).Context(ctx).Do()
}

// New constructs the Gmail connector for ONE account: instanceID is that
// account's Gmail instance id ("gmail:<label>", from config.Instance.ID),
// acct is its [[google]] block, and ts is that account's own token source
// (googleauth.TokenSource over acct.ClientFilePath/acct.TokenFilePath).
// Callers build one Source per Gmail-enabled account.
//
// Pass WithPolicy(cfg.Policy()) so the operator's [attachments] block takes
// effect; without it the connector applies policy.Default().
func New(ctx context.Context, instanceID string, acct config.GoogleAccount, db *state.DB, writer *archive.Writer, limiter *ratelimit.Units, logger *slog.Logger, ts oauth2.TokenSource, opts ...Option) (*Source, error) {
	o := newOptions(opts)
	// Every Gmail response body is capped, not just messages.get: Go's
	// net/http inflates a gzip response with no ceiling, so an unbounded body
	// anywhere is an unbounded allocation. The token source is layered UNDER
	// the cap so refresh traffic is covered too.
	hc := &http.Client{
		Transport: &oauth2.Transport{
			Source: ts,
			Base:   source.CapResponseBody(http.DefaultTransport, rawBodyCap(o.pol.Settings().MaxMessageBytes)),
		},
	}
	svc, err := gmailv1.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, err
	}
	return newSource(instanceID, acct, db, writer, limiter, logger, &liveAPI{svc: svc}, opts...)
}

// rawBodyCap converts attachments.max_message_bytes into an HTTP response
// ceiling. messages.get?format=raw returns the RFC 822 message base64url
// encoded inside a JSON envelope, so the body runs about 4/3 the message plus
// the envelope. Doubling (plus a megabyte of slack) means a legitimate
// message of exactly max_message_bytes is never truncated, while the
// gzip-inflation hole is still bounded. The EXACT cap is then enforced on the
// decoded bytes in archiveOne, which is the number the operator configured.
//
// max <= 0 means the operator disabled the cap; no wrapper is installed.
func rawBodyCap(max int64) int64 {
	if max <= 0 {
		return 0
	}
	return 2*max + 1<<20
}
