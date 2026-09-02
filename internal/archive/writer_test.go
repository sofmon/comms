package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"save/internal/emailpipe"
	"save/internal/naming"
)

// tzAms is a fixed zone so tests never depend on the host tz database.
var tzAms = time.FixedZone("Europe/Amsterdam", 3*60*60)

func fullEmailFixture(t *testing.T) (*emailpipe.EmailDoc, EmailMeta, string) {
	t.Helper()
	server := time.Date(2026, 8, 7, 12, 32, 5, 0, time.UTC) // 15:32:05 in Sofia
	stem := naming.EmailStem(server.In(tzAms), "gmail-work", "foo: [bar]", naming.Hash8("gmail:work:abc123"))
	ad := naming.AttachDir(stem)
	doc := &emailpipe.EmailDoc{
		Subject:   "foo: [bar]",
		MessageID: "<m1@example.com>",
		From:      []string{"Ann Example <ann@example.com>"},
		To:        []string{"Bob <bob@example.com>"},
		Date:      time.Date(2026, 8, 7, 14, 32, 5, 0, time.FixedZone("", 2*3600)),
		BodyMD:    "Hello **world**.\n",
		Headers: map[string]string{
			"precedence": "bulk",
			"list-id":    "Dev <dev.example.com>",
		},
		Files: []emailpipe.File{
			{Rel: ad + "/invoice.pdf", Content: []byte("pdf-bytes")},
			{Rel: ad + "/photo 1.png", Content: []byte("png-bytes")},
		},
		Warnings: []string{"part 3: unknown charset"},
	}
	meta := EmailMeta{
		Source:       "gmail:work",
		SourceTag:    "gmail-work",
		Account:      "user@example.com",
		AccountLabel: "work",
		StableID:     "abc123",
		ThreadID:     "thread-1",
		Labels:       []string{"INBOX", "Invoices"},
		ServerTime:   server,
	}
	return doc, meta, stem
}

func TestWriteEmailGolden(t *testing.T) {
	fullDoc, fullMeta, fullStem := fullEmailFixture(t)
	minStem := naming.EmailStem(
		time.Date(2026, 8, 7, 15, 32, 5, 0, tzAms), "fastmail-fm", "", naming.Hash8("fastmail:fm:xyz"))

	tests := []struct {
		name    string
		doc     *emailpipe.EmailDoc
		meta    EmailMeta
		wantRel string
		want    string
	}{
		{
			// YAML-hostile subject must be neutralized by yaml.v3 quoting;
			// attachments section rendered; warnings present.
			name:    "hostile subject with attachments",
			doc:     fullDoc,
			meta:    fullMeta,
			wantRel: "2026/08/07/" + fullStem + ".md",
			want: strings.ReplaceAll(`---
source: gmail:work
type: email
render_version: 2
account: user@example.com
account_label: work
message_id: <m1@example.com>
thread_id: thread-1
date: "2026-08-07T14:32:05+02:00"
date_utc: "2026-08-07T12:32:05Z"
from:
    - Ann Example <ann@example.com>
to:
    - Bob <bob@example.com>
cc: []
subject: 'foo: [bar]'
labels:
    - INBOX
    - Invoices
headers:
    list-id: Dev <dev.example.com>
    precedence: bulk
attachments:
    - STEM.d/invoice.pdf
    - STEM.d/photo 1.png
warnings:
    - 'part 3: unknown charset'
---

Hello **world**.

## Attachments

- [invoice.pdf](<STEM.d/invoice.pdf>)
- [photo 1.png](<STEM.d/photo 1.png>)
`, "STEM", fullStem),
		},
		{
			// Empty everything: warnings omitted, no body block, no
			// attachments section, zero date renders empty.
			name: "minimal",
			doc:  &emailpipe.EmailDoc{},
			meta: EmailMeta{
				Source:       "fastmail:fm",
				SourceTag:    "fastmail-fm",
				Account:      "user@example.com",
				AccountLabel: "fm",
				StableID:     "xyz",
				ServerTime:   time.Date(2026, 8, 7, 12, 32, 5, 0, time.UTC),
			},
			wantRel: "2026/08/07/" + minStem + ".md",
			want: `---
source: fastmail:fm
type: email
render_version: 2
account: user@example.com
account_label: fm
message_id: ""
thread_id: ""
date: ""
date_utc: "2026-08-07T12:32:05Z"
from: []
to: []
cc: []
subject: ""
labels: []
attachments: []
---
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &Writer{Root: t.TempDir(), TZ: tzAms}
			rel, hash, err := w.WriteEmail(tt.doc, tt.meta)
			if err != nil {
				t.Fatalf("WriteEmail: %v", err)
			}
			if rel != tt.wantRel {
				t.Errorf("rel = %q, want %q", rel, tt.wantRel)
			}
			got, err := os.ReadFile(filepath.Join(w.Root, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatalf("read written md: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("md mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, tt.want)
			}
			sum := sha256.Sum256(got)
			if hash != hex.EncodeToString(sum[:]) {
				t.Errorf("contentHash %q does not match sha256 of written bytes", hash)
			}
			// Attachment files land before/with the md, bytes intact; the
			// archive is private mail, so files are 0600 and dirs 0700.
			for _, f := range tt.doc.Files {
				p := filepath.Join(w.Root, "2026/08/07", filepath.FromSlash(f.Rel))
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("attachment %s: %v", f.Rel, err)
				}
				if !bytes.Equal(b, f.Content) {
					t.Errorf("attachment %s content mismatch", f.Rel)
				}
				if mode := fileMode(t, p); mode != 0o600 {
					t.Errorf("attachment %s mode = %o, want 0600", f.Rel, mode)
				}
			}
			if mode := fileMode(t, filepath.Join(w.Root, filepath.FromSlash(rel))); mode != 0o600 {
				t.Errorf("md mode = %o, want 0600", mode)
			}
			if mode := dirMode(t, filepath.Join(w.Root, "2026", "08", "07")); mode != 0o700 {
				t.Errorf("day dir mode = %o, want 0700", mode)
			}
		})
	}
}

func TestWriteEmailIdempotent(t *testing.T) {
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	doc, meta, _ := fullEmailFixture(t)

	rel1, hash1, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatalf("first WriteEmail: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(w.Root, filepath.FromSlash(rel1)))
	if err != nil {
		t.Fatal(err)
	}

	rel2, hash2, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatalf("second WriteEmail: %v", err)
	}
	if rel1 != rel2 || hash1 != hash2 {
		t.Errorf("re-write not stable: (%q,%q) vs (%q,%q)", rel1, hash1, rel2, hash2)
	}
	second, err := os.ReadFile(filepath.Join(w.Root, filepath.FromSlash(rel2)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("re-written md bytes differ")
	}

	// renameio must leave no temp litter behind successful writes.
	if litter := dotFiles(t, w.Root); len(litter) != 0 {
		t.Errorf("temp litter after writes: %v", litter)
	}
}

// TestWriteEmailPerAccountPaths: the same message id archived by two
// accounts must land in two files — the stem carries the file tag and the
// hash is salted with the instance id, so neither path nor hash can collide.
func TestWriteEmailPerAccountPaths(t *testing.T) {
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	server := time.Date(2026, 8, 7, 12, 32, 5, 0, time.UTC)
	doc := &emailpipe.EmailDoc{Subject: "Shared thread"}

	relA, hashA, err := w.WriteEmail(doc, EmailMeta{
		Source: "gmail:work", SourceTag: "gmail-work", Account: "jane@example.com",
		AccountLabel: "work", StableID: "same-id", ServerTime: server,
	})
	if err != nil {
		t.Fatalf("WriteEmail(work): %v", err)
	}
	relB, hashB, err := w.WriteEmail(doc, EmailMeta{
		Source: "gmail:personal", SourceTag: "gmail-personal", Account: "jane@example.net",
		AccountLabel: "personal", StableID: "same-id", ServerTime: server,
	})
	if err != nil {
		t.Fatalf("WriteEmail(personal): %v", err)
	}
	if relA == relB {
		t.Fatalf("both accounts wrote %q", relA)
	}
	if hashA == hashB {
		t.Error("frontmatter provenance is identical for the two accounts")
	}
	if !strings.Contains(relA, "_gmail-work_") || !strings.Contains(relB, "_gmail-personal_") {
		t.Errorf("stems do not carry the file tag: %q / %q", relA, relB)
	}
	// The hash8 differs too, because the instance id salts it.
	hash8 := func(rel string) string {
		stem := strings.TrimSuffix(rel, ".md")
		return stem[len(stem)-8:]
	}
	if hash8(relA) == hash8(relB) {
		t.Errorf("hash8 did not change with the instance: %q / %q", relA, relB)
	}
	for _, rel := range []string{relA, relB} {
		if _, err := os.Stat(filepath.Join(w.Root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
	body, err := os.ReadFile(filepath.Join(w.Root, filepath.FromSlash(relB)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"source: gmail:personal", "account: jane@example.net", "account_label: personal"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("frontmatter missing %q:\n%s", want, body)
		}
	}
}

func TestWriteEmailRejects(t *testing.T) {
	server := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	okMeta := EmailMeta{Source: "gmail:work", SourceTag: "gmail-work", StableID: "id1", ServerTime: server}

	tests := []struct {
		name string
		doc  *emailpipe.EmailDoc
		meta EmailMeta
	}{
		{"nil doc", nil, okMeta},
		{"missing stable id", &emailpipe.EmailDoc{}, EmailMeta{Source: "gmail:work", SourceTag: "gmail-work", ServerTime: server}},
		{"missing source", &emailpipe.EmailDoc{}, EmailMeta{SourceTag: "gmail-work", StableID: "id1", ServerTime: server}},
		{"missing source tag", &emailpipe.EmailDoc{}, EmailMeta{Source: "gmail:work", StableID: "id1", ServerTime: server}},
		// The instance id must never be used as the stem's tag: a colon is
		// not a legal filename character.
		{"instance id passed as tag", &emailpipe.EmailDoc{}, EmailMeta{
			Source: "gmail:work", SourceTag: "gmail:work", StableID: "id1", ServerTime: server}},
		{"path separator in tag", &emailpipe.EmailDoc{}, EmailMeta{
			Source: "gmail:work", SourceTag: "gmail/work", StableID: "id1", ServerTime: server}},
		// '_' separates the stem's fields, so a tag may not contain one.
		{"underscore in tag", &emailpipe.EmailDoc{}, EmailMeta{
			Source: "gmail:work", SourceTag: "gmail_work", StableID: "id1", ServerTime: server}},
		{"zero server time", &emailpipe.EmailDoc{}, EmailMeta{Source: "gmail:work", SourceTag: "gmail-work", StableID: "id1"}},
		{"escaping rel", &emailpipe.EmailDoc{
			Files: []emailpipe.File{{Rel: "../evil", Content: []byte("x")}},
		}, okMeta},
		{"absolute rel", &emailpipe.EmailDoc{
			Files: []emailpipe.File{{Rel: "/etc/evil", Content: []byte("x")}},
		}, okMeta},
		{"wrong attach dir", &emailpipe.EmailDoc{
			Files: []emailpipe.File{{Rel: "other.d/x.pdf", Content: []byte("x")}},
		}, okMeta},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &Writer{Root: t.TempDir(), TZ: tzAms}
			if _, _, err := w.WriteEmail(tt.doc, tt.meta); err == nil {
				t.Error("WriteEmail succeeded, want error")
			}
			// Nothing may be written when validation fails up front.
			if tt.doc != nil && len(tt.doc.Files) > 0 {
				var found []string
				_ = filepath.WalkDir(w.Root, func(p string, d fs.DirEntry, err error) error {
					if err == nil && !d.IsDir() {
						found = append(found, p)
					}
					return nil
				})
				if len(found) != 0 {
					t.Errorf("rejected write left files: %v", found)
				}
			}
		})
	}
}

func TestWriteChatDay(t *testing.T) {
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	rel := "2026/08/07/gchat_space_team_ab12cd34.md"

	hash, err := w.WriteChatDay(rel, []byte("first version\n"))
	if err != nil {
		t.Fatalf("WriteChatDay: %v", err)
	}
	sum := sha256.Sum256([]byte("first version\n"))
	if hash != hex.EncodeToString(sum[:]) {
		t.Errorf("hash mismatch")
	}

	// Atomic replace: same path, new content.
	if _, err := w.WriteChatDay(rel, []byte("second version\n")); err != nil {
		t.Fatalf("replace WriteChatDay: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(w.Root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second version\n" {
		t.Errorf("content after replace = %q", got)
	}
	if litter := dotFiles(t, w.Root); len(litter) != 0 {
		t.Errorf("temp litter: %v", litter)
	}

	for _, bad := range []string{"", "../x.md", "/abs.md", "a/../b.md"} {
		if _, err := w.WriteChatDay(bad, []byte("x")); err == nil {
			t.Errorf("WriteChatDay(%q) succeeded, want error", bad)
		}
	}
}

func TestSweepTemp(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "08", "07")
	tmp := filepath.Join(root, TempDirName)
	for _, d := range []string{day, tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeRaw := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Crash litter: renameio temps stranded in the .tmp scratch dir.
	writeRaw(filepath.Join(tmp, ".gchat_dm_x.md42"), "temp")
	writeRaw(filepath.Join(tmp, ".143205_gmail_x.md8081976916513588155"), "temp")
	// Everything outside .tmp must survive — including user files that the
	// old dotfile-ending-in-digit heuristic would have deleted.
	writeRaw(filepath.Join(day, "keep.md"), "keep")
	writeRaw(filepath.Join(day, ".DS_Store"), "macos")
	writeRaw(filepath.Join(day, ".notes2026"), "user notes")
	writeRaw(filepath.Join(root, ".zcompdump-host-5.9"), "zsh cache")
	writeRaw(filepath.Join(root, ".backup-2024-01"), "user backup")

	w := &Writer{Root: root, TZ: tzAms}
	removed, err := w.SweepTemp()
	if err != nil {
		t.Fatalf("SweepTemp: %v", err)
	}
	want := []string{
		TempDirName + "/.143205_gmail_x.md8081976916513588155",
		TempDirName + "/.gchat_dm_x.md42",
	}
	slices.Sort(removed)
	if !slices.Equal(removed, want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
	for _, keep := range []string{
		filepath.Join(day, "keep.md"),
		filepath.Join(day, ".DS_Store"),
		filepath.Join(day, ".notes2026"),
		filepath.Join(root, ".zcompdump-host-5.9"),
		filepath.Join(root, ".backup-2024-01"),
	} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("SweepTemp removed %s: %v", keep, err)
		}
	}
	for _, gone := range want {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("temp %s still present (err=%v)", gone, err)
		}
	}

	// Missing root (fresh machine) and a root without .tmp are both fine.
	w2 := &Writer{Root: filepath.Join(root, "does-not-exist"), TZ: tzAms}
	if removed, err := w2.SweepTemp(); err != nil || len(removed) != 0 {
		t.Errorf("SweepTemp on missing root: removed=%v err=%v", removed, err)
	}
	w3 := &Writer{Root: t.TempDir(), TZ: tzAms}
	if removed, err := w3.SweepTemp(); err != nil || len(removed) != 0 {
		t.Errorf("SweepTemp without .tmp: removed=%v err=%v", removed, err)
	}
}

func fileMode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return fi.Mode().Perm()
}

func dirMode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s is not a directory", p)
	}
	return fi.Mode().Perm()
}

// dotFiles lists all dotfiles under root (rel, slash-separated).
func dotFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
