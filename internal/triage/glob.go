package triage

import (
	"regexp"
	"strings"
)

// Glob is a compiled wildcard pattern: '*' matches any run of characters,
// '?' matches one, everything else matches itself, case-insensitively and
// across the WHOLE value (a pattern with no wildcard is an exact match, so
// "invoice" does not match "invoices" — write "*invoice*" for that). The
// grammar is deliberately smaller than filepath.Match: no character classes
// and no escapes, so a pattern reads the way the person who typed it meant
// it.
type Glob struct {
	src string
	re  *regexp.Regexp
}

// CompileGlob compiles one pattern. It cannot fail — every string is a
// valid pattern — but an empty one is rejected, since it would match only
// the empty value and is always a mistake in a rule.
func CompileGlob(pattern string) (Glob, bool) {
	if pattern == "" {
		return Glob{}, false
	}
	var b strings.Builder
	b.WriteString("(?is)^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return Glob{src: pattern, re: regexp.MustCompile(b.String())}, true
}

// String returns the pattern as written.
func (g Glob) String() string { return g.src }

// Match reports whether the whole of s matches.
func (g Glob) Match(s string) bool {
	return g.re != nil && g.re.MatchString(s)
}

// matchAny reports whether any glob matches any value; it returns the glob
// and value that matched, for the trace.
func matchAny(globs []Glob, values []string) (Glob, string, bool) {
	for _, g := range globs {
		for _, v := range values {
			if g.Match(v) {
				return g, v, true
			}
		}
	}
	return Glob{}, "", false
}

// addressForms returns the strings an address glob is matched against: the
// bare address and the full "Name <addr>" form, so "notifications@github.com"
// and "GitHub <*>" both work.
func addressForms(list []string) []string {
	out := make([]string, 0, 2*len(list))
	for _, s := range list {
		out = append(out, address(s))
		if full := strings.TrimSpace(s); full != "" && strings.ToLower(full) != address(s) {
			out = append(out, full)
		}
	}
	return out
}
