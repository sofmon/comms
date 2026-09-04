package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTriageDefaults(t *testing.T) {
	clearEnv(t)
	cfgDir := t.TempDir()
	t.Setenv("COMMS_CONFIG_DIR", cfgDir)
	cfg, err := Load(writeConfig(t, minimalValid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tr := cfg.Triage
	if tr.AfterSync || !tr.HeaderHeuristics || tr.LLM.Enabled || tr.LLM.InDaemon {
		t.Errorf("switch defaults: %+v", tr)
	}
	if tr.LLM.BaseURL != "http://127.0.0.1:1234/v1" || tr.LLM.Timeout.Duration() != 20*time.Second || tr.LLM.MaxBodyChars != 4000 {
		t.Errorf("llm defaults: %+v", tr.LLM)
	}
	if want := filepath.Join(cfgDir, "triage.toml"); tr.RulesFilePath != want {
		t.Errorf("RulesFilePath = %q, want %q", tr.RulesFilePath, want)
	}
	if len(tr.ProtectSubject) == 0 || len(tr.ProtectFrom) != 0 {
		t.Errorf("protect defaults: from %v subject %v", tr.ProtectFrom, tr.ProtectSubject)
	}
	if got := cfg.OwnAddresses(); len(got) != 2 || got[0] != "jane@example.com" || got[1] != "me@fastmail.com" {
		t.Errorf("OwnAddresses = %v", got)
	}
}

func TestTriageExplicitValues(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	home, _ := os.UserHomeDir()
	cfg, err := Load(writeConfig(t, `
[triage]
after_sync = true
header_heuristics = false
rules_file = "~/rules/triage.toml"
protect_from = ["boss@example.com"]
protect_subject = []

[triage.llm]
enabled = true
in_daemon = true
base_url = "http://localhost:11434/v1"
model = "qwen2.5"
timeout = "5s"
max_body_chars = 1200
`+minimalValid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tr := cfg.Triage
	if !tr.AfterSync || tr.HeaderHeuristics || len(tr.ProtectFrom) != 1 || len(tr.ProtectSubject) != 0 {
		t.Errorf("explicit values not applied: %+v", tr)
	}
	if want := filepath.Join(home, "rules", "triage.toml"); tr.RulesFilePath != want {
		t.Errorf("RulesFilePath = %q, want %q", tr.RulesFilePath, want)
	}
	if !tr.LLM.Enabled || !tr.LLM.InDaemon || tr.LLM.Model != "qwen2.5" || tr.LLM.Timeout.Duration() != 5*time.Second || tr.LLM.MaxBodyChars != 1200 {
		t.Errorf("llm values: %+v", tr.LLM)
	}
}

func TestTriageValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	for name, tc := range map[string]struct{ toml, want string }{
		"llm enabled without model": {"[triage.llm]\nenabled = true\n", "model is required"},
		"in_daemon without enabled": {"[triage.llm]\nin_daemon = true\n", "in_daemon"},
		"bad base_url":              {"[triage.llm]\nbase_url = \"localhost:1234\"\n", "base_url"},
		"zero timeout":              {"[triage.llm]\ntimeout = \"0s\"\n", "timeout"},
		"zero body cap":             {"[triage.llm]\nmax_body_chars = 0\n", "max_body_chars"},
		"empty protect glob":        {"[triage]\nprotect_subject = [\"*x*\", \"\"]\n", "protect_subject: pattern #2"},
		"unknown key":               {"[triage]\nafter_sinc = true\n", "unknown key"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.toml+minimalValid))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
	// The skeleton's commented defaults, uncommented, are valid.
	if _, err := Load(writeConfig(t, `
[triage]
after_sync = false
header_heuristics = true
protect_from    = []
protect_subject = ["*invoice*"]
[triage.llm]
enabled        = false
base_url       = "http://127.0.0.1:1234/v1"
timeout        = "20s"
max_body_chars = 4000
`+minimalValid)); err != nil {
		t.Errorf("the documented defaults do not validate: %v", err)
	}
}
