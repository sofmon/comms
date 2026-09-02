package state_test

import (
	"strings"
	"testing"
	"time"

	"save/internal/state"
)

func triageMsg(source, id string) state.Message {
	return state.Message{
		Source:      source,
		StableID:    id,
		TS:          time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		DayBucket:   "2026-08-07",
		RelPath:     "2026/08/07/140000_" + state.Tag(source) + "_" + id + "_ab12cd34.md",
		ContentHash: "hash-" + id,
	}
}

// TestCommittedMessageStartsInTheArchive: a fresh row has the archive
// disposition and no triage history, and reading it back carries every
// column CommitMessage wrote.
func TestCommittedMessageStartsInTheArchive(t *testing.T) {
	db := openTest(t)
	src := state.InstanceID(state.SourceGmail, "work")
	want := triageMsg(src, "m1")
	want.RFC822MsgID, want.ThreadID, want.OrigOffset = "<m1@example.com>", "t1", "+02:00"
	if err := db.CommitMessage(want); err != nil {
		t.Fatal(err)
	}

	got, ok, err := db.GetMessage(src, "m1")
	if err != nil || !ok {
		t.Fatalf("GetMessage = ok %v, err %v", ok, err)
	}
	if got.Disposition != state.DispositionArchive {
		t.Errorf("fresh row disposition = %q, want %q", got.Disposition, state.DispositionArchive)
	}
	if got.DispositionReason != "" || got.DispositionRule != "" || !got.DispositionAt.IsZero() || got.TriageDigest != "" {
		t.Errorf("fresh row carries triage history: %+v", got)
	}
	if got.RelPath != want.RelPath || got.ContentHash != want.ContentHash || !got.TS.Equal(want.TS) ||
		got.RFC822MsgID != want.RFC822MsgID || got.ThreadID != want.ThreadID || got.OrigOffset != want.OrigOffset ||
		got.DayBucket != want.DayBucket || got.Deleted {
		t.Errorf("round trip lost a column:\n got %+v\nwant %+v", got, want)
	}
	if _, ok, err := db.GetMessage(src, "nope"); err != nil || ok {
		t.Errorf("GetMessage(unknown) = ok %v, err %v; want false, nil", ok, err)
	}
}

// TestSetDispositionSurvivesRecommit: triage's verdict must outlive a
// crash-replay re-commit of the same message, and CommitMessage must refuse
// to be the thing that moves a note.
func TestSetDispositionSurvivesRecommit(t *testing.T) {
	db := openTest(t)
	src := state.InstanceID(state.SourceGmail, "work")
	m := triageMsg(src, "m1")
	if err := db.CommitMessage(m); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDisposition(src, "m1", state.DispositionSpam, "matched noise rule github", "rules:noise:github", "0123456789abcdef"); err != nil {
		t.Fatalf("SetDisposition: %v", err)
	}
	got, _, err := db.GetMessage(src, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Disposition != state.DispositionSpam || got.DispositionRule != "rules:noise:github" ||
		got.DispositionReason != "matched noise rule github" || got.TriageDigest != "0123456789abcdef" || got.DispositionAt.IsZero() {
		t.Fatalf("after SetDisposition: %+v", got)
	}

	// The re-render path re-commits with a new hash and no triage fields.
	m.ContentHash = "hash-2"
	if err := db.CommitMessage(m); err != nil {
		t.Fatal(err)
	}
	again, _, _ := db.GetMessage(src, "m1")
	if again.Disposition != state.DispositionSpam || again.DispositionRule != got.DispositionRule || again.TriageDigest != got.TriageDigest {
		t.Errorf("re-commit clobbered the disposition: %+v", again)
	}
	if again.ContentHash != "hash-2" {
		t.Errorf("re-commit did not update the hash: %+v", again)
	}

	// A commit that names the spam tree is a bug, not a move.
	spam := m
	spam.Disposition = state.DispositionSpam
	if err := db.CommitMessage(spam); err == nil || !strings.Contains(err.Error(), "SetDisposition") {
		t.Errorf("CommitMessage with disposition spam: err = %v, want a refusal pointing at SetDisposition", err)
	}
	// ...while restating the archive disposition explicitly is harmless.
	arch := m
	arch.Disposition = state.DispositionArchive
	if err := db.CommitMessage(arch); err != nil {
		t.Errorf("CommitMessage with an explicit archive disposition: %v", err)
	}

	// Back to the archive with an empty digest: "look at this again".
	if err := db.SetDisposition(src, "m1", state.DispositionArchive, "manual", "manual", ""); err != nil {
		t.Fatal(err)
	}
	back, _, _ := db.GetMessage(src, "m1")
	if back.Disposition != state.DispositionArchive || back.TriageDigest != "" || back.DispositionRule != "manual" {
		t.Errorf("after untriage-style SetDisposition: %+v", back)
	}
}

func TestSetDispositionValidates(t *testing.T) {
	db := openTest(t)
	src := state.InstanceID(state.SourceGmail, "work")
	if err := db.SetDisposition(src, "missing", state.DispositionSpam, "", "", ""); err == nil {
		t.Error("SetDisposition on an unknown message did not error")
	}
	if err := db.CommitMessage(triageMsg(src, "m1")); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDisposition(src, "m1", "trash", "", "", ""); err == nil {
		t.Error("SetDisposition accepted a disposition that is not one of the constants")
	}
	if err := db.SetDisposition(src, "m1", "", "", "", ""); err == nil {
		t.Error("SetDisposition accepted an empty disposition")
	}
}

// TestMessagesByRelPathIsExact: the untriage lookup matches the stored rel
// path byte for byte and returns nothing for a path from another root.
func TestMessagesByRelPathIsExact(t *testing.T) {
	db := openTest(t)
	src := state.InstanceID(state.SourceGmail, "work")
	m := triageMsg(src, "m1")
	if err := db.CommitMessage(m); err != nil {
		t.Fatal(err)
	}
	got, err := db.MessagesByRelPath(m.RelPath)
	if err != nil || len(got) != 1 || got[0].StableID != "m1" {
		t.Fatalf("MessagesByRelPath = %+v, %v", got, err)
	}
	if got, _ := db.MessagesByRelPath("/" + m.RelPath); len(got) != 0 {
		t.Errorf("an absolute path matched: %+v", got)
	}
}

// TestTriageDecisionsAreAccountScopedAndCounted: two accounts that archived
// the same message id keep separate decisions, the latest decision replaces
// the earlier one, and the tallies split by verdict, layer and rule.
func TestTriageDecisionsAreAccountScopedAndCounted(t *testing.T) {
	db := openTest(t)
	a := state.InstanceID(state.SourceGmail, "work")
	b := state.InstanceID(state.SourceGmail, "personal")
	const digest = "0123456789abcdef"
	put := func(src, id string, v state.TriageVerdict, layer, rule string) {
		t.Helper()
		if err := db.UpsertTriageDecision(state.TriageDecision{
			Source: src, StableID: id, Verdict: v, Layer: layer, Rule: rule, Reason: "because", Digest: digest,
		}); err != nil {
			t.Fatalf("UpsertTriageDecision(%s/%s): %v", src, id, err)
		}
	}
	put(a, "m1", state.TriageNoise, state.TriageLayerRules, "noise:github")
	put(a, "m2", state.TriageNoise, state.TriageLayerRules, "noise:github")
	put(a, "m3", state.TriageSignal, state.TriageLayerProtect, "attachment")
	put(a, "m4", state.TriageUndecided, state.TriageLayerNone, "")
	put(b, "m1", state.TriageSignal, state.TriageLayerRules, "keep:billing")
	// A later decision for a's m2 replaces the first.
	put(a, "m2", state.TriageSignal, state.TriageLayerLLM, "qwen@v1")

	d, ok, err := db.GetTriageDecision(a, "m2")
	if err != nil || !ok || d.Verdict != state.TriageSignal || d.Layer != state.TriageLayerLLM || d.Rule != "qwen@v1" || d.DecidedAt.IsZero() {
		t.Fatalf("GetTriageDecision(a/m2) = %+v, %v, %v", d, ok, err)
	}
	if _, ok, _ := db.GetTriageDecision(b, "m2"); ok {
		t.Error("b sees a's decision")
	}

	counts, err := db.CountTriage()
	if err != nil {
		t.Fatal(err)
	}
	ca := counts[a]
	if ca.Noise != 1 || ca.Signal != 2 || ca.Undecided != 1 {
		t.Errorf("a counts = %+v", ca)
	}
	// Ordered by verdict, then layer in pass order, then rule.
	var got []string
	for _, rc := range ca.ByRule {
		got = append(got, string(rc.Verdict)+"/"+rc.Layer+"/"+rc.Rule)
	}
	want := []string{"noise/rules/noise:github", "signal/protect/attachment", "signal/llm/qwen@v1", "undecided/none/"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("a by-rule order = %v, want %v", got, want)
	}
	if cb := counts[b]; cb.Signal != 1 || cb.Noise != 0 || len(cb.ByRule) != 1 {
		t.Errorf("b counts = %+v", cb)
	}
}

// TestMessagesForTriageScope: the pass sees exactly the rows it should —
// unsettled ones by default, everything under --reclassify, narrowed by
// day, since, source and by which layer decided — in a stable order.
func TestMessagesForTriageScope(t *testing.T) {
	db := openTest(t)
	a := state.InstanceID(state.SourceGmail, "work")
	b := state.InstanceID(state.SourceFastmail, "fm")
	const cur = "0123456789abcdef"
	add := func(src, id, day string, hour int) {
		t.Helper()
		m := triageMsg(src, id)
		m.DayBucket = day
		m.TS = time.Date(2026, 8, 7, hour, 0, 0, 0, time.UTC)
		if err := db.CommitMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	add(a, "fresh", "2026-08-07", 9)
	add(a, "settled", "2026-08-07", 8)
	add(a, "stale", "2026-08-06", 10)
	add(a, "by-llm", "2026-08-05", 10)
	add(b, "other", "2026-08-07", 7)
	if err := db.SetDisposition(a, "settled", state.DispositionArchive, "kept", "rules:keep:x", cur); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDisposition(a, "stale", state.DispositionSpam, "noise", "rules:noise:y", "ffffffffffffffff"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDisposition(a, "by-llm", state.DispositionSpam, "model", "llm:qwen@v1", cur); err != nil {
		t.Fatal(err)
	}
	ids := func(s state.TriageScope) string {
		t.Helper()
		rows, err := db.MessagesForTriage(s)
		if err != nil {
			t.Fatalf("MessagesForTriage(%+v): %v", s, err)
		}
		var out []string
		for _, m := range rows {
			out = append(out, m.StableID)
		}
		return strings.Join(out, ",")
	}
	for name, tc := range []struct {
		name  string
		scope state.TriageScope
		want  string
	}{
		{"unsettled only", state.TriageScope{Sources: []string{a}, Digest: cur}, "stale,fresh"},
		{"reclassify sees all, ordered by day then time", state.TriageScope{Sources: []string{a}, Digest: cur, Reclassify: true}, "by-llm,stale,settled,fresh"},
		{"one day", state.TriageScope{Sources: []string{a}, Digest: cur, Day: "2026-08-06"}, "stale"},
		{"since", state.TriageScope{Sources: []string{a}, Digest: cur, Since: "2026-08-06", Reclassify: true}, "stale,settled,fresh"},
		{"both sources, instance order", state.TriageScope{Sources: []string{a, b}, Digest: cur}, "other,stale,fresh"},
		{"only llm decisions", state.TriageScope{Sources: []string{a}, Digest: cur, Reclassify: true, Only: "llm"}, "by-llm"},
		{"only rules decisions", state.TriageScope{Sources: []string{a}, Digest: cur, Reclassify: true, Only: "rules"}, "stale,settled,fresh"},
	} {
		if got := ids(tc.scope); got != tc.want {
			t.Errorf("%s (%d): got %q, want %q", tc.name, name, got, tc.want)
		}
	}
	if _, err := db.MessagesForTriage(state.TriageScope{Digest: cur}); err == nil {
		t.Error("no sources accepted")
	}
	if _, err := db.MessagesForTriage(state.TriageScope{Sources: []string{a}, Only: "vibes"}); err == nil {
		t.Error("unknown --only accepted")
	}
}

func TestTriageDecisionValidates(t *testing.T) {
	db := openTest(t)
	src := state.InstanceID(state.SourceGmail, "work")
	base := state.TriageDecision{Source: src, StableID: "m1", Verdict: state.TriageNoise, Layer: state.TriageLayerRules, Rule: "noise:x", Reason: "r", Digest: "d"}
	for name, mut := range map[string]func(*state.TriageDecision){
		"bare kind as source": func(d *state.TriageDecision) { d.Source = state.SourceGmail },
		"empty stable id":     func(d *state.TriageDecision) { d.StableID = "" },
		"unknown verdict":     func(d *state.TriageDecision) { d.Verdict = "maybe" },
		"unknown layer":       func(d *state.TriageDecision) { d.Layer = "vibes" },
		"decisive but no rule": func(d *state.TriageDecision) {
			d.Rule = ""
		},
		"undecided with a rule": func(d *state.TriageDecision) {
			d.Verdict, d.Layer = state.TriageUndecided, state.TriageLayerNone
		},
		"no digest": func(d *state.TriageDecision) { d.Digest = "" },
	} {
		d := base
		mut(&d)
		if err := db.UpsertTriageDecision(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := db.UpsertTriageDecision(base); err != nil {
		t.Errorf("the valid shape was refused: %v", err)
	}
}
