// Package triage classifies archived email notes as signal or noise and
// moves noise into the parallel spam tree.
//
// It is a POST-PASS over notes already on disk: it never runs inside a
// connector or the writer, never fetches anything, and reads only what the
// archiver persisted — the note's frontmatter (addresses, subject, labels,
// the headers captured at ingest, the attachment list) and, for the optional
// model layer, a bounded slice of the body.
//
// Classification runs in layers and stops at the first decisive one:
//
//	0 protect  things that can never be noise: a stored PDF/Office
//	           attachment, the account's own outgoing mail, the operator's
//	           protect globs
//	1 headers  the bulk-mail markers only the archiver could capture
//	2 rules    the operator's triage.toml — keep rules, then noise rules
//	3 llm      an optional local model, off by default, never trusted
//	           with a verdict it did not return as valid JSON
//
// Every outcome — noise, signal, undecided — is recorded; undecided is never
// noise. The Decision carries a trace of what each layer saw, for
// `comms triage --explain`.
package triage

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Note is what the classifier reads from an archived email note: its
// frontmatter and the Markdown body after it. Field names mirror the
// frontmatter keys written by internal/archive.
type Note struct {
	Source        string            `yaml:"source"`
	Type          string            `yaml:"type"`
	RenderVersion int               `yaml:"render_version"` // 0 on a note written before the key existed (reads as 1)
	Account       string            `yaml:"account"`
	AccountLabel  string            `yaml:"account_label"`
	MessageID     string            `yaml:"message_id"`
	ThreadID      string            `yaml:"thread_id"`
	Date          string            `yaml:"date"`
	DateUTC       string            `yaml:"date_utc"`
	From          []string          `yaml:"from"`
	To            []string          `yaml:"to"`
	Cc            []string          `yaml:"cc"`
	Subject       string            `yaml:"subject"`
	Labels        []string          `yaml:"labels"`
	Headers       map[string]string `yaml:"headers"`
	Attachments   []string          `yaml:"attachments"` // rel paths of STORED files
	Skipped       []struct {
		Name string `yaml:"name"`
	} `yaml:"skipped_attachments"` // refused parts: not on disk, so never protective

	// Body is the Markdown after the frontmatter, verbatim.
	Body string `yaml:"-"`
}

// HasHeaders reports whether the note was rendered by a version that
// captured the triage headers at all — the difference between "the message
// carried none" and "nobody looked". See archive.EmailRenderVersion.
func (n *Note) HasHeaders() bool { return n.RenderVersion >= 2 }

// Header returns a captured header by its lowercase name, "" when absent.
func (n *Note) Header(name string) string { return n.Headers[strings.ToLower(name)] }

// ErrNotEmailNote is returned for a file that is not an email note this
// program wrote: no frontmatter, or a type other than "email" (a chat day
// file, say — chat is never triaged).
var ErrNotEmailNote = errors.New("triage: not an email note")

var fence = []byte("---\n")

// ParseNote splits a note into its YAML frontmatter and body. Only the
// leading fence pair counts: a "---" inside the body is body.
func ParseNote(b []byte) (*Note, error) {
	if !bytes.HasPrefix(b, fence) {
		return nil, fmt.Errorf("%w: no frontmatter", ErrNotEmailNote)
	}
	rest := b[len(fence):]
	end := bytes.Index(rest, []byte("\n---\n"))
	var fm, body []byte
	switch {
	case end >= 0:
		fm, body = rest[:end+1], rest[end+len("\n---\n"):]
	case bytes.HasSuffix(rest, []byte("\n---")):
		fm = rest[:len(rest)-len("---")]
	default:
		return nil, fmt.Errorf("%w: frontmatter is not closed", ErrNotEmailNote)
	}
	n := &Note{}
	if err := yaml.Unmarshal(fm, n); err != nil {
		return nil, fmt.Errorf("triage: parse frontmatter: %w", err)
	}
	if n.Type != "email" {
		return nil, fmt.Errorf("%w: type %q", ErrNotEmailNote, n.Type)
	}
	if n.RenderVersion == 0 {
		n.RenderVersion = 1
	}
	n.Body = string(body)
	return n, nil
}

// address returns the bare, lowercased address in a "Name <addr>" string
// (or the string itself, lowercased, when it carries no angle brackets).
func address(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			return strings.ToLower(strings.TrimSpace(s[i+1 : i+j]))
		}
	}
	return strings.ToLower(s)
}

// attachmentExt returns the lowercase extension of a stored attachment's
// rel path, without the dot.
func attachmentExt(rel string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(rel), "."))
}
