package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"comms/internal/config"
	"comms/internal/outbox"
	"comms/internal/state"
)

type fakeOutboundSender struct {
	calls int
	draft outbox.Draft
}

func (s *fakeOutboundSender) Send(_ context.Context, d outbox.Draft, _ time.Time) (outbox.Receipt, error) {
	s.calls++
	s.draft = d
	return outbox.Receipt{ProviderID: "remote-1", SentAt: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}, nil
}

func TestRunSendMovesSuccessfulDraftAndRecordsLedger(t *testing.T) {
	cfgDir, stateHome, archiveRoot, outboxRoot := t.TempDir(), t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "outbox")
	setTestEnv(t, cfgDir, stateHome)
	writeFile(t, filepath.Join(cfgDir, "config.toml"), fmt.Sprintf(`archive_root = %q
timezone = "UTC"
[sending]
root = %q
[[google]]
label = "work"
account = "you@example.com"
gmail = true
send_email = true
`, archiveRoot, outboxRoot))
	raw := `---
type: email
account: work
to: jane@example.com
subject: Hello
---
Hello Jane.
`
	sendPath := filepath.Join(outboxRoot, "send", "hello.md")
	writeFile(t, sendPath, raw)

	fake := &fakeOutboundSender{}
	builder := func(_ context.Context, _ *sendApp, inst config.Instance) (outbox.Sender, error) {
		if inst.ID != "gmail:work" {
			t.Fatalf("instance = %+v", inst)
		}
		return fake, nil
	}
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := runSend(cmd, false, builder); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 || fake.draft.Subject != "Hello" {
		t.Fatalf("sender = %+v", fake)
	}
	if _, err := os.Stat(sendPath); !os.IsNotExist(err) {
		t.Fatalf("send file still exists: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(outboxRoot, "archived", "2026", "09", "04", "hello_*.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("archived matches = %v, %v; output:\n%s", matches, err, output.String())
	}
	if got, err := os.ReadFile(matches[0]); err != nil || string(got) != raw {
		t.Fatalf("archived bytes changed: %q, %v", got, err)
	}

	db, err := state.Open(filepath.Join(stateHome, "comms", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	d, err := outbox.Parse("hello.md", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	row, ok, err := db.GetOutgoing(d.MessageKey)
	if err != nil || !ok || row.Status != state.OutgoingArchived || row.ProviderID != "remote-1" {
		t.Fatalf("ledger row = %+v, ok=%v err=%v", row, ok, err)
	}
	if !strings.Contains(output.String(), "1 sent") {
		t.Fatalf("output:\n%s", output.String())
	}
}

func TestRunSendDryRunDoesNotCallSenderOrMove(t *testing.T) {
	cfgDir, stateHome, archiveRoot, outboxRoot := t.TempDir(), t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "outbox")
	setTestEnv(t, cfgDir, stateHome)
	writeFile(t, filepath.Join(cfgDir, "config.toml"), fmt.Sprintf(`archive_root = %q
timezone = "UTC"
[sending]
root = %q
[[google]]
label = "work"
account = "you@example.com"
send_chat = true
`, archiveRoot, outboxRoot))
	path := filepath.Join(outboxRoot, "send", "chat.md")
	writeFile(t, path, "---\ntype: chat\naccount: work\nspace: spaces/AAA\n---\nhello\n")
	fake := &fakeOutboundSender{}
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := runSend(cmd, true, func(context.Context, *sendApp, config.Instance) (outbox.Sender, error) { return fake, nil }); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 0 {
		t.Fatalf("dry run called sender %d times", fake.calls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("dry run moved file: %v", err)
	}
	if !strings.Contains(output.String(), "would send") {
		t.Fatalf("output:\n%s", output.String())
	}
}
