package triage

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Rule is one entry of triage.toml. Every field is a list of globs; a rule
// matches a note when EVERY field the rule sets has at least one glob that
// matches at least one of the note's values for it. A rule that sets no
// field at all is an error — it would match everything.
type Rule struct {
	Name         string   `toml:"name"`
	From         []string `toml:"from"`          // bare address or "Name <addr>"
	To           []string `toml:"to"`            // any To or Cc recipient
	Subject      []string `toml:"subject"`       // whole subject
	ListID       []string `toml:"list_id"`       // the captured List-Id header
	Labels       []string `toml:"labels"`        // any label / mailbox name
	AccountLabel []string `toml:"account_label"` // the archiving account's label

	from, to, subject, listID, labels, accountLabel []Glob
}

// rulesFile is the on-disk shape of triage.toml.
type rulesFile struct {
	Keep  []Rule `toml:"keep"`
	Noise []Rule `toml:"noise"`
}

// Rules is a compiled triage.toml: keep rules, then noise rules, each in
// file order. Keep rules are consulted first, so a note both lists claim is
// kept — the operator's "keep" always wins over their "noise".
type Rules struct {
	Keep  []Rule
	Noise []Rule
	Path  string // where they were read from; "" for ParseRules
}

// LoadRules reads and compiles triage.toml. A missing file is not an error:
// it yields an empty rule set, since the protect and header layers still
// apply and an operator who never wrote rules should still get those.
func LoadRules(path string) (*Rules, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Rules{Path: path}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("triage rules: read %s: %w", path, err)
	}
	r, err := ParseRules(data)
	if err != nil {
		return nil, fmt.Errorf("triage rules: %s: %w", path, err)
	}
	r.Path = path
	return r, nil
}

// ParseRules compiles rules from TOML. Unknown keys are an error so a typo
// cannot silently disable a rule.
func ParseRules(data []byte) (*Rules, error) {
	var f rulesFile
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown key(s): %s (rule fields are name, from, to, subject, list_id, labels, account_label)", strings.Join(keys, ", "))
	}
	r := &Rules{Keep: f.Keep, Noise: f.Noise}
	var errs []error
	seen := map[string]string{}
	for _, list := range []struct {
		kind  string
		rules []Rule
	}{{"keep", r.Keep}, {"noise", r.Noise}} {
		for i := range list.rules {
			rule := &list.rules[i]
			ref := fmt.Sprintf("[[%s]] #%d", list.kind, i+1)
			if rule.Name != "" {
				ref += " " + strconvQuote(rule.Name)
			}
			if err := rule.compile(); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", ref, err))
				continue
			}
			key := list.kind + ":" + rule.Name
			if prev, dup := seen[key]; dup {
				errs = append(errs, fmt.Errorf("%s: name %q is already used by %s — names must be unique within keep and within noise, they are what the ledger records", ref, rule.Name, prev))
			}
			seen[key] = ref
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return r, nil
}

func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }

// compile validates one rule and compiles its globs.
func (r *Rule) compile() error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("name is required (it is recorded with every decision the rule makes)")
	}
	if strings.ContainsAny(r.Name, " \t\n:") {
		return fmt.Errorf("name %q may not contain whitespace or ':'", r.Name)
	}
	var errs []error
	fields := 0
	compileList := func(field string, patterns []string) []Glob {
		if len(patterns) == 0 {
			return nil
		}
		fields++
		out := make([]Glob, 0, len(patterns))
		for _, p := range patterns {
			g, ok := CompileGlob(p)
			if !ok {
				errs = append(errs, fmt.Errorf("%s: an empty pattern matches nothing useful — remove it or write \"*\"", field))
				continue
			}
			out = append(out, g)
		}
		return out
	}
	r.from = compileList("from", r.From)
	r.to = compileList("to", r.To)
	r.subject = compileList("subject", r.Subject)
	r.listID = compileList("list_id", r.ListID)
	r.labels = compileList("labels", r.Labels)
	r.accountLabel = compileList("account_label", r.AccountLabel)
	if fields == 0 {
		errs = append(errs, errors.New("the rule sets no field, so it would match every note — set at least one of from, to, subject, list_id, labels, account_label"))
	}
	return errors.Join(errs...)
}

// match reports whether the rule matches the note, with a one-line account
// of what matched for the trace.
func (r *Rule) match(n *Note) (string, bool) {
	var parts []string
	check := func(field string, globs []Glob, values []string) bool {
		if len(globs) == 0 {
			return true
		}
		g, v, ok := matchAny(globs, values)
		if !ok {
			return false
		}
		parts = append(parts, fmt.Sprintf("%s %q matched %q", field, g, v))
		return true
	}
	if !check("from", r.from, addressForms(n.From)) ||
		!check("to", r.to, addressForms(append(slices.Clone(n.To), n.Cc...))) ||
		!check("subject", r.subject, []string{n.Subject}) ||
		!check("list_id", r.listID, []string{n.Header("list-id")}) ||
		!check("labels", r.labels, n.Labels) ||
		!check("account_label", r.accountLabel, []string{n.AccountLabel}) {
		return "", false
	}
	return strings.Join(parts, ", "), true
}

// canonical renders the rule for the digest: fields in a fixed order, the
// globs within a field sorted (any-of semantics make their order
// meaningless), the rule's own position preserved by the caller.
func (r *Rule) canonical() string {
	var b strings.Builder
	b.WriteString(r.Name)
	for _, f := range []struct {
		name     string
		patterns []string
	}{
		{"from", r.From}, {"to", r.To}, {"subject", r.Subject},
		{"list_id", r.ListID}, {"labels", r.Labels}, {"account_label", r.AccountLabel},
	} {
		if len(f.patterns) == 0 {
			continue
		}
		ps := slices.Clone(f.patterns)
		slices.Sort(ps)
		fmt.Fprintf(&b, " %s=[", f.name)
		for i, p := range ps {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(strconvQuote(p))
		}
		b.WriteByte(']')
	}
	return b.String()
}

// WriteDefaultRules writes DefaultRulesTOML to path with 0600 permissions,
// refusing to overwrite an existing file: the rules are the operator's once
// written, however they started.
func WriteDefaultRules(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("triage rules file %s already exists — not overwriting", path)
		}
		return fmt.Errorf("write triage rules: %w", err)
	}
	if _, err := f.WriteString(DefaultRulesTOML); err != nil {
		f.Close()
		return fmt.Errorf("write triage rules: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write triage rules: %w", err)
	}
	return nil
}

// DefaultRulesTOML is the triage.toml `save init` writes: a starting point
// that files the mail nobody reads and keeps the mail that costs money or
// access to lose. Every rule can be edited or deleted; the protect layer
// (see the [triage] block in config.toml) still stands above all of them.
const DefaultRulesTOML = `# save — noise triage rules.
#
# ` + "`save triage`" + ` reads every archived email note and decides signal or noise.
# Noise is MOVED (never deleted) into spam_root at the same YYYY/MM/DD path;
# ` + "`save untriage <path>`" + ` moves it back. Decisions run in layers and stop
# at the first decisive one:
#
#   0. protect   never noise: a note with a stored PDF/Office attachment, mail
#                you sent yourself, and the protect_* globs in config.toml
#   1. headers   bulk-mail markers captured at archive time (Precedence: bulk,
#                Auto-Submitted, Gmail's Promotions/Social categories)
#   2. rules     THIS FILE — every [[keep]] rule first, then every [[noise]]
#                rule, each in the order written here
#   3. llm       optional, off by default; see [triage.llm] in config.toml
#
# A rule matches when EVERY field it sets has a pattern matching one of the
# note's values. Patterns are case-insensitive globs over the whole value:
# "*" matches anything, "?" one character; a pattern without a wildcard is an
# exact match. Fields: from, to (also matches Cc), subject, list_id (the
# List-Id header), labels (Gmail labels / mailbox names), account_label.
#
# Run ` + "`save triage --dry-run`" + ` after editing: it prints every move the rules
# would make, file by file, and moves nothing. ` + "`save triage --explain <note>`" + `
# shows which layer decided one note and why.

# ---- keep: signal, however noisy it looks ---------------------------------

[[keep]]
name    = "billing"
subject = ["*invoice*", "*factuur*", "*receipt*", "*payment*", "*billing*", "*statement*", "*order confirmation*"]

[[keep]]
name    = "quota-and-limits"
subject = ["*quota*", "*usage limit*", "*rate limit*", "*budget alert*", "*approaching*limit*"]

[[keep]]
name    = "security"
subject = ["*security alert*", "*new sign-in*", "*sign-in attempt*", "*password*", "*verification code*", "*2fa*", "*two-factor*", "*suspicious*"]

# ---- noise: filed under spam_root -----------------------------------------

[[noise]]
name = "github-notifications"
from = ["notifications@github.com", "noreply@github.com"]

[[noise]]
name = "cloud-monitoring"
from = ["*@alerts.cloud.google.com", "*@monitoring.googleapis.com", "no-reply@sns.amazonaws.com", "*@cloudwatch.amazonaws.com", "alerts@*", "alertmanager@*", "*@pagerduty.com", "*@opsgenie.net"]

[[noise]]
name = "no-reply-senders"
from = ["noreply@*", "no-reply@*", "no_reply@*", "donotreply@*", "do-not-reply@*", "do_not_reply@*"]

[[noise]]
name    = "newsletters-and-digests"
subject = ["*newsletter*", "*digest*", "*weekly update*", "*weekly roundup*", "*what's new*", "*product update*"]

[[noise]]
name   = "gmail-promotions-and-social"
labels = ["CATEGORY_PROMOTIONS", "CATEGORY_SOCIAL"]
`
