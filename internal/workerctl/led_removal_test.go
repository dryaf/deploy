package workerctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestNotificationRemovalWaitsForLEDAndResumesLostReplies(t *testing.T) {
	for _, failure := range []string{"daemon-unavailable", "cleanup-reply", "removal-reply", "none"} {
		t.Run(failure, func(t *testing.T) {
			r, paths := bootstrapControlFixture(t)
			r.Action, r.Inputs = "uninstall", nil
			d, err := ensureConfigDirectory(paths.Control)
			testutil.Must(t, err)
			defer d.Close()
			m := install.ManagedManifest{Mode: "verification", Root: paths.Installation, Identity: protocol.Identity{Worker: "fixture-gateway", Recovery: "initial"}}
			before := m
			j := OperationJournal{Version: 1, ID: testutil.ID(t, "n"), Complete: true,
				Pins:     OperationPins{Worker: r.Worker, Action: "apply", DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Artifacts: map[string]string{"helper": r.HelperSHA256}, Installations: map[string]string{"notification-worker": paths.Installation, "state": paths.State, "runtime": paths.Runtime}},
				Children: []ChildReference{{Name: "notification-bootstrap", Operation: testutil.ID(t, "n"), Complete: true, Receipt: protocol.Hash(m)}}}
			lock, err := d.Lock("operation.lock", true, true)
			testutil.Must(t, err)
			lock.Close()
			testutil.Must(t, d.WriteJSON("operation.json", j, false))
			inspect := func(context.Context, string) (install.InstallationObservation, error) {
				return install.InstallationObservation{Manifest: m, Phase: "complete", Unbound: true, LEDs: true}, nil
			}
			clears, removals, disableCalls := 0, 0, 0
			interrupted := false
			disable := func(_ context.Context, root, expected string, disabled bool) (install.ManagedManifest, error) {
				disableCalls++
				if root != paths.Installation || expected != protocol.Hash(before) || !disabled || removals != 0 {
					t.Fatal("cleanup authority/order changed")
				}
				if failure == "daemon-unavailable" && !interrupted {
					interrupted = true
					return m, errors.New("daemon unavailable")
				}
				m.LEDOutput = &install.ManagedLEDOutput{Operation: testutil.ID(t, "n"), Disabled: true}
				clears++
				if failure == "cleanup-reply" && !interrupted {
					interrupted = true
					return m, errors.New("cleanup response lost")
				}
				return m, nil
			}
			remove := func(_ context.Context, root, expected string) (install.ManagedManifest, error) {
				if clears != 1 || m.LEDOutput == nil || !m.LEDOutput.Disabled || expected != protocol.Hash(m) {
					t.Fatal("removal preceded owned-output cleanup")
				}
				m.Mode = "uninstalled"
				removals++
				if failure == "removal-reply" && !interrupted {
					interrupted = true
					return m, errors.New("removal response lost")
				}
				return m, nil
			}
			result, err := uninstallNotificationWithLED(context.Background(), r, paths, inspect, remove, disable)
			if failure != "none" {
				if err == nil {
					t.Fatal("lost outcome hidden")
				}
				status := controlFailure(err)
				if !strings.Contains(status.Next, "uninstall manager") {
					t.Fatal("wrong resume command", status)
				}
				if failure != "removal-reply" && (removals != 0 || status.State != "cleanup_pending") {
					t.Fatal("pending cleanup claimed removal", status, removals)
				}
				status, e := inspectNotificationControl(context.Background(), r, paths, inspect, nil)
				testutil.Must(t, e)
				if failure != "removal-reply" && !status.LEDOutputPending {
					t.Fatal("status lost pending LED cleanup", status)
				}
				result, err = uninstallNotificationWithLED(context.Background(), r, paths, inspect, remove, disable)
			}
			testutil.Must(t, err)
			if result.State != "uninstalled" || clears != 1 || removals != 1 {
				t.Fatal("removal did not converge exactly once", result, clears, removals)
			}
			calls := disableCalls
			for range 2 {
				_, err = uninstallNotificationWithLED(context.Background(), r, paths, inspect, remove, disable)
				testutil.Must(t, err)
			}
			if clears != 1 || removals != 1 || disableCalls != calls {
				t.Fatal("completed removal replayed cleanup")
			}
			status, err := inspectNotificationControl(context.Background(), r, paths, inspect, nil)
			testutil.Must(t, err)
			if status.State != "uninstalled" {
				t.Fatal("two-child removal lost canonical status", status)
			}
			var retained OperationJournal
			testutil.Must(t, d.ReadJSON("operation.json", &retained))
			cleanup := filepath.Join(paths.Control, "removal-led-"+retained.Children[0].Operation+".json")
			testutil.Must(t, os.Remove(cleanup))
			if _, err = inspectNotificationControl(context.Background(), r, paths, inspect, nil); err == nil {
				t.Fatal("status accepted missing cleanup proof")
			}
			if _, err = uninstallNotificationWithLED(context.Background(), r, paths, inspect, remove, disable); err == nil {
				t.Fatal("missing cleanup proof recreated")
			}
			if _, err = os.Stat(cleanup); !os.IsNotExist(err) || disableCalls != calls {
				t.Fatal("missing cleanup authority caused another mutation", err)
			}
		})
	}
}
