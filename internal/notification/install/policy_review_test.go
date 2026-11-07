package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestPolicyReviewChecksOwnershipWithoutMutation(t *testing.T) {
	for _, damage := range []string{"", "runtime", "lost-pointer", "unknown-directory"} {
		t.Run(damage, func(t *testing.T) {
			spec, i, services, m := unboundUpdateFixture(t)
			switch damage {
			case "runtime":
				if err := os.WriteFile(m.Paths.Config, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "lost-pointer":
				if err := os.Remove(filepath.Join(spec.Root, "installation.json")); err != nil {
					t.Fatal(err)
				}
			case "unknown-directory":
				spec.Root = filepath.Join(filepath.Dir(spec.Root), "unknown-installation")
			}
			before := activationTreeHash(t, filepath.Dir(spec.Root))
			starts, stops := services.starts, services.stops
			got, err := i.inspectPolicyReview(context.Background(), spec.Root)
			if damage == "" {
				if err != nil || protocol.Hash(got.Manifest) != protocol.Hash(m) || !got.Input.Bootstrap {
					t.Fatal("review lost canonical authority", err)
				}
			} else if err == nil {
				t.Fatal("invalid installation exported")
			}
			if before != activationTreeHash(t, filepath.Dir(spec.Root)) || starts != services.starts || stops != services.stops {
				t.Fatal("review changed files or services")
			}
		})
	}
}

func TestPolicyReviewRetainsDeliveryAndCommandAdmission(t *testing.T) {
	spec, i, _, active, record := managedCommandsFixture(t)
	ctx := context.Background()
	m, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false)
	if err != nil {
		t.Fatal(err)
	}
	services := i.services.(*fakeDeliveryServices)
	starts, stops := services.starts, services.stops
	before := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
	for range 2 {
		got, e := i.inspectPolicyReview(ctx, spec.Root)
		if e != nil || protocol.Hash(got.Manifest) != protocol.Hash(m) || got.Input.Worker.Interaction == nil || got.Manifest.Commands.Mode != "read" {
			t.Fatal("import altered admission", e)
		}
	}
	if before != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) || starts != services.starts || stops != services.stops {
		t.Fatal("import mutated admitted installation")
	}
}

func TestPolicyReviewRetainsCanonicalNativeSignalManifest(t *testing.T) {
	spec, i, _, native := freshSignalGatewayFixture(t, true)
	ctx := context.Background()
	installed, err := i.ensureVerification(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	before := activationTreeHash(t, spec.Root, spec.State, native.Spec.Root, native.Spec.AccountState, native.Spec.Inbox.State)
	for range 2 {
		review, err := i.inspectPolicyReview(ctx, spec.Root)
		if err != nil || review.Native == nil || review.Signal == nil || protocol.Hash(review.Native) != protocol.Hash(native) || protocol.Hash(review.Signal) != protocol.Hash(native.Spec) || protocol.Hash(review.Manifest) != protocol.Hash(installed) {
			t.Fatal("native review changed ownership", err)
		}
	}
	if before != activationTreeHash(t, spec.Root, spec.State, native.Spec.Root, native.Spec.AccountState, native.Spec.Inbox.State) {
		t.Fatal("native review mutated retained files")
	}
}
