//go:build notificationintegration

package install

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

// Exercise the actual helper and Unix socket while keeping service management
// local to this process fixture. No provider send or host systemd mutation occurs.
func TestManagedNativeBootstrapAndEnrollment(t *testing.T) {
	testManagedNativeBootstrapAndEnrollment(t, false, false, false, false)
}
func TestManagedNativeBootstrapLEDAndEnrollment(t *testing.T) {
	testManagedNativeBootstrapAndEnrollment(t, true, false, false, false)
}
func TestManagedNativeUnboundLEDRemoval(t *testing.T) {
	testManagedNativeBootstrapAndEnrollment(t, true, true, false, false)
}
func TestManagedNativeLimitsAfterLEDOutput(t *testing.T) {
	testManagedNativeBootstrapAndEnrollment(t, true, true, true, false)
}
func TestManagedNativeBootstrapCommandEnrollment(t *testing.T) {
	testManagedNativeBootstrapAndEnrollment(t, false, false, false, true)
}
func testManagedNativeBootstrapAndEnrollment(t *testing.T, withLED, remove, withLimits, withCommands bool) {
	spec, i, f, _ := managedFixture(t)
	if withCommands {
		qualificationFixtureInputs(t, spec.RuntimeFile)
	}
	short, err := os.MkdirTemp("", "gateway-native-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(short)
	spec.State = filepath.Join(short, "state")
	raw, err := os.ReadFile(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	var input RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	if withLED {
		input.LEDSocket = ledfixture.Start(t)
		endpoint, _, err := led.InspectEndpoint(context.Background(), input.LEDSocket)
		if err != nil {
			t.Fatal(err)
		}
		input.LEDEndpoint = &endpoint
	}
	consumers := input.Worker.Producers
	input.Bootstrap, input.Worker.Producers = true, nil
	resolved, err := input.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	op, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := worker.BootstrapGateway(context.Background(), spec.State, op, resolved.Worker)
	if err != nil {
		t.Fatal(err)
	}
	save := func() {
		body, e := input.Bytes()
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(spec.RuntimeFile, body, 0600); e != nil {
			t.Fatal(e)
		}
	}
	save()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", spec.BinaryFile, "./cmd/deploy")
	build.Dir = repo
	if out, e := process.LimitedOutput(build, time.Minute, 16384); e != nil {
		t.Fatal(e, string(out))
	}
	if err = os.Chmod(spec.BinaryFile, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(raw)
	native := &executableManagedServices{fakeManagedServices: f, root: spec.Root}
	i.services, i.probe = native, probeManagedBinary
	t.Cleanup(func() {
		if native.cmd != nil {
			native.cmd.Process.Kill()
			select {
			case <-native.done:
			case <-time.After(5 * time.Second):
			}
			if native.log != nil {
				native.log.Close()
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	before, err := i.ensureVerification(ctx, spec)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(filepath.Dir(spec.Root), "runtime-output"))
		t.Fatal(err, string(log))
	}
	if before.Identity != reserved.Identity {
		t.Fatal("bootstrap changed identity")
	}
	api := client.Client{Socket: before.Paths.Socket, Identity: before.Identity}
	var health worker.Health
	if _, err = api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); err != nil || health.Condition != "unknown" || !health.VerificationOnly {
		t.Fatal(health, err)
	}
	pid := native.cmd.Process.Pid
	if again, e := i.ensureVerification(ctx, spec); e != nil || again.Revision != before.Revision || native.cmd.Process.Pid != pid {
		t.Fatal("unchanged apply restarted service", e)
	}
	if !withLED {
		binary, e := os.ReadFile(spec.BinaryFile)
		if e != nil {
			t.Fatal(e)
		}
		binary = append(binary, []byte("native artifact update fixture")...)
		if e = os.WriteFile(spec.BinaryFile, binary, 0700); e != nil {
			t.Fatal(e)
		}
		spec.BinarySHA256 = managedDigest(binary)
		expected := protocol.Hash(before)
		updated, e := i.updateBootstrapVerification(ctx, spec, expected)
		if e != nil || updated.Identity != before.Identity || updated.Installation != before.Installation || updated.Revision == before.Revision || updated.HelperHash != before.HelperHash || native.cmd.Process.Pid == pid {
			t.Fatal("native artifact update lost authority", updated, e)
		}
		pid = native.cmd.Process.Pid
		if again, e := i.updateBootstrapVerification(ctx, spec, expected); e != nil || protocol.Hash(again) != protocol.Hash(updated) || native.cmd.Process.Pid != pid {
			t.Fatal("native artifact retry restarted", e)
		}
		before = updated
	}
	if remove {
		ledAPI := led.Client{Socket: input.LEDSocket, Budget: time.Second, Endpoint: input.LEDEndpoint}
		foreign := led.State{ID: "foreign.client", LED: "green", Pattern: "blink-fast", Priority: 95, Persistent: true}
		if err = ledAPI.Set(ctx, foreign); err != nil {
			t.Fatal(err)
		}
		if _, err = i.uninstallExpected(ctx, spec.Root, protocol.Hash(before)); err == nil {
			t.Fatal("unbound uninstall discarded LED output obligations")
		}
		if native.cmd.Process.Pid != pid {
			t.Fatal("refused removal changed service")
		}
		cleaned, e := i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(before), true)
		if e != nil {
			t.Fatal(e)
		}
		removed, e := i.uninstallExpected(ctx, spec.Root, protocol.Hash(cleaned))
		if e != nil || removed.Mode != "uninstalled" || removed.Identity != before.Identity {
			t.Fatal("removal after cleanup failed", removed, e)
		}
		if _, e = i.uninstallExpected(ctx, spec.Root, protocol.Hash(cleaned)); e != nil {
			t.Fatal("completed removal retry failed", e)
		}
		entries, e := ledAPI.List(ctx)
		if e != nil || len(entries) != 1 || entries[0].State != foreign {
			t.Fatal("removal changed foreign output or left owned output", entries, e)
		}
		observation, e := i.observeInstallation(ctx, spec.Root, false)
		if e != nil || !observation.LEDs || observation.Manifest.LEDOutput == nil || !observation.Manifest.LEDOutput.Disabled {
			t.Fatal("retained removal lost its LED disposition", observation, e)
		}
		restored, e := i.restoreBootstrapVerification(ctx, spec.Root, protocol.Hash(removed))
		if e != nil || restored.Identity != before.Identity || restored.Installation != before.Installation || restored.Mode != "verification" || restored.LEDOutput == nil || !restored.LEDOutput.Disabled {
			t.Fatal("activity restore lost retained authority", restored, e)
		}
		restoredPID := native.cmd.Process.Pid
		if again, e := i.restoreBootstrapVerification(ctx, spec.Root, protocol.Hash(removed)); e != nil || protocol.Hash(again) != protocol.Hash(restored) || native.cmd.Process.Pid != restoredPID {
			t.Fatal("activity restore retry restarted service", e)
		}
		reenabled, e := i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(restored), false)
		if e != nil || reenabled.LEDOutput == nil || reenabled.LEDOutput.Disabled || reenabled.Identity != before.Identity {
			t.Fatal("activity restore failed to re-enable output", e)
		}
		api = client.Client{Socket: reenabled.Paths.Socket, Identity: reenabled.Identity}
		health = worker.Health{}
		if _, e = api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); e != nil || !health.VerificationOnly || health.Condition == "ok" {
			t.Fatal("restored gateway claimed production health", health, e)
		}
		if withLimits {
			current := reenabled
			for _, disabled := range []bool{true, false} {
				current, e = i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(current), disabled)
				if e != nil {
					t.Fatal(e)
				}
				input.Worker.MaxMessages++
				save()
				previous := current
				pid := native.cmd.Process.Pid
				current, e = i.migrateBootstrapLimits(ctx, spec, protocol.Hash(previous))
				if e != nil || current.Identity != previous.Identity || protocol.Hash(current.LEDOutput) != protocol.Hash(previous.LEDOutput) || native.cmd.Process.Pid == pid {
					t.Fatal("native LED limits handoff", e)
				}
				pid = native.cmd.Process.Pid
				if again, e := i.migrateBootstrapLimits(ctx, spec, protocol.Hash(previous)); e != nil || protocol.Hash(again) != protocol.Hash(current) || native.cmd.Process.Pid != pid {
					t.Fatal("native limits retry", e)
				}
				api = client.Client{Socket: current.Paths.Socket, Identity: current.Identity}
				health = worker.Health{}
				if _, e = api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); e != nil || !health.VerificationOnly || health.LEDOutputDisposition == nil || health.LEDOutputDisposition.Disabled != disabled {
					t.Fatal("native limits output status", health, e)
				}
			}
			input.Bootstrap, input.Worker.Producers = false, consumers
			save()
			previous := current
			current, e = i.enrollVerificationConsumers(ctx, spec, protocol.Hash(previous))
			if e != nil || current.Identity != previous.Identity || protocol.Hash(current.LEDOutput) != protocol.Hash(previous.LEDOutput) {
				t.Fatal("native enrollment after limits/output", e)
			}
			pid := native.cmd.Process.Pid
			if again, e := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(previous)); e != nil || protocol.Hash(again) != protocol.Hash(current) || native.cmd.Process.Pid != pid {
				t.Fatal("native enrollment retry", e)
			}
			api = client.Client{Socket: current.Paths.Socket, Identity: current.Identity}
			health = worker.Health{}
			if _, e = api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); e != nil || !health.VerificationOnly || health.LEDOutputDisposition == nil || health.LEDOutputDisposition.Disabled {
				t.Fatal("native enrollment output status", health, e)
			}
			input.Worker.LEDBindings = []worker.LEDBinding{{ID: "backup", Producer: consumers[0].Producer, Subjects: []string{"*"}, LED: "green"}}
			save()
			previous = current
			current, e = i.migrateVerificationLEDBindings(ctx, spec, protocol.Hash(previous))
			if e != nil || current.Identity != previous.Identity || protocol.Hash(current.LEDOutput) != protocol.Hash(previous.LEDOutput) {
				t.Fatal("native mapping after output/enrollment", e)
			}
			pid = native.cmd.Process.Pid
			if again, e := i.migrateVerificationLEDBindings(ctx, spec, protocol.Hash(previous)); e != nil || protocol.Hash(again) != protocol.Hash(current) || native.cmd.Process.Pid != pid {
				t.Fatal("native mapping retry", e)
			}
			api = client.Client{Socket: current.Paths.Socket, Identity: current.Identity}
			health = worker.Health{}
			if _, e = api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); e != nil || !health.VerificationOnly || health.LEDOutputDisposition == nil || health.LEDOutputDisposition.Disabled {
				t.Fatal("native mapping output status", health, e)
			}
		}
		return
	}
	input.Bootstrap, input.Worker.Producers = false, consumers
	if withCommands {
		p := consumers[0].Producer
		destination := interaction.Destination{ID: "private", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"}
		input.Telegram = &provider.TelegramAccount{Account: "123", TokenFile: input.Providers["telegram"].TokenFile, BotName: "ExampleBot"}
		input.Worker.Interaction = &interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "backup/manager", Producer: p, Generation: "one", Commands: map[string]interaction.Descriptor{"status": {ID: "backup.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "status"}}}}, Destinations: []interaction.Destination{destination}, Permissions: []interaction.Permission{{Actor: "789", Conversation: destination.ID, Worker: p.ID, Permissions: []string{"read"}, Scope: interaction.Scope{WholeWorker: true}}}}
	}
	save()
	after, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before))
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(filepath.Dir(spec.Root), "runtime-output"))
		t.Fatal(err, string(log))
	}
	if after.Identity != before.Identity || after.Installation != before.Installation || after.Revision == before.Revision {
		t.Fatal("enrollment replaced identity or failed to revise runtime")
	}
	api = client.Client{Socket: after.Paths.Socket, Identity: after.Identity}
	health = worker.Health{}
	if _, err = api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); err != nil || !health.VerificationOnly || health.InstallationRevision != after.Revision || health.Condition == "ok" {
		t.Fatal(health, err)
	}
	if withCommands {
		destination := input.Worker.Interaction.Destinations[0]
		inbound := interaction.Inbound{Provider: destination.Provider, Account: destination.Account, NativeID: "first", Actor: "789", Conversation: destination.ID, DestinationFingerprint: destination.Fingerprint(), SentAt: time.Now().UTC(), Text: "backup/manager status"}
		body, _ := json.Marshal(inbound)
		if _, err = api.Call(ctx, protocol.Request{Operation: "command.ingest", Interaction: body}, nil); err == nil {
			t.Fatal("verification admitted a command")
		}
		pid := native.cmd.Process.Pid
		if again, e := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before)); e != nil || protocol.Hash(again) != protocol.Hash(after) || native.cmd.Process.Pid != pid {
			t.Fatal("command enrollment retry changed runtime", e)
		}
	}
	if withLED {
		ledAPI := led.Client{Socket: input.LEDSocket, Budget: time.Second, Endpoint: input.LEDEndpoint}
		deadline := time.Now().Add(3 * time.Second)
		found := false
		for time.Now().Before(deadline) {
			entries, err := ledAPI.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Pattern == "heartbeat" {
					t.Fatal("bootstrap/enrollment invented healthy evidence")
				}
				if entry.Persistent && entry.Priority < 10 {
					found = true
				}
			}
			if found {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !found {
			t.Fatal("aggregate unknown output missing")
		}
	}
	if err = native.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}
