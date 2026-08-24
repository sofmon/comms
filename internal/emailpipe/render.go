package emailpipe

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"github.com/gabriel-vasile/mimetype"
	"github.com/jhillyerd/enmime/v2"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// maxHTMLBytes guards the html-to-markdown conversion: an HTML body larger
// than this falls back to the text part (or a truncated conversion).
const maxHTMLBytes = 2 << 20 // 2 MiB

// mdConverter is built once; the v2 converter is safe for concurrent use.
var mdConverter = converter.NewConverter(
	converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		table.NewTablePlugin(),
		strikethrough.NewStrikethroughPlugin(),
	),
)

// Render turns raw RFC 2822 bytes into an EmailDoc under the default
// attachment policy. See RenderWithOptions.
func Render(raw []byte, attachDirName string) (*EmailDoc, error) {
	return RenderWithOptions(raw, attachDirName, Options{})
}

// RenderWithOptions turns raw RFC 2822 bytes into an EmailDoc.
// attachDirName is the final path element of the sibling attachment
// directory; every File.Rel and every rewritten link starts with it.
// Per-part MIME problems become Warnings, never an error; an error is
// returned only when the input cannot be parsed as a message at all.
//
// Every attachment, inline part and extracted data: URI image is put to
// opts.Policy. Accepted parts land in doc.Files with their bytes; refused
// ones land in doc.Skipped with their identity, size, sniffed type and
// reason, and a line in doc.Warnings. Nothing is ever dropped in silence.
func RenderWithOptions(raw []byte, attachDirName string, opts Options) (*EmailDoc, error) {
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("emailpipe: parse message: %w", err)
	}

	doc := &EmailDoc{}
	for _, e := range env.Errors {
		doc.Warnings = append(doc.Warnings, e.Error())
	}

	doc.Subject = env.GetHeader("Subject")
	doc.MessageID = env.GetHeader("Message-Id")
	doc.From = addresses(env, "From", &doc.Warnings)
	doc.To = addresses(env, "To", &doc.Warnings)
	doc.Cc = addresses(env, "Cc", &doc.Warnings)
	if d, err := env.Date(); err == nil {
		doc.Date = d
	} else {
		doc.Warnings = append(doc.Warnings, fmt.Sprintf("missing or unparseable Date header: %v", err))
	}

	// Files first (never-drop rule): every attachment, inline, and other
	// part is named and put to the policy before body conversion is
	// attempted. A part the policy refuses is recorded, not dropped.
	parts := orderedFileParts(env)
	ad := newAdmitter(opts.Policy, attachDirName, doc, opts)
	partRel := make(map[*enmime.Part]string, len(parts))
	partSkip := make(map[*enmime.Part]File)
	for i, p := range parts {
		f, stored := ad.admit(candidate{
			origName: p.FileName,
			fallback: func() string { return partFallback(i, p) },
			declared: p.ContentType,
			partKey:  partKey(i, p),
			content:  p.Content,
		})
		if stored {
			partRel[p] = f.Rel
		} else {
			partSkip[p] = f
		}
	}

	// cid: targets live in Inlines and OtherParts (multipart/related parts
	// commonly carry no Content-Disposition at all). First part wins. A
	// referenced part the policy refused is tracked separately so the body
	// degrades to alt text with a reason rather than to a dead link.
	cids := cidIndex{rel: map[string]string{}, skipped: map[string]File{}}
	for _, s := range [][]*enmime.Part{env.Inlines, env.OtherParts} {
		for _, p := range s {
			if p.ContentID == "" {
				continue
			}
			if rel, saved := partRel[p]; saved {
				if _, dup := cids.rel[p.ContentID]; !dup {
					cids.rel[p.ContentID] = rel
				}
				continue
			}
			if f, refused := partSkip[p]; refused {
				if _, dup := cids.skipped[p.ContentID]; !dup {
					cids.skipped[p.ContentID] = f
				}
			}
		}
	}

	doc.BodyMD = renderBody(env, ad, cids, doc)
	return doc, nil
}

// cidIndex resolves a cid: reference to either the stored part's relative
// path or the record of the part the policy refused.
type cidIndex struct {
	rel     map[string]string
	skipped map[string]File
}

// partFallback is the name an unnamed part is stored under. It is keyed on
// the part's index among ALL file parts — stored and skipped alike — so a
// later policy widening cannot renumber the parts around a recovered one.
func partFallback(i int, p *enmime.Part) string {
	return fmt.Sprintf("attachment-%d%s", i+1, sniffExt(p.Content))
}

// partKey is the part's dotted MIME index within the message (enmime's
// PartID, e.g. "2.1.3") — half of the identity a retro-fetch needs. The
// positional fallback is defensive: enmime assigns a PartID to every part it
// parses.
func partKey(i int, p *enmime.Part) string {
	if p.PartID != "" {
		return p.PartID
	}
	return fmt.Sprintf("#%d", i+1)
}

// addresses returns the RFC 2047-decoded address list for key, formatted
// "Name <addr>". A present-but-unparseable header degrades to the raw
// decoded string plus a warning so no information is lost.
func addresses(env *enmime.Envelope, key string, warnings *[]string) []string {
	raw := strings.TrimSpace(env.GetHeader(key))
	if raw == "" {
		return nil
	}
	list, err := env.AddressList(key)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("unparseable %s header: %v", key, err))
		return []string{raw}
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		if a.Name != "" {
			out = append(out, a.Name+" <"+a.Address+">")
		} else {
			out = append(out, a.Address)
		}
	}
	return out
}

// orderedFileParts returns Attachments ∪ Inlines ∪ OtherParts in MIME part
// (document) order, excluding the text parts enmime already surfaced as the
// message body. Every unnamed inline text/plain part is body (enmime
// concatenates them all into Envelope.Text), but only ONE text/html part —
// the depth-first first — becomes Envelope.HTML; any later unnamed HTML
// part must still be saved as a file (never-drop rule). The walk also
// rescues leaf text/html parts enmime dropped from all three slices (a
// second undispositioned HTML sibling matches none of its part matchers).
func orderedFileParts(env *enmime.Envelope) []*enmime.Part {
	consumedHTML := consumedHTMLPart(env)
	want := make(map[*enmime.Part]bool)
	var union []*enmime.Part
	for _, s := range [][]*enmime.Part{env.Attachments, env.Inlines, env.OtherParts} {
		for _, p := range s {
			if p == nil || want[p] || isBodyPart(p, consumedHTML) {
				continue
			}
			want[p] = true
			union = append(union, p)
		}
	}
	if env.Root == nil {
		return union
	}
	// rescueHTML catches leaf text/html parts absent from every enmime
	// slice: without a disposition they are neither Inlines nor Attachments,
	// and the OtherParts matcher excludes text/* — silently lost otherwise.
	rescueHTML := func(p *enmime.Part) bool {
		return !want[p] && p != consumedHTML && p.FirstChild == nil &&
			p.Disposition != "attachment" &&
			strings.ToLower(p.ContentType) == "text/html"
	}
	ordered := make([]*enmime.Part, 0, len(union))
	seen := make(map[*enmime.Part]bool, len(union))
	// Iterative preorder walk: push NextSibling below FirstChild so children
	// are visited before later siblings (document order).
	stack := []*enmime.Part{env.Root}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if p == nil {
			continue
		}
		if !seen[p] && (want[p] || rescueHTML(p)) {
			seen[p] = true
			ordered = append(ordered, p)
		}
		stack = append(stack, p.NextSibling, p.FirstChild)
	}
	// Parts not reachable from Root (defensive): keep them, slice order.
	for _, p := range union {
		if !seen[p] {
			ordered = append(ordered, p)
		}
	}
	return ordered
}

// consumedHTMLPart returns the one text/html part enmime rendered into
// Envelope.HTML, mirroring its matchHTMLBodyPart rule: the depth-first
// first text/html part not disposed as an attachment (Part.ContentType is
// already lowercased by enmime).
func consumedHTMLPart(env *enmime.Envelope) *enmime.Part {
	if env.Root == nil {
		return nil
	}
	return env.Root.DepthMatchFirst(func(p *enmime.Part) bool {
		return p.ContentType == "text/html" && p.Disposition != "attachment"
	})
}

// isBodyPart reports whether p is an unnamed text part that enmime already
// rendered into Envelope.Text/HTML. All unnamed non-attachment text/plain
// parts qualify (enmime concatenates every one into Envelope.Text); among
// text/html parts only htmlBody — the single part enmime consumed for
// Envelope.HTML — does, so later HTML parts are saved as files.
func isBodyPart(p *enmime.Part, htmlBody *enmime.Part) bool {
	if p.FileName != "" || p.Disposition == "attachment" {
		return false
	}
	if strings.ToLower(p.ContentType) == "text/plain" {
		return true
	}
	return p == htmlBody
}

// sniffExt returns a file extension (with leading dot) detected from
// content, or "" when nothing recognizable is found.
//
// The detection covers the WHOLE buffer: save/internal/policy's package init
// calls mimetype.SetLimit(0). With the library's 4096-byte default an
// ordinary .docx whose word/ entry sits past 4 KB sniffs as application/zip,
// and the allowlist would refuse a real business document.
func sniffExt(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	return mimetype.Detect(content).Extension()
}

// renderBody produces BodyMD. It may append to doc.Files (extracted data:
// URI images) and doc.Warnings. Failure at any stage degrades to the text
// part, then to an empty body — never to an error.
func renderBody(env *enmime.Envelope, ad *admitter, cids cidIndex, doc *EmailDoc) string {
	htmlSrc := env.HTML
	if htmlSrc == "" {
		return env.Text
	}
	if len(htmlSrc) > maxHTMLBytes {
		if env.Text != "" {
			doc.Warnings = append(doc.Warnings,
				fmt.Sprintf("html body is %d bytes (limit %d); used the text part instead", len(htmlSrc), maxHTMLBytes))
			return env.Text
		}
		doc.Warnings = append(doc.Warnings,
			fmt.Sprintf("html body is %d bytes (limit %d) and no text part exists; converted truncated html", len(htmlSrc), maxHTMLBytes))
		htmlSrc = htmlSrc[:maxHTMLBytes]
	}

	root, err := html.Parse(strings.NewReader(htmlSrc))
	if err != nil {
		doc.Warnings = append(doc.Warnings, fmt.Sprintf("html body parse failed: %v; used the text part instead", err))
		return env.Text
	}
	stripUnwanted(root)
	htmlKey := ""
	if hp := consumedHTMLPart(env); hp != nil {
		htmlKey = hp.PartID
	}
	rewriteImages(root, ad, cids, htmlKey, doc)

	md, err := mdConverter.ConvertNode(root)
	if err != nil {
		doc.Warnings = append(doc.Warnings, fmt.Sprintf("html-to-markdown conversion failed: %v; used the text part instead", err))
		return env.Text
	}
	return strings.TrimSpace(string(md))
}

// stripUnwanted removes head, style, and script subtrees in place.
func stripUnwanted(root *html.Node) {
	var doomed []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				switch c.DataAtom {
				case atom.Head, atom.Style, atom.Script:
					doomed = append(doomed, c)
					continue
				}
			}
			walk(c)
		}
	}
	walk(root)
	for _, n := range doomed {
		n.Parent.RemoveChild(n)
	}
}

// rewriteImages rewrites <img> srcs in place: cid: references point at the
// saved inline part's relative path, and data: URI payloads are extracted
// into files so the Markdown never embeds megabytes of base64.
//
// Anything that does not end up on disk — an unresolvable cid:, a cid: whose
// part the policy refused, an undecodable data: URI, or a data: image the
// policy refused — degrades to the img's alt text plus a warning. The
// Markdown never keeps a link to a file that was not written.
func rewriteImages(root *html.Node, ad *admitter, cids cidIndex, htmlKey string, doc *EmailDoc) {
	var imgs []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.DataAtom == atom.Img {
				imgs = append(imgs, c)
			}
			walk(c)
		}
	}
	walk(root)

	inlineN := 0
	for _, n := range imgs {
		src := strings.TrimSpace(getAttr(n, "src"))
		lower := strings.ToLower(src)
		switch {
		case strings.HasPrefix(lower, "cid:"):
			id := src[len("cid:"):]
			if rel, ok := resolveCID(id, cids.rel); ok {
				setAttr(n, "src", rel)
				continue
			}
			if f, ok := resolveSkippedCID(id, cids.skipped); ok {
				// The part exists but was refused: say so next to the alt
				// text rather than leaving a link to a file nobody wrote.
				doc.Warnings = append(doc.Warnings,
					fmt.Sprintf("inline image cid:%s is not stored (%s); the body shows its alt text instead", id, f.SkipReason))
				replaceWithText(n, imgPlaceholder(n))
				continue
			}
			doc.Warnings = append(doc.Warnings, fmt.Sprintf("unresolved inline image cid:%s", id))
			replaceWithText(n, imgPlaceholder(n))
		case strings.HasPrefix(lower, "data:"):
			mediaType, content, ok := parseDataURI(src)
			if !ok {
				doc.Warnings = append(doc.Warnings, "undecodable data: URI image dropped")
				replaceWithText(n, imgPlaceholder(n))
				continue
			}
			// The counter advances for refused images too, so a policy
			// widening cannot renumber the ones already on disk.
			inlineN++
			idx := inlineN
			f, stored := ad.admit(candidate{
				fallback: func() string {
					return fmt.Sprintf("inline-%d%s", idx, extForMediaType(mediaType, content))
				},
				declared: mediaType,
				partKey:  fmt.Sprintf("%s#data-%d", htmlKey, idx),
				content:  content,
			})
			if !stored {
				replaceWithText(n, imgPlaceholder(n))
				continue
			}
			setAttr(n, "src", f.Rel)
		}
	}
}

// resolveCID matches a cid: reference against the stored parts' Content-IDs,
// which enmime already stripped of angle brackets and URL-unescaped.
func resolveCID(id string, cidRel map[string]string) (string, bool) {
	return lookupCID(id, cidRel)
}

// resolveSkippedCID is resolveCID over the parts the policy refused.
func resolveSkippedCID(id string, cidSkipped map[string]File) (File, bool) {
	return lookupCID(id, cidSkipped)
}

// lookupCID tries the reference verbatim, then path- and query-unescaped.
func lookupCID[T any](id string, m map[string]T) (T, bool) {
	if v, ok := m[id]; ok {
		return v, true
	}
	if u, err := url.PathUnescape(id); err == nil {
		if v, ok := m[u]; ok {
			return v, true
		}
	}
	if u, err := url.QueryUnescape(id); err == nil {
		if v, ok := m[u]; ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// imgPlaceholder is the text an unrenderable image degrades to.
func imgPlaceholder(n *html.Node) string {
	if alt := strings.TrimSpace(getAttr(n, "alt")); alt != "" {
		return alt
	}
	return "[image]"
}

// parseDataURI decodes a data: URI. Base64 payloads are decoded (tolerating
// embedded whitespace and missing padding); other payloads are
// percent-decoded.
func parseDataURI(src string) (mediaType string, data []byte, ok bool) {
	rest := src[len("data:"):]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", nil, false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	isBase64 := false
	if strings.HasSuffix(strings.ToLower(meta), ";base64") {
		isBase64 = true
		meta = meta[:len(meta)-len(";base64")]
	}
	mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(meta, ";", 2)[0]))
	if mediaType == "" {
		mediaType = "text/plain"
	}
	if !isBase64 {
		s, err := url.PathUnescape(payload)
		if err != nil {
			s = payload
		}
		return mediaType, []byte(s), true
	}
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, payload)
	b, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(cleaned)
		if err != nil {
			return mediaType, nil, false
		}
	}
	return mediaType, b, true
}

// mediaTypeExt maps common inline-image media types to extensions;
// extForMediaType falls back to content sniffing for anything else.
var mediaTypeExt = map[string]string{
	"image/png":                ".png",
	"image/jpeg":               ".jpg",
	"image/jpg":                ".jpg",
	"image/gif":                ".gif",
	"image/webp":               ".webp",
	"image/svg+xml":            ".svg",
	"image/bmp":                ".bmp",
	"image/tiff":               ".tiff",
	"image/avif":               ".avif",
	"image/heic":               ".heic",
	"image/heif":               ".heif",
	"image/x-icon":             ".ico",
	"image/vnd.microsoft.icon": ".ico",
}

func extForMediaType(mediaType string, content []byte) string {
	if ext, ok := mediaTypeExt[mediaType]; ok {
		return ext
	}
	return sniffExt(content)
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func setAttr(n *html.Node, key, val string) {
	for i := range n.Attr {
		if n.Attr[i].Key == key {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

// replaceWithText swaps n for a bare text node.
func replaceWithText(n *html.Node, text string) {
	if n.Parent == nil {
		return
	}
	t := &html.Node{Type: html.TextNode, Data: text}
	n.Parent.InsertBefore(t, n)
	n.Parent.RemoveChild(n)
}
