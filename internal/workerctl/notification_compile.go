package workerctl

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/provider"
)

// bootstrapSupport rejects unsupported transitions before credential writes or
// gateway initialization. Their implementation must use the relevant domain
// lifecycle, rather than quietly dropping settings from the runtime.
func directNotificationSupport(d config.DesiredWorkers) error {
	n := d.Notification
	if n == nil || !config.WorkerEnabled(n.Enabled) {
		return fmt.Errorf("bootstrap requires an enabled notification_worker")
	}
	if len(n.Providers) == 0 || len(n.Providers) > 2 {
		return fmt.Errorf("provider_setup_required: declare Telegram and/or Signal")
	}
	if n.StatusLEDs.Enabled && len(n.StatusLEDs.Bindings) != 0 && !deferredBackupLEDBindings(d) {
		return fmt.Errorf("led_setup_required: bindings require the declared local backup-worker producer")
	}
	if n.Commands.Enabled {
		if err := initialBackupCommandSupport(d); err != nil {
			return err
		}
	}
	if len(n.Producers) > 0 && !deferredBackupProducer(d) {
		return fmt.Errorf("consumer_setup_required: short setup supports only the declared local backup-worker producer and its reporting route")
	}
	for name, p := range n.Providers {
		if name != "telegram" && name != "signal" {
			return fmt.Errorf("unsupported notification provider")
		}
		if p.Conversation != "" {
			return fmt.Errorf("conversation_setup_required: interactive provider declarations require enrollment")
		}
		if name == "telegram" && !n.Commands.Enabled && (p.Account != "" || p.BotName != "") {
			// A disabled command policy retains its bot and permission settings.
			// The target coordinator still requires the original admitted receiver
			// before it can interpret this as a command-activity transition.
			declared := d
			copy := *n
			declared.Notification = &copy
			copy.Commands.Enabled = true
			if err := declaredBackupCommandSupport(declared, false); err != nil {
				return fmt.Errorf("conversation_setup_required: disabled commands require their complete retained policy: %w", err)
			}
		}
	}
	return nil
}

// Desired mappings wait for the specialist's real producer reservation. Until
// then the gateway renders only its aggregate verification output.
func deferredBackupLEDBindings(d config.DesiredWorkers) bool {
	if d.Notification == nil || d.Backup == nil || !config.WorkerEnabled(d.Backup.Enabled) || !config.WorkerEnabled(d.Backup.Notifications.Enabled) || d.Backup.Notifications.Worker == "" {
		return false
	}
	for _, binding := range d.Notification.StatusLEDs.Bindings {
		if binding.Producer != "backup-worker" {
			return false
		}
	}
	return true
}

// A notification-first gateway may retain desired backup policy before the
// specialist reserves its identity. This grants no enrollment or health claim;
// the exact policy is compiled only when the real reservation is supplied.
func deferredBackupProducer(d config.DesiredWorkers) bool {
	if d.Notification == nil || len(d.Notification.Producers) != 1 || d.Backup == nil || !config.WorkerEnabled(d.Backup.Enabled) || !config.WorkerEnabled(d.Backup.Notifications.Enabled) || d.Backup.Notifications.Worker == "" {
		return false
	}
	p, ok := d.Notification.Producers["backup-worker"]
	return ok && slices.Contains(p.Routes, d.Backup.Notifications.Route)
}
func bootstrapSupport(d config.DesiredWorkers) error {
	if err := directNotificationSupport(d); err != nil {
		return err
	}
	if len(d.Notification.Providers) != 1 {
		return fmt.Errorf("provider_setup_required: Telegram-only compiler requires Telegram alone")
	}
	if _, ok := d.Notification.Providers["telegram"]; !ok {
		return fmt.Errorf("signal_setup_required: fresh Signal requires its service enrollment")
	}
	return nil
}

// CompileUnboundNotification consumes bounded secret values only on the target.
// It returns the raw input document; callers persist it and resolve once per read.
// Provider fingerprints stay in private generated configuration, not plan output.
func CompileUnboundNotification(d config.DesiredWorkers, paths ResolvedPaths, home string, secrets map[string][]byte) (install.RuntimeInput, error) {
	var zero install.RuntimeInput
	if err := bootstrapSupport(d); err != nil {
		return zero, err
	}
	n := d.Notification
	p := n.Providers["telegram"]
	if n.Commands.Enabled && p.BotName == "" {
		return zero, fmt.Errorf("missing_input: notification_worker.providers.telegram.bot_name")
	}
	tokenPath, chatPath, err := telegramTargetPaths(p, paths, home)
	if err != nil {
		return zero, err
	}
	values := map[string]string{}
	for _, field := range []string{"token", "chat_id"} {
		raw, ok := secrets[field]
		if !ok || len(raw) == 0 || len(raw) > 4096 {
			return zero, fmt.Errorf("credential_input_required: Telegram %s", field)
		}
		v := strings.TrimSpace(string(raw))
		if v == "" || strings.ContainsAny(v, "\x00\r\n") {
			return zero, fmt.Errorf("invalid Telegram %s input", field)
		}
		values[field] = v
	}
	if !regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`).MatchString(values["token"]) {
		return zero, fmt.Errorf("invalid Telegram token format")
	}
	if p.Account != "" && strings.SplitN(values["token"], ":", 2)[0] != p.Account {
		return zero, fmt.Errorf("Telegram command account differs from the supplied bot credential")
	}
	if len(n.Routes) == 0 {
		return zero, fmt.Errorf("notification routes are required")
	}
	socket, err := declaredLEDSocket(n.StatusLEDs, home, paths.targetUID)
	if err != nil {
		return zero, err
	}
	compiled, err := CompileNotification(unboundCommandDeclaration(d), TargetFacts{Home: home}, paths, NotificationBindings{LEDSocket: socket, Providers: map[string]provider.Profile{"telegram": {Kind: "telegram", TokenFile: tokenPath, ChatFile: chatPath, Destination: provider.TelegramIdentity(values["token"], values["chat_id"])}}})
	return compiled.Input, err
}

func telegramTargetPaths(p config.ProviderDeclaration, paths ResolvedPaths, home string) (string, string, error) {
	resolve := func(target, input, field string) (string, error) {
		if target != "" && input != "" {
			return "", fmt.Errorf("ambiguous Telegram credential reference")
		}
		if target != "" {
			return ResolveTargetPath(target, home)
		}
		if input != "" {
			return filepath.Join(paths.Credentials, "notification-worker", "telegram-"+field), nil
		}
		return "", fmt.Errorf("Telegram %s credential reference required", field)
	}
	token, err := resolve(p.TokenFile, p.TokenInput, "token")
	if err != nil {
		return "", "", err
	}
	chat := ""
	if p.Conversation == "" {
		chat, err = resolve(p.ChatIDFile, p.ChatIDInput, "chat-id")
	} else if p.ChatIDFile != "" || p.ChatIDInput != "" {
		return "", "", fmt.Errorf("interactive destination excludes fixed chat credentials")
	}

	if err != nil {
		return "", "", err
	}
	for _, credential := range []string{token, chat} {
		if credential == "" {
			continue
		}
		if credential == paths.Runtime || within(credential, paths.State) || within(credential, paths.Installation) || within(credential, paths.Control) || within(credential, paths.UnitDirectory) {
			return "", "", fmt.Errorf("credential overlaps managed runtime or state")
		}
	}
	if token == chat {
		return "", "", fmt.Errorf("Telegram credential references must be distinct")
	}
	return token, chat, nil
}
