package archive

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// quarantineRE is the shape verified against real on-disk values on macOS 26:
// flags;lowercase-hex-epoch;agent;uppercase-UUID.
var quarantineRE = regexp.MustCompile(`^0081;[0-9a-f]+;comms;[0-9A-F]{8}-[0-9A-F]{4}-4[0-9A-F]{3}-[89AB][0-9A-F]{3}-[0-9A-F]{12}$`)

func TestQuarantineValue(t *testing.T) {
	got := quarantineValue(time.Unix(0x68b8a4c0, 0), "3F2504E0-4F89-11D3-9A0C-0305E82C3301")
	want := "0081;68b8a4c0;comms;3F2504E0-4F89-11D3-9A0C-0305E82C3301"
	if got != want {
		t.Errorf("quarantineValue = %q, want %q", got, want)
	}
	// The flags field is a literal copied from an observed-good value; its
	// bits are undocumented, so nothing may derive it.
	if !strings.HasPrefix(got, "0081;") {
		t.Error("flags field is not the literal 0081")
	}
	if v := quarantineValue(time.Now(), newQuarantineUUID()); !quarantineRE.MatchString(v) {
		t.Errorf("generated value %q does not match the verified format", v)
	}
}

func TestNewQuarantineUUIDIsUniqueAndVersion4(t *testing.T) {
	seen := make(map[string]bool, 64)
	for range 64 {
		id := newQuarantineUUID()
		if len(id) != 36 || strings.ToUpper(id) != id {
			t.Fatalf("uuid %q is not an uppercase 36-char UUID", id)
		}
		if id[14] != '4' {
			t.Fatalf("uuid %q is not version 4", id)
		}
		if seen[id] {
			t.Fatalf("uuid %q repeated", id)
		}
		seen[id] = true
	}
}

func TestQuarantineModeZeroValueTags(t *testing.T) {
	// The config default is attachments.quarantine = true, so a Writer built
	// without thinking about the field must tag rather than skip.
	var m QuarantineMode
	if !m.enabled() {
		t.Error("the zero QuarantineMode does not tag")
	}
	if !QuarantineFor(true).enabled() || QuarantineFor(false).enabled() {
		t.Error("QuarantineFor does not follow the config flag")
	}
}

func TestOriginWhereFroms(t *testing.T) {
	tests := []struct {
		in   Origin
		want []string
	}{
		{Origin{}, nil},
		{Origin{Source: "gmail:work"}, []string{"gmail:work"}},
		{Origin{Source: "gmail:work", Ref: "<m1@example.com>"}, []string{"gmail:work", "<m1@example.com>"}},
		// A hostile Ref may not smuggle line structure into the metadata.
		{Origin{Ref: "a\nb\r\nc"}, []string{"a b c"}},
		{Origin{Ref: "   "}, nil},
	}
	for _, tt := range tests {
		if got := tt.in.WhereFroms(); !slices.Equal(got, tt.want) {
			t.Errorf("Origin%+v.WhereFroms() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestTagOnlyAttachments: notes are comms's own text. Tagging them would claim
// they were downloaded, and would put Obsidian's own vault files behind a
// Gatekeeper prompt.
func TestTagOnlyAttachments(t *testing.T) {
	doc, meta, _ := fullEmailFixture(t)
	var tagged []string
	w := &Writer{
		Root: t.TempDir(), TZ: tzAms,
		TagOrigin: func(p string, o Origin) error {
			if o.Source != "gmail:work" || o.Ref != "<m1@example.com>" {
				t.Errorf("attachment tagged with the wrong origin: %+v", o)
			}
			tagged = append(tagged, p)
			return nil
		},
	}
	if _, _, err := w.WriteEmail(doc, meta); err != nil {
		t.Fatal(err)
	}
	if len(tagged) != len(doc.Files) {
		t.Fatalf("tagged %d files, want %d (the note must not be tagged)", len(tagged), len(doc.Files))
	}
	// Every tag lands on a staged temp file, never on a visible path: the
	// attribute must already be there when the rename makes the file
	// observable.
	for _, p := range tagged {
		if !strings.Contains(p, "/"+TempDirName+"/") {
			t.Errorf("tagged %q, which is not a staged temp file", p)
		}
	}
}

// TestQuarantineOffSkipsTagging: attachments.quarantine = false must reach
// the syscall, not merely the config struct.
func TestQuarantineOffSkipsTagging(t *testing.T) {
	doc, meta, _ := fullEmailFixture(t)
	calls := 0
	w := &Writer{
		Root: t.TempDir(), TZ: tzAms,
		Quarantine: QuarantineOff,
		TagOrigin:  func(string, Origin) error { calls++; return nil },
	}
	if _, _, err := w.WriteEmail(doc, meta); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("QuarantineOff still tagged %d files", calls)
	}
}

// TestTagFailureAbortsBeforeAnythingIsVisible: a tagging error is reported
// while the file is still a temp file, so an attachment never lands in the
// vault silently missing the attribute it was supposed to carry.
func TestTagFailureAbortsBeforeAnythingIsVisible(t *testing.T) {
	doc, meta, stem := fullEmailFixture(t)
	boom := errors.New("setxattr refused")
	w := &Writer{
		Root: t.TempDir(), TZ: tzAms,
		TagOrigin: func(string, Origin) error { return boom },
	}
	_, _, err := w.WriteEmail(doc, meta)
	if !errors.Is(err, boom) {
		t.Fatalf("WriteEmail error = %v, want it to wrap %v", err, boom)
	}
	// Nothing visible: not the attachment whose tag failed, and not the note.
	for _, rel := range []string{
		"2026/08/07/" + stem + ".md",
		"2026/08/07/" + stem + ".d/invoice.pdf",
	} {
		if _, err := os.Stat(filepath.Join(w.Root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s exists after a tagging failure (err=%v)", rel, err)
		}
	}
	if files := dotFiles(t, w.Root); len(files) != 0 {
		t.Errorf("failed write left temp litter: %v", files)
	}
}

// TestWriteChatAttachmentTags: chat blobs are downloaded bytes and get the
// same treatment as mail attachments; the day file, which comms renders
// itself, does not.
func TestWriteChatAttachmentTags(t *testing.T) {
	var tagged []Origin
	w := &Writer{
		Root: t.TempDir(), TZ: tzAms,
		TagOrigin: func(_ string, o Origin) error { tagged = append(tagged, o); return nil },
	}
	if _, err := w.WriteChatDay("2026/08/07/gchat_space_x_ab12cd34.md", []byte("day\n")); err != nil {
		t.Fatal(err)
	}
	if len(tagged) != 0 {
		t.Fatalf("the chat day file was tagged: %+v", tagged)
	}
	origin := Origin{Source: "gchat:work", Ref: "spaces/AAA/messages/BBB"}
	hash, err := w.WriteChatAttachment("2026/08/07/gchat_space_x_ab12cd34.d/120000_deadbeef_photo.png", []byte("png"), origin)
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		t.Error("no content hash returned")
	}
	if !slices.Equal(tagged, []Origin{origin}) {
		t.Errorf("chat attachment tagged with %+v", tagged)
	}
	for _, bad := range []string{"", "../x.png", "/abs.png"} {
		if _, err := w.WriteChatAttachment(bad, []byte("x"), origin); err == nil {
			t.Errorf("WriteChatAttachment(%q) succeeded, want error", bad)
		}
	}
}

// TestBplistStrings pins the encoding against the bytes plutil accepts and
// against a real Safari download's xattr: header, array marker, refs, string
// objects, one-byte offset table, 32-byte trailer.
func TestBplistStrings(t *testing.T) {
	got := bplistStrings([]string{"gmail:work", "<m1@example.com>"})
	want := concat(
		[]byte("bplist00"),
		[]byte{0xA2, 0x01, 0x02},           // array of 2, refs to objects 1 and 2
		[]byte{0x5A}, []byte("gmail:work"), // ASCII, short-form length 10
		[]byte{0x5F, 0x10, 0x10}, []byte("<m1@example.com>"), // ASCII, long-form length 16
		[]byte{0x08, 0x0B, 0x16}, // offset table
		[]byte{0, 0, 0, 0, 0, 0, 0x01, 0x01},
		be64(3), be64(0), be64(41),
	)
	if !slices.Equal(got, want) {
		t.Errorf("bplistStrings mismatch:\ngot  %x\nwant %x", got, want)
	}

	// Non-ASCII spills into a UTF-16BE object, whose length counts code
	// units rather than bytes.
	uni := bplistStrings([]string{"é"})
	wantUni := concat(
		[]byte("bplist00"),
		[]byte{0xA1, 0x01},
		[]byte{0x61, 0x00, 0xE9}, // UTF-16BE, 1 code unit
		[]byte{0x08, 0x0A},
		[]byte{0, 0, 0, 0, 0, 0, 0x01, 0x01},
		be64(2), be64(0), be64(13),
	)
	if !slices.Equal(uni, wantUni) {
		t.Errorf("utf-16 mismatch:\ngot  %x\nwant %x", uni, wantUni)
	}

	// Degenerate inputs produce nothing rather than a malformed plist that
	// Finder would choke on.
	if b := bplistStrings(nil); b != nil {
		t.Errorf("empty input produced %x", b)
	}
	if b := bplistStrings(make([]string, 20)); b != nil {
		t.Errorf("oversized input produced %x", b)
	}
	// A long string forces the long-form length and still lands a valid,
	// one-byte-wide offset table.
	long := bplistStrings([]string{strings.Repeat("u", 200)})
	if len(long) == 0 || long[10] != 0x5F || long[11] != 0x10 || long[12] != 200 {
		t.Errorf("long string not encoded with the 1-byte integer form: %x", long[:16])
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func be64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }
