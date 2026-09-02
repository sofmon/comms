package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"save/internal/paths"
)

// Triage is the [triage] block: how `save triage` decides what is noise.
// The decision engine lives in internal/triage; this block configures the
// protect layer (things that can never be noise), the header layer's
// switch, where the rules file is, and the optional model layer.
type Triage struct {
	// AfterSync runs the rules-only layers (0–2) after every successful sync
	// pass of a mail account, in `save sync` and in the daemon. Off by
	// default: triage is a separate, explicit pass until the operator has
	// seen a dry run they like.
	AfterSync bool `toml:"after_sync"`

	// HeaderHeuristics enables layer 1: Precedence: bulk/junk, Auto-Submitted,
	// X-Auto-Response-Suppress and Gmail's Promotions/Social categories are
	// decisive noise. Default true.
	HeaderHeuristics bool `toml:"header_heuristics"`

	// RulesFile is the triage.toml with the [[keep]] and [[noise]] rules;
	// empty means <configdir>/triage.toml.
	RulesFile string `toml:"rules_file"`

	// ProtectFrom and ProtectSubject are layer-0 globs: a note whose sender
	// or subject matches one is signal before any other layer runs — above
	// the header heuristics, which the [[keep]] rules in triage.toml cannot
	// override. Case-insensitive, whole-value, "*" and "?".
	ProtectFrom    []string `toml:"protect_from"`
	ProtectSubject []string `toml:"protect_subject"`

	LLM TriageLLM `toml:"llm"`

	// RulesFilePath is RulesFile resolved (never read from TOML).
	RulesFilePath string `toml:"-"`
}

// TriageLLM is [triage.llm]: the optional model layer, consulted only for
// notes layers 0–2 left undecided, over an OpenAI-compatible chat endpoint.
type TriageLLM struct {
	// Enabled turns the layer on for `save triage`. Default false.
	Enabled bool `toml:"enabled"`

	// InDaemon also lets the after_sync pass consult the model. Default
	// false, so a stopped LM Studio can never stall the archiver; the
	// after_sync pass is otherwise rules-only.
	InDaemon bool `toml:"in_daemon"`

	// BaseURL is the endpoint's base, e.g. "http://127.0.0.1:1234/v1"; the
	// layer POSTs to <base_url>/chat/completions.
	BaseURL string `toml:"base_url"`

	// Model is the model name sent in every request. Required when enabled.
	Model string `toml:"model"`

	// Timeout bounds one request; a timeout is undecided, never noise.
	Timeout Duration `toml:"timeout"`

	// MaxBodyChars caps the body excerpt sent with the frontmatter fields.
	MaxBodyChars int `toml:"max_body_chars"`
}

// DefaultTriage returns the documented [triage] defaults.
func DefaultTriage() Triage {
	return Triage{
		AfterSync:        false,
		HeaderHeuristics: true,
		ProtectSubject:   []string{"*invoice*", "*factuur*", "*receipt*", "*security alert*", "*new sign-in*"},
		LLM: TriageLLM{
			Enabled:      false,
			InDaemon:     false,
			BaseURL:      "http://127.0.0.1:1234/v1",
			Timeout:      Duration(20 * time.Second),
			MaxBodyChars: 4000,
		},
	}
}

// resolve fills in RulesFilePath from RulesFile (tilde-expanded) or the
// default location beside config.toml.
func (t *Triage) resolve() error {
	if t.RulesFile == "" {
		t.RulesFilePath = filepath.Join(paths.ConfigDir(), "triage.toml")
		return nil
	}
	p, err := expandTilde(t.RulesFile)
	if err != nil {
		return fmt.Errorf("triage.rules_file: %w", err)
	}
	t.RulesFilePath = p
	return nil
}

// validate reports every problem in the block at once. Glob syntax cannot
// fail; only an empty pattern is a mistake worth refusing here — the rules
// file itself is validated by internal/triage when it is read.
func (t Triage) validate() error {
	var errs []error
	for _, f := range []struct {
		key      string
		patterns []string
	}{{"protect_from", t.ProtectFrom}, {"protect_subject", t.ProtectSubject}} {
		for i, p := range f.patterns {
			if strings.TrimSpace(p) == "" {
				errs = append(errs, fmt.Errorf("triage.%s: pattern #%d is empty — remove it (an empty glob matches nothing useful)", f.key, i+1))
			}
		}
	}
	l := t.LLM
	if l.InDaemon && !l.Enabled {
		errs = append(errs, errors.New("triage.llm.in_daemon = true has no effect while triage.llm.enabled = false — enable the layer, or drop in_daemon"))
	}
	if l.Enabled && strings.TrimSpace(l.Model) == "" {
		errs = append(errs, errors.New("triage.llm.model is required when triage.llm.enabled = true (the name your endpoint serves the model under)"))
	}
	if u, err := url.Parse(l.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("triage.llm.base_url %q must be an http(s) URL such as \"http://127.0.0.1:1234/v1\"", l.BaseURL))
	}
	if l.Timeout.Duration() <= 0 {
		errs = append(errs, fmt.Errorf("triage.llm.timeout must be a positive duration, got %q", l.Timeout.Duration()))
	}
	if l.MaxBodyChars <= 0 {
		errs = append(errs, fmt.Errorf("triage.llm.max_body_chars must be positive, got %d", l.MaxBodyChars))
	}
	return errors.Join(errs...)
}

// OwnAddresses returns every configured account's address: mail sent FROM
// one of them is the operator's own and is never noise.
func (c *Config) OwnAddresses() []string {
	out := make([]string, 0, len(c.Google)+len(c.FastMail))
	for _, g := range c.Google {
		out = append(out, g.Account)
	}
	for _, f := range c.FastMail {
		out = append(out, f.Account)
	}
	return out
}
