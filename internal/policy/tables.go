package policy

import "slices"

// The default allow and deny tables.
//
// These are the shipped policy: the business core of real correspondence,
// generous on purpose. Gmail and Google Chat already refuse ~50 executable
// extensions at the transport layer — including inside zip/tgz and including
// password-protected archives — so narrowing past the common business formats
// costs real archive fidelity for near-zero added safety on two of the three
// sources. FastMail is the one channel where this table is genuinely
// load-bearing.
//
// Each entry maps a normalized extension (lowercase, no leading dot) to the
// sniffed content types permitted for it. The tolerated non-mismatches
// documented on Policy.Decide sit on top of these sets; they are code, not
// data, and are versioned into the digest by tolerationRulesVersion.
//
// The strings on the right are exactly what github.com/gabriel-vasile/mimetype
// v1.4.15 reports. Where mimetype has no type of its own for a format, the
// type of the format it is structurally identical to is listed instead, with a
// comment: .dotx/.xltx/.potx are OOXML zips holding word//xl//ppt/, so they
// sniff as plain docx/xlsx/pptx.

// tolerationRulesVersion is bumped whenever the tolerated-non-mismatch rules
// in Policy.permits change. It is hashed into PolicyDigest so that a code
// change to the tolerations invalidates stored skip decisions exactly the way
// a config change does.
const tolerationRulesVersion = 1

// cryptoBlobMaxBytes bounds the application/octet-stream toleration for
// S/MIME and PGP parts. Those are a few kilobytes in practice; the bound stops
// the toleration from becoming a general "any bytes under .p7s" hole.
const cryptoBlobMaxBytes = 1 << 20 // 1 MiB

const (
	octetStream = "application/octet-stream"
	oleStorage  = "application/x-ole-storage"
)

// defaultAllow is the built-in extension -> permitted sniffed types table.
// zip, svg and the macro-enabled Office extensions are NOT here: they are
// added by New only when their flag is on.
var defaultAllow = map[string][]string{
	// ---- Documents -------------------------------------------------------
	// The single most important business attachment. Stored verbatim and
	// never defanged: rewriting PDF bytes to strip /JavaScript would break
	// signatures and violate archive fidelity, and Preview and Quick Look
	// have no PDF JavaScript engine.
	"pdf": {"application/pdf"},

	// Modern Office. REQUIRES the whole-buffer sniff limit (see Init): with
	// mimetype's 4096-byte default these sniff as application/zip.
	"docx": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	"dotx": {
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document", // mimetype has no dotx type
		"application/vnd.openxmlformats-officedocument.wordprocessingml.template",
	},
	"xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	"xltx": {
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", // mimetype has no xltx type
		"application/vnd.openxmlformats-officedocument.spreadsheetml.template",
	},
	"pptx": {"application/vnd.openxmlformats-officedocument.presentationml.presentation"},
	"potx": {
		"application/vnd.openxmlformats-officedocument.presentationml.presentation", // mimetype has no potx type
		"application/vnd.openxmlformats-officedocument.presentationml.template",
	},

	// Legacy Office. All four OLE formats are genuinely indistinguishable —
	// mimetype often stops at the application/x-ole-storage parent and
	// returns an EMPTY extension — so the extension is the sole
	// discriminator, and oleFamilyExts tolerates the parent type.
	"doc": {oleStorage, "application/msword"},
	"xls": {oleStorage, "application/vnd.ms-excel"},
	"ppt": {oleStorage, "application/vnd.ms-powerpoint"},

	// OpenDocument. Detected from the stored "mimetype" entry at offset 30,
	// so detection is stronger here than for OOXML.
	"odt": {"application/vnd.oasis.opendocument.text"},
	"ods": {"application/vnd.oasis.opendocument.spreadsheet"},
	"odp": {"application/vnd.oasis.opendocument.presentation"},
	"odg": {"application/vnd.oasis.opendocument.graphics"},

	"rtf":  {"text/rtf", "application/rtf"}, // legal and older correspondence; no macros
	"epub": {"application/epub+zip"},        // a zip variant with a fixed, non-executable structure

	// ---- Text ------------------------------------------------------------
	"txt":  {"text/plain"},
	"md":   {"text/plain"},
	"log":  {"text/plain"},
	"csv":  {"text/csv", "text/plain"},
	"tsv":  {"text/tab-separated-values", "text/plain"},
	"json": {"application/json", "application/x-ndjson", "text/plain"},
	"xml":  {"text/xml", "application/xml", "text/plain"},
	"yaml": {"text/plain"},
	"yml":  {"text/plain"},

	// ---- Images ----------------------------------------------------------
	"jpg":  {"image/jpeg"},
	"jpeg": {"image/jpeg"},
	"png":  {"image/png", "image/apng"},
	"gif":  {"image/gif"},
	"webp": {"image/webp"},
	// The default iPhone capture format. heic and heif cross-permit: the
	// brand in the ftyp box decides which of the four mimetype reports.
	"heic": {"image/heic", "image/heic-sequence", "image/heif", "image/heif-sequence"},
	"heif": {"image/heic", "image/heic-sequence", "image/heif", "image/heif-sequence"},
	"tif":  {"image/tiff"},
	"tiff": {"image/tiff"},
	"bmp":  {"image/bmp"},
	"avif": {"image/avif"},

	// ---- Calendar and contacts -------------------------------------------
	"ics": {"text/calendar", "text/plain"}, // invites are correspondence
	"vcf": {"text/vcard", "text/plain"},

	// ---- Audio -----------------------------------------------------------
	// Voice notes and recorded calls, mostly via Chat. Expect the size cap to
	// bind here. ISO-BMFF audio is reported by brand, so the mp4 family
	// cross-permits.
	"mp3":  {"audio/mpeg"},
	"m4a":  {"audio/x-m4a", "audio/mp4", "video/mp4"},
	"aac":  {"audio/aac", "audio/x-m4a", "audio/mp4"},
	"wav":  {"audio/wav"},
	"flac": {"audio/flac"},
	"ogg":  {"application/ogg", "audio/ogg", "video/ogg"},
	"opus": {"application/ogg", "audio/ogg"},

	// ---- Video -----------------------------------------------------------
	"mp4":  {"video/mp4", "audio/mp4"},
	"m4v":  {"video/x-m4v", "video/mp4"},
	"mov":  {"video/quicktime"},
	"webm": {"video/webm", "audio/webm"},

	// ---- Signatures and keys ---------------------------------------------
	// S/MIME and PGP parts ride along on a large fraction of business mail.
	// Tiny and inert; skipping them would spam every note with meaningless
	// skip entries. cryptoBlobExts additionally tolerates
	// application/octet-stream below cryptoBlobMaxBytes.
	"p7s": {"application/pkcs7-signature", "application/pkcs7-mime", "text/plain"},
	"p7m": {"application/pkcs7-mime", "application/pkcs7-signature", "text/plain"},
	"asc": {"text/plain"},
	"sig": {"text/plain"},
	"pgp": {"text/plain"},

	// ---- Nested mail -----------------------------------------------------
	// Forwarded mail IS the correspondence this archive exists to keep.
	// The note must say that save does NOT recurse: the nested message's own
	// attachments are neither extracted nor policy-checked, they ride along
	// inside the file.
	"eml": {"message/rfc822", "text/plain"},
	"msg": {oleStorage, "application/vnd.ms-outlook"},
}

// macroOfficeAllow is folded into the allowlist only when
// allow_macro_office = true. mimetype v1.4.15 reports every one of these as
// the plain non-macro type, so the extension is all there is to key on.
var macroOfficeAllow = map[string][]string{
	"docm": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	"dotm": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	"xlsm": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	"xltm": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	"pptm": {"application/vnd.openxmlformats-officedocument.presentationml.presentation"},
	"potm": {"application/vnd.openxmlformats-officedocument.presentationml.presentation"},
}

// macroOfficeExts is the extension-keyed macro-Office deny set. Content
// sniffing provides ZERO protection here.
var macroOfficeExts = map[string]bool{
	"docm": true, "dotm": true,
	"xlsm": true, "xltm": true,
	"pptm": true, "potm": true,
}

// ooxmlTypes are the sniffed types whose bytes are an OOXML zip, and thus the
// only ones the vbaProject central-directory check is run against.
var ooxmlTypes = map[string]bool{
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
}

// textFamilyExts are the extensions for which mimetype's heuristic text
// subtypes must not be treated as a mismatch.
var textFamilyExts = map[string]bool{
	"txt": true, "md": true, "log": true,
	"csv": true, "tsv": true, "json": true, "xml": true,
	"yaml": true, "yml": true, "ics": true, "vcf": true,
	"rtf": true, "asc": true, "sig": true, "pgp": true,
}

// textFamilyTypes are the inert text-DATA types tolerated under any
// textFamilyExts extension. text/html, image/svg+xml and the script text
// types (text/javascript, text/x-python, text/x-php, …) are deliberately
// absent: those are markup and programs, not data, and none of their
// extensions is on the allowlist either.
var textFamilyTypes = map[string]bool{
	"text/plain":                    true,
	"text/csv":                      true,
	"text/tab-separated-values":     true,
	"application/json":              true,
	"application/x-ndjson":          true,
	"text/xml":                      true,
	"application/xml":               true,
	"text/rtf":                      true,
	"application/rtf":               true,
	"text/calendar":                 true,
	"text/vcard":                    true,
	"application/x-subrip":          true,
	"application/vnd.apple.mpegurl": true,
}

// oleFamilyExts tolerate the application/x-ole-storage parent type.
var oleFamilyExts = map[string]bool{"doc": true, "xls": true, "ppt": true, "msg": true}

// cryptoBlobExts tolerate application/octet-stream below cryptoBlobMaxBytes.
var cryptoBlobExts = map[string]bool{"p7s": true, "p7m": true, "asc": true, "sig": true, "pgp": true}

// hardDenyMacOS are the macOS-executable extensions. A 0600 file cannot be
// execve'd, but the extension is the invitation — .command and .terminal are
// one consent click deep, and the interpreter-runnable text formats are worse
// because they look inert.
var hardDenyMacOS = map[string]bool{
	"app": true, "dmg": true, "pkg": true, "mpkg": true,
	"command": true, "tool": true, "terminal": true, "workflow": true,
	"scpt": true, "scptd": true, "applescript": true,
	"jar": true, "iso": true,
	"webloc": true, "inetloc": true, "fileloc": true,
	"sh": true, "bash": true, "zsh": true,
	"py": true, "pl": true, "rb": true,
	"macho": true, "dylib": true, "so": true,
}

// hardDenyWindows are the Windows payload extensions. Inert on this Mac, but
// harmful if forwarded — and Gmail and Chat already refuse nearly this exact
// list at the transport layer, so denying costs no real fidelity.
var hardDenyWindows = map[string]bool{
	"exe": true, "dll": true, "com": true, "bat": true, "cmd": true,
	"scr": true, "pif": true, "vbs": true, "vbe": true, "js": true,
	"jse": true, "wsf": true, "wsh": true, "hta": true,
	"msi": true, "msix": true, "msp": true, "mst": true,
	"lnk": true, "cpl": true, "chm": true, "reg": true, "ps1": true,
	"xll": true, "apk": true, "cab": true,
	"ade": true, "adp": true, "mde": true, "sct": true, "shb": true,
	"ins": true, "isp": true, "nsh": true, "sys": true, "vxd": true,
	"lib": true, "jnlp": true, "vhd": true, "img": true,
}

// HardDenied reports whether ext can never be stored, whatever the
// configuration, and why. allow_extensions naming one of these is a
// configuration ERROR rather than a silent no-op, so the operator is told
// instead of quietly not getting what they asked for.
//
// Macro-enabled Office, SVG and zip are NOT hard-denied: each has its own
// documented flag (allow_macro_office, allow_svg, allow_containers).
func HardDenied(ext string) (why string, hard bool) {
	switch {
	case hardDenyMacOS[ext]:
		return "." + ext + " is a macOS-executable type (the built-in hard-deny list)", true
	case hardDenyWindows[ext]:
		return "." + ext + " is a Windows payload type (the built-in hard-deny list)", true
	}
	return "", false
}

// FlagGated reports whether ext is governed by a dedicated [attachments] flag
// rather than by the allow/deny lists, and names that flag. Listing one of
// these in allow_extensions is a configuration error: the flag is the only
// place the decision belongs, so the effective allowlist — and the policy
// digest computed from it — stays truthful.
func FlagGated(ext string) (flag string, gated bool) {
	switch {
	case macroOfficeExts[ext]:
		return "allow_macro_office", true
	case ext == "svg":
		return "allow_svg", true
	case ext == "zip":
		return "allow_containers", true
	}
	return "", false
}

// HardDeniedExtensions returns every hard-denied extension, sorted. Used by
// `save doctor` and by the config error message.
func HardDeniedExtensions() []string {
	out := make([]string, 0, len(hardDenyMacOS)+len(hardDenyWindows))
	for ext := range hardDenyMacOS {
		out = append(out, ext)
	}
	for ext := range hardDenyWindows {
		out = append(out, ext)
	}
	slices.Sort(out)
	return out
}
