package config

import (
	"strings"
	"testing"

	"save/internal/policy"
)

// withAccounts appends the minimum accounts a config needs to be valid, so a
// test can focus on the [attachments] block alone.
func withAccounts(attachments string) string {
	return attachments + `
[[google]]
label   = "work"
account = "jane@example.com"
gmail   = true
`
}

func TestAttachmentDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("SAVE_CONFIG_DIR", t.TempDir())

	cfg, err := Load(writeConfig(t, minimalValid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Attachments
	for _, tc := range []struct {
		name string
		got  Bytes
		want Bytes
	}{
		{"max_size", a.MaxSize, 50 * MB},
		{"chat_max_size (inherits max_size)", a.ChatMaxSize, 50 * MB},
		{"max_message_bytes", a.MaxMessageBytes, 100 * MB},
		{"max_per_message", a.MaxPerMessage, 150 * MB},
		{"run_budget", a.RunBudget, 2 * GB},
		{"free_space_floor", a.FreeSpaceFloor, 5 * GB},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	if a.MaxPartsPerMessage != 500 {
		t.Errorf("max_parts_per_message = %d, want 500", a.MaxPartsPerMessage)
	}
	if !a.AllowContainers {
		t.Error("allow_containers must default to true")
	}
	if a.AllowSVG {
		t.Error("allow_svg must default to false")
	}
	if a.AllowMacroOffice {
		t.Error("allow_macro_office must default to false")
	}
	if !a.Quarantine {
		t.Error("quarantine must default to true")
	}
	if a.OnMismatch != policy.OnMismatchSkip {
		t.Errorf("on_mismatch = %q, want %q", a.OnMismatch, policy.OnMismatchSkip)
	}
	if a.ScanAction != policy.ScanActionRecord {
		t.Errorf("scan_action = %q, want %q", a.ScanAction, policy.ScanActionRecord)
	}
	if len(a.ScanCommand) != 0 {
		t.Errorf("scan_command must default to empty, got %v", a.ScanCommand)
	}
}

func TestAttachmentOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("SAVE_CONFIG_DIR", t.TempDir())

	cfg, err := Load(writeConfig(t, withAccounts(`
[attachments]
max_size              = "25MiB"
chat_max_size         = "120MB"
max_message_bytes     = "80MB"
max_per_message       = "140MB"
max_parts_per_message = 42
run_budget            = "0"
free_space_floor      = "0"
allow_extensions      = ["7z=application/x-7z-compressed", "sql"]
deny_extensions       = ["gif"]
allow_containers      = false
allow_svg             = true
allow_macro_office    = true
on_mismatch           = "store-warn"
quarantine            = false
scan_command          = ["clamdscan", "--fdpass", "--no-summary"]
scan_action           = "reject"
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Attachments
	if a.MaxSize != 25*MiB {
		t.Errorf("max_size = %s", a.MaxSize)
	}
	if a.ChatMaxSize != 120*MB {
		t.Errorf("chat_max_size = %s", a.ChatMaxSize)
	}
	if a.MaxPartsPerMessage != 42 || a.RunBudget != 0 || a.FreeSpaceFloor != 0 {
		t.Errorf("counts/budgets not applied: %+v", a)
	}
	if a.AllowContainers || !a.AllowSVG || !a.AllowMacroOffice || a.Quarantine {
		t.Errorf("booleans not applied: %+v", a)
	}
	if a.OnMismatch != policy.OnMismatchStoreWarn || a.ScanAction != policy.ScanActionReject {
		t.Errorf("enums not applied: %+v", a)
	}
	if strings.Join(a.ScanCommand, " ") != "clamdscan --fdpass --no-summary" {
		t.Errorf("scan_command = %v", a.ScanCommand)
	}

	// The block reaches the decision engine intact.
	p, err := cfg.Policy()
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	if p.PermittedTypes("gif") != nil {
		t.Error("deny_extensions did not reach the policy")
	}
	if p.PermittedTypes("sql") == nil || p.PermittedTypes("7z") == nil {
		t.Error("allow_extensions did not reach the policy")
	}
	if p.PermittedTypes("zip") != nil {
		t.Error("allow_containers = false did not reach the policy")
	}
	if p.PermittedTypes("svg") == nil || p.PermittedTypes("docm") == nil {
		t.Error("allow_svg / allow_macro_office did not reach the policy")
	}
	if got := p.MaxSizeFor(true); got != int64(120*MB) {
		t.Errorf("chat cap reached the policy as %d", got)
	}
}

func TestAttachmentValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv("SAVE_CONFIG_DIR", t.TempDir())

	for _, tc := range []struct{ name, block, want string }{
		{"zero max_size", `max_size = "0"`, "attachments.max_size must be a positive size"},
		{"negative size", `max_size = "-5MB"`, "cannot be negative"},
		{"garbage size", `max_size = "50 gigs"`, "unknown unit"},
		{"zero parts", `max_parts_per_message = 0`, "attachments.max_parts_per_message"},
		{"negative parts", `max_parts_per_message = -1`, "attachments.max_parts_per_message"},
		{"zero max_message_bytes", `max_message_bytes = "0"`, "attachments.max_message_bytes"},
		{"zero max_per_message", `max_per_message = "0"`, "attachments.max_per_message"},
		{"max_size above max_per_message", "max_size = \"200MB\"", "larger than attachments.max_per_message"},
		{"chat cap above max_per_message", "chat_max_size = \"200MB\"", "larger than attachments.max_per_message"},
		{"run_budget under max_size", "run_budget = \"1MB\"", "smaller than attachments.max_size"},
		{"bad on_mismatch", `on_mismatch = "warn"`, "attachments.on_mismatch"},
		{"bad scan_action", `scan_action = "quarantine"`, "attachments.scan_action"},
		{"reject with no scanner", `scan_action = "reject"`, "needs attachments.scan_command"},
		{"executable cannot be allowed", `allow_extensions = ["exe"]`, "cannot be allowed"},
		{"dmg cannot be allowed", `allow_extensions = [".DMG"]`, "cannot be allowed"},
		{"dotted extension", `allow_extensions = ["tar.gz"]`, "attachments.allow_extensions"},
		{"svg has its own flag", `allow_extensions = ["svg"]`, "allow_svg"},
		{"zip has its own flag", `allow_extensions = ["zip"]`, "allow_containers"},
		{"docm has its own flag", `allow_extensions = ["docm"]`, "allow_macro_office"},
		{"bad content type", `allow_extensions = ["7z=nope"]`, "type/subtype"},
		{"typed deny", `deny_extensions = ["pdf=application/pdf"]`, "extension only"},
		{"unknown key", `max_sizes = "50MB"`, "unknown key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, withAccounts("[attachments]\n"+tc.block+"\n")))
			if err == nil {
				t.Fatalf("expected an error for %s", tc.block)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not mention %q:\n%v", tc.want, err)
			}
		})
	}
}

// TestAttachmentValidationReportsEverythingAtOnce keeps the errors.Join
// contract: one run of `save doctor` should list every problem.
func TestAttachmentValidationReportsEverythingAtOnce(t *testing.T) {
	clearEnv(t)
	t.Setenv("SAVE_CONFIG_DIR", t.TempDir())
	_, err := Load(writeConfig(t, withAccounts(`
[attachments]
max_size              = "0"
max_parts_per_message = 0
on_mismatch           = "warn"
`)))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"max_size", "max_parts_per_message", "on_mismatch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("combined error is missing %q:\n%v", want, err)
		}
	}
}

// TestChatMaxSizeInherits: an unset chat_max_size resolves to max_size at load
// time, so every caller computes the same policy digest.
func TestChatMaxSizeInherits(t *testing.T) {
	clearEnv(t)
	t.Setenv("SAVE_CONFIG_DIR", t.TempDir())
	cfg, err := Load(writeConfig(t, withAccounts("[attachments]\nmax_size = \"30MB\"\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Attachments.ChatMaxSize != 30*MB {
		t.Errorf("chat_max_size = %s, want the inherited 30MB", cfg.Attachments.ChatMaxSize)
	}
	// EffectiveChatMaxSize works for a Config built in code, which never went
	// through Load.
	a := DefaultAttachments()
	a.MaxSize = 7 * MB
	if got := a.EffectiveChatMaxSize(); got != 7*MB {
		t.Errorf("EffectiveChatMaxSize = %s, want 7MB", got)
	}
}

// TestSkeletonAttachmentsBlock: `save init` must produce a file that loads,
// keeps the documented defaults, and is honest about what quarantine buys.
func TestSkeletonAttachmentsBlock(t *testing.T) {
	if !strings.Contains(skeleton, "[attachments]") {
		t.Fatal("the skeleton must carry an [attachments] block")
	}
	for _, want := range []string{
		"max_size", "chat_max_size", "max_message_bytes", "max_per_message",
		"max_parts_per_message", "run_budget", "free_space_floor",
		"allow_extensions", "deny_extensions", "allow_containers", "allow_svg",
		"allow_macro_office", "on_mismatch", "quarantine", "scan_command", "scan_action",
	} {
		if !strings.Contains(skeleton, want) {
			t.Errorf("the skeleton does not document %q", want)
		}
	}
	// Honesty about quarantine: consent prompt and Protected View, never
	// "scanned for malware".
	if !strings.Contains(skeleton, "Protected View") || !strings.Contains(skeleton, "consent prompt") {
		t.Error("the quarantine comment must describe the consent prompt and Office Protected View")
	}
	if !strings.Contains(skeleton, "It is NOT a malware scan") {
		t.Error("the quarantine comment must say outright that it is not a malware scan")
	}
	for _, forbidden := range []string{"scanned for malware", "scans for malware", "antivirus protection"} {
		if strings.Contains(strings.ToLower(skeleton), forbidden) {
			t.Errorf("the skeleton claims %q, which is false: XProtect has no signatures for the allowlisted types", forbidden)
		}
	}
	// Recoverability is the promise that makes an allowlist acceptable.
	if !strings.Contains(skeleton, "save refetch") {
		t.Error("the skeleton must tell the operator that skips are recoverable via `save refetch`")
	}
	if !strings.Contains(skeleton, "NOTHING IS EVER DROPPED SILENTLY") {
		t.Error("the skeleton must state the never-drop-silently rule")
	}
}

// TestDefaultConfigPolicyBuilds: the shipped defaults produce a working
// engine, and Config.Policy is the single place callers get one.
func TestDefaultConfigPolicyBuilds(t *testing.T) {
	cfg := Default()
	p, err := cfg.Policy()
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	if p.PolicyDigest() == "" {
		t.Error("a policy must always have a digest")
	}
	if p.PermittedTypes("pdf") == nil {
		t.Error("pdf must be allowlisted by default")
	}
	if p.PermittedTypes("exe") != nil || p.PermittedTypes("docm") != nil || p.PermittedTypes("svg") != nil {
		t.Error("exe, docm and svg must not be allowlisted by default")
	}
	if p.PermittedTypes("zip") == nil {
		t.Error("zip must be allowlisted by default (allow_containers = true)")
	}
}
