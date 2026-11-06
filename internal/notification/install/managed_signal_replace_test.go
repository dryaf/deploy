package install

import (
	"context"
	"errors"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestManagedSignalVerifierReplacement(t *testing.T) {
	for _, profiles := range []bool{false, true} {
		t.Run(map[bool]string{false: "canonical", true: "profiles"}[profiles], func(t *testing.T) {
			spec, installer, manager, serviceManager, before := managedSignalProfilesFixture(t, true)
			ctx := context.Background()
			if profiles {
				var err error
				before, err = installer.prepare(ctx, "migrate-signal-profiles", spec)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, original := effectiveRuntimeForTest(t, before)
			state := activationTreeHash(t, spec.State)
			telegram := managedTelegramReceiptForTest(t, before)
			si := signalServiceInstaller{manager: serviceManager, probe: func(context.Context, *safefs.Directory, SignalServiceManifest) error { return nil }}
			for generation := 0; generation < 2; generation++ {
				prior, err := inspectSignalService(before.SignalVerification.SignalVerificationReference)
				if err != nil {
					t.Fatal(err)
				}
				if err = manager.Stop(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err = si.uninstall(ctx, prior.Spec.Root); err != nil {
					t.Fatal(err)
				}
				next, err := si.setup(ctx, replacementServiceSpec(t, prior))
				if err != nil {
					t.Fatal(err)
				}
				// Ordinary update cannot silently adopt a changed verifier.
				if _, err = installer.prepare(ctx, "update", spec); err == nil {
					t.Fatal("implicit replacement accepted")
				}
				ref := next.Spec.Endpoint
				ref.ServiceRoot, ref.ServiceHash = next.Spec.Root, next.ManifestHash
				spec.SignalVerification = &ref
				installer.signalCheck = func(ctx context.Context, _ ManagedManifest) error { return serviceManager.Verify(ctx, next) }
				stop := errors.New("interrupt gateway rebind")
				installer.fault = func(at string) error {
					if at == "manifest-published" {
						return stop
					}
					return nil
				}
				if _, err = installer.prepare(ctx, "supervise-signal", spec); !errors.Is(err, stop) {
					t.Fatal(err)
				}
				installer.fault = nil
				after, err := installer.resume(ctx, spec.Root)
				if err != nil {
					t.Fatal(err)
				}
				_, effective := effectiveRuntimeForTest(t, after)
				if protocol.Hash(original) != protocol.Hash(effective) || telegram != managedTelegramReceiptForTest(t, after) || state != activationTreeHash(t, spec.State) {
					t.Fatal("replacement changed state, profiles or Telegram renewal")
				}
				before = after
				spec.SignalVerification = nil
				before, err = installer.prepare(ctx, "update", spec)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := installer.uninstall(ctx, spec.Root); err != nil {
				t.Fatal(err)
			}
			if _, err := si.uninstall(ctx, before.SignalVerification.ServiceRoot); err != nil {
				t.Fatal(err)
			}
			if _, err := installer.resume(ctx, spec.Root); err != nil {
				t.Fatal("retired replacement lineage", err)
			}
		})
	}
}
