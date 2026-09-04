package triage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"comms/internal/state"
)

const noteHead = `---
source: gmail:work
type: email
render_version: 2
account: you@example.com
account_label: work
message_id: <m1@example.com>
thread_id: t1
date: "2026-08-07T14:32:05+02:00"
date_utc: "2026-08-07T12:32:05Z"
`

// note builds a v2 note from a frontmatter tail (from:, subject:, ... lines)
// and a body.
func note(t *testing.T, tail, body string) *Note {
	t.Helper()
	n, err := ParseNote([]byte(noteHead + tail + "---\n" + body))
	if err != nil {
		t.Fatalf("ParseNote: %v", err)
	}
	return n
}

func TestParseNote(t *testing.T) {
	n := note(t, `from:
    - GitHub <notifications@github.com>
to:
    - you@example.com
cc: []
subject: '[org/repo] Bump deps (#12)'
labels:
    - INBOX
headers:
    list-id: org/repo <repo.org.github.com>
    precedence: list
attachments:
    - STEM.d/report.PDF
skipped_attachments:
    - name: budget.docm
      size: 1
      reason: macro_office
      disposition: discarded
      recoverable: true
`, "Body here.\n\n---\n\nnot a fence\n")
	if n.Source != "gmail:work" || n.AccountLabel != "work" || n.Subject != "[org/repo] Bump deps (#12)" {
		t.Errorf("identity fields: %+v", n)
	}
	if n.RenderVersion != 2 || !n.HasHeaders() {
		t.Errorf("render version = %d", n.RenderVersion)
	}
	if n.Header("List-ID") != "org/repo <repo.org.github.com>" || n.Header("precedence") != "list" {
		t.Errorf("headers: %v", n.Headers)
	}
	if len(n.Attachments) != 1 || attachmentExt(n.Attachments[0]) != "pdf" {
		t.Errorf("attachments: %v", n.Attachments)
	}
	if len(n.Skipped) != 1 || n.Skipped[0].Name != "budget.docm" {
		t.Errorf("skipped: %+v", n.Skipped)
	}
	if n.Body != "Body here.\n\n---\n\nnot a fence\n" {
		t.Errorf("body = %q", n.Body)
	}
	if address(n.From[0]) != "notifications@github.com" || address("Plain@Example.COM") != "plain@example.com" {
		t.Error("address extraction")
	}

	// A v1 note (no render_version key) reads as version 1 with no headers.
	v1 := strings.Replace(noteHead, "render_version: 2\n", "", 1)
	n1, err := ParseNote([]byte(v1 + "from: []\nsubject: x\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n1.RenderVersion != 1 || n1.HasHeaders() {
		t.Errorf("v1 note: version %d, HasHeaders %v", n1.RenderVersion, n1.HasHeaders())
	}

	for name, in := range map[string]string{
		"no frontmatter": "just text\n",
		"unclosed":       "---\nsource: x\n",
		"chat day file":  "---\ntype: chat-day\n---\n",
		"bad yaml":       "---\nsubject: [\n---\n",
	} {
		if _, err := ParseNote([]byte(in)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := ParseNote([]byte("---\ntype: chat-day\n---\n")); !errors.Is(err, ErrNotEmailNote) {
		t.Errorf("chat day file: err = %v, want ErrNotEmailNote", err)
	}
	// A frontmatter-only note with no trailing newline after the fence.
	if _, err := ParseNote([]byte("---\ntype: email\n---")); err != nil {
		t.Errorf("fence at EOF: %v", err)
	}
}

func TestGlob(t *testing.T) {
	for _, tc := range []struct {
		pattern, value string
		want           bool
	}{
		{"*invoice*", "Your INVOICE for August", true},
		{"invoice", "invoices", false},
		{"invoice", "Invoice", true},
		{"noreply@*", "noreply@example.com", true},
		{"noreply@*", "x-noreply@example.com", false},
		{"*@alerts.cloud.google.com", "monitoring@alerts.cloud.google.com", true},
		{"a?c", "abc", true},
		{"a?c", "abbc", false},
		{"[org/repo]*", "[org/repo] Bump", true}, // brackets are literal, not a class
		{"a.b", "axb", false},                    // dots are literal
		{"*", "", true},
		{"*ünïcode*", "Some ÜNÏCODE here", true},
	} {
		g, ok := CompileGlob(tc.pattern)
		if !ok {
			t.Fatalf("CompileGlob(%q) failed", tc.pattern)
		}
		if got := g.Match(tc.value); got != tc.want {
			t.Errorf("%q.Match(%q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
	if _, ok := CompileGlob(""); ok {
		t.Error("an empty pattern compiled")
	}
}

func TestParseRulesValidates(t *testing.T) {
	good, err := ParseRules([]byte(`
[[keep]]
name = "billing"
subject = ["*invoice*"]

[[noise]]
name = "gh"
from = ["notifications@github.com"]
list_id = ["*.github.com*"]
`))
	if err != nil || len(good.Keep) != 1 || len(good.Noise) != 1 {
		t.Fatalf("ParseRules(good) = %+v, %v", good, err)
	}
	for name, tc := range map[string]struct{ in, want string }{
		"unknown key":     {"[[noise]]\nname = \"x\"\nfrm = [\"a\"]\n", "unknown key"},
		"no name":         {"[[noise]]\nfrom = [\"a\"]\n", "name is required"},
		"name with colon": {"[[noise]]\nname = \"a:b\"\nfrom = [\"a\"]\n", "':'"},
		"no fields":       {"[[keep]]\nname = \"x\"\n", "sets no field"},
		"empty pattern":   {"[[noise]]\nname = \"x\"\nfrom = [\"\"]\n", "empty pattern"},
		"duplicate name":  {"[[noise]]\nname = \"x\"\nfrom = [\"a\"]\n[[noise]]\nname = \"x\"\nfrom = [\"b\"]\n", "already used"},
		"bad toml":        {"[[noise]\n", "parse"},
	} {
		_, err := ParseRules([]byte(tc.in))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want mention of %q", name, err, tc.want)
		}
	}
	// The same name in keep and in noise is allowed: they are different ids.
	if _, err := ParseRules([]byte("[[keep]]\nname = \"x\"\nfrom = [\"a\"]\n[[noise]]\nname = \"x\"\nfrom = [\"b\"]\n")); err != nil {
		t.Errorf("keep and noise sharing a name: %v", err)
	}
	// The shipped defaults must parse.
	if _, err := ParseRules([]byte(DefaultRulesTOML)); err != nil {
		t.Errorf("DefaultRulesTOML does not parse: %v", err)
	}
	// A missing file is an empty rule set, not an error.
	r, err := LoadRules(t.TempDir() + "/nope.toml")
	if err != nil || len(r.Keep)+len(r.Noise) != 0 {
		t.Errorf("LoadRules(missing) = %+v, %v", r, err)
	}
}

// stubJudge is a model layer with a scripted answer.
type stubJudge struct {
	noise  bool
	reason string
	err    error
	asked  int
}

func (s *stubJudge) Rule() string { return "stub@v1" }
func (s *stubJudge) Judge(context.Context, *Note) (bool, string, error) {
	s.asked++
	return s.noise, s.reason, s.err
}

func classifier(t *testing.T, rulesTOML string, judge Judge) *Classifier {
	t.Helper()
	rules, err := ParseRules([]byte(rulesTOML))
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{
		Rules:            rules,
		OwnAddresses:     []string{"You <you@example.com>"},
		ProtectFrom:      []string{"boss@example.com"},
		ProtectSubject:   []string{"*invoice*", "*security alert*"},
		HeaderHeuristics: true,
		Judge:            judge,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const testRules = `
[[keep]]
name = "team-list"
list_id = ["*team.example.com*"]

[[keep]]
name = "billing"
subject = ["*receipt*"]

[[noise]]
name = "github"
from = ["notifications@github.com"]

[[noise]]
name = "promos"
labels = ["CATEGORY_PROMOTIONS"]

[[noise]]
name = "personal-newsletters"
subject = ["*newsletter*"]
account_label = ["personal"]
`

// TestClassifyLayers walks every layer: what decides, in which order, and
// what each records. The reasons are asserted loosely (a substring) and the
// rule ids exactly, since the ids are what the ledger and --only key on.
func TestClassifyLayers(t *testing.T) {
	c := classifier(t, testRules, nil)
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		tail    string
		verdict state.TriageVerdict
		layer   string
		rule    string
		reason  string
	}{
		{"stored pdf protects even github", `from:
    - GitHub <notifications@github.com>
subject: bump
attachments:
    - x.d/Report.pdf
`, state.TriageSignal, state.TriageLayerProtect, "attachment", "stored .pdf attachment"},
		{"skipped docx does not protect", `from:
    - GitHub <notifications@github.com>
subject: bump
skipped_attachments:
    - name: budget.docx
`, state.TriageNoise, state.TriageLayerRules, "noise:github", "noise rule github"},
		{"an image does not protect", `from:
    - GitHub <notifications@github.com>
subject: bump
attachments:
    - x.d/logo.png
`, state.TriageNoise, state.TriageLayerRules, "noise:github", "github"},
		{"own outgoing mail", `from:
    - You <YOU@example.com>
subject: newsletter draft
headers:
    precedence: bulk
`, state.TriageSignal, state.TriageLayerProtect, "own-address", "own address"},
		{"protect_from glob", `from:
    - The Boss <boss@example.com>
subject: fyi
headers:
    precedence: bulk
`, state.TriageSignal, state.TriageLayerProtect, "from:boss@example.com", "protect_from"},
		{"protect_subject beats a bulk header", `from:
    - billing@vendor.example
subject: Your Invoice #42
headers:
    precedence: bulk
`, state.TriageSignal, state.TriageLayerProtect, "subject:*invoice*", "protect_subject"},
		{"precedence bulk", `from:
    - news@vendor.example
subject: hello
headers:
    precedence: Bulk
`, state.TriageNoise, state.TriageLayerHeaders, "precedence:bulk", "Precedence: bulk"},
		{"auto-submitted", `from:
    - cron@host.example
subject: job done
headers:
    auto-submitted: auto-generated
`, state.TriageNoise, state.TriageLayerHeaders, "auto-submitted", "Auto-Submitted"},
		{"auto-submitted no is not automation", `from:
    - person@host.example
subject: hi
headers:
    auto-submitted: "no"
`, state.TriageUndecided, state.TriageLayerNone, "", "no layer decided"},
		{"gmail promotions category", `from:
    - shop@vendor.example
subject: sale
labels:
    - CATEGORY_PROMOTIONS
`, state.TriageNoise, state.TriageLayerHeaders, "category:promotions", "Promotions"},
		{"list headers alone are not decisive", `from:
    - dev@list.example
subject: patch review
headers:
    list-id: <dev.list.example>
    list-unsubscribe: <mailto:x>
`, state.TriageUndecided, state.TriageLayerNone, "", "no layer decided"},
		{"keep rule on list_id wins over noise", `from:
    - notifications@github.com
subject: something
headers:
    list-id: Team <team.example.com>
`, state.TriageSignal, state.TriageLayerRules, "keep:team-list", "keep rule team-list"},
		{"keep rule by subject", `from:
    - notifications@github.com
subject: Your receipt
`, state.TriageSignal, state.TriageLayerRules, "keep:billing", "keep rule billing"},
		{"noise rule by from, display-name form", `from:
    - GitHub <notifications@github.com>
subject: '[repo] PR'
`, state.TriageNoise, state.TriageLayerRules, "noise:github", "noise rule github"},
		{"noise rule needs every field", `from:
    - news@vendor.example
subject: Monthly Newsletter
`, state.TriageUndecided, state.TriageLayerNone, "", "no layer decided"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := c.Classify(ctx, note(t, tc.tail, ""))
			if d.Verdict != tc.verdict || d.Layer != tc.layer || d.Rule != tc.rule {
				t.Fatalf("decision = %s/%s/%s, want %s/%s/%s\n%s", d.Verdict, d.Layer, d.Rule, tc.verdict, tc.layer, tc.rule, strings.Join(d.Trace, "\n"))
			}
			if !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("reason %q does not mention %q", d.Reason, tc.reason)
			}
			if d.Transient {
				t.Error("a rules decision is never transient")
			}
			if len(d.Trace) == 0 {
				t.Error("no trace")
			}
		})
	}
	// The account_label field: the same newsletter IS noise on the personal
	// account.
	tail := "from:\n    - news@vendor.example\nsubject: Monthly Newsletter\n"
	n := note(t, tail, "")
	n.AccountLabel = "personal"
	if d := c.Classify(ctx, n); d.Rule != "noise:personal-newsletters" {
		t.Errorf("account_label rule: %s/%s", d.Layer, d.Rule)
	}
	// RuleID is what the messages row records.
	if got := (Decision{Layer: state.TriageLayerRules, Rule: "noise:github"}).RuleID(); got != "rules:noise:github" {
		t.Errorf("RuleID = %q", got)
	}
	if got := (Decision{Layer: state.TriageLayerNone}).RuleID(); got != "" {
		t.Errorf("undecided RuleID = %q", got)
	}
}

// TestClassifyV1NoteSkipsHeaderLayer: a note written before headers were
// captured cannot be judged on them — the layer says so and falls through.
func TestClassifyV1NoteSkipsHeaderLayer(t *testing.T) {
	c := classifier(t, testRules, nil)
	v1 := strings.Replace(noteHead, "render_version: 2\n", "", 1)
	n, err := ParseNote([]byte(v1 + "from:\n    - news@vendor.example\nsubject: hello\nheaders:\n    precedence: bulk\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := c.Classify(context.Background(), n)
	if d.Verdict != state.TriageUndecided {
		t.Errorf("v1 note decided by headers it should not trust: %+v", d)
	}
	if !strings.Contains(strings.Join(d.Trace, "\n"), "before headers were captured") {
		t.Errorf("trace does not explain the version gap:\n%s", strings.Join(d.Trace, "\n"))
	}
	// ...but the Gmail category, always in the frontmatter, still counts.
	n.Labels = []string{"CATEGORY_SOCIAL"}
	if d := c.Classify(context.Background(), n); d.Rule != "category:social" {
		t.Errorf("category on a v1 note: %+v", d)
	}
}

// TestClassifyHeuristicsOff: the switch disables layer 1 entirely.
func TestClassifyHeuristicsOff(t *testing.T) {
	rules, _ := ParseRules([]byte(testRules))
	c, err := New(Options{Rules: rules, HeaderHeuristics: false})
	if err != nil {
		t.Fatal(err)
	}
	n := note(t, "from:\n    - news@vendor.example\nsubject: hello\nheaders:\n    precedence: bulk\nlabels:\n    - CATEGORY_SOCIAL\n", "")
	if d := c.Classify(context.Background(), n); d.Verdict != state.TriageUndecided {
		t.Errorf("headers decided with heuristics off: %+v", d)
	}
}

// TestClassifyModelLayer: the judge runs only when nothing else decided,
// its verdicts carry its rule id, and every failure is undecided AND
// transient — never noise.
func TestClassifyModelLayer(t *testing.T) {
	ctx := context.Background()
	undecided := "from:\n    - person@host.example\nsubject: hi\n"

	j := &stubJudge{noise: true, reason: "reads like a marketing blast"}
	c := classifier(t, testRules, j)
	d := c.Classify(ctx, note(t, undecided, "Buy now!"))
	if d.Verdict != state.TriageNoise || d.Layer != state.TriageLayerLLM || d.Rule != "stub@v1" || !strings.Contains(d.Reason, "marketing") {
		t.Errorf("model noise: %+v", d)
	}
	if d.RuleID() != "llm:stub@v1" {
		t.Errorf("RuleID = %q", d.RuleID())
	}

	// A rules decision never consults the model.
	j.asked = 0
	c.Classify(ctx, note(t, "from:\n    - notifications@github.com\nsubject: x\n", ""))
	if j.asked != 0 {
		t.Error("the model was asked about a note the rules decided")
	}

	j2 := &stubJudge{noise: false, reason: "a person wrote this"}
	if d := classifier(t, testRules, j2).Classify(ctx, note(t, undecided, "")); d.Verdict != state.TriageSignal || d.Layer != state.TriageLayerLLM {
		t.Errorf("model signal: %+v", d)
	}

	broken := &stubJudge{noise: true, err: errors.New("connection refused")}
	d = classifier(t, testRules, broken).Classify(ctx, note(t, undecided, ""))
	if d.Verdict != state.TriageUndecided || d.Layer != state.TriageLayerNone || !d.Transient {
		t.Errorf("model failure must be undecided and transient, got %+v", d)
	}
	if !strings.Contains(d.Reason, "connection refused") {
		t.Errorf("failure reason = %q", d.Reason)
	}
}

// TestDigest: the digest moves with everything that can change a verdict
// and with nothing else — glob order within a field is not a change; a
// rule's position, the protect globs, the switch and the model are.
func TestDigest(t *testing.T) {
	base := func() Options {
		rules, _ := ParseRules([]byte(testRules))
		return Options{Rules: rules, OwnAddresses: []string{"you@example.com"},
			ProtectSubject: []string{"*invoice*", "*receipt*"}, HeaderHeuristics: true}
	}
	digest := func(o Options) string {
		c, err := New(o)
		if err != nil {
			t.Fatal(err)
		}
		return c.Digest()
	}
	d0 := digest(base())
	if len(d0) != 16 {
		t.Fatalf("digest %q is not 16 hex chars", d0)
	}
	same := base()
	same.ProtectSubject = []string{"*receipt*", "*invoice*"} // reordered
	same.OwnAddresses = []string{"YOU@EXAMPLE.COM"}          // case
	if d := digest(same); d != d0 {
		t.Error("a reorder or case change moved the digest")
	}
	for name, mut := range map[string]func(*Options){
		"protect glob added": func(o *Options) { o.ProtectSubject = append(o.ProtectSubject, "*x*") },
		"own address added":  func(o *Options) { o.OwnAddresses = append(o.OwnAddresses, "me@example.net") },
		"heuristics off":     func(o *Options) { o.HeaderHeuristics = false },
		"model on":           func(o *Options) { o.Judge = &stubJudge{} },
		"rule glob changed":  func(o *Options) { o.Rules.Noise[0].From = []string{"other@example.com"} },
		"rule order swapped": func(o *Options) { o.Rules.Noise[0], o.Rules.Noise[1] = o.Rules.Noise[1], o.Rules.Noise[0] },
		"rule renamed":       func(o *Options) { o.Rules.Keep[0].Name = "renamed" },
		"rule moved to noise": func(o *Options) {
			o.Rules.Noise = append(o.Rules.Noise, o.Rules.Keep[0])
			o.Rules.Keep = o.Rules.Keep[1:]
		},
	} {
		o := base()
		mut(&o)
		if d := digest(o); d == d0 {
			t.Errorf("%s did not move the digest", name)
		}
	}
}
