package install

import (
	"context"
	"errors"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"os"
	"testing"
)

func TestManagedEnrolledCommandsDurableBoundaries(t *testing.T) {
	testManagedEnrolledCommands(t, "")
}
func TestManagedEnrolledCommandPolicyDurableBoundaries(t *testing.T) {
	testManagedEnrolledCommands(t, "policy")
}
func TestManagedEnrolledCommandActivityDurableBoundaries(t *testing.T) {
	for _, direction := range []string{"disable", "enable"} {
		t.Run(direction, func(t *testing.T) { testManagedEnrolledCommands(t, direction) })
	}
}
func TestManagedEnrolledCommandProvidersDurableBoundaries(t *testing.T) {
	testManagedEnrolledCommands(t, "providers")
}
func testManagedEnrolledCommands(t *testing.T, update string) {
	kind := "enrolled-commands"
	if update != "" {
		kind = "enrolled-command-policy"
	}
	if update == "disable" || update == "enable" {
		kind = "enrolled-command-activity"
	}
	if update == "providers" {
		kind = "enrolled-providers"
	}

	for _, boundary := range []string{"prepared", "stop-returned", kind + "-migrated", "files", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, f, unbound := providerUpdateFixture(t)
			raw, err := os.ReadFile(spec.RuntimeFile)
			if err != nil {
				t.Fatal(err)
			}
			var input RuntimeInput
			if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
				t.Fatal(err)
			}
			input.Bootstrap = false
			input.Worker.Producers = []worker.Enrollment{{Producer: protocol.Producer{ID: "backup-worker", Epoch: "reserved"}, Required: true, Routes: []string{"ops"}, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, MaxMessages: 10}}
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(unbound))
			if err != nil {
				t.Fatal(err)
			}
			p := input.Worker.Producers[0].Producer
			input.Worker.Interaction = &interaction.Config{Revision: "commands", Workers: []interaction.Worker{{Alias: "backup", Producer: p, Generation: "reserved", Commands: map[string]interaction.Descriptor{"status": {ID: "backup.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "status"}}}}, Destinations: []interaction.Destination{{ID: "ops", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"}}, Permissions: []interaction.Permission{{Actor: "789", Conversation: "ops", Worker: p.ID, Permissions: []string{"read"}, Scope: interaction.Scope{WholeWorker: true}}}}
			input.Telegram = &provider.TelegramAccount{Account: "123", BotName: "fixture_bot", TokenFile: input.Providers["telegram"].TokenFile}
			raw, err = input.Bytes()
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
			probe := func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.bootstrapLimitsProbe = probe
			if update != "" {
				i.enrolledLimitsProbe = probe
				before, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-commands")
				testutil.Must(t, err)
				input.Worker.Interaction.Revision = "revised"
				input.Worker.Interaction.Workers[0].Alias = "backup-renamed"
				input.Worker.Interaction.Destinations[0].NativeID = "654"
				input.Worker.Interaction.Destinations[0].Generation = "two"
				input.Worker.Interaction.Permissions[0].Actor = "987"
				raw, err = input.Bytes()
				testutil.Must(t, err)
				testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
				if update == "providers" {
					before, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-command-policy")
					testutil.Must(t, err)
					oldRuntime, e := input.Resolve()
					testutil.Must(t, e)
					p := input.Providers["telegram"]
					p.TokenFile += "-commands"
					testutil.Must(t, os.WriteFile(p.TokenFile, []byte("123:replacement-command-token"), 0600))
					input.Providers["telegram"] = p
					input.Telegram.TokenFile = p.TokenFile
					nextRuntime, e := input.Resolve()
					testutil.Must(t, e)
					testutil.Must(t, ValidateManagedEnrolledProviders(before, oldRuntime, nextRuntime))
					bad := nextRuntime
					account := *bad.Telegram
					account.BotName = "different_bot"
					bad.Telegram = &account
					if ValidateManagedEnrolledProviders(before, oldRuntime, bad) == nil {
						t.Fatal("command bot renamed during token update")
					}
					account = *nextRuntime.Telegram
					account.TokenFile = oldRuntime.Telegram.TokenFile
					bad.Telegram = &account
					if ValidateManagedEnrolledProviders(before, oldRuntime, bad) == nil {
						t.Fatal("alert and intake tokens diverged")
					}
					raw, err = input.Bytes()
					testutil.Must(t, err)
					testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
				}
				if update == "disable" || update == "enable" {
					before, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-command-policy")
					testutil.Must(t, err)
					commandPolicy, account := input.Worker.Interaction, input.Telegram
					input.Worker.Interaction, input.Telegram = nil, nil
					raw, err = input.Bytes()
					testutil.Must(t, err)
					testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
					if update == "enable" {
						before, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-command-activity")
						testutil.Must(t, err)
						input.Worker.Interaction, input.Telegram = commandPolicy, account
						raw, err = input.Bytes()
						testutil.Must(t, err)
						testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
					}
				}
			}
			i.enrolledLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("older helper") }
			stops := f.stops
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), kind); err == nil || f.stops != stops {
				t.Fatal("incapable helper stopped service", err)
			}
			i.enrolledLimitsProbe = probe
			stop := errors.New("lost reply")
			i.fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), kind); !errors.Is(err, stop) {
				t.Fatal(boundary, err)
			}
			i.fault = nil
			after, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), kind)
			if err != nil {
				t.Fatal(err)
			}
			if before.Identity != after.Identity || before.Installation != after.Installation || after.Mode != "verification" || before.ConfigHash == after.ConfigHash {
				t.Fatal("changed authority")
			}
			starts := f.starts
			if again, e := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), kind); e != nil || protocol.Hash(again) != protocol.Hash(after) || f.starts != starts {
				t.Fatal("replayed completion", e)
			}
			runtime, err := input.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			s, err := worker.OpenManagedVerification(spec.State, runtime.Worker, after.Identity)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
		})
	}
}
