package fastmail

import (
	"comms/internal/policy"
	"comms/internal/source"
)

// Option adjusts a connector at construction.
//
// The attachment policy and the free-space probe arrive this way rather than
// as positional parameters so that every existing call site keeps compiling
// and keeps getting the safe default (the built-in allowlist). A caller that
// has a config should always pass WithPolicy(cfg.Policy()) — otherwise the
// operator's [attachments] block is silently ignored.
//
// FastMail is the channel where the allowlist is most load-bearing: unlike
// Gmail and Google Chat, no upstream refusal of executable attachment types
// is confirmed for it, so nothing but this policy stands between the mailbox
// and the vault.
type Option func(*options)

type options struct {
	pol *policy.Policy

	// freeSpace reports the bytes free on the archive volume, or <= 0 for
	// "unknown" (which disables the free-space floor, per policy.Input).
	freeSpace func(root string) int64
}

func newOptions(opts []Option) options {
	o := options{pol: policy.Default(), freeSpace: defaultFreeSpace}
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	if o.pol == nil {
		o.pol = policy.Default()
	}
	if o.freeSpace == nil {
		o.freeSpace = defaultFreeSpace
	}
	return o
}

// WithPolicy sets the attachment storage policy applied to every attachment,
// inline part and embedded image of every message this connector archives.
// nil selects policy.Default().
func WithPolicy(p *policy.Policy) Option {
	return func(o *options) {
		if p != nil {
			o.pol = p
		}
	}
}

// WithFreeSpace replaces the free-space probe. It exists for tests, which
// need to drive the free-space floor without filling a real disk.
func WithFreeSpace(fn func(root string) int64) Option {
	return func(o *options) {
		if fn != nil {
			o.freeSpace = fn
		}
	}
}

// defaultFreeSpace probes the archive volume. A probe failure answers
// "unknown" (0), which disables the floor for that decision rather than
// refusing every attachment.
func defaultFreeSpace(root string) int64 {
	n, err := source.FreeSpace(root)
	if err != nil {
		return 0
	}
	return n
}
