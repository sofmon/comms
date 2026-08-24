package policy

import (
	"strings"
	"testing"

	"github.com/gabriel-vasile/mimetype"

	"save/internal/naming"
)

// decideCase is one row of the allow/deny table.
type decideCase struct {
	name       string
	file       string // the FINAL sanitized filename presented to Decide
	content    []byte
	settings   func(*Settings)
	wantStore  bool
	wantReason string
	wantSniff  string // "" skips the assertion
	wantExt    string // "" skips the assertion
	wantDerive bool
}

func runDecide(t *testing.T, cases []decideCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultSettings()
			if tc.settings != nil {
				tc.settings(&s)
			}
			p, err := New(s)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			v := p.Decide(Input{Name: tc.file, Content: tc.content})
			if v.Store != tc.wantStore || v.Reason != tc.wantReason {
				t.Errorf("Decide(%q) = {Store:%v Reason:%q sniff:%q ext:%q}\n want {Store:%v Reason:%q}\n detail: %s",
					tc.file, v.Store, v.Reason, v.SniffedType, v.NormalizedExt, tc.wantStore, tc.wantReason, v.Detail)
			}
			if tc.wantSniff != "" && v.SniffedType != tc.wantSniff {
				t.Errorf("SniffedType = %q, want %q", v.SniffedType, tc.wantSniff)
			}
			if tc.wantExt != "" && v.NormalizedExt != tc.wantExt {
				t.Errorf("NormalizedExt = %q, want %q", v.NormalizedExt, tc.wantExt)
			}
			if v.DerivedExt != tc.wantDerive {
				t.Errorf("DerivedExt = %v, want %v", v.DerivedExt, tc.wantDerive)
			}
			if !ValidReason(v.Reason) {
				t.Errorf("Reason %q is not in the enum", v.Reason)
			}
			if !v.Store && v.Reason == ReasonNone {
				t.Error("a refusal must always carry a Reason — nothing is ever dropped silently")
			}
			if !v.Store && v.Detail == "" {
				t.Error("a refusal must carry a Detail for the note")
			}
		})
	}
}

// TestDecideAllows covers every shape on the default allowlist that can be
// built from real magic bytes.
func TestDecideAllows(t *testing.T) {
	runDecide(t, []decideCase{
		{name: "pdf", file: "invoice.pdf", content: pdfBytes(), wantStore: true, wantSniff: "application/pdf", wantExt: "pdf"},
		{name: "docx", file: "contract.docx", content: docxBytes(t), wantStore: true,
			wantSniff: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{name: "dotx sniffs as docx", file: "letterhead.dotx", content: docxBytes(t), wantStore: true},
		{name: "xlsx", file: "budget.xlsx", content: xlsxBytes(t), wantStore: true},
		{name: "xltx sniffs as xlsx", file: "template.xltx", content: xlsxBytes(t), wantStore: true},
		{name: "pptx", file: "deck.pptx", content: pptxBytes(t), wantStore: true},
		{name: "potx sniffs as pptx", file: "brand.potx", content: pptxBytes(t), wantStore: true},
		{name: "legacy doc tolerates ole parent", file: "old.doc", content: oleBytes(), wantStore: true,
			wantSniff: "application/x-ole-storage"},
		{name: "legacy xls tolerates ole parent", file: "old.xls", content: oleBytes(), wantStore: true},
		{name: "legacy ppt tolerates ole parent", file: "old.ppt", content: oleBytes(), wantStore: true},
		{name: "msg tolerates ole parent", file: "forward.msg", content: oleBytes(), wantStore: true},
		{name: "odt", file: "notes.odt", content: odfBytes(t, "application/vnd.oasis.opendocument.text"), wantStore: true},
		{name: "ods", file: "sheet.ods", content: odfBytes(t, "application/vnd.oasis.opendocument.spreadsheet"), wantStore: true},
		{name: "odp", file: "talk.odp", content: odfBytes(t, "application/vnd.oasis.opendocument.presentation"), wantStore: true},
		{name: "odg", file: "draw.odg", content: odfBytes(t, "application/vnd.oasis.opendocument.graphics"), wantStore: true},
		{name: "epub", file: "book.epub", content: odfBytes(t, "application/epub+zip"), wantStore: true},
		{name: "rtf", file: "letter.rtf", content: rtfBytes(), wantStore: true, wantSniff: "text/rtf"},
		{name: "txt", file: "readme.txt", content: textBytes(), wantStore: true, wantSniff: "text/plain"},
		{name: "md", file: "notes.md", content: textBytes(), wantStore: true},
		{name: "log", file: "build.log", content: textBytes(), wantStore: true},
		{name: "csv", file: "export.csv", content: csvBytes(), wantStore: true, wantSniff: "text/csv"},
		{name: "json", file: "payload.json", content: jsonBytes(), wantStore: true, wantSniff: "application/json"},
		{name: "xml invoice", file: "ubl.xml", content: xmlBytes(), wantStore: true, wantSniff: "text/xml"},
		{name: "yaml sniffs as text", file: "conf.yaml", content: textBytes(), wantStore: true},
		{name: "yml sniffs as text", file: "conf.yml", content: textBytes(), wantStore: true},
		{name: "png", file: "shot.png", content: pngBytes(), wantStore: true, wantSniff: "image/png"},
		{name: "jpg", file: "photo.jpg", content: jpegBytes(), wantStore: true, wantSniff: "image/jpeg"},
		{name: "jpeg", file: "photo.jpeg", content: jpegBytes(), wantStore: true},
		{name: "gif", file: "anim.gif", content: gifBytes(), wantStore: true, wantSniff: "image/gif"},
		{name: "ics invite", file: "meeting.ics", content: icsBytes(), wantStore: true, wantSniff: "text/calendar"},
		{name: "vcf contact", file: "card.vcf", content: vcfBytes(), wantStore: true, wantSniff: "text/vcard"},
		{name: "eml forwarded mail", file: "fwd.eml", content: emlBytes(), wantStore: true, wantSniff: "message/rfc822"},
		{name: "zip with allow_containers", file: "bundle.zip", content: plainZipBytes(t), wantStore: true,
			wantSniff: "application/zip"},
		{name: "uppercase extension normalizes", file: "INVOICE.PDF", content: pdfBytes(), wantStore: true, wantExt: "pdf"},
		{name: "svg with allow_svg", file: "logo.svg", content: svgBytes(),
			settings: func(s *Settings) { s.AllowSVG = true }, wantStore: true, wantSniff: "image/svg+xml"},
		{name: "docm with allow_macro_office", file: "budget.docm", content: docmBytes(t),
			settings: func(s *Settings) { s.AllowMacroOffice = true }, wantStore: true},
		{name: "operator-added extension accepts any content", file: "backup.7z", content: randomBytes(64),
			settings: func(s *Settings) { s.AllowExtensions = []string{"7z"} }, wantStore: true,
			wantSniff: octetStream},
		{name: "operator-added extension with an explicit type", file: "backup.7z", content: plainZipBytes(t),
			settings: func(s *Settings) { s.AllowExtensions = []string{"7z=application/zip"} }, wantStore: true},
	})
}

// TestDecideDenies covers the refusals, one per Reason.
func TestDecideDenies(t *testing.T) {
	runDecide(t, []decideCase{
		{name: "windows payload", file: "setup.exe", content: exeBytes(),
			wantReason: ReasonNotAllowlistedExtension, wantExt: "exe"},
		{name: "double extension keys on the last one", file: "invoice.pdf.exe", content: pdfBytes(),
			wantReason: ReasonNotAllowlistedExtension, wantExt: "exe", wantSniff: "application/pdf"},
		{name: "macos shell script", file: "install.sh", content: textBytes(),
			wantReason: ReasonNotAllowlistedExtension, wantExt: "sh"},
		{name: "python script", file: "run.py", content: textBytes(),
			wantReason: ReasonNotAllowlistedExtension},
		{name: "html is not on the list", file: "page.html", content: []byte("<!DOCTYPE html><html></html>"),
			wantReason: ReasonNotAllowlistedExtension},
		{name: "other containers are not on the list", file: "archive.7z", content: randomBytes(64),
			wantReason: ReasonNotAllowlistedExtension},
		{name: "tar is not on the list", file: "archive.tar", content: randomBytes(64),
			wantReason: ReasonNotAllowlistedExtension},
		{name: "operator removed pdf", file: "invoice.pdf", content: pdfBytes(),
			settings:   func(s *Settings) { s.DenyExtensions = []string{"pdf"} },
			wantReason: ReasonNotAllowlistedExtension},
		{name: "deny beats allow for the same extension", file: "invoice.pdf", content: pdfBytes(),
			settings: func(s *Settings) {
				s.AllowExtensions = []string{"pdf"}
				s.DenyExtensions = []string{"pdf"}
			},
			wantReason: ReasonNotAllowlistedExtension},

		{name: "macro office by extension", file: "budget.xlsm", content: xlsxBytes(t),
			wantReason: ReasonMacroOffice, wantExt: "xlsm"},
		{name: "docm renamed to docx caught by the central directory", file: "budget.docx", content: docmBytes(t),
			wantReason: ReasonMacroOffice,
			wantSniff:  "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},

		{name: "svg denied by default", file: "logo.svg", content: svgBytes(),
			wantReason: ReasonSVGDenied, wantExt: "svg"},
		{name: "svg content hiding under xml", file: "logo.xml", content: svgBytes(),
			wantReason: ReasonSVGDenied, wantSniff: "image/svg+xml"},

		{name: "zip with allow_containers off", file: "bundle.zip", content: plainZipBytes(t),
			settings:   func(s *Settings) { s.AllowContainers = false },
			wantReason: ReasonContainerDenied},

		{name: "pdf that is really a zip", file: "invoice.pdf", content: plainZipBytes(t),
			wantReason: ReasonExtensionContentMismatch, wantSniff: "application/zip", wantExt: "pdf"},
		{name: "png that is really a pdf", file: "shot.png", content: pdfBytes(),
			wantReason: ReasonExtensionContentMismatch},
		{name: "pdf that is unrecognized bytes", file: "invoice.pdf", content: randomBytes(4096),
			wantReason: ReasonExtensionContentMismatch, wantSniff: octetStream},
		{name: "txt that is really an executable", file: "notes.txt", content: exeBytes(),
			wantReason: ReasonExtensionContentMismatch},
		{name: "txt that is really html is not tolerated", file: "notes.txt",
			content:    []byte("<!DOCTYPE html>\n<html><body>hi</body></html>"),
			wantReason: ReasonExtensionContentMismatch, wantSniff: "text/html"},
	})
}

// TestDecideToleratedNonMismatches locks in the disagreements that must NOT be
// reported, so the rule does not spam every note with noise.
func TestDecideToleratedNonMismatches(t *testing.T) {
	runDecide(t, []decideCase{
		{name: "csv that sniffs as plain text", file: "export.csv", content: textBytes(), wantStore: true,
			wantSniff: "text/plain"},
		{name: "tsv that sniffs as plain text", file: "export.tsv", content: textBytes(), wantStore: true},
		{name: "json that sniffs as plain text", file: "data.json", content: textBytes(), wantStore: true},
		{name: "xml that sniffs as plain text", file: "data.xml", content: textBytes(), wantStore: true},
		{name: "ics that sniffs as plain text", file: "invite.ics", content: textBytes(), wantStore: true},
		{name: "vcf that sniffs as plain text", file: "card.vcf", content: textBytes(), wantStore: true},
		{name: "txt holding json", file: "notes.txt", content: jsonBytes(), wantStore: true,
			wantSniff: "application/json"},
		{name: "md holding csv", file: "table.md", content: csvBytes(), wantStore: true},
		{name: "xml holding a +xml subtype", file: "feed.xml", wantStore: true,
			content: []byte(`<?xml version="1.0"?><rss version="2.0"><channel></channel></rss>`)},
		{name: "json holding a +json subtype", file: "place.json", wantStore: true,
			content: []byte(`{"type":"Feature","geometry":{"type":"Point","coordinates":[1,2]},"properties":{}}`)},

		{name: "0-byte pdf stores under its declared extension", file: "empty.pdf", content: []byte{},
			wantStore: true, wantSniff: "text/plain", wantExt: "pdf"},
		{name: "0-byte docx stores too", file: "empty.docx", content: nil, wantStore: true},

		{name: "tiny p7s that sniffs as octet-stream", file: "smime.p7s", content: randomBytes(2048),
			wantStore: true, wantSniff: octetStream},
		{name: "tiny p7m that sniffs as octet-stream", file: "smime.p7m", content: randomBytes(512),
			wantStore: true},
		{name: "pgp signature as armored text", file: "msg.asc", content: textBytes(), wantStore: true},
		{name: "oversized p7s is not tolerated", file: "smime.p7s", content: randomBytes(cryptoBlobMaxBytes + 1),
			wantReason: ReasonExtensionContentMismatch, wantSniff: octetStream},
	})
}

// TestDecideExtensionless covers inline parts that arrive with no filename.
func TestDecideExtensionless(t *testing.T) {
	runDecide(t, []decideCase{
		{name: "png derives .png", file: "", content: pngBytes(), wantStore: true,
			wantExt: "png", wantDerive: true, wantSniff: "image/png"},
		{name: "pdf derives .pdf", file: "attachment", content: pdfBytes(), wantStore: true,
			wantExt: "pdf", wantDerive: true},
		{name: "docx derives .docx", file: "", content: docxBytes(t), wantStore: true,
			wantExt: "docx", wantDerive: true},
		{name: "text derives .txt", file: "", content: textBytes(), wantStore: true,
			wantExt: "txt", wantDerive: true},
		{name: "octet-stream is refused", file: "", content: randomBytes(4096),
			wantReason: ReasonNotAllowlistedContent, wantSniff: octetStream},
		{name: "ole has no unambiguous extension", file: "", content: oleBytes(),
			wantReason: ReasonNotAllowlistedContent, wantSniff: "application/x-ole-storage"},
		{name: "executable content is refused", file: "", content: exeBytes(),
			wantReason: ReasonNotAllowlistedContent},
		{name: "svg content reports the actionable reason", file: "", content: svgBytes(),
			wantReason: ReasonSVGDenied},
		{name: "zip content with allow_containers off", file: "", content: plainZipBytes(t),
			settings:   func(s *Settings) { s.AllowContainers = false },
			wantReason: ReasonContainerDenied},
		{name: "zip content with allow_containers on derives .zip", file: "", content: plainZipBytes(t),
			wantStore: true, wantExt: "zip", wantDerive: true},
		{name: "a leading dot is not an extension", file: ".bashrc", content: textBytes(),
			wantStore: true, wantExt: "txt", wantDerive: true},
	})
}

// TestSniffLimitDocx is the regression test for the single highest-impact
// finding: with mimetype's 4096-byte default limit a real .docx sniffs as
// application/zip and this policy would WRONGLY SKIP it.
func TestSniffLimitDocx(t *testing.T) {
	content := bigDocxBytes(t)
	if len(content) < 5000 {
		t.Fatalf("fixture is only %d bytes; the word/ entry must sit past mimetype's 4096-byte default", len(content))
	}

	// Reproduce the bug with the stock limit.
	mimetype.SetLimit(4096)
	if got := Sniff(content); got != "application/zip" {
		t.Fatalf("with the stock 4096-byte limit the fixture sniffs as %q; it must reproduce the application/zip misdetection this test exists for", got)
	}
	p := Default()
	if v := p.Decide(Input{Name: "contract.docx", Content: content}); v.Store {
		t.Error("with the stock limit a real docx must be (wrongly) refused — that is the bug Init fixes")
	}

	// Init restores the whole-buffer limit; the same bytes are now a docx.
	Init()
	t.Cleanup(Init)
	const want = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	if got := Sniff(content); got != want {
		t.Fatalf("after Init the fixture sniffs as %q, want %q", got, want)
	}
	v := p.Decide(Input{Name: "contract.docx", Content: content})
	if !v.Store || v.Reason != ReasonNone {
		t.Errorf("Decide = {Store:%v Reason:%q}; a real docx must be stored once the sniff limit is lifted", v.Store, v.Reason)
	}
	if v.SniffedType != want {
		t.Errorf("SniffedType = %q, want %q", v.SniffedType, want)
	}
}

// TestInitIsAutomatic guards the "impossible to forget" requirement: importing
// the package is enough, because init calls Init.
func TestInitIsAutomatic(t *testing.T) {
	content := bigDocxBytes(t)
	if got := Sniff(content); !strings.Contains(got, "wordprocessingml") {
		t.Fatalf("package init must have lifted the sniff limit; a >4KB docx sniffed as %q", got)
	}
}

// TestSizeCaps covers every cap and budget, and the order they are applied in.
func TestSizeCaps(t *testing.T) {
	p, err := New(Settings{
		MaxSize:            1000,
		ChatMaxSize:        2000,
		MaxPerMessage:      5000,
		MaxPartsPerMessage: 3,
		RunBudget:          10000,
		FreeSpaceFloor:     4096,
		AllowContainers:    true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	big := func(n int) []byte { return append(pdfBytes(), make([]byte, n)...) }

	t.Run("over max_size", func(t *testing.T) {
		v := p.Decide(Input{Name: "a.pdf", Content: big(1200)})
		if v.Store || v.Reason != ReasonOverSizeCap {
			t.Fatalf("got {Store:%v Reason:%q}, want over_size_cap", v.Store, v.Reason)
		}
		if !strings.Contains(v.Detail, "max_size") {
			t.Errorf("Detail should name the key that refused it: %q", v.Detail)
		}
		if v.SniffedType != "application/pdf" {
			t.Errorf("SniffedType = %q; a skip record must still carry the sniffed type", v.SniffedType)
		}
	})
	t.Run("chat gets its own cap", func(t *testing.T) {
		in := Input{Name: "a.pdf", Content: big(1200), Chat: true}
		if v := p.Decide(in); !v.Store {
			t.Fatalf("1.2KB Chat attachment under a 2000-byte chat cap: %q", v.Reason)
		}
		in.Content = big(2200)
		v := p.Decide(in)
		if v.Store || v.Reason != ReasonOverSizeCap || !strings.Contains(v.Detail, "chat_max_size") {
			t.Fatalf("got {Store:%v Reason:%q Detail:%q}, want over_size_cap naming chat_max_size", v.Store, v.Reason, v.Detail)
		}
	})
	t.Run("over max_per_message", func(t *testing.T) {
		v := p.Decide(Input{Name: "a.pdf", Content: big(500), MessageBytesSoFar: 4800})
		if v.Store || v.Reason != ReasonOverMessageBudget {
			t.Fatalf("got {Store:%v Reason:%q}, want over_message_budget", v.Store, v.Reason)
		}
	})
	t.Run("over max_parts_per_message", func(t *testing.T) {
		v := p.Decide(Input{Name: "a.pdf", Content: pdfBytes(), PartsSoFar: 3})
		if v.Store || v.Reason != ReasonOverMessageBudget {
			t.Fatalf("got {Store:%v Reason:%q}, want over_message_budget", v.Store, v.Reason)
		}
	})
	t.Run("over run_budget", func(t *testing.T) {
		v := p.Decide(Input{Name: "a.pdf", Content: big(500), RunBytesSoFar: 9800})
		if v.Store || v.Reason != ReasonOverRunBudget {
			t.Fatalf("got {Store:%v Reason:%q}, want over_run_budget", v.Store, v.Reason)
		}
	})
	t.Run("free space floor", func(t *testing.T) {
		v := p.Decide(Input{Name: "a.pdf", Content: big(500), FreeSpace: 4500})
		if v.Store || v.Reason != ReasonFreeSpaceFloor {
			t.Fatalf("got {Store:%v Reason:%q}, want free_space_floor", v.Store, v.Reason)
		}
		if v := p.Decide(Input{Name: "a.pdf", Content: big(500), FreeSpace: 0}); !v.Store {
			t.Errorf("FreeSpace 0 means unknown and must not refuse: %q", v.Reason)
		}
	})
	t.Run("policy beats size so refetch can never resurrect an exe", func(t *testing.T) {
		v := p.Decide(Input{Name: "huge.exe", Content: append(exeBytes(), make([]byte, 9000)...)})
		if v.Reason != ReasonNotAllowlistedExtension {
			t.Fatalf("Reason = %q; a huge .exe must report not_allowlisted_extension, never over_size_cap — "+
				"otherwise raising max_size would make `save refetch` fetch it", v.Reason)
		}
	})
	t.Run("zero caps mean unlimited", func(t *testing.T) {
		open, err := New(Settings{})
		if err != nil {
			t.Fatal(err)
		}
		if v := open.Decide(Input{Name: "a.pdf", Content: big(1 << 20)}); !v.Store {
			t.Errorf("a zero cap must mean no limit, got %q", v.Reason)
		}
	})
}

// TestPreCheck covers the pre-download filter used by the Chat connector.
func TestPreCheck(t *testing.T) {
	p := Default()

	t.Run("refuses a denied extension without any bytes", func(t *testing.T) {
		v := p.PreCheck(Input{Name: "payload.exe", Size: 10})
		if v.Store || v.Reason != ReasonNotAllowlistedExtension {
			t.Fatalf("got {Store:%v Reason:%q}", v.Store, v.Reason)
		}
		if v.SniffedType != "" {
			t.Errorf("PreCheck must not claim a sniffed type: %q", v.SniffedType)
		}
	})
	t.Run("refuses an oversized blob without downloading it", func(t *testing.T) {
		v := p.PreCheck(Input{Name: "movie.mp4", Size: 200 * 1000 * 1000, Chat: true})
		if v.Store || v.Reason != ReasonOverSizeCap {
			t.Fatalf("got {Store:%v Reason:%q}", v.Store, v.Reason)
		}
	})
	t.Run("passing is not an authorization", func(t *testing.T) {
		v := p.PreCheck(Input{Name: "invoice.pdf", Size: 1000})
		if !v.Store {
			t.Fatalf("expected a pass, got %q", v.Reason)
		}
		// The same name with lying content is still refused by Decide.
		full := p.Decide(Input{Name: "invoice.pdf", Content: exeBytes()})
		if full.Store {
			t.Error("Decide must re-check the content after the download")
		}
	})
	t.Run("extensionless is deferred, never pre-refused", func(t *testing.T) {
		if v := p.PreCheck(Input{Name: "", Size: 100}); !v.Store {
			t.Errorf("PreCheck cannot decide an extensionless part without bytes: %q", v.Reason)
		}
	})
}

// TestOnMismatchStoreWarn: store-warn softens the extension/content
// disagreement and nothing else.
func TestOnMismatchStoreWarn(t *testing.T) {
	s := DefaultSettings()
	s.OnMismatch = OnMismatchStoreWarn
	p, err := New(s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	v := p.Decide(Input{Name: "invoice.pdf", Content: plainZipBytes(t)})
	if !v.Store || v.Reason != ReasonExtensionContentMismatch || !v.Warned() {
		t.Errorf("store-warn should store under protest: {Store:%v Reason:%q Warned:%v}", v.Store, v.Reason, v.Warned())
	}

	for _, tc := range []struct {
		name   string
		in     Input
		reason string
	}{
		{"denied extension", Input{Name: "run.exe", Content: exeBytes()}, ReasonNotAllowlistedExtension},
		{"macro office", Input{Name: "a.docm", Content: docxBytes(t)}, ReasonMacroOffice},
		{"svg", Input{Name: "a.svg", Content: svgBytes()}, ReasonSVGDenied},
		{"size cap", Input{Name: "a.pdf", Content: append(pdfBytes(), make([]byte, 60*1000*1000)...)}, ReasonOverSizeCap},
	} {
		if v := p.Decide(tc.in); v.Store || v.Reason != tc.reason {
			t.Errorf("%s under store-warn: {Store:%v Reason:%q}; store-warn must soften ONLY extension_content_mismatch",
				tc.name, v.Store, v.Reason)
		}
	}
}

// TestScanHook covers the two scan_action modes.
func TestScanHook(t *testing.T) {
	for _, tc := range []struct {
		action    string
		wantStore bool
	}{
		{ScanActionRecord, true},
		{ScanActionReject, false},
	} {
		s := DefaultSettings()
		s.ScanAction = tc.action
		s.ScanCommand = []string{"clamdscan", "--fdpass"}
		p, err := New(s)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		v := p.Decide(Input{Name: "invoice.pdf", Content: pdfBytes(), ScanFlagged: true})
		if v.Store != tc.wantStore || v.Reason != ReasonScanHookFlagged {
			t.Errorf("scan_action=%q: {Store:%v Reason:%q}, want Store=%v scan_hook_flagged",
				tc.action, v.Store, v.Reason, tc.wantStore)
		}
		if tc.wantStore && !v.Warned() {
			t.Errorf("scan_action=record must mark the file as stored under protest")
		}
	}
}

// TestHardDenyCannotBeAllowed: allow_extensions naming an executable type is a
// configuration ERROR, never a silent no-op.
func TestHardDenyCannotBeAllowed(t *testing.T) {
	for _, ext := range []string{"exe", "EXE", ".exe", "dmg", "sh", "app", "js", "jar"} {
		s := DefaultSettings()
		s.AllowExtensions = []string{ext}
		if _, err := New(s); err == nil {
			t.Errorf("allow_extensions = [%q] must be rejected", ext)
		} else if !strings.Contains(err.Error(), "cannot be allowed") {
			t.Errorf("allow_extensions = [%q]: unhelpful error %v", ext, err)
		}
	}
	// The flag-gated ones are NOT hard denies.
	for _, ext := range []string{"docm", "svg", "zip", "7z"} {
		if _, hard := HardDenied(ext); hard {
			t.Errorf("%q must be flag-gated or merely off the list, not hard-denied", ext)
		}
	}
}

// TestFlagGatedExtensionsRejectAllowList: svg/zip/macro-Office have their own
// flags, and listing them in allow_extensions must say so instead of being a
// silent no-op that leaves the digest describing a policy that is not applied.
func TestFlagGatedExtensionsRejectAllowList(t *testing.T) {
	for _, tc := range []struct{ ext, flag string }{
		{"svg", "allow_svg"},
		{"zip", "allow_containers"},
		{"docm", "allow_macro_office"},
		{"xlsm", "allow_macro_office"},
		{"potm", "allow_macro_office"},
	} {
		for _, on := range []bool{false, true} {
			s := DefaultSettings()
			s.AllowSVG, s.AllowContainers, s.AllowMacroOffice = on, on, on
			s.AllowExtensions = []string{tc.ext}
			_, err := New(s)
			if err == nil {
				t.Errorf("allow_extensions = [%q] (flags %v) must be rejected", tc.ext, on)
				continue
			}
			if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("allow_extensions = [%q]: error should point at %s, got %v", tc.ext, tc.flag, err)
			}
		}
	}
	// deny_extensions may still remove them, which is honest narrowing.
	s := DefaultSettings()
	s.DenyExtensions = []string{"zip"}
	p, err := New(s)
	if err != nil {
		t.Fatalf("deny_extensions must be able to remove a flag-gated extension: %v", err)
	}
	if p.PermittedTypes("zip") != nil {
		t.Error("deny_extensions = [\"zip\"] should have removed it")
	}
}

// TestNewRejectsBadSettings covers the constructor's validation.
func TestNewRejectsBadSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Settings)
		want string
	}{
		{"bad on_mismatch", func(s *Settings) { s.OnMismatch = "warn" }, "on_mismatch"},
		{"bad scan_action", func(s *Settings) { s.ScanAction = "delete" }, "scan_action"},
		{"empty allow entry", func(s *Settings) { s.AllowExtensions = []string{" "} }, "allow_extensions"},
		{"dotted allow entry", func(s *Settings) { s.AllowExtensions = []string{"tar.gz"} }, "allow_extensions"},
		{"bad content type", func(s *Settings) { s.AllowExtensions = []string{"7z=notatype"} }, "type/subtype"},
		{"empty type list", func(s *Settings) { s.AllowExtensions = []string{"7z="} }, "nothing after"},
		{"typed deny", func(s *Settings) { s.DenyExtensions = []string{"pdf=application/pdf"} }, "extension only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultSettings()
			tc.mut(&s)
			_, err := New(s)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v does not mention %q", err, tc.want)
			}
		})
	}
}

// TestPermittedTypesAndAccessors covers the read-only surface downstream
// callers use to render `save doctor` output.
func TestPermittedTypesAndAccessors(t *testing.T) {
	p := Default()
	if got := p.PermittedTypes("pdf"); len(got) != 1 || got[0] != "application/pdf" {
		t.Errorf("PermittedTypes(pdf) = %v", got)
	}
	if got := p.PermittedTypes("exe"); got != nil {
		t.Errorf("PermittedTypes(exe) = %v, want nil", got)
	}
	exts := p.AllowedExtensions()
	if !sortedUnique(exts) {
		t.Errorf("AllowedExtensions must be sorted and unique: %v", exts)
	}
	for _, want := range []string{"pdf", "docx", "heic", "zip", "eml"} {
		if !contains(exts, want) {
			t.Errorf("AllowedExtensions is missing %q", want)
		}
	}
	for _, notWant := range []string{"exe", "svg", "docm", "7z"} {
		if contains(exts, notWant) {
			t.Errorf("AllowedExtensions must not contain %q by default", notWant)
		}
	}
	if p.MaxSizeFor(false) != 50*1000*1000 {
		t.Errorf("MaxSizeFor(mail) = %d", p.MaxSizeFor(false))
	}
	if p.MaxSizeFor(true) != p.MaxSizeFor(false) {
		t.Error("chat inherits max_size when chat_max_size is unset")
	}
}

// TestReasonsEnum keeps the enum and its helpers in sync.
func TestReasonsEnum(t *testing.T) {
	if len(Reasons()) != 11 {
		t.Errorf("Reasons() has %d entries; update the DB CHECK constraint and the note renderer too", len(Reasons()))
	}
	seen := map[string]bool{}
	for _, r := range Reasons() {
		if seen[r] {
			t.Errorf("duplicate reason %q", r)
		}
		seen[r] = true
		if !ValidReason(r) {
			t.Errorf("ValidReason(%q) = false", r)
		}
		if r != strings.ToLower(r) || strings.ContainsAny(r, " -") {
			t.Errorf("reason %q must be a lowercase snake_case token", r)
		}
	}
	if !ValidReason(ReasonNone) {
		t.Error("the empty reason must be valid")
	}
	if ValidReason("nope") {
		t.Error("ValidReason must reject unknown strings")
	}
	if !Transient(ReasonOverRunBudget) || !Transient(ReasonFreeSpaceFloor) {
		t.Error("run budget and free space are transient")
	}
	if Transient(ReasonNotAllowlistedExtension) || Transient(ReasonMacroOffice) {
		t.Error("policy refusals are not transient")
	}
}

// TestSanitizedAllowlistedNamesStayAllowlisted is the invariant the iCloud
// sanitizer could silently break: SanitizeFilename appends ".bin" to a name
// whose extension iCloud refuses to sync, which would move the name off the
// allowlist. Every allowlisted extension must survive sanitizing.
func TestSanitizedAllowlistedNamesStayAllowlisted(t *testing.T) {
	p := Default()
	for _, ext := range p.AllowedExtensions() {
		for _, stem := range []string{"invoice", "Q3 Report", "naïve", "Dropbox", "~$draft", "a.NoSync"} {
			raw := stem + "." + ext
			final := naming.SanitizeFilename(raw, "attachment-1.bin")
			got := NormalizeExt(final)
			if got != ext {
				t.Errorf("SanitizeFilename(%q) = %q, whose extension normalizes to %q, want %q",
					raw, final, got, ext)
			}
			if p.PermittedTypes(got) == nil {
				t.Errorf("SanitizeFilename(%q) = %q moved the name off the allowlist", raw, final)
			}
		}
	}
}

// TestSanitizerBinRewriteIsRefused is the other half: a name whose extension
// iCloud refuses becomes ".bin", which is NOT allowlisted, and the policy must
// key on the name that would really land on disk.
func TestSanitizerBinRewriteIsRefused(t *testing.T) {
	final := naming.SanitizeFilename("draft.tmp", "attachment-1.bin")
	if final != "draft.tmp.bin" {
		t.Fatalf("naming.SanitizeFilename(draft.tmp) = %q; this test encodes the sanitizer's contract", final)
	}
	v := Default().Decide(Input{Name: final, Content: textBytes()})
	if v.Store || v.Reason != ReasonNotAllowlistedExtension || v.NormalizedExt != "bin" {
		t.Errorf("Decide(%q) = {Store:%v Reason:%q Ext:%q}; the decision must key on the FINAL sanitized name",
			final, v.Store, v.Reason, v.NormalizedExt)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func sortedUnique(xs []string) bool {
	for i := 1; i < len(xs); i++ {
		if xs[i-1] >= xs[i] {
			return false
		}
	}
	return true
}
