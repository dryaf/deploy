package workerctl

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/testutil"
)

func declareFixtureReadCommands(t *testing.T, r *ControlRequest) {
	t.Helper()
	p := r.Desired.Notification.Providers["telegram"]
	p.Account, p.BotName = "123", "FixtureBot"
	r.Desired.Notification.Providers["telegram"] = p
	r.Desired.Notification.Commands = config.CommandDeclaration{Enabled: true, Mode: "read", Workers: map[string]string{"backup": "backup-worker"}, Conversations: map[string]config.ConversationDeclaration{"ops": {Provider: "telegram", Account: "123", NativeID: "456"}}, Permissions: []config.PermissionDeclaration{{Actor: "789", Conversation: "ops", Worker: "backup", Permissions: []string{"read"}, Scope: config.PermissionScope{WholeWorker: true}}}}
	r.DesiredHash, _ = r.Desired.Fingerprint()
}

func TestDeclaredReadCommandsEnrollReservedProducerWithoutAdmission(t *testing.T) {
	for _, mode := range []string{"read", "mutation"} {
		t.Run(mode, func(t *testing.T) { testDeclaredCommandsEnroll(t, mode) })
	}
}

func testDeclaredCommandsEnroll(t *testing.T, mode string) {
	r, paths := bootstrapControlFixture(t)
	declareFixtureReadCommands(t, &r)
	r.Desired.Notification.Commands.Mode = mode
	admission := "read"
	if mode == "mutation" {
		admission = "all"
		r.Desired.Notification.Commands.Permissions[0].Permissions = []string{"read", "incident.manage"}
	}
	testutil.Must(t, directNotificationSupport(r.Desired))
	input, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs)
	testutil.Must(t, err)
	if !input.Bootstrap || input.Worker.Interaction != nil || input.Telegram != nil || len(input.Worker.Producers) != 0 {
		t.Fatal("notification-first bootstrap granted command intake")
	}
	before, err := input.Resolve()
	testutil.Must(t, err)
	parent, err := ensureConfigDirectory(filepath.Dir(paths.State))
	testutil.Must(t, err)
	parent.Close()
	bootstrap, err := worker.BootstrapGateway(context.Background(), paths.State, testutil.ID(t, "n"), before.Worker)
	testutil.Must(t, err)
	reservation := backup.ProducerReservation{Version: 1, Producer: protocol.Producer{ID: testutil.ID(t, "w"), Epoch: testutil.ID(t, "n")}, Generation: testutil.ID(t, "g")}
	compiled, err := compileReservedNotification(r.Desired, r.Facts, paths, input, reservation)
	testutil.Must(t, err)
	next, err := compiled.Input.Resolve()
	testutil.Must(t, err)
	if next.Worker.Interaction == nil || next.Telegram == nil || compiled.CommandMode != admission || next.Worker.Interaction.Workers[0].Producer != reservation.Producer || next.Worker.Interaction.Workers[0].Generation != reservation.Generation || next.Providers["telegram"] != before.Providers["telegram"] {
		t.Fatal("lost reserved command policy or changed fixed alert provider")
	}
	for _, descriptor := range next.Worker.Interaction.Workers[0].Commands {
		if mode == "read" && descriptor.Effect != "read" {
			t.Fatal("compiled mutation descriptor")
		}
	}
	op := testutil.ID(t, "n")
	receipt, err := worker.EnrollBootstrap(context.Background(), paths.State, bootstrap.Identity, next.Worker, op)
	testutil.Must(t, err)
	if receipt.Version != 3 {
		t.Fatal("command enrollment did not retain versioned policy")
	}
	store, err := worker.OpenManagedVerification(paths.State, next.Worker, bootstrap.Identity)
	testutil.Must(t, err)
	destination := next.Worker.Interaction.Destinations[0]
	_, err = store.AdmitCommand(context.Background(), interaction.Inbound{Provider: "telegram", Account: "123", NativeID: "first", Actor: "789", Conversation: destination.ID, DestinationFingerprint: destination.Fingerprint(), SentAt: time.Now().UTC(), Text: "backup status"})
	if err == nil {
		t.Fatal("enrollment admitted commands")
	}
	testutil.Must(t, store.Close())
	again, err := compileReservedNotification(r.Desired, r.Facts, paths, input, reservation)
	testutil.Must(t, err)
	if protocol.Hash(again) != protocol.Hash(compiled) {
		t.Fatal("command compilation changed on retry")
	}
}

func TestInitialReadCommandsLeaveMissingBotNameToGuidedConfiguration(t *testing.T) {
	r, paths := bootstrapControlFixture(t)
	declareFixtureReadCommands(t, &r)
	p := r.Desired.Notification.Providers["telegram"]
	p.BotName = ""
	r.Desired.Notification.Providers["telegram"] = p
	// Structural support must not prevent the CLI's existing missing-input wizard.
	testutil.Must(t, directNotificationSupport(r.Desired))
	if !slices.Contains(MissingForWorker("notification-worker", r.Desired), "notification_worker.providers.telegram.bot_name") {
		t.Fatal("missing bot username was not offered to the wizard")
	}
	if _, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs); err == nil {
		t.Fatal("target bootstrap accepted incomplete command provider settings")
	}
}

func TestInitialReadCommandsRejectUnsupportedPoliciesBeforeBootstrap(t *testing.T) {
	for _, change := range []string{"mutation", "rights", "worker", "signal", "alert-conversation", "credential-account", "missing-backup"} {
		t.Run(change, func(t *testing.T) {
			r, paths := bootstrapControlFixture(t)
			declareFixtureReadCommands(t, &r)
			switch change {
			case "mutation":
				r.Desired.Notification.Commands.Mode = "all"
			case "rights":
				r.Desired.Notification.Commands.Permissions[0].Permissions = []string{"job.request"}
			case "worker":
				r.Desired.Notification.Commands.Workers["backup"] = "other"
			case "signal":
				r.Desired.Notification.Providers["signal"] = config.ProviderDeclaration{}
			case "alert-conversation":
				p := r.Desired.Notification.Providers["telegram"]
				p.Conversation = "ops"
				r.Desired.Notification.Providers["telegram"] = p
			case "credential-account":
				r.Inputs["token"] = []byte("999:different")
			case "missing-backup":
				r.Desired.Backup = nil
			}
			if _, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs); err == nil {
				t.Fatal("compiled unsupported command workflow")
			}
		})
	}
}

func TestDisabledCommandDeclarationRetainsTelegramAccountDuringRotation(t *testing.T) {
	r, paths := bootstrapControlFixture(t)
	declareFixtureReadCommands(t, &r)
	r.Desired.Notification.Commands.Enabled = false
	inputs := map[string][]byte{"token": []byte("123:replacement-for-the-same-bot"), "chat_id": []byte("654")}
	if _, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, inputs); err != nil {
		t.Fatal(err)
	}
	inputs["token"] = []byte("456:different-account")
	if _, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, inputs); err == nil {
		t.Fatal("disabled commands lost retained account")
	}
}
