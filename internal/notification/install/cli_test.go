package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func TestSnapshotCLIIsOfflineAndDryRunDoesNotCreateArtifacts(t *testing.T) {
	_, config := runtimeFixture(t)
	state := filepath.Join(t.TempDir(), "worker")
	if err := RunCLI([]string{"init", "--local", "--config", config, "--state-dir", state}); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "snapshot")
	old := ui.DryRun
	ui.DryRun = true
	t.Cleanup(func() { ui.DryRun = old })
	args := []string{"snapshot", "--local", "--config", config, "--state-dir", state, "--to", artifact}
	if err := RunCLI(args); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatal("dry-run created snapshot")
	}
	ui.DryRun = false
	if err := RunCLI(args); err != nil {
		t.Fatal(err)
	}
	if err := RunCLI([]string{"snapshot-verify", "--local", "--config", config, "--from", artifact}); err != nil {
		t.Fatal(err)
	}
	runtime, err := ReadRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.VerifySnapshot(context.Background(), artifact, runtime.Worker); err != nil {
		t.Fatal(err)
	}
	if err = RunCLI(append(args, "--channel", "telegram")); err == nil {
		t.Fatal("snapshot accepted send options")
	}
	if err = RunCLI([]string{"recover", "--local", "--config", config, "--state-dir", state, "--from", artifact}); err == nil {
		t.Fatal("unfinished restore exposed to CLI")
	}
}

func runtimeFixture(t *testing.T) (Runtime, string) {
	t.Helper()
	c := Runtime{Version: 1, Worker: worker.Config{Producers: []worker.Enrollment{{Producer: protocol.Producer{ID: "fixture", Epoch: "one"}, Required: true, Routes: []string{"ops"}, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, MaxMessages: 100}}, Routes: map[string]worker.Route{"ops": {Revision: "one", Primary: "telegram"}}, MaxMessages: 100, MaxPayloadBytes: 1 << 20, MaxDatabaseBytes: 16 << 20}, Providers: map[string]provider.Profile{"telegram": {Kind: "telegram", TokenFile: "/private/token", ChatFile: "/private/chat", Destination: provider.TelegramIdentity("123:secret", "chat-one")}}}
	path := filepath.Join(t.TempDir(), "runtime.json")
	writeRuntime(t, path, c)
	return c, path
}

func TestStagedTelegramRejectsMutationActivation(t *testing.T) {
	c, path := runtimeFixture(t)
	p := c.Worker.Producers[0].Producer
	d := interaction.Descriptor{ID: "fixture.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "fixture"}
	c.Worker.Interaction = &interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "fixture/local", Producer: p, Generation: "one", Commands: map[string]interaction.Descriptor{"status": d}}}, Destinations: []interaction.Destination{{ID: "private", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"}}}
	c.Telegram = &provider.TelegramAccount{Account: "123", TokenFile: "/private/token", BotName: "ExampleBot"}
	writeRuntime(t, path, c)
	if _, err := ReadRuntime(path); err != nil {
		t.Fatal(err)
	}
	d.Effect = "mutation"
	d.Permission = "job.request"
	c.Worker.Interaction.Workers[0].Commands["status"] = d
	writeRuntime(t, path, c)
	parsed, err := ReadRuntime(path)
	if err != nil {
		t.Fatal("mutation configuration cannot be staged", err)
	}
	if unmanagedCommandPolicy(parsed) == nil {
		t.Fatal("unqualified mutation enabled")
	}
}
func writeRuntime(t *testing.T, path string, c Runtime) {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeLEDBindingsRequireSocketAndArePinnedByInit(t *testing.T) {
	c, path := runtimeFixture(t)
	c.Worker.LEDBindings = []worker.LEDBinding{{ID: "backup-status", Producer: c.Worker.Producers[0].Producer, Subjects: []string{"*"}, LED: "green"}}
	writeRuntime(t, path, c)
	if _, err := ReadRuntime(path); err == nil {
		t.Fatal("LED bindings silently ignored without socket")
	}
	c.LEDSocket = "/private/disposable-led.sock"
	writeRuntime(t, path, c)
	state := filepath.Join(t.TempDir(), "state")
	if err := RunCLI([]string{"init", "--local", "--config", path, "--state-dir", state}); err != nil {
		t.Fatal(err)
	}
	// Init only writes local state: the deliberately nonexistent socket is never contacted.
	runtime, err := ReadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := worker.Open(state, runtime.Worker, false)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	c.Worker.LEDBindings[0].Subjects = []string{"changed"}
	writeRuntime(t, path, c)
	runtime, err = ReadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	if s, err = worker.Open(state, runtime.Worker, false); err == nil {
		s.Close()
		t.Fatal("configuration edit reused an old LED mapping")
	}
}

func TestRouteFingerprintChangesWithDestination(t *testing.T) {
	c, path := runtimeFixture(t)
	first, err := ReadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Providers["telegram"]
	p.Destination = provider.TelegramIdentity("123:secret", "chat-two")
	c.Providers["telegram"] = p
	writeRuntime(t, path, c)
	second, err := ReadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Worker.Routes["ops"].Revision == second.Worker.Routes["ops"].Revision {
		t.Fatal("changed recipient retained queued route identity")
	}
}

func TestInteractiveAlertProfilePinsEnrolledConversationAndGeneration(t *testing.T) {
	c, path := runtimeFixture(t)
	d := interaction.Destination{ID: "ops", Generation: "one", Provider: "telegram", Account: "123", NativeID: "-100", Thread: "42"}
	c.Worker.Interaction = &interaction.Config{Revision: "one", Destinations: []interaction.Destination{d}, Workers: []interaction.Worker{{Alias: "fixture/local", Producer: c.Worker.Producers[0].Producer, Generation: "one", Commands: map[string]interaction.Descriptor{"status": {ID: "fixture.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "fixture"}}}}}
	p := c.Providers["telegram"]
	p.ChatFile = ""
	p.Conversation = &d
	p.Destination = d.Fingerprint()
	c.Providers["telegram"] = p
	writeRuntime(t, path, c)
	first, err := ReadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Generation = "two"
	p.Destination = d.Fingerprint()
	c.Providers["telegram"] = p
	c.Worker.Interaction.Destinations[0] = d
	writeRuntime(t, path, c)
	second, err := ReadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Worker.Routes["ops"].Revision == second.Worker.Routes["ops"].Revision {
		t.Fatal("conversation generation did not fence queued alerts")
	}
	d.Thread = "99"
	p.Destination = d.Fingerprint()
	c.Providers["telegram"] = p
	writeRuntime(t, path, c)
	if _, err = ReadRuntime(path); err == nil {
		t.Fatal("unenrolled thread accepted")
	}
}

func TestDryRunValidatesWithoutCreatingState(t *testing.T) {
	_, path := runtimeFixture(t)
	state := filepath.Join(t.TempDir(), "absent")
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	if err := RunCLI([]string{"init", "--local", "--config", path, "--state-dir", state}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("dry run created state")
	}
	if err := os.WriteFile(path, []byte(`{"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RunCLI([]string{"init", "--local", "--config", path, "--state-dir", state}); err == nil {
		t.Fatal("dry run accepted invalid runtime")
	}
}
