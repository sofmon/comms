package triage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"save/internal/state"
)

// Judge is the optional model layer. Judge returns the model's verdict on a
// note; err covers everything that is not a well-formed verdict — a
// timeout, an unreachable endpoint, a reply that is not the JSON asked for.
// The classifier treats every error as undecided and never as noise.
type Judge interface {
	// Rule is the rule id recorded with the decision, e.g. "qwen2.5@v1":
	// model plus prompt version, so `save triage --reclassify --only llm`
	// can find exactly the decisions this judge made.
	Rule() string
	Judge(ctx context.Context, n *Note) (noise bool, reason string, err error)
}

// Classifier holds everything a pass decides with. Build one with New and
// keep it for the whole pass: Digest is computed once.
type Classifier struct {
	rules        *Rules
	ownAddresses []string // lowercase
	protectFrom  []Glob
	protectSubj  []Glob
	heuristics   bool
	judge        Judge
	digest       string
}

// Options configures a Classifier.
type Options struct {
	// Rules is the compiled triage.toml; nil means no operator rules.
	Rules *Rules

	// OwnAddresses are the configured accounts' addresses: mail FROM any
	// of them is the operator's own outgoing mail and is never noise.
	OwnAddresses []string

	// ProtectFrom and ProtectSubject are the [triage] protect globs from
	// config.toml, the layer-0 patterns that can never be noise.
	ProtectFrom, ProtectSubject []string

	// HeaderHeuristics enables layer 1.
	HeaderHeuristics bool

	// Judge is the optional layer 3; nil disables it.
	Judge Judge
}

// New builds a Classifier, compiling the protect globs.
func New(o Options) (*Classifier, error) {
	c := &Classifier{
		rules:      o.Rules,
		heuristics: o.HeaderHeuristics,
		judge:      o.Judge,
	}
	if c.rules == nil {
		c.rules = &Rules{}
	}
	for _, a := range o.OwnAddresses {
		if a = address(a); a != "" {
			c.ownAddresses = append(c.ownAddresses, a)
		}
	}
	for _, p := range o.ProtectFrom {
		g, ok := CompileGlob(p)
		if !ok {
			return nil, fmt.Errorf("triage: protect_from: empty pattern")
		}
		c.protectFrom = append(c.protectFrom, g)
	}
	for _, p := range o.ProtectSubject {
		g, ok := CompileGlob(p)
		if !ok {
			return nil, fmt.Errorf("triage: protect_subject: empty pattern")
		}
		c.protectSubj = append(c.protectSubj, g)
	}
	c.digest = digestOf(c.canonical())
	return c, nil
}

// Rules returns the compiled rule set the classifier runs.
func (c *Classifier) Rules() *Rules { return c.rules }

// HasJudge reports whether the model layer is configured.
func (c *Classifier) HasJudge() bool { return c.judge != nil }

// Digest identifies everything that can change a verdict: the protect
// configuration, the header-heuristics switch, every rule, and the model
// layer's identity. A message decided under the current digest is settled;
// a different digest re-opens it. Sixteen hex characters, like the
// attachment policy digest.
func (c *Classifier) Digest() string { return c.digest }

// digestVersion prefixes the canonical encoding; bump it when the ENCODING
// changes shape or when a built-in list (protectedExts, the header rules)
// changes meaning, since those are not otherwise visible in the bytes.
const digestVersion = 1

func (c *Classifier) canonical() string {
	var b strings.Builder
	fmt.Fprintf(&b, "triage/v%d\n", digestVersion)
	fmt.Fprintf(&b, "heuristics=%s\n", strconv.FormatBool(c.heuristics))
	own := slices.Clone(c.ownAddresses)
	slices.Sort(own)
	fmt.Fprintf(&b, "own=%s\n", strings.Join(own, ","))
	globs := func(gs []Glob) string {
		ss := make([]string, len(gs))
		for i, g := range gs {
			ss[i] = strconv.Quote(g.String())
		}
		slices.Sort(ss)
		return strings.Join(ss, " ")
	}
	fmt.Fprintf(&b, "protect_from=[%s]\n", globs(c.protectFrom))
	fmt.Fprintf(&b, "protect_subject=[%s]\n", globs(c.protectSubj))
	for i := range c.rules.Keep {
		fmt.Fprintf(&b, "keep:%s\n", c.rules.Keep[i].canonical())
	}
	for i := range c.rules.Noise {
		fmt.Fprintf(&b, "noise:%s\n", c.rules.Noise[i].canonical())
	}
	if c.judge != nil {
		fmt.Fprintf(&b, "llm=%s\n", c.judge.Rule())
	} else {
		b.WriteString("llm=off\n")
	}
	return b.String()
}

func digestOf(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:16]
}

// Decision is the outcome of classifying one note.
type Decision struct {
	Verdict state.TriageVerdict
	Layer   string // one of the state.TriageLayer constants
	Rule    string // rule id within the layer; "" when undecided
	Reason  string // for people

	// Transient is set when the deciding layer FAILED rather than declined
	// (the model timed out, say): the verdict is undecided and the note must
	// not be settled under the digest, so the next pass tries again.
	Transient bool

	// Trace is one line per layer consulted, for `save triage --explain`.
	Trace []string
}

// RuleID is the machine-readable "<layer>:<rule>" recorded on the messages
// row as disposition_rule, or "" when nothing decided.
func (d Decision) RuleID() string {
	if d.Layer == state.TriageLayerNone {
		return ""
	}
	return d.Layer + ":" + d.Rule
}

// protectedExts are the stored-attachment extensions that make a note
// signal regardless of anything else: documents someone bothered to attach.
// Images, calendar invites and archives are deliberately absent — a
// newsletter is full of images and a bulk invite is still bulk.
var protectedExts = []string{
	"pdf",
	"doc", "docx", "dot", "dotx", "rtf",
	"xls", "xlsx", "xlt", "xltx", "csv",
	"ppt", "pptx", "pot", "potx",
	"odt", "ods", "odp",
}

// Classify runs the layers over one note and stops at the first decisive
// one. ctx only matters to the model layer.
func (c *Classifier) Classify(ctx context.Context, n *Note) Decision {
	d := Decision{Verdict: state.TriageUndecided, Layer: state.TriageLayerNone}
	if c.protect(n, &d) || c.headers(n, &d) || c.userRules(n, &d) {
		return d
	}
	if c.judge == nil {
		d.Trace = append(d.Trace, "layer 3 llm: off")
		d.Reason = "no layer decided"
		return d
	}
	return c.model(ctx, n, d)
}

// protect is layer 0.
func (c *Classifier) protect(n *Note, d *Decision) bool {
	decide := func(rule, reason string) bool {
		d.Verdict, d.Layer, d.Rule, d.Reason = state.TriageSignal, state.TriageLayerProtect, rule, reason
		d.Trace = append(d.Trace, "layer 0 protect: "+reason+" → SIGNAL")
		return true
	}
	for _, rel := range n.Attachments {
		if ext := attachmentExt(rel); slices.Contains(protectedExts, ext) {
			return decide("attachment", fmt.Sprintf("stored .%s attachment %q", ext, rel[strings.LastIndex(rel, "/")+1:]))
		}
	}
	for _, f := range n.From {
		if a := address(f); a != "" && slices.Contains(c.ownAddresses, a) {
			return decide("own-address", "sent from the account's own address "+a)
		}
	}
	if g, v, ok := matchAny(c.protectFrom, addressForms(n.From)); ok {
		return decide("from:"+g.String(), fmt.Sprintf("protect_from %q matched %q", g, v))
	}
	if g, v, ok := matchAny(c.protectSubj, []string{n.Subject}); ok {
		return decide("subject:"+g.String(), fmt.Sprintf("protect_subject %q matched %q", g, v))
	}
	d.Trace = append(d.Trace, "layer 0 protect: no stored document attachment, not from an own address, no protect glob matched")
	return false
}

// headers is layer 1: the strongest bulk markers only. List-Id and
// List-Unsubscribe alone are NOT decisive — a work mailing list carries
// both — and are left to the rules (list_id) and the model. Gmail's
// Updates and Forums categories are not decisive either: Updates is where
// receipts and confirmations land.
func (c *Classifier) headers(n *Note, d *Decision) bool {
	if !c.heuristics {
		d.Trace = append(d.Trace, "layer 1 headers: off (triage.header_heuristics = false)")
		return false
	}
	decide := func(rule, reason string) bool {
		d.Verdict, d.Layer, d.Rule, d.Reason = state.TriageNoise, state.TriageLayerHeaders, rule, reason
		d.Trace = append(d.Trace, "layer 1 headers: "+reason+" → NOISE")
		return true
	}
	// Categories were always in the frontmatter, so they apply to every
	// note; the captured headers only to notes rendered since the capture.
	for _, l := range n.Labels {
		switch strings.ToUpper(l) {
		case "CATEGORY_PROMOTIONS":
			return decide("category:promotions", "Gmail filed it under Promotions")
		case "CATEGORY_SOCIAL":
			return decide("category:social", "Gmail filed it under Social")
		}
	}
	if !n.HasHeaders() {
		d.Trace = append(d.Trace, fmt.Sprintf("layer 1 headers: note has render_version %d, written before headers were captured — nothing to go on", n.RenderVersion))
		return false
	}
	if p := strings.ToLower(n.Header("precedence")); p == "bulk" || p == "junk" {
		return decide("precedence:"+p, "Precedence: "+p)
	}
	if a := strings.ToLower(n.Header("auto-submitted")); a != "" && a != "no" {
		return decide("auto-submitted", "Auto-Submitted: "+a)
	}
	if n.Header("x-auto-response-suppress") != "" {
		return decide("x-auto-response-suppress", "X-Auto-Response-Suppress is set (automated sender)")
	}
	var seen []string
	for _, h := range []string{"list-id", "list-unsubscribe", "feedback-id", "x-github-reason", "x-mailer"} {
		if n.Header(h) != "" {
			seen = append(seen, h)
		}
	}
	if len(seen) > 0 {
		d.Trace = append(d.Trace, "layer 1 headers: "+strings.Join(seen, ", ")+" present but not decisive on their own")
	} else {
		d.Trace = append(d.Trace, "layer 1 headers: no bulk markers")
	}
	return false
}

// userRules is layer 2: every keep rule, then every noise rule.
func (c *Classifier) userRules(n *Note, d *Decision) bool {
	for i := range c.rules.Keep {
		r := &c.rules.Keep[i]
		if why, ok := r.match(n); ok {
			d.Verdict, d.Layer, d.Rule, d.Reason = state.TriageSignal, state.TriageLayerRules, "keep:"+r.Name, "keep rule "+r.Name+": "+why
			d.Trace = append(d.Trace, "layer 2 rules: keep "+r.Name+" ("+why+") → SIGNAL")
			return true
		}
	}
	for i := range c.rules.Noise {
		r := &c.rules.Noise[i]
		if why, ok := r.match(n); ok {
			d.Verdict, d.Layer, d.Rule, d.Reason = state.TriageNoise, state.TriageLayerRules, "noise:"+r.Name, "noise rule "+r.Name+": "+why
			d.Trace = append(d.Trace, "layer 2 rules: noise "+r.Name+" ("+why+") → NOISE")
			return true
		}
	}
	d.Trace = append(d.Trace, fmt.Sprintf("layer 2 rules: none of %d keep and %d noise rules matched", len(c.rules.Keep), len(c.rules.Noise)))
	return false
}

// model is layer 3. Any failure is undecided AND transient: the note stays
// where it is and is not settled, so a later pass asks again.
func (c *Classifier) model(ctx context.Context, n *Note, d Decision) Decision {
	noise, reason, err := c.judge.Judge(ctx, n)
	rule := c.judge.Rule()
	if err != nil {
		d.Transient = true
		d.Reason = "model layer failed: " + err.Error()
		d.Trace = append(d.Trace, "layer 3 llm ("+rule+"): "+err.Error()+" → undecided, will retry")
		return d
	}
	d.Layer, d.Rule = state.TriageLayerLLM, rule
	if noise {
		d.Verdict = state.TriageNoise
		d.Reason = "model: " + reason
		d.Trace = append(d.Trace, "layer 3 llm ("+rule+"): "+reason+" → NOISE")
	} else {
		d.Verdict = state.TriageSignal
		d.Reason = "model: " + reason
		d.Trace = append(d.Trace, "layer 3 llm ("+rule+"): "+reason+" → SIGNAL")
	}
	return d
}
