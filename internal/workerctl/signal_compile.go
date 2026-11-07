package workerctl

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
)

// CompileFreshSignalNotification compiles direct-alert profiles from the exact
// service enrollment and bounded target-private inputs. It grants no delivery.
func CompileFreshSignalNotification(d config.DesiredWorkers, paths ResolvedPaths, home string, secrets map[string][]byte, service install.FreshSignalManifest) (install.RuntimeInput, error) {
	var zero install.RuntimeInput
	n := d.Notification
	if n == nil || !config.WorkerEnabled(n.Enabled) || n.Commands.Enabled || len(n.Producers) > 0 && !deferredBackupProducer(d) {
		return zero, fmt.Errorf("fresh direct Signal setup requires ordinary provider policy without interactive consumers")
	}
	p, ok := n.Providers["signal"]
	if !ok || p.Conversation != "" || p.Account != "" && p.Account != service.Spec.Inbox.Account {
		return zero, fmt.Errorf("Signal account declaration differs from fresh service")
	}
	resolved, err := ResolveSignalPaths(p, paths, home, service.Spec.CLISHA256)
	if err != nil {
		return zero, err
	}
	socket := service.Spec.Socket
	if socket == "" {
		socket = filepath.Join(service.Spec.Root, "api.sock")
	}
	if resolved.Account != service.Spec.AccountState || resolved.Inbox != service.Spec.Inbox.State || resolved.Service != service.Spec.Root || resolved.Socket != socket || resolved.AccountFile != service.Spec.AccountFile || resolved.RecipientFile != service.Spec.RecipientFile {
		return zero, fmt.Errorf("fresh Signal service paths differ from declaration")
	}
	if inputDigest(secrets["signal_account"]) != service.Spec.AccountSHA256 || inputDigest(secrets["signal_recipient"]) != service.Spec.RecipientSHA256 {
		return zero, fmt.Errorf("Signal private inputs differ from owned service")
	}
	return compileDirectSignalNotification(d, paths, home, secrets, resolved)
}

func compileDirectSignalNotification(d config.DesiredWorkers, paths ResolvedPaths, home string, secrets map[string][]byte, resolved ResolvedSignalPaths) (install.RuntimeInput, error) {
	var zero install.RuntimeInput
	n := d.Notification
	value := func(field string) (string, error) {
		raw := secrets[field]
		v := strings.TrimSpace(string(raw))
		if len(raw) == 0 || len(raw) > 4096 || v == "" || strings.ContainsAny(v, "\x00\r\n") {
			return "", fmt.Errorf("private provider input required: %s", field)
		}
		return v, nil
	}
	account, err := value("signal_account")
	if err != nil {
		return zero, err
	}
	recipient, err := value("signal_recipient")
	if err != nil {
		return zero, err
	}
	profiles := map[string]provider.Profile{"signal": {Kind: "signal", Socket: resolved.Socket, Destination: relay.Destination(account, recipient)}}
	if telegram, ok := n.Providers["telegram"]; ok {
		if telegram.Conversation != "" || telegram.Account != "" || telegram.BotName != "" {
			return zero, fmt.Errorf("interactive Telegram requires explicit enrollment")
		}
		tokenPath, chatPath, err := telegramTargetPaths(telegram, paths, home)
		if err != nil {
			return zero, err
		}
		token, err := value("token")
		if err != nil {
			return zero, err
		}
		chat, err := value("chat_id")
		if err != nil {
			return zero, err
		}
		if !regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`).MatchString(token) {
			return zero, fmt.Errorf("invalid Telegram token format")
		}
		profiles["telegram"] = provider.Profile{Kind: "telegram", TokenFile: tokenPath, ChatFile: chatPath, Destination: provider.TelegramIdentity(token, chat)}
	}
	socket, err := declaredLEDSocket(n.StatusLEDs, home, paths.targetUID)
	if err != nil {
		return zero, err
	}
	compiled, err := CompileNotification(d, TargetFacts{Home: home}, paths, NotificationBindings{Providers: profiles, LEDSocket: socket})
	return compiled.Input, err
}
