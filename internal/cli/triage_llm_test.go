package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"save/internal/config"
	"save/internal/state"
)

// llmServer is a scripted OpenAI-compatible endpoint for the CLI tests.
func llmServer(t *testing.T, reply string, status int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen2.5"}]}`))
			return
		}
		w.WriteHeader(status)
		if status/100 != 2 {
			_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": reply}}},
		})
	}))
	t.Cleanup(s.Close)
	return s
}

// llmServerCounting is llmServer answering "noise" and counting the calls.
func llmServerCounting(t *testing.T, calls *int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		*calls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": `{"noise": true, "reason": "bulk"}`}}},
		})
	}))
	t.Cleanup(s.Close)
	return s
}

// enableLLM appends an enabled [triage.llm] block pointing at the server
// to the test config.
func enableLLM(t *testing.T, url string) {
	t.Helper()
	cfgPath := config.DefaultPath()
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, cfgPath, string(b)+"\n[triage.llm]\nenabled = true\nbase_url = \""+url+"/v1\"\nmodel = \"qwen2.5\"\ntimeout = \"2s\"\n")
}

// TestTriageModelLayerDecidesTheUndecided: with the model on, the note the
// rules left alone is judged, moved when the model says noise, and recorded
// under the model's rule id — while the notes the rules decided are never
// sent to it.
func TestTriageModelLayerDecidesTheUndecided(t *testing.T) {
	root, rels := triageArchive(t)
	s := llmServer(t, `{"noise": true, "reason": "reads like a mass mailing"}`, http.StatusOK)
	enableLLM(t, s.URL)
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		t.Fatal(err)
	}
	src := state.InstanceID(state.SourceGmail, "work")

	var out bytes.Buffer
	if err := runTriage(&out, triageOpts{dryRun: true}); err != nil {
		t.Fatalf("dry run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "model qwen2.5") || !strings.Contains(out.String(), "WOULD MOVE to spam: 2 note(s)") ||
		!strings.Contains(out.String(), rels[2]+"  → spam  llm:qwen2.5@v1") {
		t.Errorf("dry run with the model:\n%s", out.String())
	}

	out.Reset()
	if err := runTriage(&out, triageOpts{}); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if !statAt(t, cfg.SpamRoot, rels[2]) || statAt(t, root, rels[2]) {
		t.Error("the model's noise verdict did not move the note")
	}
	if !statAt(t, root, rels[1]) {
		t.Error("the protected note moved")
	}
	db := openTestDB(t)
	m, _, _ := db.GetMessage(src, "plain")
	if m.Disposition != state.DispositionSpam || m.DispositionRule != "llm:qwen2.5@v1" || !strings.Contains(m.DispositionReason, "mass mailing") {
		t.Errorf("row: %+v", m)
	}
	dec, _, _ := db.GetTriageDecision(src, "plain")
	if dec.Layer != state.TriageLayerLLM || dec.Rule != "qwen2.5@v1" {
		t.Errorf("ledger: %+v", dec)
	}
	// --explain shows the model's line too.
	out.Reset()
	if err := runTriage(&out, triageOpts{explain: filepath.Join(cfg.SpamRoot, filepath.FromSlash(rels[2]))}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "layer 3 llm (qwen2.5@v1): reads like a mass mailing → NOISE") {
		t.Errorf("explain:\n%s", out.String())
	}
}

// TestTriageModelFailureIsUndecidedAndRetried: a dead endpoint leaves the
// note where it is, unsettled — so the next pass asks again — and says so.
func TestTriageModelFailureIsUndecidedAndRetried(t *testing.T) {
	root, rels := triageArchive(t)
	s := llmServer(t, "", http.StatusServiceUnavailable)
	enableLLM(t, s.URL)
	src := state.InstanceID(state.SourceGmail, "work")

	var out bytes.Buffer
	if err := runTriage(&out, triageOpts{}); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "model unavailable for 1 note(s)") || !statAt(t, root, rels[2]) {
		t.Errorf("a model failure was not left alone:\n%s", out.String())
	}
	db := openTestDB(t)
	m, _, _ := db.GetMessage(src, "plain")
	if m.Disposition != state.DispositionArchive || m.TriageDigest != "" || !strings.Contains(m.DispositionReason, "model layer failed") {
		t.Errorf("row after a model failure: %+v", m)
	}
	dec, ok, _ := db.GetTriageDecision(src, "plain")
	if !ok || dec.Verdict != state.TriageUndecided || dec.Layer != state.TriageLayerNone {
		t.Errorf("ledger after a model failure: %+v", dec)
	}
	db.Close()
	// The next pass looks at it again (the settled ones are not in scope).
	out.Reset()
	if err := runTriage(&out, triageOpts{dryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2 note(s) in scope") { // the unsettled note and the missing one
		t.Errorf("the failed note was not retried:\n%s", out.String())
	}
}

// TestDoctorPingsTheModelOnlyWhenEnabled: the endpoint is probed only with
// the layer on, and an unanswering one is a warning, not a problem.
func TestDoctorPingsTheModelOnlyWhenEnabled(t *testing.T) {
	cfgDir, stateHome := t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	writeFile(t, filepath.Join(cfgDir, "config.toml"), plainConfig(t.TempDir()))
	doctor := func() (string, int) {
		var out strings.Builder
		d := &doctorReport{out: &out}
		cfg, err := config.Load(config.DefaultPath())
		if err != nil {
			t.Fatal(err)
		}
		d.checkTriage(cfg)
		return out.String(), d.problems
	}
	if text, _ := doctor(); !strings.Contains(text, "model layer off") || strings.Contains(text, "endpoint") {
		t.Errorf("model off:\n%s", text)
	}
	s := llmServer(t, "", http.StatusOK)
	enableLLM(t, s.URL)
	text, problems := doctor()
	if problems != 0 || !strings.Contains(text, "answers and lists \"qwen2.5\"") {
		t.Errorf("model on and reachable: problems %d\n%s", problems, text)
	}
	s.Close()
	text, problems = doctor()
	if problems != 0 || !strings.Contains(text, "is not answering") {
		t.Errorf("model on and dead: problems %d\n%s", problems, text)
	}
}
