package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestFreshSignalRemovalResumesAndRetainsAccount(t *testing.T) {
	binary := freshSignalTestBinary(t)
	for _, phase := range []string{"remove-intent", "removing", "stop-returned", "disable-returned", "unit-removed", "removed"} {
		t.Run(phase, func(t *testing.T) {
			s, i, manager := freshSignalFixture(t, binary)
			ctx := context.Background()
			m, err := i.setup(ctx, s)
			testutil.Must(t, err)
			i.fault = func(p string) error {
				if p == phase {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err = i.uninstall(ctx, m.reference()); err == nil {
				t.Fatal("removal did not interrupt")
			}
			if _, err = i.setup(ctx, s); err == nil {
				t.Fatal("removal intent permitted setup")
			}
			if _, err = InspectFreshSignal(ctx, m.reference(), false); err == nil {
				t.Fatal("removed service admitted as live dependency")
			}
			i.fault = nil
			_, err = i.uninstall(ctx, m.reference())
			testutil.Must(t, err)
			stops, disables, reloads := manager.stops, manager.disables, manager.reloads
			_, err = i.uninstall(ctx, m.reference())
			testutil.Must(t, err)
			if manager.stops != stops || manager.disables != disables || manager.reloads != reloads || manager.starts != 1 {
				t.Fatal("completed removal replayed manager action")
			}
			if _, err = os.Stat(filepath.Join(s.UnitDirectory, freshSignalUnit)); !os.IsNotExist(err) {
				t.Fatal("unit retained", err)
			}
			testutil.Must(t, relay.InspectFreshSignalInbox(s.Inbox))
			testutil.Must(t, signalaccount.InspectNativeService(s.AccountState, m.binding()))
			if _, err = signalaccount.AcquireRuntime(s.AccountState); err == nil {
				t.Fatal("ordinary account runtime admitted after service removal")
			}
			_, j, err := inspectRetainedFreshSignal(m.reference())
			testutil.Must(t, err)
			if j.Phase != "removed" {
				t.Fatal(j.Phase)
			}
			// Losing removal authority may not reinitialize the service.
			testutil.Must(t, os.Remove(filepath.Join(s.Root, "remove.json")))
			if _, err = i.uninstall(ctx, m.reference()); err == nil {
				t.Fatal("lost removal authority accepted")
			}
			if _, err = i.setup(ctx, s); err == nil {
				t.Fatal("removed service recreated")
			}
		})
	}
}

func TestFreshSignalRemovalRefusesForeignUnitBeforeMutation(t *testing.T) {
	s, i, manager := freshSignalFixture(t, freshSignalTestBinary(t))
	m, err := i.setup(context.Background(), s)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(filepath.Join(s.UnitDirectory, freshSignalUnit), []byte("foreign"), 0600))
	if _, err = i.uninstall(context.Background(), m.reference()); err == nil {
		t.Fatal("foreign unit accepted")
	}
	if manager.stops != 0 || manager.disables != 0 {
		t.Fatal("foreign unit touched")
	}
	if _, err = os.Stat(filepath.Join(s.Root, "remove.json")); !os.IsNotExist(err) {
		t.Fatal("removal intent published before ownership check", err)
	}
}
