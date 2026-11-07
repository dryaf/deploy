package install

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestPausedDeliveryLimitsPreserveQueueOwnerAndReporting(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "delivery-limits-migrated", "files", "published", "start-returned", "complete", "native:delivery-limits-migrated", "native:start-returned", "signal-only:delivery-limits-migrated", "producer:prepared", "producer:stop-returned", "producer:delivery-limits-migrated", "producer:files", "producer:published", "producer:start-returned", "producer:complete", "producer:native:delivery-limits-migrated", "producer:signal-only:start-returned", "routes:prepared", "routes:stop-returned", "routes:delivery-limits-migrated", "routes:files", "routes:published", "routes:start-returned", "routes:complete", "routes:native:delivery-limits-migrated", "routes:signal-only:start-returned", "destination:prepared", "destination:stop-returned", "destination:delivery-limits-migrated", "destination:files", "destination:published", "destination:start-returned", "destination:complete", "destination:native:delivery-limits-migrated", "destination:native:start-returned", "enrollment:signal-only:prepared", "enrollment:signal-only:stop-returned", "enrollment:signal-only:delivery-limits-migrated", "enrollment:signal-only:files", "enrollment:signal-only:published", "enrollment:signal-only:start-returned", "enrollment:signal-only:complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			kind := "limits"
			if strings.HasPrefix(boundary, "enrollment:") {
				kind = "provider-enrollment"
				boundary = strings.TrimPrefix(boundary, "enrollment:")
			}
			if strings.HasPrefix(boundary, "destination:") {
				kind = "destination"
				boundary = strings.TrimPrefix(boundary, "destination:")
			}
			if strings.HasPrefix(boundary, "routes:") {
				kind = "routes"
				boundary = strings.TrimPrefix(boundary, "routes:")
			}
			if strings.HasPrefix(boundary, "producer:") {
				kind = "producer"
				boundary = strings.TrimPrefix(boundary, "producer:")
			}
			spec, i, services, c, before, q := deliveryFixture(t)
			qualificationHash := protocol.Hash(q)
			signalOnly := strings.HasPrefix(boundary, "signal-only:")
			boundary = strings.Replace(boundary, "signal-only:", "native:", 1)
			nativeCase := strings.HasPrefix(boundary, "native:")
			boundary = strings.TrimPrefix(boundary, "native:")
			var native FreshSignalManifest
			if nativeCase {
				spec, i, c, native = freshSignalGatewayFixture(t, false, !signalOnly)
				services = i.services.(*fakeDeliveryServices)
				var e error
				before, e = i.ensureVerification(ctx, spec)
				testutil.Must(t, e)
				qualificationHash = qualifyPausedDirectDelivery(t, before).QualificationHash
			}

			i.deliveryLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.enrolledLimitsProbe = i.deliveryLimitsProbe
			active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), qualificationHash, false)
			testutil.Must(t, err)
			producer := c.Worker.Producers[0].Producer
			s, err := openManagedStore(active, c)
			testutil.Must(t, err)
			testutil.Must(t, s.Activate(producer, "one"))
			now := time.Now().UTC()
			msg := protocol.Message{Producer: producer, Sequence: 1, ID: "retained-during-budgets", Generation: "one", Subject: "subject", Kind: "report", Severity: "info", ObservedAt: now, ExpiresAt: now.Add(time.Minute), Text: "pending report", Route: "ops", ReportKey: "day", ReportType: "daily", ReportRevision: 1, BasisRevision: 1}
			var queuedHash string
			if kind != "routes" && kind != "destination" && kind != "provider-enrollment" {
				queued, e := s.Enqueue(ctx, msg)
				testutil.Must(t, e)
				queuedHash = protocol.Hash(queued)
			}
			testutil.Must(t, s.Close())
			originalProfile, err := InspectManagedReportProfile(spec.Root, active.Identity, producer, "ops")
			testutil.Must(t, err)
			current := active
			for cycle := 0; cycle < 2; cycle++ {
				raw, err := os.ReadFile(spec.RuntimeFile)
				testutil.Must(t, err)
				var input RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes))
				if kind == "provider-enrollment" {
					if cycle == 0 {
						token := filepath.Join(filepath.Dir(spec.RuntimeFile), "added-token")
						chat := filepath.Join(filepath.Dir(spec.RuntimeFile), "added-chat")
						testutil.Must(t, os.WriteFile(token, []byte("123:added-token"), 0600))
						testutil.Must(t, os.WriteFile(chat, []byte("900"), 0600))
						input.Providers["telegram"] = provider.Profile{Kind: "telegram", TokenFile: token, ChatFile: chat, Destination: provider.TelegramIdentity("123:added-token", "900")}
					} else {
						delete(input.Providers, "telegram")
					}
				} else if kind == "destination" {
					p := input.Providers["telegram"]
					p.ChatFile = filepath.Join(filepath.Dir(spec.RuntimeFile), fmt.Sprintf("destination-%d", cycle))
					testutil.Must(t, os.WriteFile(p.ChatFile, []byte(fmt.Sprintf("%d", 900+cycle)), 0600))
					token, e := provider.ReadSecret(p.TokenFile)
					testutil.Must(t, e)
					p.Destination = provider.TelegramIdentity(token, fmt.Sprintf("%d", 900+cycle))
					input.Providers["telegram"] = p
				} else if kind == "routes" {
					input.Worker.Routes = maps.Clone(input.Worker.Routes)
					route := input.Worker.Routes["ops"]
					route.Revision = fmt.Sprintf("audit-%d", cycle)
					input.Worker.Routes[fmt.Sprintf("audit%d", cycle)] = route
				} else if kind == "producer" {
					input.Worker.Producers[0].MaxMessages += 10
					input.Worker.Producers[0].MaxSilenceSeconds += 60
					input.Worker.Producers[0].MaxEvidenceSeconds += 3600
				} else {
					input.Worker.MaxMessages += 10
				}
				raw, err = input.Bytes()
				testutil.Must(t, err)
				next, err := input.Resolve()
				testutil.Must(t, err)
				review, err := i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(current), raw, kind)
				testutil.Must(t, err)
				if protocol.Hash(review.Before) != protocol.Hash(current) {
					t.Fatal("review changed owner")
				}
				paused, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(current), "", false)
				testutil.Must(t, err)
				testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
				lost := errors.New("lost budget change reply")
				i.fault = func(at string) error {
					if at == boundary {
						return lost
					}
					return nil
				}
				if _, err = i.changePausedRuntime(ctx, spec, protocol.Hash(paused), kind); !errors.Is(err, lost) {
					t.Fatal("missed migration boundary", err)
				}
				i.fault = nil
				changed, err := i.changePausedRuntime(ctx, spec, protocol.Hash(paused), kind)
				testutil.Must(t, err)
				starts, stops := services.starts, services.stops
				repeated, err := i.changePausedRuntime(ctx, spec, protocol.Hash(paused), kind)
				testutil.Must(t, err)
				observed, complete, err := i.inspectPausedRuntimeChange(ctx, spec, protocol.Hash(paused), kind)
				testutil.Must(t, err)
				if !complete || protocol.Hash(observed) != protocol.Hash(changed) || protocol.Hash(repeated) != protocol.Hash(changed) || starts != services.starts || stops != services.stops {
					t.Fatal("completed retry replayed service effects")
				}
				if changed.Mode != "verification" || changed.DeliveryOwnerHash != active.DeliveryOwnerHash || changed.Identity != active.Identity || changed.DeliveryOwnerConfigHash != protocol.Hash(c.Worker) {
					t.Fatal("limits replaced original owner")
				}
				s, err = openManagedStore(changed, next)
				testutil.Must(t, err)
				if kind != "routes" && kind != "destination" && kind != "provider-enrollment" {
					got, e := s.Get(ctx, producer, msg.ID)
					testutil.Must(t, e)
					if protocol.Hash(got) != queuedHash {
						t.Fatal("changed queued work")
					}
				}
				status, err := s.OwnershipStatus(producer, "one")
				testutil.Must(t, err)
				testutil.Must(t, s.Close())
				if !status.Active {
					t.Fatal("limits changed pending work or generation")
				}
				admission := qualifyPausedDirectDelivery(t, changed)
				current, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(changed), admission.QualificationHash, false)
				testutil.Must(t, err)
				_, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(current), protocol.Hash(active))
				testutil.Must(t, err)
				_, err = InspectCapturedReportProfile(ctx, spec.Root, protocol.Hash(current), originalProfile)
				testutil.Must(t, err)
				if nativeCase {
					got, err := InspectFreshSignal(ctx, *current.FreshSignal, false)
					testutil.Must(t, err)
					if protocol.Hash(got) != protocol.Hash(native) {
						t.Fatal("budgets replaced native Signal ownership")
					}
				}

			}
		})
	}
}
