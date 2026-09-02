package emailpipe

import (
	"bytes"
	"strings"
	"testing"
)

// TestTriageHeadersCaptured: the bulk-mail headers are persisted lowercase-
// keyed, decoded, unfolded, and with a repeated header's values joined —
// and nothing outside TriageHeaderNames comes along.
func TestTriageHeadersCaptured(t *testing.T) {
	doc := render(t, "bulk_headers.eml")
	want := map[string]string{
		"list-id":                  "org/repo <repo.org.github.com>",
		"list-unsubscribe":         "<mailto:unsub@github.com>, <https://github.com/notifications/unsubscribe/abc>, <https://example.com/second>",
		"precedence":               "list",
		"x-github-reason":          "subscribed",
		"auto-submitted":           "auto-generated",
		"x-auto-response-suppress": "All",
		"feedback-id":              "1234:github",
		"x-mailer":                 "Bulkmailer™ 9000",
	}
	if len(doc.Headers) != len(want) {
		t.Errorf("Headers has %d entries, want %d: %v", len(doc.Headers), len(want), doc.Headers)
	}
	for k, v := range want {
		if doc.Headers[k] != v {
			t.Errorf("Headers[%q] = %q, want %q", k, doc.Headers[k], v)
		}
	}
	if _, ok := doc.Headers["x-unrelated"]; ok {
		t.Error("a header outside TriageHeaderNames was captured")
	}
}

// TestTriageHeadersAbsentIsNil: a message without any of the headers yields
// a nil map, so the frontmatter omits the key entirely and a plain note's
// bytes do not change shape.
func TestTriageHeadersAbsentIsNil(t *testing.T) {
	if doc := render(t, "plain.eml"); doc.Headers != nil {
		t.Errorf("plain.eml captured headers: %v", doc.Headers)
	}
}

// TestTriageHeadersAreOneLineAndCapped: a header is sender-controlled text
// headed for YAML, so newlines are flattened and the length is bounded.
func TestTriageHeadersAreOneLineAndCapped(t *testing.T) {
	long := strings.Repeat("x", maxTriageHeaderBytes+100)
	raw := "From: a@example.com\r\nSubject: s\r\n" +
		"List-Id: first\r\n line\r\n\tcontinued\r\n" +
		"Feedback-ID: " + long + "\r\n" +
		"Precedence:    \r\n" +
		"Content-Type: text/plain\r\n\r\nbody\r\n"
	doc, err := Render([]byte(raw), attachDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Headers["list-id"]; got != "first line continued" {
		t.Errorf("folded header = %q, want it unfolded onto one line", got)
	}
	if got := doc.Headers["feedback-id"]; len(got) > maxTriageHeaderBytes+len("…") || !strings.HasSuffix(got, "…") {
		t.Errorf("long header not capped: %d bytes, suffix %q", len(got), got[len(got)-3:])
	}
	if _, ok := doc.Headers["precedence"]; ok {
		t.Error("a blank header was recorded")
	}
	for k, v := range doc.Headers {
		if bytes.ContainsAny([]byte(v), "\r\n") {
			t.Errorf("Headers[%q] contains a line break: %q", k, v)
		}
	}
}
