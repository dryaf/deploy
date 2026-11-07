package workerctl

import (
	"context"
	"errors"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestNotificationUninstallWaitsForSignalRemoval(t *testing.T) {
	ctx := context.Background()
	m := install.ManagedManifest{Root: "/gateway", Mode: "verification", FreshSignal: &install.FreshSignalReference{Root: "/signal", ManifestHash: "fixture"}}
	expected := protocol.Hash(m)
	removedSignal, stoppedGateway := false, false
	removes := 0
	s := notificationUninstallStep{root: m.Root, expected: expected,
		inspect: func(context.Context, string) (install.InstallationObservation, error) {
			return install.InstallationObservation{Manifest: m, Phase: "complete", Unbound: true}, nil
		},
		remove: func(_ context.Context, root, receipt string) (install.ManagedManifest, error) {
			if receipt != expected {
				t.Fatal("receipt changed")
			}
			m.Mode, stoppedGateway = "uninstalled", true
			return m, nil
		},
		inspectSignal: func(_ context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, bool, error) {
			if ref != *m.FreshSignal {
				t.Fatal("child reference changed")
			}
			return install.FreshSignalManifest{}, removedSignal, nil
		},
		removeSignal: func(_ context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, error) {
			if !stoppedGateway {
				t.Fatal("Signal stopped before gateway")
			}
			removes++
			if removes == 1 {
				return install.FreshSignalManifest{}, errors.New("interrupted child removal")
			}
			removedSignal = true
			return install.FreshSignalManifest{}, nil
		},
	}
	if err := s.Run(ctx, "op"); err == nil {
		t.Fatal("interruption hidden")
	}
	if _, done, err := s.Inspect(ctx, "op"); err != nil || done {
		t.Fatal("partial uninstall claimed complete", done, err)
	}
	testutil.Must(t, s.Run(ctx, "op"))
	if receipt, done, err := s.Inspect(ctx, "op"); err != nil || !done || receipt != protocol.Hash(m) {
		t.Fatal("completed uninstall not recognized", done, err)
	}
}
