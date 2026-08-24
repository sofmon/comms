// Package fastmail archives one configured FastMail account over JMAP
// (RFC 8620/8621) using a read-only bearer token.
//
// One Source is one account: it is constructed with that account's instance
// id ("fastmail:<label>"), which keys every cursor, message, attachment and
// failure row it writes, and whose file tag ("fastmail-<label>") names the
// account in every filename. Two accounts therefore share nothing but the
// database file.
//
// Every run authenticates by fetching the session resource and discovers
// the API URL, primary mail account id, and blob download template from it
// (hostnames are never hardcoded beyond the fixed session endpoint), then
// maps mailbox ids to names and junk/trash roles. With no committed Email
// state the connector backfills: Email/query sorted by receivedAt
// ascending, filtered to mailboxes other than junk/trash, anchor-paged
// with the anchor persisted after every fully-processed page; the object
// state captured from the first Email/get response is promoted to the
// incremental cursor only when the enumeration completes. Incremental runs
// walk Email/changes: created emails in scope are archived, updated but
// unarchived ids are re-evaluated (junk rescue), and updates/destroys on
// archived ids touch only the DB (files are immutable).
//
// Write ordering follows the archive law: files first, DB row second,
// cursor last. Item failures go through the state failures ledger and
// never abort a run; only auth, cancellation, store, and
// network-after-retries errors do.
package fastmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/core"
	"git.sr.ht/~rockorager/go-jmap/mail"

	"save/internal/archive"
	"save/internal/config"
	"save/internal/policy"
	"save/internal/ratelimit"
	"save/internal/retry"
	"save/internal/source"
	"save/internal/state"
)

var _ source.Source = (*Source)(nil)

// SessionEndpoint is FastMail's fixed JMAP session resource. It is the only
// URL this connector knows a priori; the API and download URLs always come
// from the session object it returns.
const SessionEndpoint = "https://api.fastmail.com/jmap/session"

const (
	// defaultMaxCalls is assumed when the session's core capability is
	// missing or unparseable. The connector never chains more than two
	// calls (Email/query + Email/get), so 2 is enough and conservative.
	defaultMaxCalls = 2
	// fallbackBodyCap bounds maxBodyValueBytes in the bodyValues fallback.
	fallbackBodyCap = 1 << 20
	// maxSessionBytes bounds the session document read.
	maxSessionBytes = 1 << 22
)

// Cursor kinds under (source=<this account's instance id>, scope="").
const (
	cursorEmailState     = "email_state"           // committed Email object state for Email/changes
	cursorBackfillAnchor = "backfill_anchor"       // last email id of the last fully-processed page
	cursorBootstrapState = "bootstrap_email_state" // state captured from the first backfill response
)

// Tunables sized per the plan (~200); variables so tests can shrink them.
var (
	backfillPageLimit = 200
	changesBatchLimit = uint64(200)
	getChunkSize      = 200
)

var (
	retryOpts      = retry.Options{}
	checkRetryOpts = retry.Options{MaxAttempts: 2, Budget: 30 * time.Second}
)

// TokenFunc returns the current bearer token. It is called once per run so
// a rotated token file takes effect without restarting the daemon.
type TokenFunc func() (string, error)

// FileTokenSource returns a TokenFunc that yields literal when non-empty
// (this account's $SAVE_FASTMAIL_TOKEN[_<LABEL>] override, resolved into
// FastMailAccount.Token) and otherwise reads path — the account's per-label
// token file — trimming surrounding whitespace.
func FileTokenSource(literal, path string) TokenFunc {
	return func() (string, error) {
		if tok := strings.TrimSpace(literal); tok != "" {
			return tok, nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("fastmail: read token: %w", err)
		}
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", fmt.Errorf("fastmail: token file %s is empty", path)
		}
		return tok, nil
	}
}

// api is the JMAP transport seam: *jmap.Client implements it in production,
// tests substitute a fake.
type api interface {
	Do(req *jmap.Request) (*jmap.Response, error)
	DownloadWithContext(ctx context.Context, accountID jmap.ID, blobID jmap.ID) (io.ReadCloser, error)
}

var _ api = (*jmap.Client)(nil)

// conn is one authenticated run's connection: the transport plus the
// session-derived values every request needs.
type conn struct {
	api       api
	accountID jmap.ID
	maxCalls  int // session maxCallsInRequest
}

// Source is the FastMail connector for one account; it implements
// source.Source.
type Source struct {
	// inst is this account's instance id ("fastmail:<label>"): the source
	// key of every row this connector writes. tag is its filename-safe
	// rendering ("fastmail-<label>") and is the only form allowed in a path.
	inst string
	tag  string

	acct   config.FastMailAccount
	db     *state.DB
	writer *archive.Writer
	lim    *ratelimit.Units // one unit per HTTP round trip; nil disables pacing
	log    *slog.Logger
	tokens TokenFunc

	// pol decides every attachment; never nil (policy.Default() when the
	// caller passed none). freeSpace probes the archive volume for the
	// free-space floor.
	pol       *policy.Policy
	freeSpace func(root string) int64

	// runBytes is the attachment bytes stored by the current Sync, charged
	// against attachments.run_budget. It is reset at the top of Sync and is
	// safe as a plain field because one Source archives one account and its
	// Sync is never re-entered concurrently.
	runBytes int64

	endpoint string
	// dial builds the per-run connection; a test seam over connect.
	dial func(ctx context.Context) (*conn, error)
}

// New constructs the connector for one configured account. instanceID is
// that account's state key, state.InstanceID(state.SourceFastmail,
// acct.Label); tokens yields the account's bearer token (the caller resolves
// it from acct.Token / acct.TokenFilePath). lim may be nil (no pacing).
//
// Pass WithPolicy(cfg.Policy()) so the operator's [attachments] block takes
// effect; without it the connector applies policy.Default().
func New(instanceID string, acct config.FastMailAccount, db *state.DB, w *archive.Writer, lim *ratelimit.Units, log *slog.Logger, tokens TokenFunc, opts ...Option) *Source {
	if log == nil {
		log = slog.Default()
	}
	o := newOptions(opts)
	s := &Source{
		inst:      instanceID,
		tag:       state.Tag(instanceID),
		acct:      acct,
		db:        db,
		writer:    w,
		lim:       lim,
		log:       log,
		tokens:    tokens,
		pol:       o.pol,
		freeSpace: o.freeSpace,
		endpoint:  SessionEndpoint,
	}
	s.dial = s.connect
	return s
}

// maxBlobBytes is the ceiling on one downloaded RFC 5322 blob, straight from
// attachments.max_message_bytes: unlike Gmail's base64-in-JSON envelope, a
// JMAP blob download IS the message, so no headroom factor is needed. 0 means
// the operator disabled the cap.
func (s *Source) maxBlobBytes() int64 { return s.pol.Settings().MaxMessageBytes }

// Name implements source.Source: the instance id, which is what StartRun and
// the cursor/failure tables are keyed by.
func (s *Source) Name() string { return s.inst }

// checkInstance rejects a connector whose instance id is not the well-formed
// id of its own account. A bare "fastmail" kind, or another label's id, would
// silently merge two accounts' cursors and message rows into one archive, so
// both entry points fail loudly instead.
func (s *Source) checkInstance() error {
	want := state.InstanceID(state.SourceFastmail, s.acct.Label)
	if _, _, ok := state.SplitInstance(s.inst); !ok || s.inst != want {
		return fmt.Errorf("fastmail: instance id %q does not name the configured account (label %q, want %q)",
			s.inst, s.acct.Label, want)
	}
	return nil
}

// Check probes auth and reachability with a single session GET. It mutates
// nothing.
func (s *Source) Check(ctx context.Context) error {
	if err := s.checkInstance(); err != nil {
		return err
	}
	token, err := s.tokens()
	if err != nil {
		return retry.MarkAuthBroken(err)
	}
	cl := (&jmap.Client{SessionEndpoint: s.endpoint}).WithAccessToken(token)
	_, err = fetchSession(ctx, cl.HttpClient, s.endpoint, checkRetryOpts)
	return err
}

// connect authenticates and derives the per-run connection from the session.
func (s *Source) connect(ctx context.Context) (*conn, error) {
	token, err := s.tokens()
	if err != nil {
		return nil, retry.MarkAuthBroken(err)
	}
	cl := (&jmap.Client{SessionEndpoint: s.endpoint}).WithAccessToken(token)
	// Cap every response body this client will ever produce. Go's net/http
	// inflates a Content-Encoding: gzip response with no ceiling, and neither
	// the JMAP method decode nor the blob io.ReadAll bounds it, so an
	// unbounded body is an unbounded allocation. download() additionally
	// enforces the exact cap so the limit is testable through the api seam.
	if cl.HttpClient != nil {
		cl.HttpClient.Transport = source.CapResponseBody(cl.HttpClient.Transport, blobBodyCap(s.maxBlobBytes()))
	}
	sess, err := fetchSession(ctx, cl.HttpClient, s.endpoint, retryOpts)
	if err != nil {
		return nil, err
	}
	if sess.APIURL == "" || sess.DownloadURL == "" {
		return nil, errors.New("fastmail: session lacks apiUrl or downloadUrl")
	}
	acct, ok := sess.PrimaryAccounts[mail.URI]
	if !ok || acct == "" {
		return nil, errors.New("fastmail: session has no primary mail account")
	}
	maxCalls := defaultMaxCalls
	if capv, ok := sess.Capabilities[jmap.CoreURI]; ok {
		if cc, ok := capv.(*core.Core); ok && cc.MaxCallsInRequest > 0 {
			maxCalls = int(cc.MaxCallsInRequest)
		}
	}
	cl.Session = sess // client.Do POSTs to sess.APIURL from here on
	return &conn{api: cl, accountID: acct, maxCalls: maxCalls}, nil
}

// fetchSession GETs and parses the session resource, classifying HTTP
// failures for the shared retry machinery (the library's Authenticate
// collapses every non-200 into one opaque error, so the GET is done here).
func fetchSession(ctx context.Context, hc *http.Client, endpoint string, opts retry.Options) (*jmap.Session, error) {
	var sess *jmap.Session
	err := retry.Do(ctx, opts, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err // *url.Error is a net.Error: Transient
		}
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return retry.MarkAuthBroken(fmt.Errorf("fastmail: session fetch: HTTP %d", resp.StatusCode))
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			return &transientError{fmt.Errorf("fastmail: session fetch: HTTP %d", resp.StatusCode)}
		default:
			return fmt.Errorf("fastmail: session fetch: HTTP %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxSessionBytes))
		if err != nil {
			return &transientError{fmt.Errorf("fastmail: read session: %w", err)}
		}
		got := &jmap.Session{}
		if err := json.Unmarshal(body, got); err != nil {
			return fmt.Errorf("fastmail: parse session: %w", err)
		}
		sess = got
		return nil
	})
	return sess, err
}

// doReq performs one JMAP request with pacing and retry. Method-level
// errors ride inside the successful response and surface via unpack.
func (s *Source) doReq(ctx context.Context, c *conn, req *jmap.Request) (*jmap.Response, error) {
	req.Context = ctx
	var resp *jmap.Response
	err := retry.Do(ctx, retryOpts, func() error {
		if s.lim != nil {
			if err := s.lim.Wait(ctx, 1); err != nil {
				return err
			}
		}
		r, err := c.api.Do(req)
		if err != nil {
			return classifyTransport(err)
		}
		resp = r
		return nil
	})
	return resp, err
}

// blobBodyCap adds slack over the exact message cap so the transport-level
// wrapper never truncates a legitimate body that the exact check would have
// accepted (a JMAP method response carries envelope JSON around its payload,
// and a blob may be chunk-framed). The exact cap lives in download().
func blobBodyCap(max int64) int64 {
	if max <= 0 {
		return 0
	}
	return max + max/4 + 1<<20
}

// download fetches one blob (the raw RFC 5322 message) with pacing and
// retry, refusing anything over attachments.max_message_bytes.
//
// The cap is not decoration: without it the whole message is buffered in RAM
// before enmime ever sees it, and a single oversized (or gzip-bomb) blob
// would be an unbounded allocation. Exceeding it is a deterministic property
// of the message, so it must NOT be wrapped as transient — the caller records
// it as an item failure rather than retrying it forever.
func (s *Source) download(ctx context.Context, c *conn, blobID jmap.ID) ([]byte, error) {
	max := s.maxBlobBytes()
	var raw []byte
	err := retry.Do(ctx, retryOpts, func() error {
		if s.lim != nil {
			if err := s.lim.Wait(ctx, 1); err != nil {
				return err
			}
		}
		rc, err := c.api.DownloadWithContext(ctx, c.accountID, blobID)
		if err != nil {
			return classifyTransport(err)
		}
		defer rc.Close()
		b, err := source.ReadAllCapped(rc, max)
		if errors.Is(err, source.ErrResponseTooLarge) {
			return fmt.Errorf("fastmail: blob %s is over attachments.max_message_bytes (%d); raise the cap to archive it: %w", blobID, max, err)
		}
		if err != nil {
			return &transientError{fmt.Errorf("fastmail: read blob %s: %w", blobID, err)}
		}
		raw = b
		return nil
	})
	return raw, err
}

// unpack finds callID's response invocation and returns its typed value; a
// method-level "error" invocation becomes a classified Go error.
func unpack[T any](resp *jmap.Response, callID string) (*T, error) {
	for _, inv := range resp.Responses {
		if inv.CallID != callID {
			continue
		}
		if me, ok := inv.Args.(*jmap.MethodError); ok {
			return nil, classifyMethod(me)
		}
		v, ok := inv.Args.(*T)
		if !ok {
			return nil, fmt.Errorf("fastmail: unexpected response type %T for call %s", inv.Args, callID)
		}
		return v, nil
	}
	return nil, fmt.Errorf("fastmail: no response for call %s", callID)
}

// classifyMethod maps JMAP method errors onto the shared retry classes:
// both lost-cursor shapes (a destroyed backfill anchor, an expired changes
// state) become CursorGone for the caller to reinterpret in context.
func classifyMethod(me *jmap.MethodError) error {
	switch me.Type {
	case "anchorNotFound", "cannotCalculateChanges":
		return retry.MarkCursorGone(me)
	case "serverUnavailable":
		return &transientError{me}
	}
	return me
}

// classifyTransport maps HTTP-level failures from the JMAP client onto the
// shared retry classes. Plain network errors already classify as Transient.
//
// go-jmap builds a typed *jmap.RequestError only when the error response
// carries Content-Type exactly "application/json"; FastMail serves
// request-level errors as problem+json and its blob host serves plain error
// pages, so in practice non-2xx statuses surface as bare errors formatted
// "HTTP <code> <status>" (go-jmap client.go decodeHttpError). The status is
// parsed out of that shape too so 401/403/429/5xx keep their retry classes.
func classifyTransport(err error) error {
	status := 0
	var re *jmap.RequestError
	if errors.As(err, &re) {
		status = re.Status
	} else if n, _ := fmt.Sscanf(err.Error(), "HTTP %d", &status); n != 1 {
		status = 0
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return retry.MarkAuthBroken(err)
	case status == http.StatusTooManyRequests || status >= 500:
		return &transientError{err}
	}
	return err
}

// transientError implements net.Error so retry.Classify treats wrapped
// FastMail HTTP failures (429, 5xx) as retriable without teaching the
// shared retry package about JMAP.
type transientError struct{ err error }

func (e *transientError) Error() string   { return e.err.Error() }
func (e *transientError) Unwrap() error   { return e.err }
func (e *transientError) Timeout() bool   { return false }
func (e *transientError) Temporary() bool { return true }

// runFatal reports errors that must abort the whole run rather than be
// recorded against one item: cancellation, broken credentials, and
// network/server trouble that already survived retry — those would fail
// every subsequent item and poison the failures ledger.
func runFatal(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return true
	}
	switch retry.Classify(err) {
	case retry.AuthBroken, retry.Transient, retry.RateLimited:
		return true
	}
	return false
}
