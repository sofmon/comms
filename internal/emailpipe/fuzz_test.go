package emailpipe

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"comms/internal/policy"
)

// FuzzRender feeds arbitrary bytes through the full pipeline. Render may
// return an error, but it must never panic: connectors hand it whatever the
// mail provider stored, including decades-old malformed messages.
//
// The invariants checked on every input are the ones the archive writer and
// the skip ledger depend on: a stored file has a path under the attachment
// dir and bytes to write, and a refused file has neither but is still a
// complete record.
func FuzzRender(f *testing.F) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		f.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".eml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("testdata", e.Name()))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw)
	}
	f.Add([]byte(""))
	f.Add([]byte("no header separator at all"))
	f.Add([]byte("Content-Type: multipart/mixed; boundary=\"b\"\n\n--b\n--b--"))
	f.Add([]byte("Content-Type: text/html\n\n<img src=\"cid:x\"><img src=\"data:image/gif;base64,!!!\">"))
	f.Add([]byte("Content-Type: text/html\n\n<table><tr><td>a<img src=\"data:,plain\"></td></tr></table>"))
	f.Add([]byte("Subject: =?windows-1251?B?broken?=\nContent-Type: multipart/related; boundary=b\n\n--b\nContent-ID: <x>\nContent-Type: image/png\n\nnotpng\n--b--"))
	// Policy paths: a hostile extension, a cid: pointing at a part that will
	// be refused, and a data: URI carrying an SVG (a program, denied by
	// default) next to one that decodes to nothing.
	f.Add([]byte("Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: application/octet-stream\nContent-Disposition: attachment; filename=\"x.exe\"\n\nMZ\x90\x00payload\n--b--"))
	f.Add([]byte("Content-Type: multipart/related; boundary=b\n\n--b\nContent-Type: text/html\n\n<img src=\"cid:s\" alt=\"alt text\">\n--b\nContent-Type: image/svg+xml\nContent-ID: <s>\nContent-Disposition: inline; filename=\"a.svg\"\n\n<svg xmlns=\"http://www.w3.org/2000/svg\"/>\n--b--"))
	f.Add([]byte("Content-Type: text/html\n\n<img src=\"data:image/svg+xml;base64," +
		base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)) + "\" alt=\"a\">"))
	// A message with more parts than any per-message cap: every part past the
	// cap must be recorded rather than dropped, and the walk must stay
	// iterative.
	f.Add(flatParts(600))
	// Deep multipart nesting is deliberately NOT a fuzz seed — see
	// TestRenderDeeplyNestedMultipart for why, and for the bounded test that
	// covers it instead.

	f.Fuzz(func(t *testing.T, raw []byte) {
		doc, err := Render(raw, "a.d")
		if err != nil {
			return
		}
		var stored int64
		for _, file := range doc.Files {
			if !strings.HasPrefix(file.Rel, "a.d/") {
				t.Errorf("File.Rel %q escapes the attach dir", file.Rel)
			}
			if file.Skipped || file.Content == nil {
				t.Errorf("stored file %q: skipped=%v content=%v", file.Rel, file.Skipped, file.Content == nil)
			}
			if file.SniffedType == "" || file.Name == "" || file.PartKey == "" {
				t.Errorf("stored file %q is an incomplete record: %+v", file.Rel, file)
			}
			if !policy.ValidReason(file.SkipReason) {
				t.Errorf("stored file %q has reason %q", file.Rel, file.SkipReason)
			}
			stored += file.Size
		}
		if stored != doc.StoredBytes {
			t.Errorf("StoredBytes = %d, sum of Files = %d", doc.StoredBytes, stored)
		}
		for _, file := range doc.Skipped {
			if !file.Skipped || file.Rel != "" || file.Content != nil {
				t.Errorf("skipped file %q leaks a path or bytes: %+v", file.Name, file)
			}
			if file.Name == "" || file.PartKey == "" || file.SniffedType == "" {
				t.Errorf("skipped file is an incomplete record: %+v", file)
			}
			if file.SkipReason == policy.ReasonNone || !policy.ValidReason(file.SkipReason) {
				t.Errorf("skipped file %q has reason %q", file.Name, file.SkipReason)
			}
		}
		if doc.PolicyDigest == "" {
			t.Error("PolicyDigest is empty")
		}
		// (source, stable_id, part_key) is the skip ledger's primary key.
		seen := map[string]bool{}
		for _, file := range append(append([]File{}, doc.Files...), doc.Skipped...) {
			if seen[file.PartKey] {
				t.Errorf("duplicate PartKey %q (%s)", file.PartKey, file.Name)
			}
			seen[file.PartKey] = true
		}
	})
}

// flatParts builds a message with n sibling attachment parts.
func flatParts(n int) []byte {
	var b strings.Builder
	b.WriteString("Subject: many\nMIME-Version: 1.0\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=\"b\"\n\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "--b\nContent-Type: application/pdf\nContent-Disposition: attachment; filename=\"f%d.pdf\"\n\n%%PDF-1.7\n%%%%EOF\n", i)
	}
	b.WriteString("--b--\n")
	return []byte(b.String())
}

// TestRenderDeeplyNestedMultipart covers the recursion this package does NOT
// own: the part WALK in render.go is iterative and stack-safe, but enmime's
// parse upstream of it is recursive. A deep tree must not blow the stack.
//
// This is a bounded unit test rather than a fuzz seed on purpose. enmime's
// parse cost is superlinear in nesting depth — measured on this machine, 1000
// levels take ~0.4s, 10000 take ~30s, and a 68 KB mutation the fuzzer derived
// from a 1000-level seed burned over 137s of CPU without finishing. Seeding
// the corpus with deep nesting therefore turns `go test ./...` into a
// coin-flip hang. The real defence is a cap on the RAW message bytes and on
// the part count before enmime is handed the message
// (attachments.max_message_bytes), which lives in the connectors, not here.
func TestRenderDeeplyNestedMultipart(t *testing.T) {
	doc, err := Render(nestedMultipart(200), attachDir)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(doc.Files) != 0 || len(doc.Skipped) != 0 {
		t.Errorf("Files = %v, Skipped = %v, want none (the only leaf is the body)", rels(doc), skipNames(doc))
	}
	if !strings.Contains(doc.BodyMD, "leaf") {
		t.Errorf("BodyMD = %q, want the nested text leaf", doc.BodyMD)
	}
}

// nestedMultipart builds a multipart tree depth levels deep with one text
// leaf at the bottom.
func nestedMultipart(depth int) []byte {
	var b strings.Builder
	b.WriteString("Subject: deep\nMIME-Version: 1.0\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=\"b0\"\n\n")
	for i := 0; i < depth; i++ {
		fmt.Fprintf(&b, "--b%d\nContent-Type: multipart/mixed; boundary=\"b%d\"\n\n", i, i+1)
	}
	fmt.Fprintf(&b, "--b%d\nContent-Type: text/plain\n\nleaf\n--b%d--\n", depth, depth)
	for i := depth - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "--b%d--\n", i)
	}
	return []byte(b.String())
}
