package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
)

// ImportReview contains only public configuration and references. It is a
// candidate for operator review, never an installation or adoption receipt.
type ImportReview struct {
	Policy   config.DesiredWorkers `json:"policy"`
	Required []string              `json:"required_actions"`
}

func importLocator(inv config.ServerInventory, kind, alias string) config.ServerInventory {
	s, ok := inv.Servers[alias]
	if !ok {
		return inv
	}
	d := config.DesiredWorkers{}
	if s.Desired != nil {
		d = *s.Desired
	}
	if kind == "notification-worker" && d.Notification == nil {
		d.Notification = &config.NotificationDeclaration{}
	}
	if kind == "backup-worker" && d.Backup == nil {
		d.Backup = &config.BackupDeclaration{}
	}
	s.Desired = &d
	inv.Servers = maps.Clone(inv.Servers)
	inv.Servers[alias] = s
	return inv
}

func notificationImportPolicy(v install.PolicyReview, selected config.WorkerArtifacts, paths ResolvedPaths) (ImportReview, error) {
	var result ImportReview
	m, input := v.Manifest, v.Input
	n := &config.NotificationDeclaration{
		Providers: config.ProviderDeclarations{}, Routes: map[string]config.NotificationRoute{}, Producers: map[string]config.ProducerDeclaration{},
		Limits:    config.NotificationLimits{MaxMessages: input.Worker.MaxMessages, MaxPayloadBytes: input.Worker.MaxPayloadBytes, MaxDatabaseBytes: input.Worker.MaxDatabaseBytes, MinFreeBytes: input.Worker.MinFreeBytes, ReceiptAuditDays: input.Worker.ReceiptAuditDays},
		Paths:     config.WorkerPaths{Config: paths.Config, Data: paths.Data, Runtime: paths.Runtime, State: m.Paths.State, Installation: m.Root, UnitDirectory: m.UnitDirectory, DeniedPaths: slices.Clone(m.Paths.DeniedPaths)},
		Artifacts: selected,
	}
	for name, p := range input.Providers {
		if name != p.Kind {
			return result, fmt.Errorf("provider aliases require explicit profile import review")
		}
		switch name {
		case "telegram":
			declared := config.ProviderDeclaration{TokenFile: p.TokenFile, ChatIDFile: p.ChatFile}
			if input.Telegram != nil {
				declared.Account, declared.BotName = input.Telegram.Account, input.Telegram.BotName
			}
			if p.Conversation != nil {
				declared.Conversation = p.Conversation.ID
			}
			n.Providers[name] = declared
		case "signal":
			if v.Signal == nil {
				return result, fmt.Errorf("existing Signal account requires its adoption/handoff review")
			}
			s := v.Signal
			if s.Inbox.State != filepath.Join(filepath.Dir(m.Paths.State), "signal-inbox") {
				return result, fmt.Errorf("installed Signal inbox path cannot be represented by the short workflow; review native account migration")
			}
			socket := s.Socket
			if socket == "" {
				socket = s.Root + "/api.sock"
			}
			n.Providers[name] = config.ProviderDeclaration{AccountFile: s.AccountFile, RecipientFile: s.RecipientFile, Data: s.AccountState, Socket: socket, Executable: s.CLIFile, ExecutableSHA256: s.CLISHA256}
		default:
			return result, fmt.Errorf("installed provider is not representable in worker YAML")
		}
	}
	for name, route := range input.Worker.Routes {
		n.Routes[name] = config.NotificationRoute{Primary: route.Primary, Fallback: route.Fallback}
	}
	for _, p := range input.Worker.Producers {
		required := p.Required
		n.Producers[p.Producer.ID] = config.ProducerDeclaration{Required: &required, Routes: slices.Clone(p.Routes), MaxSilenceSeconds: p.MaxSilenceSeconds, MaxEvidenceSeconds: p.MaxEvidenceSeconds, MaxMessages: p.MaxMessages, PolicyHealth: p.PolicyHealth, InteractiveContext: p.InteractiveContext, ContextKinds: slices.Clone(p.ContextKinds)}
	}
	if input.Worker.Interaction != nil {
		command, err := importCommandPolicy(input)
		if err != nil {
			return result, err
		}
		n.Commands = command
		result.Required = append(result.Required, "review command manifests and retained admission separately; the candidate grants no new permissions")
	}
	if input.LEDSocket != "" {
		n.StatusLEDs = config.LEDDeclaration{Enabled: m.LEDOutput == nil || !m.LEDOutput.Disabled, Socket: input.LEDSocket, Daemon: config.LEDDaemon{Mode: "existing"}, Bindings: map[string]config.LEDBindingDeclaration{}}
		for _, binding := range input.Worker.LEDBindings {
			styles := map[string]config.LEDStyleDeclaration{}
			for name, style := range binding.Styles {
				styles[name] = config.LEDStyleDeclaration{Pattern: style.Pattern, Priority: style.Priority}
			}
			device := binding.LED
			if device == "red" {
				device = "power"
			}
			n.StatusLEDs.Bindings[binding.ID] = config.LEDBindingDeclaration{Producer: binding.Producer.ID, Subjects: slices.Clone(binding.Subjects), LED: device, Styles: styles}
			n.StatusLEDs.PowerLED = n.StatusLEDs.PowerLED || binding.AllowPower
		}
		result.Required = append(result.Required, "review existing LED daemon ownership; this candidate does not adopt a host service")
	}
	result.Policy.Notification = n
	result.Required = append(result.Required, "review the selected operator-local artifacts against the installed release", "review producer/account ownership before coordinator adoption; saving this candidate does not authorize takeover")
	return result, nil
}

func importNotificationControl(ctx context.Context, r ControlRequest, paths ResolvedPaths, inspect func(context.Context, string) (install.PolicyReview, error)) (ControlResult, error) {
	v, err := inspect(ctx, paths.Installation)
	if err != nil {
		return ControlResult{}, err
	}
	review, err := notificationImportPolicy(v, r.Desired.Notification.Artifacts, paths)
	if err != nil {
		return ControlResult{}, err
	}
	return ControlResult{Version: 1, State: "candidate", Code: "review_required", Import: &review, Next: "review the YAML candidate; identities and runtime state are unchanged; coordinator takeover remains a separate migration"}, nil
}

func importBackupControl(ctx context.Context, r ControlRequest) (ControlResult, error) {
	v, err := backup.InspectSetupPolicyReview(ctx, r.Facts.Home)
	if err != nil {
		return ControlResult{}, err
	}
	b := backupImportPolicy(v, r.Desired.Backup.Artifacts)
	review := ImportReview{Policy: config.DesiredWorkers{Backup: &b}, Required: []string{"review artifact selection and source aliases; no source enrollment or archive ownership was changed"}}
	if v.Desired == nil {
		review.Required = append(review.Required, "legacy standalone setup requires explicit backup-worker adopt after review; shared/Signal ownership requires its separate handoff")
	}
	return ControlResult{Version: 1, State: "candidate", Code: "review_required", Import: &review, Next: "review and save the candidate, then plan against the retained installation before applying"}, nil
}

func backupImportPolicy(v backup.SetupPolicyReview, selected config.WorkerArtifacts) config.BackupDeclaration {
	c, paths := v.Installation, v.Paths
	p := c.Policy
	notifications := p.Notifications.Mode != "disabled"
	storageMode := p.Storage.Mode
	if storageMode == "" {
		storageMode = "mount"
	} // Version-2 mount semantics remain explicit.
	b := config.BackupDeclaration{Sources: slices.Clone(p.Sources), Storage: config.DesiredStorage{Mode: storageMode, Path: p.Storage.Path, RequireMount: p.Storage.RequireMount, MinFreeBytes: p.Storage.MinFreeBytes}, Schedule: p.Schedule, Timezone: p.Timezone, Freshness: p.Freshness, MaxRunTime: p.MaxRunTime, MaxSnapshotBytes: p.MaxSnapshotBytes, Timeouts: p.Timeouts, Retention: p.Retention, Notifications: config.DesiredBackupNotifications{Enabled: &notifications, WorkerNotifications: p.Notifications}, Monitoring: p.Monitoring, RestoreChecks: slices.Clone(p.RestoreChecks)}
	if v.Desired != nil && v.Desired.Backup != nil {
		b = *v.Desired.Backup
	}
	b.Paths = config.WorkerPaths{Config: paths.Config, State: paths.State, Data: paths.Data, Runtime: paths.Runtime, Installation: paths.Installation, UnitDirectory: paths.UnitDirectory, DeniedPaths: slices.Clone(paths.DeniedPaths)}
	b.Artifacts = selected
	b.Artifacts.Image, b.Artifacts.RelayImage = c.Image, c.RelayImage
	return b
}

func (c CLI) importWorker(ctx context.Context, kind string, v Invocation, inv config.ServerInventory, facts TargetFacts, p Plan) error {
	if p.Artifacts == nil {
		return fmt.Errorf("helper bundle required for owned installation review")
	}
	e, err := OpenInventoryEdit(v.Options.Inventory)
	if err != nil {
		return err
	}
	if c.Controller == nil {
		c.Controller = SSHController{}
	}
	desired := *inv.Servers[v.Server].Desired
	if kind == "backup-worker" {
		desired = backupDesired(desired)
	}
	r := ControlRequest{Version: 1, Worker: kind, Server: v.Server, Action: "import", Facts: facts, Desired: desired, DesiredHash: p.DesiredHash, DesiredScope: p.DesiredScope}
	result, err := c.Controller.Apply(ctx, inv.Servers[v.Server], p.Artifacts.Helper, r)
	if err != nil {
		return err
	}
	if result.Version != 1 || result.Import == nil || result.State != "candidate" {
		return fmt.Errorf("invalid policy review response")
	}
	policy := result.Import.Policy
	if kind == "notification-worker" {
		if policy.Notification == nil || policy.Backup != nil {
			return fmt.Errorf("notification policy review changed worker kind")
		}
		err = e.Set(v.Server, []string{"notification_worker"}, policy.Notification)
	} else {
		if policy.Backup == nil || policy.Notification != nil {
			return fmt.Errorf("backup policy review changed worker kind")
		}
		err = e.Set(v.Server, []string{"backup_worker"}, policy.Backup)
	}
	if err != nil {
		return err
	}
	candidate, err := e.Candidate()
	if err != nil {
		return fmt.Errorf("installed policy cannot be represented by this inventory: %w", err)
	}
	current, err := OpenInventoryEdit(v.Options.Inventory)
	if err != nil {
		return err
	}
	if !bytes.Equal(current.Original, e.Original) {
		return fmt.Errorf("inventory changed during import review; repeat the review")
	}
	if v.Options.JSON {
		return json.NewEncoder(c.Out).Encode(struct {
			Version   int      `json:"version"`
			State     string   `json:"state"`
			Code      string   `json:"code"`
			Candidate string   `json:"candidate"`
			Required  []string `json:"required_actions"`
			Next      string   `json:"next"`
		}{1, result.State, result.Code, string(candidate), result.Import.Required, result.Next})
	}
	for _, action := range result.Import.Required {
		if _, err = fmt.Fprintln(c.Err, action); err != nil {
			return err
		}
	}
	if _, err = fmt.Fprintln(c.Err, result.Next); err != nil {
		return err
	}
	_, err = c.Out.Write(candidate)
	return err
}
