package workerctl

import (
	"context"
	"github.com/dryaf/deploy/internal/config"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestCommandReportSelectionWithExistingPermissionRetainsDelivery(t *testing.T) {
	for _, mode := range []string{"read", "mutation"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newCommandPolicyFixture(t, mode, "command-report-route-declaration")
			original, receiver := protocol.Hash(f.o.Manifest), protocol.Hash(f.receiver)
			b := *f.r.Desired.Backup
			f.r.Desired.Backup = &b
			b.Notifications.Route = "audit"
			f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
			for range 2 {
				result, handled, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
				testutil.Must(t, err)
				if !handled || result.State != "commands_disabled" {
					t.Fatal(result)
				}
			}
			if protocol.Hash(f.o.Manifest) != original || f.pauses != 0 || f.rotations != 0 {
				t.Fatal("allowed report selection changed runtime or qualification")
			}
			testutil.Must(t, verifyDeliveryProviderReceipt(f.d, f.journal(t), f.o.Manifest, f.provider.scopeInspect))
			f.toggle(t, true)
			_, _, err := reconcileCommandActivity(ctx, f.r, f.paths, f.commands)
			testutil.Must(t, err)
			if protocol.Hash(f.receiver) != receiver {
				t.Fatal("report selection replaced receiver")
			}
		})
	}
}

func TestCommandRoutingHistoryAlternatesWithProducerAndBudgets(t *testing.T) {
	for _, mode := range []string{"read", "mutation"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newCommandPolicyFixture(t, mode, "command-routes")
			receiver, owner := protocol.Hash(f.receiver), f.o.Manifest.DeliveryOwnerHash
			for _, kind := range []string{"command-routes", "command-derived-freshness", "command-limits", "command-producer"} {
				f.provider.kind = kind
				if kind == "command-producer" {
					f.producerPolicy(t, func(p *config.ProducerDeclaration) { p.MaxMessages = 1400 })
				} else {
					f.policyUpdate(t, kind, 1600)
				}
				_, handled, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
				testutil.Must(t, err)
				if !handled {
					t.Fatal("unhandled", kind)
				}
				f.ready = true
				_, handled, err = reconcileCommandRequalification(ctx, f.r, f.paths, f.renewal)
				testutil.Must(t, err)
				if !handled {
					t.Fatal("renewal lost", kind)
				}
				if protocol.Hash(f.receiver) != receiver || f.o.Manifest.DeliveryOwnerHash != owner {
					t.Fatal("history replaced original receiver/owner")
				}
			}
		})
	}
}
