package config

import (
	"errors"
	"fmt"
	"strings"

	"comms/internal/policy"
)

// Attachments is the [attachments] block: the storage policy for everything
// that arrives attached to a message.
//
// comms stores an attachment only when its final extension is allowlisted AND
// the content sniffed from its magic bytes is permitted for that extension.
// Anything else is REFUSED, never silently dropped: the refusal is written
// into the note (frontmatter `skipped_attachments:` and a visible entry under
// "## Attachments") and into the state DB with the identity needed to fetch it
// later, so widening this block and running `comms refetch` recovers it.
//
// The decision engine itself lives in internal/policy; this block is only its
// configuration. PolicySettings converts one into the other.
type Attachments struct {
	// MaxSize is the per-attachment cap. Gmail delivers at most ~50 MB per
	// message, so the default essentially never binds there; Google Chat
	// allows 200 MB per file, which is where it actually bites — and Chat is
	// also the only source where refusing saves the download.
	MaxSize Bytes `toml:"max_size"`

	// ChatMaxSize is the per-attachment cap for Google Chat uploads. Unset
	// (or 0) inherits MaxSize; raise it if large work media is worth keeping.
	ChatMaxSize Bytes `toml:"chat_max_size"`

	// MaxMessageBytes caps the raw RFC822 buffer a single message may occupy
	// before it is parsed at all. It is deliberately smaller than
	// MaxPerMessage: it bounds the untrusted bytes held in memory, not the
	// bytes written to disk.
	MaxMessageBytes Bytes `toml:"max_message_bytes"`

	// MaxPerMessage caps the total attachment bytes stored for one message,
	// and MaxPartsPerMessage the number of parts, so a single hostile message
	// cannot fill the volume or the note.
	MaxPerMessage      Bytes `toml:"max_per_message"`
	MaxPartsPerMessage int   `toml:"max_parts_per_message"`

	// RunBudget caps the attachment bytes one sync pass will store; the next
	// pass picks up where it left off. 0 means no budget.
	RunBudget Bytes `toml:"run_budget"`

	// FreeSpaceFloor is the free space the archive volume must retain: an
	// attachment that would eat into it is refused while the (tiny) note is
	// still written, so mail keeps archiving on a full disk. 0 disables the
	// check.
	FreeSpaceFloor Bytes `toml:"free_space_floor"`

	// AllowExtensions ADDS to the built-in allowlist; DenyExtensions
	// SUBTRACTS from it. Deny always wins. Neither replaces the built-in
	// list. An entry may be a bare extension ("7z"), which accepts any
	// content for it because comms has no content table for a format it does
	// not know, or an explicit mapping ("7z=application/x-7z-compressed"),
	// which keeps both halves of the rule enforceable.
	AllowExtensions []string `toml:"allow_extensions"`
	DenyExtensions  []string `toml:"deny_extensions"`

	// AllowContainers keeps .zip (default true). comms never decompresses it,
	// so it is inert bytes on disk — but it is also opaque, so the allowlist
	// says nothing about what is inside.
	AllowContainers bool `toml:"allow_containers"`

	// AllowSVG keeps .svg (default false). SVG is the one image format that
	// is a program, and the archive lives in an Obsidian vault — Obsidian is
	// Electron and renders SVG in-note, with no double-click involved.
	AllowSVG bool `toml:"allow_svg"`

	// AllowMacroOffice keeps docm/xlsm/pptm/dotm/xltm/potm (default false).
	// These must be denied by EXTENSION: mimetype reports them as plain
	// docx/xlsx/pptx, so content sniffing offers zero protection.
	AllowMacroOffice bool `toml:"allow_macro_office"`

	// OnMismatch decides what happens when the extension is allowlisted but
	// the content disagrees with it: "skip" (default) or "store-warn".
	OnMismatch string `toml:"on_mismatch"`

	// Quarantine tags every stored attachment com.apple.quarantine (default
	// true). See the skeleton comments for what that does and does not buy.
	Quarantine bool `toml:"quarantine"`

	// ScanCommand is an optional argv (never a shell line) run against the
	// temp file before it is renamed into place. Exit 0 clean, 1 infected,
	// anything else an error — an error is never treated as clean.
	ScanCommand []string `toml:"scan_command"`

	// ScanAction is what a flagged file gets: "record" (default; store it and
	// record the verdict) or "reject" (skip it).
	ScanAction string `toml:"scan_action"`
}

// DefaultAttachments returns the documented [attachments] defaults.
func DefaultAttachments() Attachments {
	return Attachments{
		MaxSize:            50 * MB,
		ChatMaxSize:        0, // inherit MaxSize
		MaxMessageBytes:    100 * MB,
		MaxPerMessage:      150 * MB,
		MaxPartsPerMessage: 500,
		RunBudget:          2 * GB,
		FreeSpaceFloor:     5 * GB,
		AllowContainers:    true,
		AllowSVG:           false,
		AllowMacroOffice:   false,
		OnMismatch:         policy.OnMismatchSkip,
		Quarantine:         true,
		ScanAction:         policy.ScanActionRecord,
	}
}

// applyDerived resolves the keys whose default is another key's value, so the
// effective policy — and therefore the policy digest — is explicit rather than
// computed differently by each caller. Load calls it before Validate; it is
// idempotent.
func (a *Attachments) applyDerived() {
	if a.ChatMaxSize <= 0 {
		a.ChatMaxSize = a.MaxSize
	}
	if a.OnMismatch == "" {
		a.OnMismatch = policy.OnMismatchSkip
	}
	if a.ScanAction == "" {
		a.ScanAction = policy.ScanActionRecord
	}
}

// EffectiveChatMaxSize is the per-attachment cap for Chat: chat_max_size when
// set, max_size otherwise. Load already folds this into ChatMaxSize; the
// method exists for a Config assembled in code rather than parsed.
func (a Attachments) EffectiveChatMaxSize() Bytes {
	if a.ChatMaxSize > 0 {
		return a.ChatMaxSize
	}
	return a.MaxSize
}

// PolicySettings converts the block into the decision engine's input. Use
// policy.New on the result — or Config.Policy, which does both.
func (a Attachments) PolicySettings() policy.Settings {
	return policy.Settings{
		MaxSize:            a.MaxSize.Bytes(),
		ChatMaxSize:        a.EffectiveChatMaxSize().Bytes(),
		MaxMessageBytes:    a.MaxMessageBytes.Bytes(),
		MaxPerMessage:      a.MaxPerMessage.Bytes(),
		MaxPartsPerMessage: a.MaxPartsPerMessage,
		RunBudget:          a.RunBudget.Bytes(),
		FreeSpaceFloor:     a.FreeSpaceFloor.Bytes(),
		AllowExtensions:    a.AllowExtensions,
		DenyExtensions:     a.DenyExtensions,
		AllowContainers:    a.AllowContainers,
		AllowSVG:           a.AllowSVG,
		AllowMacroOffice:   a.AllowMacroOffice,
		OnMismatch:         a.OnMismatch,
		Quarantine:         a.Quarantine,
		ScanCommand:        a.ScanCommand,
		ScanAction:         a.ScanAction,
	}
}

// Policy builds the attachment decision engine for this configuration. It is
// the one place every caller should get a *policy.Policy from, so they all
// share a single PolicyDigest.
func (c *Config) Policy() (*policy.Policy, error) {
	p, err := policy.New(c.Attachments.PolicySettings())
	if err != nil {
		return nil, fmt.Errorf("attachments: %w", err)
	}
	return p, nil
}

// validate reports every problem in the block at once. Byte caps are checked
// here, where the TOML key names are; the extension lists and the enums are
// checked by policy.New so there is exactly one implementation of each rule.
func (a Attachments) validate() error {
	var errs []error

	// Caps that must be positive: a zero here is a typo, not a policy.
	for _, c := range []struct {
		key string
		v   Bytes
		why string
	}{
		{"max_size", a.MaxSize, "the per-attachment cap"},
		{"max_message_bytes", a.MaxMessageBytes, "the raw message buffer cap"},
		{"max_per_message", a.MaxPerMessage, "the per-message attachment cap"},
	} {
		if c.v <= 0 {
			errs = append(errs, fmt.Errorf("attachments.%s must be a positive size (%s), got %q — write e.g. \"50MB\"", c.key, c.why, c.v))
		}
	}
	if a.ChatMaxSize < 0 {
		errs = append(errs, fmt.Errorf("attachments.chat_max_size must not be negative, got %q (omit it to inherit max_size)", a.ChatMaxSize))
	}
	if a.MaxPartsPerMessage <= 0 {
		errs = append(errs, fmt.Errorf("attachments.max_parts_per_message must be a positive count, got %d", a.MaxPartsPerMessage))
	}
	if a.RunBudget < 0 {
		errs = append(errs, fmt.Errorf("attachments.run_budget must not be negative, got %q (0 means no budget)", a.RunBudget))
	}
	if a.FreeSpaceFloor < 0 {
		errs = append(errs, fmt.Errorf("attachments.free_space_floor must not be negative, got %q (0 disables the check)", a.FreeSpaceFloor))
	}

	// A per-attachment cap above the per-message cap can never be reached.
	if a.MaxSize > 0 && a.MaxPerMessage > 0 && a.MaxSize > a.MaxPerMessage {
		errs = append(errs, fmt.Errorf("attachments.max_size (%s) is larger than attachments.max_per_message (%s), so no attachment that big could ever be stored — raise max_per_message or lower max_size",
			a.MaxSize, a.MaxPerMessage))
	}
	if a.EffectiveChatMaxSize() > 0 && a.MaxPerMessage > 0 && a.EffectiveChatMaxSize() > a.MaxPerMessage {
		errs = append(errs, fmt.Errorf("attachments.chat_max_size (%s) is larger than attachments.max_per_message (%s), so no Chat attachment that big could ever be stored",
			a.EffectiveChatMaxSize(), a.MaxPerMessage))
	}
	if a.RunBudget > 0 && a.MaxSize > 0 && a.RunBudget < a.MaxSize {
		errs = append(errs, fmt.Errorf("attachments.run_budget (%s) is smaller than attachments.max_size (%s), so a single large attachment would exhaust the pass — raise run_budget",
			a.RunBudget, a.MaxSize))
	}

	// The scan hook.
	for i, arg := range a.ScanCommand {
		if strings.TrimSpace(arg) == "" && i == 0 {
			errs = append(errs, errors.New(`attachments.scan_command: the first element must be the program to run, e.g. ["clamdscan", "--fdpass", "--no-summary"]`))
		}
	}
	if a.ScanAction == policy.ScanActionReject && len(a.ScanCommand) == 0 {
		errs = append(errs, fmt.Errorf("attachments.scan_action = %q needs attachments.scan_command to be set — there is no scanner to reject on", policy.ScanActionReject))
	}

	// The extension lists and the two enums, from the single implementation.
	if _, err := policy.New(a.PolicySettings()); err != nil {
		errs = append(errs, fmt.Errorf("attachments.%w", err))
	}
	return errors.Join(errs...)
}
