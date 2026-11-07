package workerctl

import (
	"fmt"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

func initialBackupCommandSupport(d config.DesiredWorkers) error {
	return declaredBackupCommandSupport(d, true)
}

func declaredBackupCommandSupport(d config.DesiredWorkers, requirePermissions bool) error {
	n, b := d.Notification, d.Backup
	if n == nil || !n.Commands.Enabled {
		return nil
	}
	if (n.Commands.Mode != "read" && n.Commands.Mode != "mutation") || b == nil || !config.WorkerEnabled(b.Enabled) || !config.WorkerEnabled(b.Notifications.Enabled) || b.Notifications.Worker == "" {
		return fmt.Errorf("consumer_setup_required: initial commands require read or mutation mode and a declared local shared backup worker")
	}
	p, ok := n.Providers["telegram"]
	if !ok || len(n.Providers) != 1 || p.Conversation != "" || p.Account == "" {
		return fmt.Errorf("conversation_setup_required: initial commands require Telegram account/bot_name and a separate fixed alert destination")
	}
	if len(n.Commands.Workers) != 1 || len(n.Commands.Conversations) == 0 || requirePermissions && len(n.Commands.Permissions) == 0 {
		return fmt.Errorf("declare one backup command alias, conversations and explicit permissions")
	}
	for _, producer := range n.Commands.Workers {
		if producer != "backup-worker" {
			return fmt.Errorf("initial commands require the local backup-worker producer")
		}
	}
	for _, conversation := range n.Commands.Conversations {
		if conversation.Provider != "telegram" || conversation.Account != p.Account {
			return fmt.Errorf("initial command conversations must use the declared Telegram account")
		}
	}
	for _, permission := range n.Commands.Permissions {
		for _, right := range permission.Permissions {
			if right != "read" && !(n.Commands.Mode == "mutation" && right == "incident.manage") {
				return fmt.Errorf("initial commands support read and, in mutation mode, incident.manage permissions only")
			}
		}
	}
	return nil
}

// A notification-first bootstrap retains desired command policy in the outer
// declaration, but creates neither command aliases nor intake before reservation.
func unboundCommandDeclaration(d config.DesiredWorkers) config.DesiredWorkers {
	if d.Notification != nil && d.Notification.Commands.Enabled {
		n := *d.Notification
		d.Notification = &n
		n.Commands.Enabled = false
	}
	return d
}

func compileReservedNotification(d config.DesiredWorkers, f TargetFacts, paths ResolvedPaths, input install.RuntimeInput, reservation backup.ProducerReservation) (CompiledNotification, error) {
	b := NotificationBindings{Providers: input.Providers, LEDSocket: input.LEDSocket, LEDEndpoint: input.LEDEndpoint, Producers: map[string]ProducerBinding{"backup-worker": {Producer: reservation.Producer, Generation: reservation.Generation}}}
	if d.Notification.Commands.Enabled {
		if err := initialBackupCommandSupport(d); err != nil {
			return CompiledNotification{}, err
		}
		producer := b.Producers["backup-worker"]
		producer.Commands = backup.BackupCommandDescriptors()
		if d.Notification.Commands.Mode == "mutation" {
			producer.Commands = backup.BackupIncidentCommandDescriptors()
		}
		b.Producers["backup-worker"] = producer
		b.Conversations = map[string]interaction.Destination{}
		for name, declared := range d.Notification.Commands.Conversations {
			// This generation labels a retained destination declaration. Producer
			// and gateway identities always come from their domain reservations.
			generation := protocol.Hash(struct {
				Name        string
				Destination config.ConversationDeclaration
			}{name, declared})
			b.Conversations[name] = interaction.Destination{ID: name, Generation: generation, Provider: declared.Provider, Account: declared.Account, NativeID: declared.NativeID, Thread: declared.Thread}
		}
		p := d.Notification.Providers["telegram"]
		b.Telegram = &provider.TelegramAccount{Account: p.Account, BotName: p.BotName, TokenFile: input.Providers["telegram"].TokenFile}
	}
	return CompileNotification(d, f, paths, b)
}
