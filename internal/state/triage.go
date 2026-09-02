package state

import (
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// TriageVerdict is what a triage pass concluded about one email note.
type TriageVerdict string

const (
	// TriageNoise: the note belongs in the spam tree.
	TriageNoise TriageVerdict = "noise"

	// TriageSignal: the note belongs in the archive tree. A protected note, a
	// keep rule, an LLM "not noise", and `save untriage` all record this.
	TriageSignal TriageVerdict = "signal"

	// TriageUndecided: no layer reached a verdict. The note stays where it is
	// — undecided is never noise — and is recorded so status can count how
	// much of the archive the rules do not cover.
	TriageUndecided TriageVerdict = "undecided"
)

// TriageVerdicts returns every valid verdict, in a stable order. The schema's
// CHECK constraint is generated from this list.
func TriageVerdicts() []TriageVerdict {
	return []TriageVerdict{TriageNoise, TriageSignal, TriageUndecided}
}

// Triage layers, in the order a pass runs them. A decision records the layer
// that was decisive; an undecided one records TriageLayerNone.
const (
	TriageLayerProtect = "protect" // layer 0: things that can never be noise
	TriageLayerHeaders = "headers" // layer 1: MIME/header heuristics captured at ingest
	TriageLayerRules   = "rules"   // layer 2: the user's triage.toml
	TriageLayerLLM     = "llm"     // layer 3: the optional local model
	TriageLayerManual  = "manual"  // `save untriage`
	TriageLayerNone    = "none"    // no layer decided
)

// TriageLayers returns every valid layer, in pass order. The schema's CHECK
// constraint is generated from this list.
func TriageLayers() []string {
	return []string{TriageLayerProtect, TriageLayerHeaders, TriageLayerRules, TriageLayerLLM, TriageLayerManual, TriageLayerNone}
}

// ValidTriageLayer reports whether layer is one of the TriageLayer constants.
func ValidTriageLayer(layer string) bool { return slices.Contains(TriageLayers(), layer) }

// TriageDecision is the latest verdict recorded for one message: which layer
// decided, under which rule, why, and under which triage digest.
type TriageDecision struct {
	Source   string // instance id, e.g. "gmail:work"
	StableID string
	Verdict  TriageVerdict
	Layer    string // one of the TriageLayer constants
	Rule     string // rule id within the layer, e.g. "attachment", "noise:github"; "" for TriageLayerNone
	Reason   string // human-readable; never parsed
	Digest   string // the triage digest the decision was made under
	// DecidedAt is when the decision was recorded. Zero on input means "now".
	DecidedAt time.Time
}

// UpsertTriageDecision records the latest decision for a message, replacing
// any earlier one. It validates in Go so the error names the field rather
// than a constraint.
func (d *DB) UpsertTriageDecision(t TriageDecision) error {
	where := t.Source + "/" + t.StableID
	if err := requireInstance("record triage decision "+t.StableID, t.Source); err != nil {
		return err
	}
	if t.StableID == "" {
		return fmt.Errorf("state: record triage decision %s: stable id is required", where)
	}
	if !slices.Contains(TriageVerdicts(), t.Verdict) {
		return fmt.Errorf("state: record triage decision %s: verdict %q is not one of %v", where, t.Verdict, TriageVerdicts())
	}
	if !ValidTriageLayer(t.Layer) {
		return fmt.Errorf("state: record triage decision %s: layer %q is not one of %v", where, t.Layer, TriageLayers())
	}
	if (t.Layer == TriageLayerNone) != (t.Rule == "") {
		return fmt.Errorf("state: record triage decision %s: layer %q and rule %q disagree — a decisive layer names its rule, an undecided one has none", where, t.Layer, t.Rule)
	}
	if t.Digest == "" {
		return fmt.Errorf("state: record triage decision %s: the triage digest is required", where)
	}
	at := t.DecidedAt
	if at.IsZero() {
		at = time.Now()
	}
	_, err := d.sql.Exec(`
		INSERT INTO triage_decisions (source, stable_id, verdict, layer, rule, reason, digest, decided_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(source, stable_id) DO UPDATE SET
			verdict    = excluded.verdict,
			layer      = excluded.layer,
			rule       = excluded.rule,
			reason     = excluded.reason,
			digest     = excluded.digest,
			decided_at = excluded.decided_at`,
		t.Source, t.StableID, string(t.Verdict), t.Layer, t.Rule, t.Reason, t.Digest, fmtTime(at))
	if err != nil {
		return fmt.Errorf("state: record triage decision %s: %w", where, err)
	}
	return nil
}

// GetTriageDecision returns the latest decision for a message; ok is false
// when no pass has looked at it.
func (d *DB) GetTriageDecision(source, stableID string) (t TriageDecision, ok bool, err error) {
	var verdict, decidedAt string
	err = d.sql.QueryRow(`
		SELECT source, stable_id, verdict, layer, rule, reason, digest, decided_at
		FROM triage_decisions WHERE source = ? AND stable_id = ?`, source, stableID).
		Scan(&t.Source, &t.StableID, &verdict, &t.Layer, &t.Rule, &t.Reason, &t.Digest, &decidedAt)
	if err == sql.ErrNoRows {
		return TriageDecision{}, false, nil
	}
	if err != nil {
		return TriageDecision{}, false, fmt.Errorf("state: get triage decision %s/%s: %w", source, stableID, err)
	}
	t.Verdict = TriageVerdict(verdict)
	if t.DecidedAt, err = parseTime(decidedAt); err != nil {
		return TriageDecision{}, false, err
	}
	return t, true, nil
}

// TriageRuleCount is one (verdict, layer, rule) bucket of an instance's
// triage ledger.
type TriageRuleCount struct {
	Verdict TriageVerdict
	Layer   string
	Rule    string
	Count   int64
}

// TriageCounts is one instance's triage ledger tally for `save status`.
type TriageCounts struct {
	Noise     int64
	Signal    int64
	Undecided int64

	// ByRule splits the three totals by the deciding layer and rule, in a
	// stable order: verdict, then layer in pass order, then rule.
	ByRule []TriageRuleCount
}

// CountTriage returns per-instance ledger tallies keyed by instance id.
// Instances with no decisions are absent from the map.
func (d *DB) CountTriage() (map[string]TriageCounts, error) {
	rows, err := d.sql.Query(`
		SELECT source, verdict, layer, rule, COUNT(*)
		FROM triage_decisions
		GROUP BY source, verdict, layer, rule
		ORDER BY source, verdict, layer, rule`)
	if err != nil {
		return nil, fmt.Errorf("state: triage counts: %w", err)
	}
	defer rows.Close()
	out := make(map[string]TriageCounts)
	for rows.Next() {
		var source, verdict string
		var rc TriageRuleCount
		if err := rows.Scan(&source, &verdict, &rc.Layer, &rc.Rule, &rc.Count); err != nil {
			return nil, fmt.Errorf("state: triage counts: %w", err)
		}
		rc.Verdict = TriageVerdict(verdict)
		c := out[source]
		switch rc.Verdict {
		case TriageNoise:
			c.Noise += rc.Count
		case TriageSignal:
			c.Signal += rc.Count
		case TriageUndecided:
			c.Undecided += rc.Count
		}
		c.ByRule = append(c.ByRule, rc)
		out[source] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: triage counts: %w", err)
	}
	layerOrder := TriageLayers()
	for src, c := range out {
		slices.SortStableFunc(c.ByRule, func(a, b TriageRuleCount) int {
			if a.Verdict != b.Verdict {
				return slices.Index(TriageVerdicts(), a.Verdict) - slices.Index(TriageVerdicts(), b.Verdict)
			}
			if a.Layer != b.Layer {
				return slices.Index(layerOrder, a.Layer) - slices.Index(layerOrder, b.Layer)
			}
			if a.Rule < b.Rule {
				return -1
			}
			if a.Rule > b.Rule {
				return 1
			}
			return 0
		})
		out[src] = c
	}
	return out, nil
}
