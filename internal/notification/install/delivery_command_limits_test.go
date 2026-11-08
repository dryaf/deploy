package install

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestCommandDeliveryBudgetsPreserveAuthorityAcrossInterruptions(t *testing.T) {
	for _, kind := range []string{"command-limits", "command-freshness", "command-producer", "command-routes", "command-provider", "command-destination"} {
		t.Run(kind, func(t *testing.T) {

			for _, boundary := range []string{"prepared", "stop-returned", "delivery-limits-migrated", "files", "published", "start-returned", "complete"} {
				t.Run(boundary, func(t *testing.T) {
					ctx := context.Background()
					spec, i, runtime, active, report := managedCommandsFixture(t)
					nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
					i.commandProvidersProbe = nop
					i.commandPauseProbe, i.commandLimitsProbe, i.commandFreshnessProbe, i.commandProducerProbe, i.commandRoutesProbe, i.enrolledLimitsProbe = nop, nop, nop, nop, nop, nop
					enabled, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(report), "read", false)
					testutil.Must(t, err)
					store, err := openManagedStore(enabled, runtime)
					testutil.Must(t, err)
					testutil.Must(t, store.BindManagedCommandAuthority(func(context.Context) error { return nil }))
					dst := runtime.Worker.Interaction.Destinations[0]
					inbound := interaction.Inbound{Provider: "telegram", Account: "123", NativeID: "budget-pending", Actor: "789", Conversation: dst.ID, DestinationFingerprint: dst.Fingerprint(), SentAt: time.Now().UTC(), Text: "worker/local status"}
					_, err = store.AdmitCommand(ctx, inbound)
					testutil.Must(t, err)
					testutil.Must(t, store.Close())
					current, err := i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(enabled), "", "", false)
					testutil.Must(t, err)
					store, err = openManagedStore(current, runtime)
					testutil.Must(t, err)
					producer := runtime.Worker.Producers[0].Producer
					pending, err := store.Commands(ctx, producer)
					testutil.Must(t, err)
					testutil.Must(t, store.Close())
					if len(pending) != 1 {
						t.Fatal("missing pending command")
					}
					original := current
					var latestQualification string
					for cycle := 0; cycle < 2; cycle++ {
						raw, err := os.ReadFile(spec.RuntimeFile)
						testutil.Must(t, err)
						var input RuntimeInput
						testutil.Must(t, safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes))
						if commandProviderKind(kind) {
							updateCommandProviderInput(t, &input, kind)
						} else if kind == "command-routes" {
							input.Worker.Routes = maps.Clone(input.Worker.Routes)
							input.Worker.Routes[fmt.Sprint("audit", len(input.Worker.Routes))] = input.Worker.Routes[input.Worker.Producers[0].Routes[0]]
						} else if kind == "command-producer" {
							input.Worker.Producers[0].MaxMessages += 10
						} else if kind == "command-freshness" {
							input.Worker.Producers[0].MaxSilenceSeconds += 10
							input.Worker.Producers[0].MaxEvidenceSeconds += 100
						} else {
							input.Worker.MaxMessages += 10
						}
						raw, err = input.Bytes()
						testutil.Must(t, err)
						next, err := input.Resolve()
						testutil.Must(t, err)
						if cycle == 0 {
							if _, err = i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(enabled), raw, kind); err == nil {
								t.Fatal("review accepted active intake")
							}
						}
						_, err = i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(current), raw, kind)
						testutil.Must(t, err)
						paused, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(current), "", false)
						testutil.Must(t, err)
						testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
						lost := errors.New("lost command budget reply")
						i.fault = func(at string) error {
							if at == boundary {
								return lost
							}
							return nil
						}
						_, err = i.changePausedRuntime(ctx, spec, protocol.Hash(paused), kind)
						if !errors.Is(err, lost) {
							t.Fatal("missed budget boundary", err)
						}
						i.fault = nil
						changed, err := i.changePausedRuntime(ctx, spec, protocol.Hash(paused), kind)
						testutil.Must(t, err)
						observed, complete, err := i.inspectPausedRuntimeChange(ctx, spec, protocol.Hash(paused), kind)
						testutil.Must(t, err)
						if !complete || protocol.Hash(observed) != protocol.Hash(changed) || changed.CommandHistoryHash != original.CommandHistoryHash || changed.Commands != nil || changed.DeliveryOwnerHash != original.DeliveryOwnerHash {
							t.Fatal("changed authority")
						}
						store, err = openManagedStore(changed, next)
						testutil.Must(t, err)
						retained, err := store.Commands(ctx, producer)
						testutil.Must(t, err)
						if !reflect.DeepEqual(pending, retained) {
							t.Fatal("changed queued command")
						}
						testutil.Must(t, store.Close())
						fresh := freshDeliveryQualification(t, spec.Root)
						latestQualification = protocol.Hash(fresh)
						current, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(changed), protocol.Hash(fresh), false)
						testutil.Must(t, err)
						if current.Commands != nil || current.CommandHistoryHash != original.CommandHistoryHash {
							t.Fatal("renewal changed command admission")
						}
						_, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(current), protocol.Hash(original))
						testutil.Must(t, err)
						runtime = next
					}
					_, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(current), latestQualification, "read", false)
					testutil.Must(t, err)
				})
			}

		})
	}
}

func TestCommandBudgetCapabilityRefusedBeforePause(t *testing.T) {
	for _, kind := range []string{"command-limits", "command-freshness", "command-producer", "command-routes", "command-provider", "command-destination", "command-declaration"} {
		t.Run(kind, func(t *testing.T) {

			for _, failure := range []int{1, 2} {
				t.Run(fmt.Sprint(failure), func(t *testing.T) {
					ctx := context.Background()
					spec, i, _, m, report := managedCommandsFixture(t)
					i.commandPauseProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
					var err error
					m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(report), "read", false)
					testutil.Must(t, err)
					m, err = i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
					testutil.Must(t, err)
					raw, err := os.ReadFile(spec.RuntimeFile)
					testutil.Must(t, err)
					var input RuntimeInput
					testutil.Must(t, safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes))
					if commandProviderKind(kind) {
						updateCommandProviderInput(t, &input, kind)
					} else if kind == "command-routes" {
						input.Worker.Routes = maps.Clone(input.Worker.Routes)
						input.Worker.Routes[fmt.Sprint("audit", len(input.Worker.Routes))] = input.Worker.Routes[input.Worker.Producers[0].Routes[0]]
					} else if kind == "command-producer" {
						input.Worker.Producers[0].MaxMessages += 10
					} else if kind == "command-freshness" {
						input.Worker.Producers[0].MaxSilenceSeconds += 10
						input.Worker.Producers[0].MaxEvidenceSeconds += 100
					} else if kind != "command-declaration" {
						input.Worker.MaxMessages += 10
					}
					raw, err = input.Bytes()
					testutil.Must(t, err)
					before, err := os.ReadFile(filepath.Join(spec.Root, "lifecycle.json"))
					testutil.Must(t, err)
					calls := 0
					i.commandLimitsProbe = func(context.Context, *safefs.Directory, string, string) error {
						calls++
						if calls == failure {
							return errors.New("old helper")
						}
						return nil
					}
					i.commandFreshnessProbe = i.commandLimitsProbe
					i.commandProducerProbe = i.commandLimitsProbe
					i.commandRoutesProbe = i.commandLimitsProbe
					i.commandProvidersProbe = i.commandLimitsProbe
					if _, err = i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(m), raw, kind); err == nil || calls != failure {
						t.Fatal("missed helper refusal", err, calls)
					}
					after, err := os.ReadFile(filepath.Join(spec.Root, "lifecycle.json"))
					testutil.Must(t, err)
					if string(before) != string(after) {
						t.Fatal("failed review retained intent")
					}
				})
			}

		})
	}
}

func TestCommandDeliveryBudgetsRetainLEDOutputAndForeignClients(t *testing.T) {
	for _, kind := range []string{"command-limits", "command-freshness", "command-producer", "command-routes", "command-provider", "command-destination"} {
		t.Run(kind, func(t *testing.T) {
			version := 36
			if commandProviderKind(kind) {
				version = 40
			}
			if kind == "command-routes" {
				version = 38
			} else if kind == "command-producer" {
				version = 39
			} else if kind == "command-freshness" {
				version = 37
			}

			ctx := context.Background()
			spec, i, _, m, q, api := productionOutputFixture(t, true, false)
			nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.commandProvidersProbe = nop
			i.commandPauseProbe, i.commandLimitsProbe, i.commandFreshnessProbe, i.commandProducerProbe, i.commandRoutesProbe, i.enrolledLimitsProbe = nop, nop, nop, nop, nop, nop
			var err error
			m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(q), "read", false)
			testutil.Must(t, err)
			m, err = i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
			testutil.Must(t, err)
			m, err = i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(m), true)
			testutil.Must(t, err)
			original, foreign := m, api.states["foreign"]
			raw, err := os.ReadFile(spec.RuntimeFile)
			testutil.Must(t, err)
			var input RuntimeInput
			testutil.Must(t, safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes))
			if commandProviderKind(kind) {
				updateCommandProviderInput(t, &input, kind)
			} else if kind == "command-routes" {
				input.Worker.Routes = maps.Clone(input.Worker.Routes)
				input.Worker.Routes[fmt.Sprint("audit", len(input.Worker.Routes))] = input.Worker.Routes[input.Worker.Producers[0].Routes[0]]
			} else if kind == "command-producer" {
				input.Worker.Producers[0].MaxMessages += 10
			} else if kind == "command-freshness" {
				input.Worker.Producers[0].MaxEvidenceSeconds += 100
			} else {
				input.Worker.MaxMessages += 10
			}
			raw, err = input.Bytes()
			testutil.Must(t, err)
			hashes := func() map[string]string {
				out := map[string]string{}
				for _, root := range []string{spec.Root, spec.State, spec.UnitDirectory} {
					testutil.Must(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
						if e != nil {
							return e
						}
						if d.Type().IsRegular() && !strings.HasSuffix(p, "state.db-wal") && !strings.HasSuffix(p, "state.db-shm") {
							b, e := os.ReadFile(p)
							if e != nil {
								return e
							}
							out[p] = managedDigest(b)
						}
						return nil
					}))
				}
				return out
			}
			// Read-only SQLite access can create WAL coordination sidecars. Compare
			// the database and durable ownership/history records, not those sidecars.
			beforeFiles := hashes()
			_, err = i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(m), raw, kind)
			testutil.Must(t, err)
			if !reflect.DeepEqual(beforeFiles, hashes()) {
				t.Fatal("budget review changed durable records")
			}
			m, err = i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(m), "", false)
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
			m, err = i.changePausedRuntime(ctx, spec, protocol.Hash(m), kind)
			testutil.Must(t, err)
			var receipt struct {
				Receipt  worker.EnrolledLimitsReceipt `json:"receipt"`
				Complete bool                         `json:"complete"`
			}
			state, err := safefs.Open(spec.State)
			testutil.Must(t, err)
			err = state.ReadJSON("enrolled-limits.json", &receipt)
			state.Close()
			testutil.Must(t, err)
			if !receipt.Complete || receipt.Receipt.Version != version || receipt.Receipt.CommandHistoryHash != original.CommandHistoryHash {
				t.Fatal("missing command/LED receipt pins")
			}
			if !reflect.DeepEqual(m.LEDOutput, original.LEDOutput) || api.states["foreign"] != foreign {
				t.Fatal("budget change altered LED ownership")
			}
			fresh := qualifyPausedLED(t, m)
			m, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), fresh.QualificationHash, false)
			testutil.Must(t, err)
			if m.CommandHistoryHash != original.CommandHistoryHash || m.Commands != nil || !reflect.DeepEqual(m.LEDOutput, original.LEDOutput) || api.states["foreign"] != foreign {
				t.Fatal("renewal changed retained ownership")
			}
			_, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(m), protocol.Hash(original))
			testutil.Must(t, err)

		})
	}
}

func updateCommandProviderInput(t *testing.T, input *RuntimeInput, kind string) {
	t.Helper()
	p := input.Providers["telegram"]
	root := t.TempDir()
	p.TokenFile = filepath.Join(root, "token")
	testutil.Must(t, os.WriteFile(p.TokenFile, []byte("123:rotated-token"), 0600))
	input.Telegram.TokenFile = p.TokenFile
	if kind == "command-destination" {
		p.ChatFile = filepath.Join(root, "chat")
		testutil.Must(t, os.WriteFile(p.ChatFile, []byte("987"), 0600))
		// Alternate destinations on repeated rotations.
		if old, _ := provider.ReadSecret(input.Providers["telegram"].ChatFile); old == "987" {
			testutil.Must(t, os.WriteFile(p.ChatFile, []byte("986"), 0600))
		}
		p.Destination = provider.TelegramIdentity("123", string(mustReadSC4(t, p.ChatFile)))
	}
	input.Providers["telegram"] = p
}

func mustReadSC4(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	testutil.Must(t, err)
	return b
}
