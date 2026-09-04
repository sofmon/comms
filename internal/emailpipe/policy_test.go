package emailpipe

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"comms/internal/naming"
	"comms/internal/policy"
)

// Attachment-policy tests.
//
// The fixtures build real magic bytes rather than mocking the sniffer: the
// whole point of the allowlist is that the extension and the CONTENT agree,
// so a mocked sniffer would test nothing. archive/zip appears here and only
// here in this package — a test file — which keeps the never-decompress
// invariant ("no non-test file imports archive/zip") literally true.

// ---------------------------------------------------------------- fixtures

func pdfBytes() []byte {
	return []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\ntrailer\n%%EOF\n")
}

func pngBytes() []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
}

func svgBytes() []byte {
	return []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="1" height="1">` +
		`<script>alert(1)</script></svg>`)
}

func exeBytes() []byte {
	return append([]byte("MZ\x90\x00"), bytes.Repeat([]byte{0}, 128)...)
}

func zipOf(t *testing.T, entries [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		f, err := w.CreateHeader(&zip.FileHeader{Name: e[0], Method: zip.Store})
		if err != nil {
			t.Fatalf("zip entry %q: %v", e[0], err)
		}
		if _, err := f.Write([]byte(e[1])); err != nil {
			t.Fatalf("zip write %q: %v", e[0], err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// bigDocxBytes is the regression fixture for the whole-buffer sniff limit:
// the word/ entry sits well past mimetype's 4096-byte default, so unless
// policy's init has called mimetype.SetLimit(0) this ordinary .docx sniffs as
// application/zip and the allowlist refuses a real business document.
func bigDocxBytes(t *testing.T) []byte {
	t.Helper()
	filler := strings.Repeat(`<Override PartName="/x" ContentType="y"/>`+"\n", 200) // ~8 KB
	return zipOf(t, [][2]string{
		{"[Content_Types].xml", "<Types>" + filler + "</Types>"},
		{"word/document.xml", "<w:document/>"},
	})
}

func docmBytes(t *testing.T) []byte {
	t.Helper()
	return zipOf(t, [][2]string{
		{"[Content_Types].xml", "<Types/>"},
		{"word/document.xml", "<w:document/>"},
		{"word/vbaProject.bin", "\x00\x01macro payload"},
	})
}

// part is one MIME part of a synthesized message.
type part struct {
	ctype    string
	filename string
	cid      string
	inline   bool
	body     []byte
}

// message builds a multipart/mixed message with a text body plus parts.
func message(t *testing.T, parts ...part) []byte {
	t.Helper()
	return messageOfType(t, "multipart/mixed", "<p>body text</p>", "text/html", parts...)
}

// messageOfType builds a message whose first part is the given body and whose
// remaining parts are attachments/inlines, base64-encoded.
func messageOfType(t *testing.T, multipartType, body, bodyType string, parts ...part) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString("From: sender@example.com\r\n")
	b.WriteString("To: you@example.com\r\n")
	b.WriteString("Subject: Policy fixture\r\n")
	b.WriteString("Message-ID: <policy-1@example.com>\r\n")
	b.WriteString("Date: Mon, 10 Aug 2026 09:00:00 +0000\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: " + multipartType + "; boundary=\"bnd\"\r\n\r\n")
	b.WriteString("--bnd\r\nContent-Type: " + bodyType + "; charset=utf-8\r\n\r\n")
	b.WriteString(body + "\r\n")
	for _, p := range parts {
		b.WriteString("--bnd\r\n")
		b.WriteString("Content-Type: " + p.ctype)
		if p.filename != "" {
			b.WriteString("; name=\"" + p.filename + "\"")
		}
		b.WriteString("\r\n")
		switch {
		case p.cid != "":
			b.WriteString("Content-ID: <" + p.cid + ">\r\n")
			b.WriteString("Content-Disposition: inline")
		case p.inline:
			b.WriteString("Content-Disposition: inline")
		default:
			b.WriteString("Content-Disposition: attachment")
		}
		if p.filename != "" {
			b.WriteString("; filename=\"" + p.filename + "\"")
		}
		b.WriteString("\r\nContent-Transfer-Encoding: base64\r\n\r\n")
		b.WriteString(wrap76(base64.StdEncoding.EncodeToString(p.body)))
		b.WriteString("\r\n")
	}
	b.WriteString("--bnd--\r\n")
	return []byte(b.String())
}

func wrap76(s string) string {
	var out strings.Builder
	for len(s) > 76 {
		out.WriteString(s[:76])
		out.WriteString("\r\n")
		s = s[76:]
	}
	out.WriteString(s)
	return out.String()
}

// ----------------------------------------------------------------- helpers

func renderPol(t *testing.T, raw []byte, pol *policy.Policy) *EmailDoc {
	t.Helper()
	doc, err := RenderWithOptions(raw, attachDir, Options{Policy: pol})
	if err != nil {
		t.Fatalf("RenderWithOptions: %v", err)
	}
	checkSkipRecords(t, doc)
	return doc
}

// checkSkipRecords asserts the structural contract of every File: a stored
// file has a Rel under the attachment dir and non-nil Content, a skipped one
// has neither but keeps a complete, honest record.
func checkSkipRecords(t *testing.T, doc *EmailDoc) {
	t.Helper()
	if doc.PolicyDigest == "" {
		t.Error("PolicyDigest is empty; a skip record cannot say which policy produced it")
	}
	var stored int64
	for i, f := range doc.Files {
		if f.Skipped {
			t.Errorf("Files[%d] %q is marked Skipped", i, f.Rel)
		}
		if f.Rel == "" || f.Content == nil || f.Name == "" {
			t.Errorf("Files[%d] = %+v, want Rel, Content and Name set", i, f)
		}
		if f.SniffedType == "" {
			t.Errorf("Files[%d] %q has no SniffedType", i, f.Rel)
		}
		if f.Size != int64(len(f.Content)) {
			t.Errorf("Files[%d] %q Size = %d, len(Content) = %d", i, f.Rel, f.Size, len(f.Content))
		}
		if !policy.ValidReason(f.SkipReason) {
			t.Errorf("Files[%d] %q SkipReason = %q, not a policy reason", i, f.Rel, f.SkipReason)
		}
		if f.Warned != (f.SkipReason != policy.ReasonNone) {
			t.Errorf("Files[%d] %q Warned = %v with reason %q", i, f.Rel, f.Warned, f.SkipReason)
		}
		stored += f.Size
	}
	if stored != doc.StoredBytes {
		t.Errorf("StoredBytes = %d, sum of Files = %d", doc.StoredBytes, stored)
	}
	// (source, stable_id, part_key) is the skip ledger's primary key, so two
	// parts of one message may never share a PartKey.
	seen := map[string]bool{}
	for _, f := range append(append([]File{}, doc.Files...), doc.Skipped...) {
		if seen[f.PartKey] {
			t.Errorf("duplicate PartKey %q (%s)", f.PartKey, f.Name)
		}
		seen[f.PartKey] = true
	}
	for i, f := range doc.Skipped {
		if !f.Skipped || f.Warned {
			t.Errorf("Skipped[%d] = %+v, want Skipped and not Warned", i, f)
		}
		if f.Rel != "" || f.Content != nil {
			t.Errorf("Skipped[%d] %q leaks a path or bytes: rel=%q content=%d",
				i, f.Name, f.Rel, len(f.Content))
		}
		if f.Name == "" {
			t.Errorf("Skipped[%d] has no Name; a retro-fetch would not know what to call it", i)
		}
		if f.PartKey == "" {
			t.Errorf("Skipped[%d] %q has no PartKey; it could never be re-fetched", i, f.Name)
		}
		// The honesty rule: the sniffed type is recorded even on a refusal.
		if f.SniffedType == "" {
			t.Errorf("Skipped[%d] %q has no SniffedType", i, f.Name)
		}
		if f.SkipReason == policy.ReasonNone || !policy.ValidReason(f.SkipReason) {
			t.Errorf("Skipped[%d] %q SkipReason = %q", i, f.Name, f.SkipReason)
		}
		if f.SkipDetail == "" {
			t.Errorf("Skipped[%d] %q has no SkipDetail", i, f.Name)
		}
		if !hasWarning(doc, f.Name) {
			t.Errorf("Skipped[%d] %q is not mentioned in Warnings: %v", i, f.Name, doc.Warnings)
		}
	}
}

func onlySkipped(t *testing.T, doc *EmailDoc) File {
	t.Helper()
	checkSkipRecords(t, doc)
	if len(doc.Skipped) != 1 {
		t.Fatalf("Skipped = %v, want exactly one", skipNames(doc))
	}
	return doc.Skipped[0]
}

func skipNames(doc *EmailDoc) []string {
	out := make([]string, len(doc.Skipped))
	for i, f := range doc.Skipped {
		out[i] = fmt.Sprintf("%s(%s)", f.Name, f.SkipReason)
	}
	return out
}

func skipByName(t *testing.T, doc *EmailDoc, name string) File {
	t.Helper()
	for _, f := range doc.Skipped {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no skipped file named %q; skipped = %v", name, skipNames(doc))
	return File{}
}

func mustPolicy(t *testing.T, mutate func(*policy.Settings)) *policy.Policy {
	t.Helper()
	s := policy.DefaultSettings()
	mutate(&s)
	p, err := policy.New(s)
	if err != nil {
		t.Fatalf("policy.New: %v", err)
	}
	return p
}

// ------------------------------------------------------------------- tests

// TestRenderPolicyAllowlist is the headline case: hostile and macro-bearing
// parts are refused and recorded while the business core is stored, all in
// one message.
func TestRenderPolicyAllowlist(t *testing.T) {
	raw := message(t,
		part{ctype: "application/pdf", filename: "invoice.pdf", body: pdfBytes()},
		part{ctype: "application/octet-stream", filename: "setup.exe", body: exeBytes()},
		part{ctype: "application/vnd.ms-word.document.macroEnabled.12", filename: "macros.docm", body: docmBytes(t)},
		part{ctype: "image/svg+xml", filename: "logo.svg", body: svgBytes()},
		part{
			ctype:    "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			filename: "contract.docx",
			body:     bigDocxBytes(t),
		},
	)
	doc := renderPol(t, raw, nil) // nil == policy.Default()

	wantRels(t, doc, attachDir+"/invoice.pdf", attachDir+"/contract.docx")
	if len(doc.Files[1].Content) <= 4096 {
		t.Fatalf("the .docx fixture is %d bytes; it must exceed mimetype's 4096-byte default limit to be a regression test",
			len(doc.Files[1].Content))
	}
	if got := doc.Files[1].SniffedType; got != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		t.Errorf("the >4KB .docx sniffed as %q — mimetype.SetLimit(0) is not in effect", got)
	}

	wantSkips := map[string]string{
		"setup.exe":   policy.ReasonNotAllowlistedExtension,
		"macros.docm": policy.ReasonMacroOffice,
		"logo.svg":    policy.ReasonSVGDenied,
	}
	if len(doc.Skipped) != len(wantSkips) {
		t.Fatalf("Skipped = %v, want %v", skipNames(doc), wantSkips)
	}
	for name, reason := range wantSkips {
		f := skipByName(t, doc, name)
		if f.SkipReason != reason {
			t.Errorf("%s: SkipReason = %q, want %q", name, f.SkipReason, reason)
		}
		if f.SniffedType == "" {
			t.Errorf("%s: SniffedType is empty on a skip", name)
		}
		if f.OrigName != name {
			t.Errorf("%s: OrigName = %q", name, f.OrigName)
		}
		if f.DeclaredType == "" {
			t.Errorf("%s: DeclaredType is empty", name)
		}
	}
	// The .docm was refused on its extension: mimetype reports it as a plain
	// .docx, so content sniffing offers zero protection there.
	if got := skipByName(t, doc, "macros.docm").SniffedType; !strings.Contains(got, "wordprocessingml.document") {
		t.Errorf("docm sniffed as %q, want the plain docx type (that is why the extension is the only defence)", got)
	}
}

// TestRenderPolicyMacroOfficeByContent: a .docm renamed to .docx is invisible
// to the sniffer, so the zip CENTRAL DIRECTORY name check is what catches it.
func TestRenderPolicyMacroOfficeByContent(t *testing.T) {
	raw := message(t, part{
		ctype:    "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		filename: "harmless.docx",
		body:     docmBytes(t),
	})
	doc := renderPol(t, raw, nil)

	wantRels(t, doc)
	if f := onlySkipped(t, doc); f.SkipReason != policy.ReasonMacroOffice {
		t.Errorf("SkipReason = %q, want macro_office", f.SkipReason)
	}
}

// TestRenderPolicyOverSizeCap: the per-attachment cap refuses the part and
// records the exact byte count, so `comms refetch` can re-decide it offline
// after the cap is raised.
func TestRenderPolicyOverSizeCap(t *testing.T) {
	big := append(pdfBytes(), bytes.Repeat([]byte("A"), 200<<10)...)
	raw := message(t,
		part{ctype: "application/pdf", filename: "small.pdf", body: pdfBytes()},
		part{ctype: "application/pdf", filename: "huge.pdf", body: big},
	)
	pol := mustPolicy(t, func(s *policy.Settings) { s.MaxSize = 64 << 10 })
	doc := renderPol(t, raw, pol)

	wantRels(t, doc, attachDir+"/small.pdf")
	f := onlySkipped(t, doc)
	if f.SkipReason != policy.ReasonOverSizeCap {
		t.Fatalf("SkipReason = %q, want over_size_cap", f.SkipReason)
	}
	if f.Size != int64(len(big)) {
		t.Errorf("Size = %d, want the decoded byte count %d", f.Size, len(big))
	}
	if f.SniffedType != "application/pdf" {
		t.Errorf("SniffedType = %q; an over-cap skip is still sniffed", f.SniffedType)
	}
	if !strings.Contains(f.SkipDetail, "max_size") {
		t.Errorf("SkipDetail = %q, want the config key that would undo it", f.SkipDetail)
	}
}

// TestRenderPolicyPartCountCap: parts past attachments.max_parts_per_message
// are recorded, never dropped, and the ones that fit are the first ones in
// document order.
func TestRenderPolicyPartCountCap(t *testing.T) {
	var parts []part
	for i := 1; i <= 5; i++ {
		parts = append(parts, part{
			ctype:    "application/pdf",
			filename: fmt.Sprintf("doc-%d.pdf", i),
			body:     pdfBytes(),
		})
	}
	pol := mustPolicy(t, func(s *policy.Settings) { s.MaxPartsPerMessage = 2 })
	doc := renderPol(t, message(t, parts...), pol)

	wantRels(t, doc, attachDir+"/doc-1.pdf", attachDir+"/doc-2.pdf")
	if len(doc.Skipped) != 3 {
		t.Fatalf("Skipped = %v, want 3", skipNames(doc))
	}
	for _, f := range doc.Skipped {
		if f.SkipReason != policy.ReasonOverMessageBudget {
			t.Errorf("%s: SkipReason = %q, want over_message_budget", f.Name, f.SkipReason)
		}
		if !strings.Contains(f.SkipDetail, "max_parts_per_message") {
			t.Errorf("%s: SkipDetail = %q", f.Name, f.SkipDetail)
		}
	}
}

// TestRenderPolicyPerMessageBudget: the per-message decoded total is charged
// against stored bytes only, so a refused part does not consume the budget.
func TestRenderPolicyPerMessageBudget(t *testing.T) {
	body := append(pdfBytes(), bytes.Repeat([]byte("B"), 8<<10)...)
	raw := message(t,
		part{ctype: "application/pdf", filename: "a.pdf", body: body},
		part{ctype: "application/pdf", filename: "b.pdf", body: body},
		part{ctype: "application/pdf", filename: "c.pdf", body: pdfBytes()},
	)
	pol := mustPolicy(t, func(s *policy.Settings) { s.MaxPerMessage = int64(len(body)) + 100 })
	doc := renderPol(t, raw, pol)

	// a.pdf fits; b.pdf does not; c.pdf is tiny and still fits in what is left.
	wantRels(t, doc, attachDir+"/a.pdf", attachDir+"/c.pdf")
	f := onlySkipped(t, doc)
	if f.Name != "b.pdf" || f.SkipReason != policy.ReasonOverMessageBudget {
		t.Errorf("skipped %q/%q, want b.pdf/over_message_budget", f.Name, f.SkipReason)
	}
	if doc.StoredBytes != int64(len(body)+len(pdfBytes())) {
		t.Errorf("StoredBytes = %d, want only the stored parts", doc.StoredBytes)
	}
}

// TestRenderPolicyRunBudget: the run budget carries across messages via
// Options.RunBytesSoFar, and it is a transient reason — `comms refetch`
// retries it regardless of the policy digest.
func TestRenderPolicyRunBudget(t *testing.T) {
	raw := message(t, part{ctype: "application/pdf", filename: "a.pdf", body: pdfBytes()})
	pol := mustPolicy(t, func(s *policy.Settings) { s.RunBudget = 1 << 20 })
	doc, err := RenderWithOptions(raw, attachDir, Options{Policy: pol, RunBytesSoFar: 1 << 20})
	if err != nil {
		t.Fatalf("RenderWithOptions: %v", err)
	}
	checkSkipRecords(t, doc)

	wantRels(t, doc)
	f := onlySkipped(t, doc)
	if f.SkipReason != policy.ReasonOverRunBudget {
		t.Fatalf("SkipReason = %q, want over_run_budget", f.SkipReason)
	}
	if !policy.Transient(f.SkipReason) {
		t.Error("over_run_budget must be a transient reason")
	}
}

// TestRenderSkippedCIDDegradesToAltText: an inline image the policy refuses
// must never leave a link to a file nobody wrote.
func TestRenderSkippedCIDDegradesToAltText(t *testing.T) {
	body := `<p>before</p><img src="cid:logo@example.com" alt="Acme logo"><p>after</p>`
	raw := messageOfType(t, "multipart/related", body, "text/html",
		part{ctype: "image/svg+xml", filename: "logo.svg", cid: "logo@example.com", body: svgBytes()},
	)
	doc := renderPol(t, raw, nil)

	wantRels(t, doc)
	f := onlySkipped(t, doc)
	if f.SkipReason != policy.ReasonSVGDenied {
		t.Fatalf("SkipReason = %q, want svg_denied", f.SkipReason)
	}
	if !strings.Contains(doc.BodyMD, "Acme logo") {
		t.Errorf("BodyMD did not degrade to the alt text:\n%s", doc.BodyMD)
	}
	if strings.Contains(doc.BodyMD, "logo.svg") || strings.Contains(doc.BodyMD, "cid:") {
		t.Errorf("BodyMD kept a dead link:\n%s", doc.BodyMD)
	}
	if !hasWarning(doc, "cid:logo@example.com is not stored (svg_denied)") {
		t.Errorf("missing the skipped-cid warning: %v", doc.Warnings)
	}
	// The regular unresolved-cid message must not be used for this case: it
	// would say the reference was dangling when in fact the part was refused.
	if hasWarning(doc, "unresolved inline image") {
		t.Errorf("a refused cid: part was reported as unresolved: %v", doc.Warnings)
	}
}

// TestRenderStoredCIDStillLinks is the control for the test above.
func TestRenderStoredCIDStillLinks(t *testing.T) {
	body := `<img src="cid:pic@example.com" alt="Acme logo">`
	raw := messageOfType(t, "multipart/related", body, "text/html",
		part{ctype: "image/png", filename: "pic.png", cid: "pic@example.com", body: pngBytes()},
	)
	doc := renderPol(t, raw, nil)

	wantRels(t, doc, attachDir+"/pic.png")
	if !strings.Contains(doc.BodyMD, "("+attachDir+"/pic.png)") {
		t.Errorf("BodyMD lost the inline link:\n%s", doc.BodyMD)
	}
}

// TestRenderDataURISVGSkipped: the data: URI path writes bytes into the vault
// exactly like an attachment, so it goes through the same policy.
func TestRenderDataURISVGSkipped(t *testing.T) {
	svg := base64.StdEncoding.EncodeToString(svgBytes())
	png := base64.StdEncoding.EncodeToString(pngBytes())
	body := `<img src="data:image/svg+xml;base64,` + svg + `" alt="vector logo">` +
		`<img src="data:image/png;base64,` + png + `" alt="raster logo">`
	raw := messageOfType(t, "multipart/mixed", body, "text/html")
	doc := renderPol(t, raw, nil)

	// The counter does not rewind: the stored PNG stays inline-2 so that
	// allowing SVG later cannot renumber a file already on disk.
	wantRels(t, doc, attachDir+"/inline-2.png")
	f := onlySkipped(t, doc)
	if f.Name != "inline-1.svg" || f.SkipReason != policy.ReasonSVGDenied {
		t.Errorf("skipped %q/%q, want inline-1.svg/svg_denied", f.Name, f.SkipReason)
	}
	if f.DeclaredType != "image/svg+xml" || f.SniffedType != "image/svg+xml" {
		t.Errorf("declared/sniffed = %q/%q", f.DeclaredType, f.SniffedType)
	}
	if !strings.HasSuffix(f.PartKey, "#data-1") {
		t.Errorf("PartKey = %q, want a data: URI key", f.PartKey)
	}
	if !strings.Contains(doc.BodyMD, "vector logo") {
		t.Errorf("BodyMD did not degrade to alt text:\n%s", doc.BodyMD)
	}
	if strings.Contains(doc.BodyMD, "data:") || strings.Contains(doc.BodyMD, ".svg") {
		t.Errorf("BodyMD kept the SVG:\n%s", doc.BodyMD)
	}
}

// TestRenderSkippedPartsReserveNames: a refused part still owns its filename,
// so a later widening writes it under the name that was recorded instead of
// colliding with a neighbour that took it in the meantime.
func TestRenderSkippedPartsReserveNames(t *testing.T) {
	raw := message(t,
		part{ctype: "application/pdf", filename: "invoice.pdf", body: exeBytes()}, // lying part
		part{ctype: "application/pdf", filename: "invoice.pdf", body: pdfBytes()},
	)
	doc := renderPol(t, raw, nil)

	wantRels(t, doc, attachDir+"/invoice_2.pdf")
	f := onlySkipped(t, doc)
	if f.Name != "invoice.pdf" {
		t.Errorf("skipped Name = %q, want the reserved invoice.pdf", f.Name)
	}
	if f.SkipReason != policy.ReasonExtensionContentMismatch {
		t.Errorf("SkipReason = %q, want extension_content_mismatch", f.SkipReason)
	}
}

// TestRenderStoreWarnKeepsTheFile: on_mismatch = "store-warn" widens the rule
// without losing the record — the file lands on disk AND the disagreement is
// reported.
func TestRenderStoreWarnKeepsTheFile(t *testing.T) {
	raw := message(t, part{ctype: "application/pdf", filename: "invoice.pdf", body: exeBytes()})
	pol := mustPolicy(t, func(s *policy.Settings) { s.OnMismatch = policy.OnMismatchStoreWarn })
	doc := renderPol(t, raw, pol)

	wantRels(t, doc, attachDir+"/invoice.pdf")
	if len(doc.Skipped) != 0 {
		t.Errorf("Skipped = %v, want none", skipNames(doc))
	}
	f := doc.Files[0]
	if !f.Warned || f.SkipReason != policy.ReasonExtensionContentMismatch {
		t.Errorf("stored file = %+v, want Warned with extension_content_mismatch", f)
	}
	if !hasWarning(doc, "stored under protest") {
		t.Errorf("a file stored under protest must say so: %v", doc.Warnings)
	}
}

// TestRenderExtensionlessDerivesExtension: an inline part with no filename
// extension takes one from its content, and the File says the extension was
// derived so the note can be honest about it.
func TestRenderExtensionlessDerivesExtension(t *testing.T) {
	raw := message(t, part{ctype: "image/png", filename: "screenshot", body: pngBytes()})
	doc := renderPol(t, raw, nil)

	wantRels(t, doc, attachDir+"/screenshot.png")
	f := doc.Files[0]
	if !f.DerivedExt {
		t.Error("DerivedExt = false; the .png came from the content, not the sender")
	}
	if f.OrigName != "screenshot" {
		t.Errorf("OrigName = %q, want the sender's name unchanged", f.OrigName)
	}
}

// TestRenderExtensionlessUnknownSkipped: bytes matching no known format and
// carrying no extension cannot be named honestly, so they are recorded.
func TestRenderExtensionlessUnknownSkipped(t *testing.T) {
	blob := make([]byte, 512)
	for i := range blob {
		blob[i] = byte(i*7 + 0x80) // no magic, never valid UTF-8
	}
	raw := message(t, part{ctype: "application/octet-stream", body: blob})
	doc := renderPol(t, raw, nil)

	wantRels(t, doc)
	f := onlySkipped(t, doc)
	if f.SkipReason != policy.ReasonNotAllowlistedContent {
		t.Errorf("SkipReason = %q, want not_allowlisted_content", f.SkipReason)
	}
	if f.SniffedType != "application/octet-stream" {
		t.Errorf("SniffedType = %q", f.SniffedType)
	}
}

// TestRenderZeroByteAttachmentStored: a 0-byte attachment always sniffs
// text/plain; under an allowlisted extension it is stored without complaint,
// with a real (empty, non-nil) Content so a file is written.
func TestRenderZeroByteAttachmentStored(t *testing.T) {
	raw := message(t, part{ctype: "application/pdf", filename: "empty.pdf", body: nil})
	doc := renderPol(t, raw, nil)

	wantRels(t, doc, attachDir+"/empty.pdf")
	if doc.Files[0].Content == nil || len(doc.Files[0].Content) != 0 {
		t.Errorf("Content = %v, want a non-nil zero-length slice", doc.Files[0].Content)
	}
}

// TestRenderPolicyRunsOnTheFinalSanitizedName is the load-bearing ordering
// test. internal/naming owns the iCloud sync-exclusion rewrite, which appends
// ".bin" to an excluded extension and therefore CHANGES the extension the
// allowlist keys on. The decision must be made on the name that lands on
// disk, not on the sender's.
func TestRenderPolicyRunsOnTheFinalSanitizedName(t *testing.T) {
	if got := naming.SanitizeFilename("notes.tmp", "x"); got != "notes.tmp.bin" {
		t.Fatalf("naming.SanitizeFilename(\"notes.tmp\") = %q, want notes.tmp.bin — this test rests on that rewrite", got)
	}
	raw := message(t, part{ctype: "text/plain", filename: "notes.tmp", body: []byte("plain text notes\n")})
	doc := renderPol(t, raw, nil)

	wantRels(t, doc)
	f := onlySkipped(t, doc)
	if f.Name != "notes.tmp.bin" {
		t.Errorf("Name = %q, want the sanitized notes.tmp.bin", f.Name)
	}
	if f.SkipReason != policy.ReasonNotAllowlistedExtension {
		t.Errorf("SkipReason = %q", f.SkipReason)
	}
	if !strings.Contains(f.SkipDetail, ".bin") {
		t.Errorf("SkipDetail = %q, want it to name the extension actually decided on", f.SkipDetail)
	}
}

// TestSanitizeKeepsAllowlistedExtensionsAllowlisted is the mandated
// counterpart: for every extension on the allowlist, and for the adversarial
// stems the iCloud sanitizer rewrites, sanitizing must NOT knock the name off
// the allowlist. Otherwise the ordering above would quietly refuse real
// business documents.
func TestSanitizeKeepsAllowlistedExtensionsAllowlisted(t *testing.T) {
	stems := []string{
		"report",
		"Dropbox",     // an excluded whole name: "_Dropbox"
		"~$q3",        // an excluded prefix: "_~$q3"
		"plan.NoSync", // an excluded substring: "plan_NoSync"
		"a b  c",
		"..hidden..",
		strings.Repeat("é", 60), // pushes the 100-byte cap on a rune boundary
	}
	exts := policy.Default().AllowedExtensions()
	if len(exts) == 0 {
		t.Fatal("the default policy allows nothing")
	}
	for _, ext := range exts {
		for _, stem := range stems {
			raw := stem + "." + ext
			got := naming.SanitizeFilename(raw, "fallback.pdf")
			if naming.SyncExcluded(got) {
				t.Errorf("SanitizeFilename(%q) = %q, still iCloud-excluded", raw, got)
			}
			if norm := policy.NormalizeExt(got); norm != ext {
				t.Errorf("SanitizeFilename(%q) = %q: extension became %q, want %q — the allowlist would refuse a legitimate file",
					raw, got, norm, ext)
			}
			// naming.Unique's collision suffix must not move the extension
			// either: the decision runs on the post-Unique name.
			taken := map[string]bool{naming.CollisionKey(got): true}
			uniq := naming.Unique(taken, got)
			if norm := policy.NormalizeExt(uniq); norm != ext {
				t.Errorf("naming.Unique(%q) = %q: extension became %q, want %q", got, uniq, norm, ext)
			}
		}
	}
}

// TestRenderPolicyDigestIsRecorded: every skip must be attributable to the
// policy that produced it, or a later run cannot tell that widening the
// policy makes the skip worth re-deciding.
func TestRenderPolicyDigestIsRecorded(t *testing.T) {
	raw := message(t, part{ctype: "application/pdf", filename: "a.pdf", body: pdfBytes()})

	doc := renderPol(t, raw, nil)
	if want := policy.Default().PolicyDigest(); doc.PolicyDigest != want {
		t.Errorf("PolicyDigest = %q, want %q", doc.PolicyDigest, want)
	}

	pol := mustPolicy(t, func(s *policy.Settings) { s.AllowSVG = true })
	widened := renderPol(t, raw, pol)
	if widened.PolicyDigest == doc.PolicyDigest {
		t.Error("widening the policy did not change the recorded digest")
	}
}

// TestRenderAttachmentsSurviveBodyFailure: the files-first rule still holds
// with the policy in the path — a body that cannot be converted must not cost
// the attachments, stored or recorded.
func TestRenderAttachmentsSurviveBodyFailure(t *testing.T) {
	var huge strings.Builder
	huge.WriteString("<html><body>")
	for huge.Len() < maxHTMLBytes+1024 {
		huge.WriteString("<p>" + strings.Repeat("x", 1023) + "</p>")
	}
	huge.WriteString("</body></html>")
	raw := messageOfType(t, "multipart/mixed", huge.String(), "text/html",
		part{ctype: "application/pdf", filename: "keep.pdf", body: pdfBytes()},
		part{ctype: "application/octet-stream", filename: "drop.exe", body: exeBytes()},
	)
	doc := renderPol(t, raw, nil)

	wantRels(t, doc, attachDir+"/keep.pdf")
	if f := onlySkipped(t, doc); f.Name != "drop.exe" {
		t.Errorf("skipped %q, want drop.exe", f.Name)
	}
}

// TestRenderPartKeysAreDistinct: the DB key is (source, stable_id, part_key),
// so two parts of one message may never share a key.
func TestRenderPartKeysAreDistinct(t *testing.T) {
	svg := base64.StdEncoding.EncodeToString(svgBytes())
	body := `<img src="data:image/svg+xml;base64,` + svg + `" alt="a">` +
		`<img src="data:image/svg+xml;base64,` + svg + `" alt="b">`
	raw := messageOfType(t, "multipart/mixed", body, "text/html",
		part{ctype: "application/pdf", filename: "a.pdf", body: pdfBytes()},
		part{ctype: "application/octet-stream", filename: "b.exe", body: exeBytes()},
	)
	doc := renderPol(t, raw, nil)

	seen := map[string]bool{}
	for _, f := range append(append([]File{}, doc.Files...), doc.Skipped...) {
		if f.PartKey == "" {
			t.Errorf("%q has no PartKey", f.Name)
		}
		if seen[f.PartKey] {
			t.Errorf("duplicate PartKey %q", f.PartKey)
		}
		seen[f.PartKey] = true
	}
	if len(seen) != 4 {
		t.Errorf("part keys = %v, want 4 distinct", seen)
	}
}
