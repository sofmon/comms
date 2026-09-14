package outbox

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseEmailAndChat(t *testing.T) {
	tests := []struct {
		name  string
		rel   string
		raw   string
		kind  Kind
		check func(*testing.T, Draft)
	}{
		{
			name: "email scalar and list addresses",
			rel:  "work/hello.md",
			raw: `---
type: email
account: work
to: Jane Example <jane@example.com>
cc:
  - boss@example.net
subject: Hello
from_name: Example Sender
reply_to: replies@example.com
---
Hello Jane,

This is plain Markdown.
`,
			kind: KindEmail,
			check: func(t *testing.T, d Draft) {
				if len(d.To) != 1 || d.To[0].Email != "jane@example.com" || len(d.CC) != 1 {
					t.Fatalf("addresses = to %#v cc %#v", d.To, d.CC)
				}
				if d.ReplyTo == nil || d.ReplyTo.Email != "replies@example.com" || d.Subject != "Hello" {
					t.Fatalf("email metadata = %+v", d)
				}
			},
		},
		{
			name: "chat reply",
			rel:  "chat.md",
			raw: `---
type: chat
account: work
space: spaces/AAAA_123
thread: spaces/AAAA_123/threads/BBBB-456
---
**Deployment complete.**
`,
			kind: KindChat,
			check: func(t *testing.T, d Draft) {
				if d.Space != "spaces/AAAA_123" || d.Thread == "" || d.Body != "**Deployment complete.**" {
					t.Fatalf("chat = %+v", d)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := Parse(tt.rel, []byte(tt.raw))
			if err != nil {
				t.Fatal(err)
			}
			if d.Kind != tt.kind || len(d.MessageKey) != 64 || len(d.ContentHash) != 64 || d.RelPath != tt.rel {
				t.Fatalf("draft identity = %+v", d)
			}
			tt.check(t, d)
		})
	}
}

func TestParseRejectsUnsafeOrAmbiguousDrafts(t *testing.T) {
	base := "---\ntype: email\naccount: work\nto: you@example.com\nsubject: Hi\n---\nbody\n"
	tests := []struct {
		name string
		rel  string
		raw  string
		want string
	}{
		{"path escape", "../x.md", base, "relative path"},
		{"no frontmatter", "x.md", "hello", "must start"},
		{"unknown field", "x.md", strings.Replace(base, "subject:", "surprise: yes\nsubject:", 1), "field surprise"},
		{"bad address", "x.md", strings.Replace(base, "you@example.com", "not an address", 1), "to address"},
		{"no recipient", "x.md", strings.Replace(base, "to: you@example.com\n", "", 1), "at least one"},
		{"empty body", "x.md", strings.Replace(base, "body\n", "\n", 1), "body is empty"},
		{"chat email fields", "x.md", "---\ntype: chat\naccount: work\nspace: spaces/A\nsubject: nope\n---\nhello\n", "email-only"},
		{"bad space", "x.md", "---\ntype: chat\naccount: work\nspace: room/A\n---\nhello\n", "spaces/<id>"},
		{"bad reply id", "x.md", "---\ntype: email\naccount: work\nto: you@example.com\nsubject: Hi\nin_reply_to: nope\n---\nhello\n", "angle brackets"},
		{"multiline subject", "x.md", "---\ntype: email\naccount: work\nto: you@example.com\nsubject: \"Hi\\nBcc: bad@example.com\"\n---\nhello\n", "single line"},
		{"chat reply metadata", "x.md", "---\ntype: chat\naccount: work\nspace: spaces/A\nin_reply_to: <x@example.com>\n---\nhello\n", "email-only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.rel, []byte(tt.raw))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestRenderTemplateRoundTripsThroughParser(t *testing.T) {
	tests := []struct {
		name  string
		spec  Template
		ready func([]byte) []byte
		check func(*testing.T, Draft)
	}{
		{
			name: "new email",
			spec: Template{Kind: KindEmail, Account: "work"},
			ready: func(b []byte) []byte {
				b = bytes.Replace(b, []byte(`to: ""`), []byte(`to: "Jane Example <jane@example.com>"`), 1)
				b = bytes.Replace(b, []byte(`subject: ""`), []byte(`subject: "Hello"`), 1)
				return append(b, []byte("Message body.\n")...)
			},
			check: func(t *testing.T, d Draft) {
				if d.Kind != KindEmail || d.Account != "work" || d.Subject != "Hello" || d.To[0].Email != "jane@example.com" {
					t.Fatalf("email draft = %+v", d)
				}
			},
		},
		{
			name: "reply email",
			spec: Template{
				Kind: KindEmail, Account: "work", To: []string{"Jane Example <jane@example.com>"},
				Subject: "Re: Project", InReplyTo: "<original@example.com>",
				References: []string{"<older@example.com>", "<original@example.com>"}, ThreadID: "gmail-thread",
			},
			ready: func(b []byte) []byte { return append(b, []byte("Reply body.\n")...) },
			check: func(t *testing.T, d Draft) {
				if d.InReplyTo != "<original@example.com>" || len(d.References) != 2 || d.ThreadID != "gmail-thread" {
					t.Fatalf("reply metadata = %+v", d)
				}
			},
		},
		{
			name: "new chat",
			spec: Template{Kind: KindChat, Account: "work"},
			ready: func(b []byte) []byte {
				b = bytes.Replace(b, []byte(`space: ""`), []byte(`space: "spaces/AAA"`), 1)
				return append(b, []byte("Chat body.\n")...)
			},
			check: func(t *testing.T, d Draft) {
				if d.Kind != KindChat || d.Space != "spaces/AAA" {
					t.Fatalf("chat draft = %+v", d)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := RenderTemplate(tt.spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse("draft.md", raw); err == nil {
				t.Fatal("untouched template is sendable")
			}
			d, err := Parse("draft.md", tt.ready(raw))
			if err != nil {
				t.Fatalf("edited generated template does not parse: %v\n%s", err, raw)
			}
			tt.check(t, d)
		})
	}
}

func TestLoadDirAndFinishArchive(t *testing.T) {
	root := t.TempDir()
	sendDir := filepath.Join(root, "send")
	archivedDir := filepath.Join(root, "archived")
	if err := os.MkdirAll(filepath.Join(sendDir, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := []byte("---\ntype: email\naccount: work\nto: you@example.com\nsubject: Hi\n---\nbody\n")
	path := filepath.Join(sendDir, "work", "hello.md")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sendDir, "ignore.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDir(sendDir)
	if err != nil || len(got) != 1 || got[0].Err != nil {
		t.Fatalf("LoadDir = %+v, %v", got, err)
	}
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.FixedZone("x", 2*3600))
	rel, err := ArchiveRel(at, got[0].Draft.RelPath, got[0].Draft.MessageKey)
	if err != nil || !strings.HasPrefix(rel, "2026/09/04/work/hello_") {
		t.Fatalf("ArchiveRel = %q, %v", rel, err)
	}
	moved, err := FinishArchive(sendDir, archivedDir, got[0].Draft.RelPath, rel, got[0].Draft.ContentHash)
	if err != nil || !moved {
		t.Fatalf("FinishArchive move = %v, %v", moved, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("source still exists: %v", err)
	}
	// Replaying after a crash between rename and DB update accepts the exact
	// destination and does not need the source to reappear.
	moved, err = FinishArchive(sendDir, archivedDir, got[0].Draft.RelPath, rel, got[0].Draft.ContentHash)
	if err != nil || moved {
		t.Fatalf("FinishArchive recovery = %v, %v", moved, err)
	}
}

func TestFinishArchiveRefusesChangedBytes(t *testing.T) {
	root := t.TempDir()
	sendDir, archivedDir := filepath.Join(root, "send"), filepath.Join(root, "archived")
	if err := os.MkdirAll(sendDir, 0o700); err != nil {
		t.Fatal(err)
	}
	original, err := Parse("x.md", []byte("---\ntype: email\naccount: work\nto: you@example.com\nsubject: Hi\n---\nbody\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sendDir, "x.md"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FinishArchive(sendDir, archivedDir, "x.md", "2026/09/04/x.md", original.ContentHash); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("error = %v", err)
	}
}
