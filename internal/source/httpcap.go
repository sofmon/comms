package source

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrResponseTooLarge is returned through a response body read when the
// server sent more bytes than attachments.max_message_bytes allows. It is a
// deterministic property of the message, so connectors route it to the
// failures ledger as an item failure rather than retrying it forever.
var ErrResponseTooLarge = errors.New("source: HTTP response body exceeds the configured cap")

// CapResponseBody wraps next so that every response body it returns stops
// with ErrResponseTooLarge after max bytes.
//
// This closes a real resource hole rather than a theoretical one: Go's
// net/http transparently decompresses a Content-Encoding: gzip response with
// NO ceiling on the inflated size, so a hostile or merely broken server can
// make a connector allocate arbitrarily much for one message. Neither the
// Gmail client (which json-decodes the whole body) nor the JMAP client
// (which io.ReadAll's a blob) bounds that on its own.
//
// max <= 0 returns next unchanged — no cap configured, no wrapper.
//
// The cap is applied to the ENCODED-then-inflated body the transport hands
// up, which for Gmail's format=raw is base64 inside a JSON envelope and so
// runs larger than the RFC 822 message it carries. Callers therefore pass a
// ceiling with headroom over attachments.max_message_bytes and additionally
// check the decoded message against the exact cap; see gmail.rawBodyCap.
func CapResponseBody(next http.RoundTripper, max int64) http.RoundTripper {
	if max <= 0 {
		return next
	}
	if next == nil {
		next = http.DefaultTransport
	}
	return &capTransport{next: next, max: max}
}

type capTransport struct {
	next http.RoundTripper
	max  int64
}

func (t *capTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &capReader{rc: resp.Body, left: t.max, max: t.max}
	return resp, nil
}

// capReader is io.LimitReader that reports the overrun instead of quietly
// looking like a clean EOF. A truncated body silently presented as EOF would
// surface as a confusing "unexpected end of JSON input" — or, worse, as a
// successfully parsed but incomplete object.
//
// It reads one byte PAST the allowance rather than stopping at it, so a body
// of exactly max bytes still reaches a genuine EOF: stopping at the allowance
// would make the very next Read indistinguishable from an overrun and reject
// a legitimate response of exactly the configured size.
type capReader struct {
	rc   io.ReadCloser
	left int64 // bytes still allowed
	max  int64
	over bool
}

func (r *capReader) Read(p []byte) (int, error) {
	if r.over {
		return 0, r.tooLarge()
	}
	if int64(len(p)) > r.left+1 {
		p = p[:r.left+1]
	}
	n, err := r.rc.Read(p)
	if int64(n) > r.left {
		r.over = true
		return 0, r.tooLarge()
	}
	r.left -= int64(n)
	return n, err
}

func (r *capReader) tooLarge() error {
	return fmt.Errorf("%w (%d bytes)", ErrResponseTooLarge, r.max)
}

func (r *capReader) Close() error { return r.rc.Close() }

// ReadAllCapped reads rc to EOF, failing with ErrResponseTooLarge as soon as
// it produces more than max bytes. It is the direct-read counterpart of
// CapResponseBody for a body a connector reads itself (a JMAP blob), and it
// reads one byte past the cap so that a body of exactly max bytes still
// succeeds. max <= 0 means no cap.
func ReadAllCapped(rc io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		return io.ReadAll(rc)
	}
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%w (%d bytes)", ErrResponseTooLarge, max)
	}
	return b, nil
}
