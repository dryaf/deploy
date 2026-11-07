package workerctl

import (
	"context"
	"fmt"
	"reflect"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func reconcileNotificationCommandDraft(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationLimitsLifecycle) (ControlResult, bool, error) {
	return reconcileNotificationPolicy(ctx, r, paths, helper, l, "command-draft")
}

func validateCommandDraftDeclaration(before, after config.DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || !reflect.DeepEqual(before.Backup, after.Backup) {
		return fmt.Errorf("edit commands separately from backup settings")
	}
	if err := initialBackupCommandSupport(after); err != nil {
		return err
	}
	next := *after.Notification
	next.Commands = before.Notification.Commands
	next.Providers = make(config.ProviderDeclarations, len(after.Notification.Providers))
	for name, p := range after.Notification.Providers {
		next.Providers[name] = p
	}
	old, ok := before.Notification.Providers["telegram"]
	p, present := next.Providers["telegram"]
	if !ok || !present || len(next.Providers) != 1 || len(before.Notification.Providers) != 1 {
		return fmt.Errorf("command drafts require the existing Telegram provider")
	}
	if old.Account != "" && old.Account != p.Account || old.BotName != "" && old.BotName != p.BotName {
		return fmt.Errorf("retain the existing Telegram bot")
	}
	p.Account, p.BotName = old.Account, old.BotName
	next.Providers["telegram"] = p
	if !reflect.DeepEqual(next, *before.Notification) {
		return fmt.Errorf("edit commands separately from credentials and other notification settings")
	}
	return nil
}

func preflightCommandDraft(r ControlRequest, paths ResolvedPaths, m install.ManagedManifest) error {
	raw, err := readLimitsRuntime(paths.Runtime)
	if err != nil {
		return err
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		return err
	}
	runtime, err := input.Resolve()
	if err != nil {
		return err
	}
	if !runtime.Bootstrap || runtime.Worker.Interaction != nil || runtime.Telegram != nil || runtime.Signal != nil || m.Commands != nil || m.CommandHistoryHash != "" || protocol.Hash(runtime) != m.ConfigHash || inputDigest(raw) != m.RuntimeHash {
		return fmt.Errorf("command draft requires an unchanged command-free bootstrap")
	}
	next, err := compileLimitsRuntime(r, paths, input)
	if err != nil {
		return err
	}
	if string(next) != string(raw) {
		return fmt.Errorf("command draft would change the installed runtime")
	}
	if r.Desired.Notification.Commands.Enabled {
		p := r.Desired.Notification.Providers["telegram"]
		return provider.CheckTelegramAccount(provider.TelegramAccount{Account: p.Account, BotName: p.BotName, TokenFile: input.Providers["telegram"].TokenFile})
	}
	return nil
}
