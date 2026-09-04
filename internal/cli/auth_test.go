package cli

import (
	"strings"
	"testing"
)

// TestResolveAuthLabels pins `comms auth <provider> [label]` resolution: the
// label may be omitted only when a single account of that kind exists, an
// unknown label lists the valid ones, and --all fans out.
func TestResolveAuthLabels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		block   string
		labels  []string
		arg     string
		all     bool
		hasAll  bool
		want    []string
		wantErr []string
	}{
		{
			name:  "single account needs no label",
			block: "google", labels: []string{"work"}, hasAll: true,
			want: []string{"work"},
		},
		{
			name:  "explicit label with one account",
			block: "google", labels: []string{"work"}, arg: "work", hasAll: true,
			want: []string{"work"},
		},
		{
			name:  "explicit label with several accounts",
			block: "google", labels: []string{"work", "personal"}, arg: "personal", hasAll: true,
			want: []string{"personal"},
		},
		{
			name:  "several accounts and no label is an error listing them",
			block: "google", labels: []string{"work", "personal"}, hasAll: true,
			wantErr: []string{"several [[google]] accounts", "comms auth google <label>", "--all", "work, personal"},
		},
		{
			name:  "--all selects every account in config order",
			block: "google", labels: []string{"work", "personal"}, all: true, hasAll: true,
			want: []string{"work", "personal"},
		},
		{
			name:  "--all with a label is refused",
			block: "google", labels: []string{"work", "personal"}, arg: "work", all: true, hasAll: true,
			wantErr: []string{"either a label or --all"},
		},
		{
			name:  "unknown label lists the configured ones",
			block: "google", labels: []string{"work", "personal"}, arg: "typo", hasAll: true,
			wantErr: []string{`no [[google]] account is labelled "typo"`, "work, personal"},
		},
		{
			name:  "no account of that kind",
			block: "fastmail", labels: nil, arg: "fm",
			wantErr: []string{"no [[fastmail]] account is configured"},
		},
		{
			name:  "fastmail multi-account error does not advertise --all",
			block: "fastmail", labels: []string{"fm", "work"},
			wantErr: []string{"several [[fastmail]] accounts", "comms auth fastmail <label>", "fm, work"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAuthLabels(tc.block, tc.labels, tc.arg, tc.all, tc.hasAll)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("resolveAuthLabels succeeded (%v), want an error", got)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				if tc.block == "fastmail" && strings.Contains(err.Error(), "--all") {
					t.Errorf("fastmail error advertises --all, which it does not support: %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAuthLabels: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("resolveAuthLabels = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAuthCommandsTakeOptionalLabel pins the CLI surface: both providers
// accept an optional positional label, and only google offers --all.
func TestAuthCommandsTakeOptionalLabel(t *testing.T) {
	root := newRoot()
	for _, c := range root.Commands() {
		if c.Name() != "auth" {
			continue
		}
		for _, sub := range c.Commands() {
			if err := sub.Args(sub, []string{"work"}); err != nil {
				t.Errorf("auth %s rejects a label argument: %v", sub.Name(), err)
			}
			if err := sub.Args(sub, nil); err != nil {
				t.Errorf("auth %s rejects an omitted label: %v", sub.Name(), err)
			}
			if err := sub.Args(sub, []string{"a", "b"}); err == nil {
				t.Errorf("auth %s accepts two labels", sub.Name())
			}
			hasAll := sub.Flags().Lookup("all") != nil
			if want := sub.Name() == "google"; hasAll != want {
				t.Errorf("auth %s --all present = %v, want %v", sub.Name(), hasAll, want)
			}
		}
		return
	}
	t.Fatal("auth command not found")
}
