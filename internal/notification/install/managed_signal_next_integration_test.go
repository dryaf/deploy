//go:build notificationintegration

package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeManagedSignalRepeatedRecovery(t *testing.T) {
	testManagedSignalRepeatedRecovery(t, func(spec *ManagedSpec, s *SignalServiceSpec, i *managedInstaller, f *fakeManagedServices, endpoint SignalVerificationManifest) signalServiceInstaller {
		spec.BinarySHA256 = managedDigest(buildSignalCommandFixture(t, "./cmd/deploy", spec.BinaryFile))
		s.BinarySHA256 = spec.BinarySHA256
		cli := filepath.Join(t.TempDir(), "signal-cli")
		source := filepath.Join(t.TempDir(), "signal.go")
		if err := os.WriteFile(source, []byte(nativeSignalCLIStub), 0600); err != nil {
			t.Fatal(err)
		}
		s.Runtime.CLIFile = cli
		s.Runtime.CLISHA256 = managedDigest(buildSignalCommandFixture(t, source, cli))
		c, err := decodeRuntime(endpoint.OriginalConfig)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(endpoint.Spec.AccountState, "test-account"), []byte(c.Signal.Account), 0600); err != nil {
			t.Fatal(err)
		}
		signalService := &executableSignalService{fakeSignalServiceManager: &fakeSignalServiceManager{units: s.UnitDirectory}, t: t}
		t.Cleanup(func() {
			if signalService.cmd != nil {
				_ = signalService.Stop(context.Background())
			}
		})
		native := &nativeDeliveryServices{executableManagedServices: &executableManagedServices{fakeManagedServices: f, root: spec.Root}, t: t}
		t.Cleanup(func() {
			if native.cmd != nil {
				_ = native.Stop(context.Background())
			}
		})
		i.services = native
		i.deliveryInspect = func(ctx context.Context, m ManagedManifest, stale bool) error {
			_, err := native.inspector(m).inspect(ctx, m, stale)
			return err
		}
		return signalServiceInstaller{manager: signalService}
	})
}
