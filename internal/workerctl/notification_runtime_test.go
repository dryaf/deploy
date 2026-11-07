package workerctl

import (
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

func TestNotificationRuntimeCompilesNamedPolicy(t *testing.T) {
	yaml := `version: 3
servers:
  manager:
    host: pi.local
    user: deploy
    notification_worker:
      providers:
        telegram:
          token_file: ~/.config/secrets/token
          bot_name: fixture_bot
          conversation: ops
      producers:
        backup-worker:
          max_messages: 321
          max_silence_seconds: 180
          max_evidence_seconds: 9000
          policy_health: true
          interactive_context: true
          context_kinds: [backup.incident]
      limits: {receipt_audit_days: 120, max_messages: 2000}
      commands:
        enabled: true
        mode: read
        workers: {backups: backup-worker}
        conversations:
          ops: {provider: telegram, account: bot, native_id: '-123', thread: '45'}
        permissions:
          - actor: operator
            conversation: ops
            worker: backups
            permissions: [read]
            scope: {subjects: [z, a]}
      status_leds:
        enabled: true
        power_led: true
        socket: ~/.local/run/leds.sock
        daemon: {mode: existing}
        bindings:
          backup-state:
            producer: backup-worker
            subjects: [z, a]
            led: power
            styles: {critical: {pattern: blink-fast, priority: 95}}
`
	inv, err := config.ParseInventory("/project/server.yaml", []byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	d := *inv.Servers["manager"].Desired
	if missing := d.MissingInputs(); len(missing) != 0 {
		t.Fatal("conversation incorrectly requires fixed chat file", missing)
	}
	facts := testFacts()
	paths, err := ResolvePaths("notification-worker", d.Notification.Paths, facts)
	if err != nil {
		t.Fatal(err)
	}
	conversation := interaction.Destination{ID: "ops", Generation: "reserved-one", Provider: "telegram", Account: "bot", NativeID: "-123", Thread: "45"}
	p := protocol.Producer{ID: "durable-backup", Epoch: "reserved-epoch"}
	bindings := NotificationBindings{Providers: map[string]provider.Profile{"telegram": {Kind: "telegram", TokenFile: "/home/remote/.config/secrets/token", Conversation: &conversation, Destination: conversation.Fingerprint()}}, Producers: map[string]ProducerBinding{"backup-worker": {Producer: p, Generation: "reserved-generation", Commands: map[string]interaction.Descriptor{"status": {ID: "backup.status", Version: 1, Permission: "read", Effect: "read", Scoped: true}, "refresh": {ID: "backup.refresh", Version: 1, Permission: "job.request", Effect: "mutation"}}}}, Conversations: map[string]interaction.Destination{"ops": conversation}, Telegram: &provider.TelegramAccount{Account: "bot", TokenFile: "/home/remote/.config/secrets/token", BotName: "fixture_bot"}, LEDSocket: "/home/remote/.local/run/leds.sock"}
	compiled, err := CompileNotification(d, facts, paths, bindings)
	if err != nil {
		t.Fatal(err)
	}
	c := compiled.Input.Worker
	if compiled.Input.Bootstrap || compiled.CommandMode != "read" || c.Producers[0].Producer != p || c.Producers[0].MaxMessages != 321 || c.Producers[0].MaxSilenceSeconds != 180 || c.Producers[0].MaxEvidenceSeconds != 9000 || !c.Producers[0].PolicyHealth || !c.Producers[0].InteractiveContext || c.ReceiptAuditDays != 120 {
		t.Fatal("producer policy lost", c)
	}
	if c.Interaction.Permissions[0].Worker != p.ID || strings.Join(c.Interaction.Permissions[0].Scope.Subjects, ",") != "a,z" || len(c.Interaction.Workers[0].Commands) != 1 {
		t.Fatal("permission alias/scope/mode compiled incorrectly", c.Interaction)
	}
	if c.LEDBindings[0].LED != "red" || !c.LEDBindings[0].AllowPower || c.LEDBindings[0].Styles["critical"].Priority != 95 || strings.Join(c.LEDBindings[0].Subjects, ",") != "a,z" {
		t.Fatal("LED policy lost", c.LEDBindings)
	}
	first, err := compiled.Input.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiled.Input.Resolve()
	if err != nil || protocol.Hash(first) != protocol.Hash(second) {
		t.Fatal("double route hashing", err)
	}
	if first.Worker.Routes["alerts"].Revision == compiled.Input.Worker.Routes["alerts"].Revision {
		t.Fatal("destination not bound to route")
	}
	d.Notification.Commands.Permissions[0].Permissions = []string{"job.request"}
	if _, err = CompileNotification(d, facts, paths, bindings); err == nil {
		t.Fatal("read mode granted mutation")
	}
}

func TestNotificationCompilerRequiresExactReservedConsumerSet(t *testing.T) {
	r, paths := bootstrapControlFixture(t)
	profile := provider.Profile{Kind: "telegram", TokenFile: paths.Credentials + "/notification-worker/telegram-token", ChatFile: paths.Credentials + "/notification-worker/telegram-chat-id", Destination: strings.Repeat("a", 64)}
	facts := NotificationBindings{Providers: map[string]provider.Profile{"telegram": profile}, Producers: map[string]ProducerBinding{"backup-worker": {Producer: protocol.Producer{ID: "reserved-backup", Epoch: "one"}}}}
	c, err := CompileNotification(r.Desired, r.Facts, paths, facts)
	if err != nil {
		t.Fatal(err)
	}
	if c.Input.Bootstrap || len(c.Input.Worker.Producers) != 1 || c.Input.Worker.Producers[0].MaxEvidenceSeconds != 7200 {
		t.Fatal("backup default enrollment missing", c.Input.Worker)
	}
	facts.Producers["invented"] = ProducerBinding{Producer: protocol.Producer{ID: "extra", Epoch: "one"}}
	if _, err = CompileNotification(r.Desired, r.Facts, paths, facts); err == nil {
		t.Fatal("accepted undeclared consumer")
	}
}

func TestNotificationFirstDefersDeclaredBackupLEDBindings(t *testing.T) {
	r, paths := bootstrapControlFixture(t)
	r.Desired.Notification.StatusLEDs = config.LEDDeclaration{Enabled: true, Socket: "/private/led.sock", Daemon: config.LEDDaemon{Mode: "existing"}, Bindings: map[string]config.LEDBindingDeclaration{"backup": {Producer: "backup-worker", Subjects: []string{"*"}, LED: "green"}}}
	input, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !input.Bootstrap || input.LEDSocket == "" || len(input.Worker.LEDBindings) != 0 || len(input.Worker.Producers) != 0 {
		t.Fatal("bootstrap fabricated mapping authority", input)
	}
	producer := protocol.Producer{ID: "real-backup", Epoch: "reserved"}
	compiled, err := CompileNotification(r.Desired, r.Facts, paths, NotificationBindings{Providers: input.Providers, LEDSocket: input.LEDSocket, Producers: map[string]ProducerBinding{"backup-worker": {Producer: producer}}})
	if err != nil || len(compiled.Input.Worker.LEDBindings) != 1 || compiled.Input.Worker.LEDBindings[0].Producer != producer {
		t.Fatal("reserved mapping unavailable", err)
	}
	r.Desired.Backup = nil
	if _, err = CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs); err == nil {
		t.Fatal("unowned mapping accepted")
	}
}
