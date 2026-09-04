// Package outbox implements comms's file-backed outbound queue. Markdown
// files are parsed strictly, assigned deterministic identities, and moved by
// atomic rename from send/ into archived/ only after remote delivery is
// durably recorded in the state database.
package outbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"comms/internal/paths"
)

const MaxDraftBytes = 1 << 20

type Kind string

const (
	KindEmail Kind = "email"
	KindChat  Kind = "chat"
)

// Address is the normalized form of one RFC 5322 mailbox from frontmatter.
type Address struct {
	Name  string
	Email string
}

func (a Address) String() string { return (&mail.Address{Name: a.Name, Address: a.Email}).String() }

// Draft is one validated Markdown file. Raw is retained so the exact bytes
// can be verified before the post-send rename.
type Draft struct {
	RelPath     string
	MessageKey  string
	ContentHash string
	Kind        Kind
	Account     string
	To          []Address
	CC          []Address
	BCC         []Address
	ReplyTo     *Address
	FromName    string
	Subject     string
	Space       string
	Thread      string
	Body        string
	Raw         []byte
}

// Receipt is the durable provider result written before the local file move.
type Receipt struct {
	ProviderID string
	SentAt     time.Time
	Reconciled bool
}

// Sender is deliberately separate from source.Source: synchronization and
// outbound mutation have different permissions and safety contracts.
type Sender interface {
	Send(context.Context, Draft, time.Time) (Receipt, error)
}

// Candidate is one .md file found under send/. A malformed file is returned
// with Err so other independent drafts can still be delivered.
type Candidate struct {
	RelPath string
	Draft   Draft
	Err     error
}

type stringList []string

func (s *stringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var v string
		if err := n.Decode(&v); err != nil {
			return err
		}
		*s = []string{v}
		return nil
	case yaml.SequenceNode:
		var v []string
		if err := n.Decode(&v); err != nil {
			return err
		}
		*s = v
		return nil
	default:
		return errors.New("must be an address string or a list of address strings")
	}
}

type metadata struct {
	Type     Kind       `yaml:"type"`
	Account  string     `yaml:"account"`
	To       stringList `yaml:"to"`
	CC       stringList `yaml:"cc"`
	BCC      stringList `yaml:"bcc"`
	ReplyTo  string     `yaml:"reply_to"`
	FromName string     `yaml:"from_name"`
	Subject  string     `yaml:"subject"`
	Space    string     `yaml:"space"`
	Thread   string     `yaml:"thread"`
}

// LoadDir recursively loads regular .md files in deterministic path order.
// Symlinked Markdown files are candidates with errors; they are never read.
func LoadDir(root string) ([]Candidate, error) {
	var out []Candidate
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(d.Name()) != ".md" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&os.ModeSymlink != 0 {
			out = append(out, Candidate{RelPath: rel, Err: errors.New("symlinked drafts are not allowed")})
			return nil
		}
		info, err := d.Info()
		if err != nil {
			out = append(out, Candidate{RelPath: rel, Err: err})
			return nil
		}
		if !info.Mode().IsRegular() {
			out = append(out, Candidate{RelPath: rel, Err: fmt.Errorf("not a regular file (mode %v)", info.Mode())})
			return nil
		}
		if info.Size() > MaxDraftBytes {
			out = append(out, Candidate{RelPath: rel, Err: fmt.Errorf("draft is %d bytes; maximum is %d", info.Size(), MaxDraftBytes)})
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			out = append(out, Candidate{RelPath: rel, Err: err})
			return nil
		}
		draft, err := Parse(rel, data)
		out = append(out, Candidate{RelPath: rel, Draft: draft, Err: err})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan outbox %s: %w", root, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

// Parse validates one frontmatter-plus-Markdown draft.
func Parse(rel string, data []byte) (Draft, error) {
	rel, err := cleanRel(rel)
	if err != nil {
		return Draft{}, err
	}
	if len(data) > MaxDraftBytes {
		return Draft{}, fmt.Errorf("draft is %d bytes; maximum is %d", len(data), MaxDraftBytes)
	}
	header, body, err := splitFrontmatter(data)
	if err != nil {
		return Draft{}, err
	}
	var meta metadata
	dec := yaml.NewDecoder(bytes.NewReader(header))
	dec.KnownFields(true)
	if err := dec.Decode(&meta); err != nil {
		return Draft{}, fmt.Errorf("frontmatter: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Draft{}, errors.New("frontmatter: only one YAML document is allowed")
		}
		return Draft{}, fmt.Errorf("frontmatter: %w", err)
	}
	meta.Account = strings.TrimSpace(meta.Account)
	meta.Subject = strings.TrimSpace(meta.Subject)
	meta.Space = strings.TrimSpace(meta.Space)
	meta.Thread = strings.TrimSpace(meta.Thread)
	body = strings.TrimRight(body, "\r\n")
	if meta.Account == "" {
		return Draft{}, errors.New("frontmatter: account is required")
	}
	if strings.TrimSpace(body) == "" {
		return Draft{}, errors.New("message body is empty")
	}

	d := Draft{RelPath: rel, Kind: meta.Type, Account: meta.Account, FromName: strings.TrimSpace(meta.FromName), Subject: meta.Subject, Space: meta.Space, Thread: meta.Thread, Body: body, Raw: append([]byte(nil), data...)}
	d.To, err = parseAddresses("to", meta.To)
	if err != nil {
		return Draft{}, err
	}
	d.CC, err = parseAddresses("cc", meta.CC)
	if err != nil {
		return Draft{}, err
	}
	d.BCC, err = parseAddresses("bcc", meta.BCC)
	if err != nil {
		return Draft{}, err
	}
	if strings.TrimSpace(meta.ReplyTo) != "" {
		list, err := parseAddresses("reply_to", []string{meta.ReplyTo})
		if err != nil {
			return Draft{}, err
		}
		d.ReplyTo = &list[0]
	}

	switch d.Kind {
	case KindEmail:
		if len(d.To)+len(d.CC)+len(d.BCC) == 0 {
			return Draft{}, errors.New("email needs at least one to, cc, or bcc recipient")
		}
		if d.Subject == "" {
			return Draft{}, errors.New("email subject is required")
		}
		if d.Space != "" || d.Thread != "" {
			return Draft{}, errors.New("email must not set space or thread")
		}
	case KindChat:
		if len(d.To)+len(d.CC)+len(d.BCC) != 0 || d.ReplyTo != nil || d.FromName != "" || d.Subject != "" {
			return Draft{}, errors.New("chat must not set email-only fields (to, cc, bcc, reply_to, from_name, subject)")
		}
		if !validSpace(d.Space) {
			return Draft{}, fmt.Errorf("chat space %q must have the form spaces/<id>", d.Space)
		}
		if d.Thread != "" && (!strings.HasPrefix(d.Thread, d.Space+"/threads/") || strings.Contains(strings.TrimPrefix(d.Thread, d.Space+"/threads/"), "/")) {
			return Draft{}, fmt.Errorf("chat thread %q must have the form %s/threads/<id>", d.Thread, d.Space)
		}
		if len([]byte(d.Body)) > 32000 {
			return Draft{}, fmt.Errorf("chat body is %d bytes; Google Chat permits at most 32000", len([]byte(d.Body)))
		}
	default:
		return Draft{}, fmt.Errorf("frontmatter: type must be %q or %q, got %q", KindEmail, KindChat, d.Kind)
	}

	contentSum := sha256.Sum256(data)
	d.ContentHash = hex.EncodeToString(contentSum[:])
	keySum := sha256.Sum256(append(append([]byte(rel), 0), data...))
	d.MessageKey = hex.EncodeToString(keySum[:])
	return d, nil
}

func splitFrontmatter(data []byte) ([]byte, string, error) {
	start := 0
	switch {
	case bytes.HasPrefix(data, []byte("---\n")):
		start = 4
	case bytes.HasPrefix(data, []byte("---\r\n")):
		start = 5
	default:
		return nil, "", errors.New("draft must start with a YAML frontmatter line: ---")
	}
	pos := start
	for pos <= len(data) {
		n := bytes.IndexByte(data[pos:], '\n')
		end, next := len(data), len(data)
		if n >= 0 {
			end, next = pos+n, pos+n+1
		}
		line := bytes.TrimSuffix(data[pos:end], []byte("\r"))
		if bytes.Equal(line, []byte("---")) {
			return data[start:pos], string(data[next:]), nil
		}
		if n < 0 {
			break
		}
		pos = next
	}
	return nil, "", errors.New("draft frontmatter has no closing --- line")
}

func parseAddresses(field string, values []string) ([]Address, error) {
	out := make([]Address, 0, len(values))
	for _, raw := range values {
		if strings.ContainsAny(raw, "\r\n") {
			return nil, fmt.Errorf("frontmatter: %s address contains a newline", field)
		}
		a, err := mail.ParseAddress(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("frontmatter: %s address %q: %w", field, raw, err)
		}
		out = append(out, Address{Name: a.Name, Email: a.Address})
	}
	return out, nil
}

func validSpace(s string) bool {
	if !strings.HasPrefix(s, "spaces/") || len(s) == len("spaces/") {
		return false
	}
	for _, r := range strings.TrimPrefix(s, "spaces/") {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// ArchiveRel chooses a deterministic date-organized destination and adds an
// eight-character message-key suffix so same-day filenames cannot collide.
func ArchiveRel(sentAt time.Time, draftRel, key string) (string, error) {
	draftRel, err := cleanRel(draftRel)
	if err != nil {
		return "", err
	}
	if len(key) < 8 {
		return "", errors.New("message key is too short")
	}
	dir, base := filepath.Split(filepath.FromSlash(draftRel))
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	base = stem + "_" + key[:8] + ext
	day := sentAt.UTC().Format("2006/01/02")
	return filepath.ToSlash(filepath.Join(day, dir, base)), nil
}

// FinishArchive completes or reconciles the atomic send/ -> archived/ rename.
// It verifies the exact draft bytes before accepting either side as durable.
func FinishArchive(sendRoot, archivedRoot, draftRel, archiveRel, contentHash string) (moved bool, err error) {
	src, err := under(sendRoot, draftRel)
	if err != nil {
		return false, err
	}
	dst, err := under(archivedRoot, archiveRel)
	if err != nil {
		return false, err
	}
	srcOK, err := regular(src)
	if err != nil {
		return false, err
	}
	dstOK, err := regular(dst)
	if err != nil {
		return false, err
	}
	switch {
	case srcOK && dstOK:
		return false, fmt.Errorf("outbox recovery is ambiguous: both %s and %s exist", src, dst)
	case !srcOK && !dstOK:
		return false, fmt.Errorf("outbox recovery failed: neither %s nor %s exists", src, dst)
	case dstOK:
		if err := verifyHash(dst, contentHash); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := verifyHash(src, contentHash); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return false, fmt.Errorf("create archived directory: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return false, fmt.Errorf("archive outgoing file: send and archived must be on the same volume: %w", err)
		}
		return false, fmt.Errorf("archive outgoing file: %w", err)
	}
	ok, err := regular(dst)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("archive outgoing file: rename succeeded but destination %s is missing", dst)
	}
	return true, nil
}

func cleanRel(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("invalid relative path %q", rel)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid relative path %q", rel)
	}
	return clean, nil
}

func under(root, rel string) (string, error) {
	rel, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if !paths.UnderDir(root, abs) {
		return "", fmt.Errorf("path %q escapes root %q", rel, root)
	}
	return abs, nil
}

func regular(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file (mode %v)", path, info.Mode())
	}
	return true, nil
}

func verifyHash(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, MaxDraftBytes+1)); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("outgoing file %s changed after it was prepared (hash %s, want %s); leaving it in place", path, got, want)
	}
	return nil
}
