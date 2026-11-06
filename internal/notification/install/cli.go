// Package install exposes local staging/diagnostics. Production activation is
// deliberately unavailable until migration and qualification are implemented.
package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

type Runtime struct {
	Version   int                         `json:"version"`
	Worker    worker.Config               `json:"worker"`
	Providers map[string]provider.Profile `json:"providers"`
	LEDSocket string                      `json:"led_socket,omitempty"`
	Telegram  *provider.TelegramAccount   `json:"interactive_telegram,omitempty"`
	Signal    *provider.SignalAccount     `json:"interactive_signal,omitempty"`
}

func ReadRuntime(path string) (c Runtime, err error) {
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	defer d.Close()
	data, err := safefs.ReadFile(d, filepath.Base(path), safefs.MaxRecordBytes)
	if err != nil {
		return c, err
	}
	return decodeRuntime(data)
}

func decodeRuntime(data []byte) (c Runtime, err error) {
	if err = safefs.DecodeJSON(data, &c, safefs.MaxRecordBytes); err != nil {
		return c, err
	}
	if c.Version != 1 {
		return c, fmt.Errorf("unsupported runtime version")
	}
	if err = c.Worker.Validate(); err != nil {
		return c, err
	}
	if c.Telegram != nil {
		if c.Worker.Interaction == nil || !filepath.IsAbs(c.Telegram.TokenFile) || !protocol.ValidToken(c.Telegram.Account) || !protocol.ValidToken(c.Telegram.BotName) {
			return c, fmt.Errorf("invalid interactive Telegram account")
		}
		found := false
		for _, d := range c.Worker.Interaction.Destinations {
			if d.Provider == "telegram" {
				if d.Account != c.Telegram.Account {
					return c, fmt.Errorf("interactive Telegram account mismatch")
				}
				found = true
			}
		}
		if !found {
			return c, fmt.Errorf("interactive Telegram has no enrolled conversations")
		}
		for _, w := range c.Worker.Interaction.Workers {
			for _, d := range w.Commands {
				if d.Effect != "read" {
					return c, fmt.Errorf("staged Telegram intake permits read commands only; mutation activation requires qualification")
				}
			}
		}
	}
	if c.Signal != nil {
		if c.Worker.Interaction == nil || c.Signal.Validate() != nil {
			return c, fmt.Errorf("invalid interactive Signal profile")
		}
		found := false
		for _, d := range c.Worker.Interaction.Destinations {
			if d.Provider == "signal" {
				if d.Account != c.Signal.Account {
					return c, fmt.Errorf("interactive Signal account mismatch")
				}
				found = true
			}
		}
		if !found {
			return c, fmt.Errorf("interactive Signal has no conversations")
		}
		for _, w := range c.Worker.Interaction.Workers {
			for _, d := range w.Commands {
				if d.Effect != "read" {
					return c, fmt.Errorf("staged Signal intake permits read commands only; mutation activation requires qualification")
				}
			}
		}
	}
	seen := map[string]bool{}
	for name, p := range c.Providers {
		if !protocol.ValidToken(name) || !safefs.DigestPattern.MatchString(p.Destination) || seen[p.Kind] {
			return c, fmt.Errorf("invalid or duplicate provider profile")
		}
		seen[p.Kind] = true
		if p.Conversation != nil {
			if c.Worker.Interaction == nil || p.Conversation.Provider != p.Kind || p.Destination != p.Conversation.Fingerprint() {
				return c, fmt.Errorf("interactive alert destination mismatch")
			}
			found := false
			for _, d := range c.Worker.Interaction.Destinations {
				if d == *p.Conversation {
					found = true
				}
			}
			if !found {
				return c, fmt.Errorf("interactive alert conversation not enrolled")
			}
			switch p.Kind {
			case "telegram":
				if !filepath.IsAbs(p.TokenFile) || p.ChatFile != "" || p.Socket != "" {
					return c, fmt.Errorf("invalid interactive Telegram profile")
				}
			case "signal":
				if !filepath.IsAbs(p.Socket) || !p.RelayIdentity.Valid() || p.TokenFile != "" || p.ChatFile != "" {
					return c, fmt.Errorf("invalid interactive Signal profile")
				}
			default:
				return c, fmt.Errorf("unknown interactive provider")
			}
			continue
		}
		switch p.Kind {
		case "telegram":
			if !filepath.IsAbs(p.TokenFile) || !filepath.IsAbs(p.ChatFile) || p.Socket != "" {
				return c, fmt.Errorf("invalid Telegram paths")
			}
		case "signal":
			if !filepath.IsAbs(p.Socket) || p.TokenFile != "" || p.ChatFile != "" {
				return c, fmt.Errorf("invalid Signal profile")
			}
		default:
			return c, fmt.Errorf("unknown provider kind")
		}
	}
	for name, route := range c.Worker.Routes {
		if _, ok := c.Providers[route.Primary]; !ok {
			return c, fmt.Errorf("missing primary profile")
		}
		if route.Fallback != "" {
			if _, ok := c.Providers[route.Fallback]; !ok {
				return c, fmt.Errorf("missing fallback profile")
			}
		}
		// Bind queued jobs to resolved destination identities, even if an operator
		// edits a profile without remembering to change the human route revision.
		route.Revision = protocol.Hash(struct{ Label, Primary, Fallback string }{route.Revision, c.Providers[route.Primary].Destination, c.Providers[route.Fallback].Destination})
		for _, profile := range []provider.Profile{c.Providers[route.Primary], c.Providers[route.Fallback]} {
			if profile.Conversation != nil {
				route.Revision = protocol.Hash(struct {
					Route       string
					Destination interaction.Destination
					Relay       protocol.Identity
				}{route.Revision, *profile.Conversation, profile.RelayIdentity})
			}
		}
		c.Worker.Routes[name] = route
	}
	if c.LEDSocket != "" && !filepath.IsAbs(c.LEDSocket) {
		return c, fmt.Errorf("LED socket must be absolute")
	}
	if len(c.Worker.LEDBindings) > 0 && c.LEDSocket == "" {
		return c, fmt.Errorf("LED bindings require an explicit LED socket")
	}
	return c, nil
}

func RunCLI(args []string) error {
	if len(args) > 0 && args[0] == "__managed-legacy-probe" {
		return runManagedCLI(args)
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: notification-worker <init|serve|status|setup|update|migrate-schema|migrate-led|adopt-recovery|adopt-signal|supervise-signal|migrate-signal-profiles|signal-service-setup|signal-service-resume|signal-service-inspect|signal-service-uninstall|upgrade-helper|renew-telegram|rollback|resume|uninstall|installation-status|qualification-template|qualify|qualification-status|activation-review|activate-delivery|deactivate-delivery|queue-inspect|producer-retire|receipts-inspect|receipts-compact|message-review|message-decide|message-decision-status|snapshot|snapshot-verify|recovery-prepare|recovery-status|recovery-checkpoint|recovery-checkpoint-verify|recovery-publication-prepare|recovery-publication-verify|recovery-publish|recovery-signal-install|recovery-signal-handoff|recovery-signal-podman-handoff|signal-verification-prepare|signal-verification-serve|recovery-telegram-check|led-migrate|recovery-led-review|recovery-led-preserve|recovery-led-adopt|recovery-led-handoff|recovery-led-import-cleanup|recovery-fence-producer|recovery-review|recovery-decide|notify-test|signal-inbox-init|signal-link-status|signal-service-status|signal-recovery-status|signal-link-resolve|__signal-relay> --local [options]; Signal/recovery/command activation is not available")
	}
	switch args[0] {
	case "activate-delivery", "deactivate-delivery":
		return runDeliveryCLI(args, os.Stdout)
	case "activation-review":
		return runActivationReviewCLI(args[1:], os.Stdout)
	case "signal-service-setup", "signal-service-resume", "signal-service-uninstall", "signal-service-inspect", "__signal-service-serve", "__signal-verification-service-probe":
		return runSignalServiceCLI(args, os.Stdout)
	case "signal-verification-prepare":
		return runSignalVerificationPrepareCLI(args[1:], os.Stdout)
	case "signal-verification-serve":
		return runSignalVerificationServeCLI(args[1:], os.Stdout)
	case "recovery-telegram-check":
		return runRecoveryTelegramCheckCLI(args[1:], os.Stdout)
	case "recovery-signal-install":
		return runRecoverySignalInstallCLI(args[1:], os.Stdout)
	case "recovery-signal-handoff":
		return runRecoverySignalNativeCLI(args[1:], os.Stdout)
	case "recovery-signal-podman-handoff":
		return runRecoverySignalPodmanCLI(args[1:], os.Stdout)
	case "__signal-podman-proof":
		return runSignalPodmanProofCLI(args[1:], os.Stdout)
	case "__signal-podman-recovery-probe":
		return runSignalPodmanCapabilityCLI(args[1:], os.Stdout)
	case "recovery-publish":
		return runRecoveryPublishCLI(args[1:], os.Stdout)
	case "recovery-publication-verify":
		return runRecoveryPublicationVerifyCLI(args[1:], os.Stdout)
	case "recovery-publication-prepare":
		return runRecoveryPublicationCLI(args[1:], os.Stdout)
	case "recovery-checkpoint-verify":
		return runRecoveryCheckpointVerifyCLI(args[1:], os.Stdout)
	case "recovery-checkpoint":
		return runRecoveryCheckpointCLI(args[1:], os.Stdout)
	case "recovery-status":
		return runRecoveryStatusCLI(args[1:], os.Stdout)
	case "recovery-prepare":
		return runRecoveryQuarantineCLI(args[1:], os.Stdout)
	case "recovery-led-import-cleanup":
		return runRecoveryLEDImportCLI(args, os.Stdout)
	case "recovery-led-handoff":
		return runRecoveryLEDEpochCLI(args, os.Stdout)
	case "producer-retire":
		return runProducerRetirementCLI(args, os.Stdout)
	case "__signal-recovery-probe":
		if len(args) != 1 {
			return fmt.Errorf("Signal recovery probe takes no arguments")
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Version int `json:"version"`
		}{signalaccount.RecoveryVersion})
	case "__signal-account-probe":
		if len(args) != 1 {
			return fmt.Errorf("Signal account probe takes no arguments")
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Version int `json:"version"`
		}{signalaccount.MaintenanceVersion})
	case "__signal-link", "signal-link-status", "signal-service-status", "signal-recovery-status", "signal-link-resolve":
		return runSignalLinkCLI(args, os.Stdout)
	case "qualification-template", "qualify", "qualification-status":
		return runQualificationCLI(args, os.Stdout)
	case "receipts-compact":
		return runReceiptCompactionCLI(args, os.Stdout)
	case "message-review", "message-decide", "message-decision-status":
		return runMessageAdministrationCLI(args, os.Stdout)
	case "queue-inspect", "receipts-inspect":
		return runInspectionCLI(args, os.Stdout)
	case "__managed-delivery-probe", "__managed-signal-profiles-probe", "migrate-signal-profiles", "__managed-signal-supervision-probe", "supervise-signal", "adopt-signal", "__managed-signal-verification-probe", "setup", "update", "migrate-schema", "migrate-led", "adopt-recovery", "upgrade-helper", "__managed-helper-upgrade-probe", "renew-telegram", "__managed-telegram-renewal-probe", "rollback", "resume", "uninstall", "installation-status", "__managed-probe", "__managed-migration-probe", "__managed-led-migration-probe", "__managed-recovery-probe", "__managed-serve":
		return runManagedCLI(args)
	}
	if args[0] == "recovery-led-review" || args[0] == "recovery-led-preserve" || args[0] == "recovery-led-adopt" {
		return runRecoveryLEDCLI(args, os.Stdout)
	}
	if args[0] == "led-migrate" {
		return runLEDMigrationCLI(args, os.Stdout)
	}
	if args[0] == "recovery-review" || args[0] == "recovery-decide" || args[0] == "recovery-fence-producer" {
		return runRecoveryCLI(args, os.Stdout)
	}
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Use an explicit local disposable installation")
	configuration := f.String("config", "", "Private runtime JSON path")
	state := f.String("state-dir", "", "Private state directory")
	socket := f.String("socket", "", "Private API socket path")
	verification := f.Bool("verification", false, "Accept durable submissions without delivery, command grants or healthy LED output")
	channel := f.String("channel", "", "Explicit provider profile for a diagnostic send")
	to := f.String("to", "", "New private snapshot directory")
	from := f.String("from", "", "Completed private snapshot directory")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 {
		return fmt.Errorf("local staging commands require --local and explicit paths")
	}
	allowed := map[string]map[string]bool{
		"init":              {"local": true, "config": true, "state-dir": true},
		"serve":             {"local": true, "config": true, "state-dir": true, "socket": true, "verification": true},
		"status":            {"local": true, "state-dir": true, "socket": true},
		"snapshot":          {"local": true, "config": true, "state-dir": true, "to": true},
		"snapshot-verify":   {"local": true, "config": true, "from": true},
		"notify-test":       {"local": true, "config": true, "channel": true},
		"__signal-relay":    {"local": true, "config": true},
		"signal-inbox-init": {"local": true, "config": true},
	}
	flags, ok := allowed[args[0]]
	if !ok {
		return fmt.Errorf("operation unavailable: production installation, activation and migration are not implemented")
	}
	var invalid bool
	f.Visit(func(v *flag.Flag) {
		if !flags[v.Name] {
			invalid = true
		}
	})
	if invalid {
		return fmt.Errorf("option not valid for operation")
	}
	for name, value := range map[string]string{"config": *configuration, "state-dir": *state, "socket": *socket, "to": *to, "from": *from} {
		if flags[name] && !filepath.IsAbs(value) {
			return fmt.Errorf("%s requires an absolute path", name)
		}
	}
	if ui.DryRun {
		if args[0] == "__signal-relay" || args[0] == "signal-inbox-init" {
			c, err := readRelayConfiguration(*configuration)
			if err != nil {
				return err
			}
			if args[0] == "signal-inbox-init" && c.Interactive == nil {
				return fmt.Errorf("interactive relay configuration required")
			}
		}
		if args[0] != "status" && args[0] != "__signal-relay" && args[0] != "signal-inbox-init" {
			c, err := ReadRuntime(*configuration)
			if err != nil {
				return err
			}
			if args[0] == "snapshot-verify" {
				if _, err = worker.VerifySnapshot(context.Background(), *from, c.Worker); err != nil {
					return err
				}
			}
		}
		fmt.Printf("[DRY] notification-worker %s: local staging/diagnostics; no files, messages or LEDs changed\n", args[0])
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if args[0] == "__signal-relay" || args[0] == "signal-inbox-init" {
		c, err := readRelayConfiguration(*configuration)
		if err != nil {
			return err
		}
		if args[0] == "signal-inbox-init" {
			if c.Interactive == nil {
				return fmt.Errorf("interactive relay configuration required")
			}
			if err = c.Interactive.Validate(); err != nil {
				return err
			}
			parent, err := safefs.Open(filepath.Dir(c.Interactive.InboxDir))
			if err != nil {
				return err
			}
			defer parent.Close()
			if err = os.Mkdir(c.Interactive.InboxDir, 0700); err != nil {
				return err
			}
			if err = parent.File().Sync(); err != nil {
				return err
			}
			inbox, err := relay.OpenInbox(c.Interactive.InboxDir, c.Interactive.Account, true)
			if err != nil {
				return err
			}
			defer inbox.Close()
			return json.NewEncoder(os.Stdout).Encode(inbox.Cursor())
		}
		return relay.Run(ctx, c)
	}
	if args[0] == "status" {
		d, err := safefs.Open(*state)
		if err != nil {
			return err
		}
		defer d.Close()
		var id protocol.Identity
		if err = d.ReadJSON("identity.json", &id); err != nil {
			return err
		}
		var h worker.Health
		_, err = (client.Client{Socket: *socket, Identity: id}).Call(ctx, protocol.Request{Operation: "system.status"}, &h)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(h)
	}
	c, err := ReadRuntime(*configuration)
	if err != nil {
		return err
	}
	if args[0] == "snapshot-verify" {
		manifest, err := worker.VerifySnapshot(ctx, *from, c.Worker)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(manifest)
	}
	if args[0] == "notify-test" {
		if _, ok := c.Providers[*channel]; !ok {
			return fmt.Errorf("select an enrolled provider with --channel")
		}
		id, err := safefs.NewID("n")
		if err != nil {
			return err
		}
		result := (provider.Adapter{Profiles: c.Providers}).Send(ctx, *channel, protocol.Message{ID: id, Producer: protocol.Producer{ID: "diagnostic", Epoch: id}, Text: "Explicit notification-worker diagnostic. This does not qualify backup delivery."})
		if !result.Accepted {
			return fmt.Errorf("diagnostic was not confirmed accepted (ambiguous=%t)", result.Ambiguous)
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Event     string `json:"event"`
			Accepted  bool   `json:"provider_accepted"`
			Qualified bool   `json:"qualified"`
		}{id, true, false})
	}
	if args[0] == "init" {
		parent, err := safefs.Open(filepath.Dir(*state))
		if err != nil {
			return err
		}
		defer parent.Close()
		if err = os.Mkdir(*state, 0700); err != nil {
			return err
		}
		if err = parent.File().Sync(); err != nil {
			return err
		}
		s, err := worker.Open(*state, c.Worker, true)
		if err != nil {
			return err
		}
		defer s.Close()
		for _, p := range c.Worker.Producers {
			if err = s.Stage(p.Producer, "staged-initial"); err != nil {
				return err
			}
		}
		return json.NewEncoder(os.Stdout).Encode(s.Identity())
	}
	open := func() (*worker.Store, error) {
		if *verification {
			return worker.OpenVerification(*state, c.Worker)
		}
		return worker.Open(*state, c.Worker, false)
	}
	s, err := open()
	if err != nil {
		return err
	}
	defer s.Close()
	if args[0] == "snapshot" {
		manifest, err := s.Snapshot(ctx, *to)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(manifest)
	}
	return serveRuntime(ctx, s, c, *socket)
}
