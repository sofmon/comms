package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"comms/internal/outbox"
)

func newTestConfig(t *testing.T, googleBlock string) (outboxRoot string) {
	t.Helper()
	cfgDir, stateHome := t.TempDir(), t.TempDir()
	outboxRoot = filepath.Join(t.TempDir(), "outbox")
	setTestEnv(t, cfgDir, stateHome)
	writeFile(t, filepath.Join(cfgDir, "config.toml"), fmt.Sprintf(`archive_root = %q
 timezone = "UTC"
[sending]
root = %q
%s
`, t.TempDir(), outboxRoot, googleBlock))
	return outboxRoot
}

func TestRunNewCreatesExclusiveInvalidTemplate(t *testing.T) {
	root := newTestConfig(t, `[[google]]
label = "work"
account = "you@example.com"
send_email = true
send_chat = true
`)
	at := time.Date(2026, 9, 5, 14, 3, 2, 0, time.UTC)
	for i := 0; i < 2; i++ {
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})
		if err := runNew(cmd, "gmail:work", "", func() time.Time { return at }); err != nil {
			t.Fatalf("runNew %d: %v", i, err)
		}
	}
	first := filepath.Join(root, "send", "20260905-140302_gmail-work.md")
	second := filepath.Join(root, "send", "20260905-140302_gmail-work-2.md")
	for _, path := range []string{first, second} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s permissions = %04o, want 0600", path, info.Mode().Perm())
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte("type: email")) || !bytes.Contains(raw, []byte(`account: "work"`)) {
			t.Fatalf("wrong template:\n%s", raw)
		}
		if _, err := outbox.Parse(filepath.Base(path), raw); err == nil {
			t.Fatalf("untouched generated template %s is sendable", path)
		}
	}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if err := runNew(cmd, "gchat:work", "", func() time.Time { return at.Add(time.Second) }); err != nil {
		t.Fatal(err)
	}
	chatPath := filepath.Join(root, "send", "20260905-140303_gchat-work.md")
	raw, err := os.ReadFile(chatPath)
	if err != nil || !bytes.Contains(raw, []byte(`space: ""`)) {
		t.Fatalf("chat template = %q, %v", raw, err)
	}
}

func TestRunNewReplyPrefillsAddressSubjectAndThreading(t *testing.T) {
	root := newTestConfig(t, `[[google]]
label = "work"
account = "you@example.com"
send_email = true
`)
	incoming := filepath.Join(t.TempDir(), "incoming.md")
	writeFile(t, incoming, `---
source: gmail:work
type: email
render_version: 2
account: you@example.com
account_label: work
message_id: <original@example.net>
thread_id: gmail-thread-123
from:
  - Jane Example <jane@example.net>
to:
  - you@example.com
subject: Project status
---
Original archived body.
`)
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)
	at := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
	if err := runNew(cmd, "gmail:work", incoming, func() time.Time { return at }); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "send", "20260905-150000_gmail-work.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("Original archived body")) {
		t.Fatal("reply template copied the original body")
	}
	draft, err := outbox.Parse(filepath.Base(path), append(raw, []byte("Thanks.\n")...))
	if err != nil {
		t.Fatalf("generated reply does not parse after adding a body: %v\n%s", err, raw)
	}
	if len(draft.To) != 1 || draft.To[0].Email != "jane@example.net" || draft.Subject != "Re: Project status" {
		t.Fatalf("reply addressing = %+v", draft)
	}
	if draft.InReplyTo != "<original@example.net>" || len(draft.References) != 1 || draft.ThreadID != "gmail-thread-123" {
		t.Fatalf("reply threading = %+v", draft)
	}
	if !strings.Contains(output.String(), "add the reply body") || !strings.Contains(output.String(), path) {
		t.Fatalf("output:\n%s", output.String())
	}
}

func TestRunNewRejectsDisabledOrIncompatibleReply(t *testing.T) {
	newTestConfig(t, `[[google]]
label = "work"
account = "you@example.com"
gmail = true
`)
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if err := runNew(cmd, "gmail:work", "", time.Now); err == nil || !strings.Contains(err.Error(), "send_email = true") {
		t.Fatalf("disabled Gmail error = %v", err)
	}

	root := newTestConfig(t, `[[google]]
label = "work"
account = "you@example.com"
send_chat = true
`)
	_ = root
	if err := runNew(cmd, "gchat:work", "incoming.md", time.Now); err == nil || !strings.Contains(err.Error(), "supported only") {
		t.Fatalf("Chat reply error = %v", err)
	}
}
