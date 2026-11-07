package workerctl

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
)

// ProducerBinding comes from a specialist's durable reservation and validated
// command manifest, never from YAML UUIDs or user-authored executable descriptors.
type ProducerBinding struct {
	Producer   protocol.Producer
	Generation string
	Commands   map[string]interaction.Descriptor
}

// NotificationBindings are canonical facts supplied by lifecycle adapters after
// resolving credentials, account ownership, conversations and specialist epochs.
type NotificationBindings struct {
	LEDEndpoint   *led.Endpoint
	Providers     map[string]provider.Profile
	Producers     map[string]ProducerBinding
	Conversations map[string]interaction.Destination
	Telegram      *provider.TelegramAccount
	Signal        *provider.SignalAccount
	LEDSocket     string
}
type CompiledNotification struct {
	Input       install.RuntimeInput
	CommandMode string
}

// CompileNotification compiles all gateway runtime policy without allocating
// identities or accessing files/providers. Lifecycle-only fields (artifact pins,
// installation paths, linking and daemon ownership) remain adapter inputs.
func CompileNotification(d config.DesiredWorkers, f TargetFacts, paths ResolvedPaths, b NotificationBindings) (CompiledNotification, error) {
	var result CompiledNotification
	n := d.Notification
	if n == nil || !config.WorkerEnabled(n.Enabled) {
		return result, fmt.Errorf("notification runtime requires an enabled declaration")
	}
	input := install.RuntimeInput{Version: 1, Providers: map[string]provider.Profile{}, Worker: worker.Config{Routes: map[string]worker.Route{}, MaxMessages: n.Limits.MaxMessages, MaxPayloadBytes: n.Limits.MaxPayloadBytes, MaxDatabaseBytes: n.Limits.MaxDatabaseBytes, MinFreeBytes: n.Limits.MinFreeBytes, ReceiptAuditDays: n.Limits.ReceiptAuditDays}}
	if len(n.Providers) != len(b.Providers) {
		return result, fmt.Errorf("provider enrollment differs from declaration")
	}
	for name, p := range n.Providers {
		profile, ok := b.Providers[name]
		if !ok || profile.Kind != name {
			return result, fmt.Errorf("provider identity unavailable: %s", name)
		}
		if name == "telegram" {
			token, chat, err := telegramTargetPaths(p, paths, f.Home)
			if err != nil {
				return result, err
			}
			if profile.TokenFile != token {
				return result, fmt.Errorf("Telegram token path differs from declaration")
			}
			if p.Conversation == "" && profile.ChatFile != chat {
				return result, fmt.Errorf("Telegram destination path differs from declaration")
			}
		} else if name == "signal" && p.Socket != "" {
			socket, err := ResolveTargetPath(p.Socket, f.Home)
			if err != nil {
				return result, err
			}
			if socket != profile.Socket {
				return result, fmt.Errorf("Signal socket differs from declaration")
			}
		}
		if p.Conversation != "" {
			conversation, ok := b.Conversations[p.Conversation]
			if !n.Commands.Enabled || !ok || profile.Conversation == nil || *profile.Conversation != conversation {
				return result, fmt.Errorf("alert conversation lacks matching command enrollment")
			}
		} else if profile.Conversation != nil {
			return result, fmt.Errorf("undeclared alert conversation")
		}
		input.Providers[name] = profile
	}
	for name, r := range n.Routes {
		input.Worker.Routes[name] = worker.Route{Revision: "declaration-v3", Primary: r.Primary, Fallback: r.Fallback}
	}
	policies := maps.Clone(n.Producers)
	if policies == nil {
		policies = map[string]config.ProducerDeclaration{}
	}
	if backup := d.Backup; backup != nil && config.WorkerEnabled(backup.Enabled) && config.WorkerEnabled(backup.Notifications.Enabled) && backup.Notifications.Worker != "" {
		if _, ok := policies["backup-worker"]; !ok {
			freshness, err := time.ParseDuration(backup.Freshness)
			if err != nil {
				return result, fmt.Errorf("backup freshness unavailable")
			}
			policies["backup-worker"] = config.ProducerDeclaration{Routes: []string{backup.Notifications.Route}, MaxSilenceSeconds: 120, MaxEvidenceSeconds: int(freshness / time.Second), MaxMessages: min(1000, n.Limits.MaxMessages), PolicyHealth: true}
		}
	}
	// A reserved producer may be absent during notification-first bootstrap. It
	// stays unbound only when NONE is reserved; a partial producer set is an error.
	if len(b.Producers) == 0 {
		if len(n.Producers) > 0 && !deferredBackupProducer(d) || n.Commands.Enabled || len(n.StatusLEDs.Bindings) > 0 && !deferredBackupLEDBindings(d) {
			return result, fmt.Errorf("declared producer, command or LED policy requires real consumer reservations")
		}
		input.Bootstrap = true
	} else {
		if len(b.Producers) != len(policies) {
			return result, fmt.Errorf("consumer reservation set differs from declaration")
		}
		for _, name := range slices.Sorted(maps.Keys(policies)) {
			p := policies[name]
			binding, ok := b.Producers[name]
			if !ok || !binding.Producer.Valid() {
				return result, fmt.Errorf("real producer reservation missing: %s", name)
			}
			routes := slices.Clone(p.Routes)
			slices.Sort(routes)
			kinds := slices.Clone(p.ContextKinds)
			slices.Sort(kinds)
			input.Worker.Producers = append(input.Worker.Producers, worker.Enrollment{Producer: binding.Producer, Required: config.WorkerEnabled(p.Required), Routes: routes, MaxSilenceSeconds: p.MaxSilenceSeconds, MaxEvidenceSeconds: p.MaxEvidenceSeconds, MaxMessages: p.MaxMessages, PolicyHealth: p.PolicyHealth, InteractiveContext: p.InteractiveContext, ContextKinds: kinds})
		}
	}
	if n.Commands.Enabled {
		interactionConfig, err := compileCommandPolicy(n.Commands, b)
		if err != nil {
			return result, err
		}
		input.Worker.Interaction = &interactionConfig
		result.CommandMode = n.Commands.Mode
		if result.CommandMode == "mutation" {
			result.CommandMode = "all"
		}
		for _, destination := range interactionConfig.Destinations {
			if destination.Provider == "telegram" {
				declared := n.Providers["telegram"]
				if b.Telegram == nil || b.Telegram.Account != destination.Account || declared.Account != b.Telegram.Account || declared.BotName != b.Telegram.BotName || b.Telegram.TokenFile != input.Providers["telegram"].TokenFile {
					return result, fmt.Errorf("Telegram intake account differs from declaration")
				}
				input.Telegram = b.Telegram
			} else if destination.Provider == "signal" {
				if b.Signal == nil || b.Signal.Account != destination.Account || n.Providers["signal"].Account != b.Signal.Account {
					return result, fmt.Errorf("Signal intake account differs from declaration")
				}
				input.Signal = b.Signal
			}
		}
	}
	if n.StatusLEDs.Enabled {
		if !config.WorkerAbsolutePath(b.LEDSocket) {
			return result, fmt.Errorf("LED daemon socket is unresolved")
		}
		if n.StatusLEDs.Socket != "" {
			socket, err := ResolveTargetPath(n.StatusLEDs.Socket, f.Home)
			if err != nil {
				return result, err
			}
			if socket != b.LEDSocket {
				return result, fmt.Errorf("LED daemon socket differs from declaration")
			}
		}
		input.LEDSocket = b.LEDSocket
		input.LEDEndpoint = b.LEDEndpoint
		if b.LEDEndpoint != nil && (b.LEDEndpoint.Validate() != nil || b.LEDEndpoint.Socket != b.LEDSocket) {
			return result, fmt.Errorf("LED endpoint differs from compiled socket")
		}
		bindings := maps.Clone(n.StatusLEDs.Bindings)
		if input.Bootstrap {
			bindings = nil // Desired mappings are applied after actual enrollment.
		}
		// No explicit bindings selects the built-in aggregate worker output. It
		// includes enrolled backup health without inventing a producer at bootstrap.
		for _, name := range slices.Sorted(maps.Keys(bindings)) {
			policy := bindings[name]
			producer, ok := b.Producers[policy.Producer]
			if !ok {
				return result, fmt.Errorf("LED producer reservation missing: %s", policy.Producer)
			}
			led := policy.LED
			if led == "power" {
				led = "red"
			}
			styles := map[string]worker.LEDStyle{}
			for condition, style := range policy.Styles {
				styles[condition] = worker.LEDStyle{Pattern: style.Pattern, Priority: style.Priority}
			}
			if len(styles) == 0 {
				styles = nil
			}
			subjects := slices.Clone(policy.Subjects)
			slices.Sort(subjects)
			input.Worker.LEDBindings = append(input.Worker.LEDBindings, worker.LEDBinding{ID: name, Producer: producer.Producer, Subjects: subjects, LED: led, AllowPower: led == "red" && n.StatusLEDs.PowerLED, Styles: styles})
		}
	}
	if _, err := input.Resolve(); err != nil {
		return result, err
	}
	result.Input = input
	return result, nil
}

func compileCommandPolicy(c config.CommandDeclaration, b NotificationBindings) (interaction.Config, error) {
	var result interaction.Config
	for _, alias := range slices.Sorted(maps.Keys(c.Workers)) {
		binding, ok := b.Producers[c.Workers[alias]]
		if !ok || !binding.Producer.Valid() || !protocol.ValidToken(binding.Generation) {
			return result, fmt.Errorf("command worker reservation missing: %s", alias)
		}
		commands := map[string]interaction.Descriptor{}
		for name, descriptor := range binding.Commands {
			if c.Mode == "mutation" || descriptor.Effect == "read" {
				commands[name] = descriptor
			}
		}
		if len(commands) == 0 {
			return result, fmt.Errorf("specialist has no eligible command manifest")
		}
		result.Workers = append(result.Workers, interaction.Worker{Alias: alias, Producer: binding.Producer, Generation: binding.Generation, Commands: commands})
	}
	for _, name := range slices.Sorted(maps.Keys(c.Conversations)) {
		declared := c.Conversations[name]
		actual, ok := b.Conversations[name]
		if !ok || actual.ID != name || actual.Provider != declared.Provider || actual.Account != declared.Account || actual.NativeID != declared.NativeID || actual.Thread != declared.Thread || !protocol.ValidToken(actual.Generation) {
			return result, fmt.Errorf("conversation reservation differs from declaration")
		}
		result.Destinations = append(result.Destinations, actual)
	}
	for _, p := range c.Permissions {
		producer, ok := b.Producers[c.Workers[p.Worker]]
		if !ok {
			return result, fmt.Errorf("permission worker reservation missing")
		}
		permissions := slices.Clone(p.Permissions)
		slices.Sort(permissions)
		for _, permission := range permissions {
			if c.Mode == "read" && permission != "read" {
				return result, fmt.Errorf("read command mode cannot grant mutation permissions")
			}
		}
		subjects := slices.Clone(p.Scope.Subjects)
		slices.Sort(subjects)
		result.Permissions = append(result.Permissions, interaction.Permission{Actor: p.Actor, Conversation: p.Conversation, Worker: producer.Producer.ID, Permissions: permissions, Scope: interaction.Scope{WholeWorker: p.Scope.WholeWorker, Subjects: subjects}})
	}
	slices.SortFunc(result.Permissions, func(a, b interaction.Permission) int { return strings.Compare(protocol.Hash(a), protocol.Hash(b)) })
	result.Revision = protocol.Hash(result)
	return result, result.Validate()
}
