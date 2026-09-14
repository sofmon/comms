package cli

import (
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"comms/internal/config"
	"comms/internal/outbox"
	"comms/internal/paths"
	"comms/internal/state"
	"comms/internal/triage"
)

func newNewCmd() *cobra.Command {
	var replyPath string
	cmd := &cobra.Command{
		Use:   "new <instance>",
		Short: "Create an editable outgoing-message draft",
		Long: "Create a provider-correct Markdown template in the configured send/ directory.\n\n" +
			"The template stays invalid until its required fields and body are filled, so an\n" +
			"untouched draft cannot be sent. Use --reply with an archived email note to\n" +
			"prefill its sender, reply subject, and standard email threading headers.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNew(cmd, args[0], replyPath, time.Now)
		},
	}
	cmd.Flags().StringVar(&replyPath, "reply", "", "prefill a reply from an archived incoming email .md file")
	return cmd
}

func runNew(cmd *cobra.Command, instanceID, replyPath string, now func() time.Time) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	inst, err := sendingInstance(cfg, instanceID)
	if err != nil {
		return err
	}

	spec := outbox.Template{Account: inst.Label}
	switch inst.Kind {
	case state.SourceGmail, state.SourceFastmail:
		spec.Kind = outbox.KindEmail
	case state.SourceGChat:
		spec.Kind = outbox.KindChat
	default:
		return fmt.Errorf("unsupported outbound instance %q", inst.ID)
	}
	var warnings []string
	if replyPath != "" {
		if spec.Kind != outbox.KindEmail {
			return errors.New("--reply is supported only for gmail:<label> and fastmail:<label> instances")
		}
		spec, warnings, err = replyTemplate(inst, replyPath)
		if err != nil {
			return err
		}
	}
	content, err := outbox.RenderTemplate(spec)
	if err != nil {
		return fmt.Errorf("render draft template: %w", err)
	}
	for _, dir := range []string{cfg.Sending.Root, cfg.Sending.SendDir()} {
		if err := paths.EnsureDir(dir); err != nil {
			return err
		}
	}
	base := fmt.Sprintf("%s_%s.md", now().Format("20060102-150405"), inst.Tag())
	path, err := createDraftExclusive(cfg.Sending.SendDir(), base, content)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "created %s (0600)\n", path)
	for _, warning := range warnings {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
	if replyPath == "" {
		fmt.Fprintln(out, "edit the required fields and message body, then run:")
	} else {
		fmt.Fprintln(out, "add the reply body, review the prefilled fields, then run:")
	}
	fmt.Fprintln(out, "  comms send --dry-run")
	fmt.Fprintln(out, "  comms send")
	return nil
}

func sendingInstance(cfg *config.Config, id string) (config.Instance, error) {
	for _, inst := range cfg.SendingInstances() {
		if inst.ID == id {
			return inst, nil
		}
	}
	kind, label, ok := state.SplitInstance(id)
	if ok {
		switch kind {
		case state.SourceGmail:
			if _, found := cfg.GoogleByLabel(label); found {
				return config.Instance{}, fmt.Errorf("%s is not send-enabled; set send_email = true and re-authorize that Google account", id)
			}
		case state.SourceGChat:
			if _, found := cfg.GoogleByLabel(label); found {
				return config.Instance{}, fmt.Errorf("%s is not send-enabled; set send_chat = true and re-authorize that Google account", id)
			}
		case state.SourceFastmail:
			if _, found := cfg.FastMailByLabel(label); found {
				return config.Instance{}, fmt.Errorf("%s is not send-enabled; set send_email = true and install a write/send token", id)
			}
		}
	}
	available := make([]string, 0, len(cfg.SendingInstances()))
	for _, inst := range cfg.SendingInstances() {
		available = append(available, inst.ID)
	}
	if len(available) == 0 {
		return config.Instance{}, fmt.Errorf("outbound instance %q is not configured; no send-enabled instances are available", id)
	}
	return config.Instance{}, fmt.Errorf("outbound instance %q is not configured and send-enabled; available: %s", id, strings.Join(available, ", "))
}

func replyTemplate(inst config.Instance, path string) (outbox.Template, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return outbox.Template{}, nil, fmt.Errorf("read reply source %s: %w", path, err)
	}
	note, err := triage.ParseNote(data)
	if err != nil {
		return outbox.Template{}, nil, fmt.Errorf("reply source %s: %w", path, err)
	}
	var recipient string
	for _, raw := range note.From {
		a, err := mail.ParseAddress(raw)
		if err == nil && a.Address != "" {
			recipient = a.String()
			break
		}
	}
	if recipient == "" {
		return outbox.Template{}, nil, fmt.Errorf("reply source %s has no usable From address", path)
	}

	spec := outbox.Template{
		Kind:    outbox.KindEmail,
		Account: inst.Label,
		To:      []string{recipient},
		Subject: replySubject(note.Subject),
	}
	var warnings []string
	if note.MessageID == "" {
		warnings = append(warnings, "the archived email has no Message-ID; the provider may not group the reply into the original conversation")
	} else if id, err := outbox.NormalizeMessageID(note.MessageID); err != nil {
		warnings = append(warnings, "the archived Message-ID is invalid; threading headers were omitted")
	} else {
		spec.InReplyTo = id
		spec.References = []string{id}
	}
	if inst.Kind == state.SourceGmail && note.Source == inst.ID {
		threadID := strings.TrimSpace(note.ThreadID)
		if strings.ContainsAny(threadID, "\r\n") {
			warnings = append(warnings, "the archived Gmail thread id is invalid; the provider thread hint was omitted")
		} else {
			spec.ThreadID = threadID
		}
	}
	return spec, warnings, nil
}

func replySubject(subject string) string {
	subject = strings.Join(strings.Fields(subject), " ")
	if strings.HasPrefix(strings.ToLower(subject), "re:") {
		return subject
	}
	if subject == "" {
		return "Re:"
	}
	return "Re: " + subject
}

func createDraftExclusive(dir, base string, content []byte) (string, error) {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for attempt := 1; attempt <= 10000; attempt++ {
		name := base
		if attempt > 1 {
			name = fmt.Sprintf("%s-%d%s", stem, attempt, ext)
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create draft %s: %w", path, err)
		}
		ok := false
		defer func() {
			if !ok {
				_ = os.Remove(path)
			}
		}()
		if _, err := f.Write(content); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("write draft %s: %w", path, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("sync draft %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("close draft %s: %w", path, err)
		}
		ok = true
		return path, nil
	}
	return "", fmt.Errorf("create draft: too many filename collisions for %s", base)
}
