package workerctl

import (
	"context"
	"errors"
	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSharedActivityCoordinatorRetriesAllPhasesAndPinsFullDeclaration(t *testing.T) {
	for _, phase := range []string{"prepared", "led-off", "paused", "service", "qualified", "led-restored", "complete"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			f := newMappingFixture(t)
			f.r.Inputs = nil
			f.o.Manifest.Mode = "delivery"
			f.o.Manifest.DeliveryOwnerHash = protocol.Hash("original owner")
			f.o.Manifest.DeliveryActivation = &install.ManagedDeliveryActivation{Operation: testutil.ID(t, "n"), Hash: protocol.Hash("original qualification")}
			f.r.DesiredScope = 1
			f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
			testutil.Must(t, retainDeclaration(f.d, f.r.Desired, f.r.DesiredHash, 1))
			j := f.journal(t)
			j.Pins.DesiredScope, j.Pins.DesiredHash = 1, f.r.DesiredHash
			j.Children[0].Name = "notification-delivery"
			j.Children[0].Receipt = protocol.Hash(f.o.Manifest)
			testutil.Must(t, f.d.WriteJSON("operation.json", j, true))
			initial := f.o.Manifest
			original := cloneSharedPlanPolicy(t, f.r.Desired)
			currentPolicy := original
			currentHash, _ := currentPolicy.Fingerprint()
			stage := "complete"
			fenced, ready := false, phase == "qualified" || phase == "led-restored" || phase == "complete"
			faulted := false
			lost := errors.New("lost coordinator phase reply")
			l := sharedActivityLifecycle{inspect: f.inspect, status: func(context.Context, string) (backup.SetupObservation, error) {
				return backup.SetupObservation{Stage: stage, DesiredHash: currentHash}, nil
			}, inputs: func(ControlRequest, ResolvedPaths, *safefs.Directory, config.DesiredWorkers) error { return nil }, boundary: func(at string) error {
				if at == phase && !faulted {
					faulted = true
					return lost
				}
				return nil
			}}
			check := func(expected string) {
				t.Helper()
				if !fenced || expected != protocol.Hash(f.o.Manifest) {
					t.Fatal("gateway changed before the dependency fence")
				}
			}
			l.pause = func(_ context.Context, r install.DeliveryPauseRequest) (install.ManagedManifest, error) {
				check(r.ManifestHash)
				f.o.Manifest.Mode = "verification"
				f.o.Manifest.DeliveryActivation = nil
				return f.o.Manifest, nil
			}
			l.service = func(_ context.Context, root, expected string, disabled bool) (install.ManagedManifest, error) {
				check(expected)
				if root != f.paths.Installation {
					t.Fatal("service retargeted")
				}
				f.o.Manifest.Mode = "verification"
				if disabled {
					f.o.Manifest.Mode = "uninstalled"
				}
				return f.o.Manifest, nil
			}
			l.output = func(_ context.Context, root, expected string, disabled bool) (install.ManagedManifest, error) {
				check(expected)
				f.o.Manifest.LEDOutput = &install.ManagedLEDOutput{Operation: testutil.ID(t, "n"), Disabled: disabled}
				return f.o.Manifest, nil
			}
			l.review = func(_ context.Context, root, hash string) (install.DeliveryAdmissionRequest, bool, error) {
				check(hash)
				return install.DeliveryAdmissionRequest{Root: root, ManifestHash: hash, QualificationHash: protocol.Hash("fresh evidence")}, ready, nil
			}
			l.ensure = func(_ context.Context, r install.DeliveryAdmissionRequest) (install.ManagedManifest, error) {
				check(r.ManifestHash)
				f.o.Manifest.Mode = "delivery"
				f.o.Manifest.DeliveryActivation = &install.ManagedDeliveryActivation{Operation: testutil.ID(t, "n"), Hash: r.QualificationHash}
				return f.o.Manifest, nil
			}
			l.reports = func(_ context.Context, home, op, hash string) error {
				if f.o.Manifest.Mode != "delivery" || hash != protocol.Hash(f.o.Manifest) {
					t.Fatal("reports started before renewed delivery")
				}
				return nil
			}
			pins := map[string]backup.SharedActivityRequest{}
			l.reconcile = func(_ context.Context, _ string, desired config.DesiredWorkers, _ string, r backup.SharedActivityRequest, gateway func() error) (backup.ActivityResult, error) {
				if old, exists := pins[r.Operation]; exists && old != r {
					t.Fatal("lost reply changed backup operation")
				}
				pins[r.Operation] = r
				fenced = true
				stage = "activity_pending"
				if e := gateway(); e != nil {
					return backup.ActivityResult{}, e
				}
				stage = "complete"
				currentPolicy = desired
				currentHash, _ = desired.Fingerprint()
				return backup.ActivityResult{Handled: true}, nil
			}
			for _, disabled := range []bool{true, false} {
				f.r.Desired = cloneSharedPlanPolicy(t, original)
				flag := !disabled
				f.r.Desired.Notification.Enabled = &flag
				f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
				faulted = false
				_, handled, err := reconcileSharedActivity(ctx, f.r, f.paths, l)
				if !handled || !errors.Is(err, lost) || !faulted {
					t.Fatal("phase not reached", phase, disabled, err)
				}
				l.boundary = nil
				result, handled, err := reconcileSharedActivity(ctx, f.r, f.paths, l)
				testutil.Must(t, err)
				if !handled {
					t.Fatal("shared retry escaped lifecycle")
				}
				if !disabled && result.State == "needs_qualification" {
					retained := f.journal(t)
					changed := f.r
					changed.Desired = cloneSharedPlanPolicy(t, f.r.Desired)
					changed.Desired.Backup.MaxSnapshotBytes++
					changed.DesiredHash, _ = changed.Desired.NotificationFingerprint()
					if _, _, e := reconcileSharedActivity(ctx, changed, f.paths, l); e == nil {
						t.Fatal("same notification fingerprint accepted a changed backup policy")
					}
					if !reflect.DeepEqual(retained, f.journal(t)) {
						t.Fatal("changed retry replaced coordinator")
					}
					ready = true
					_, _, err = reconcileSharedActivity(ctx, f.r, f.paths, l)
					testutil.Must(t, err)
				}
				for range 2 {
					_, handled, err = reconcileSharedActivity(ctx, f.r, f.paths, l)
					testutil.Must(t, err)
					if !handled {
						t.Fatal("completed retry lost activity")
					}
				}
				l.boundary = func(at string) error {
					if at == phase && !faulted {
						faulted = true
						return lost
					}
					return nil
				}
			}
			if f.o.Manifest.Identity != initial.Identity || f.o.Manifest.DeliveryOwnerHash != initial.DeliveryOwnerHash || f.o.Manifest.Mode != "delivery" {
				t.Fatal("coordinator replaced identity/owner")
			}
			final := f.journal(t)
			_, err := sharedActivityPredecessor(f.d, final)
			testutil.Must(t, err)
			// Missing typed history cannot be inferred from matching runtime state.
			testutil.Must(t, os.Remove(filepath.Join(f.paths.Control, "shared-activity-"+final.Children[0].Operation+".json")))
			if _, _, err = reconcileSharedActivity(ctx, f.r, f.paths, l); err == nil {
				t.Fatal("missing shared activity receipt was adopted")
			}
		})
	}
}

func TestSharedActivityAcceptsOnlyIdenticalRetainedOperatorInputs(t *testing.T) {
	f := newMappingFixture(t)
	testutil.Must(t, verifySharedActivityProvidedInputs(context.Background(), f.r, f.paths))
	changed := f.r
	changed.Inputs = map[string][]byte{"token": []byte("999:changed-private-input")}
	if verifySharedActivityProvidedInputs(context.Background(), changed, f.paths) == nil {
		t.Fatal("activity accepted a new credential value")
	}
}

func TestSharedActivityStopNeedsNoLingerSourceToolsOrImage(t *testing.T) {
	for _, field := range []string{"gateway", "backup", "reporting"} {
		for _, kind := range []string{"notification-worker", "backup-worker"} {
			t.Run(field+"/"+kind, func(t *testing.T) {
				d := sharedPlanFixture(t)
				off := false
				switch field {
				case "gateway":
					d.Notification.Enabled = &off
				case "backup":
					d.Backup.Enabled = &off
				case "reporting":
					d.Backup.Notifications.Enabled = &off
				}
				helper := elfFixture(t, "arm64")
				artifacts := config.WorkerArtifacts{Helper: helper.File, HelperSHA256: helper.SHA256}
				d.Notification.Artifacts, d.Backup.Artifacts = artifacts, artifacts
				facts := testFacts()
				facts.Linger = false
				facts.Tools = map[string]bool{"systemctl": true}
				p, err := ResolvePlan(Plan{Worker: kind, Server: d.Backup.Notifications.Worker}, config.ServerConfig{Desired: &d}, facts, "")
				testutil.Must(t, err)
				for _, missing := range p.Missing {
					if missing == "target.linger" || missing == "backup_worker.artifacts.image" || strings.HasPrefix(missing, "target.tools.") {
						t.Fatal("shared stop requires activation prerequisites", p.Missing)
					}
				}
				if !sharedActivityStopping(d, d.Backup.Notifications.Worker) {
					t.Fatal("shared stop was not recognized")
				}
			})
		}
	}
}

func TestSharedActivityDelegatesAnActuallyUnboundGateway(t *testing.T) {
	f := newMappingFixture(t)
	f.o.Unbound = true
	f.r.Inputs = nil
	f.r.DesiredScope = 1
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
	testutil.Must(t, retainDeclaration(f.d, f.r.Desired, f.r.DesiredHash, 1))
	j := f.journal(t)
	j.Pins.DesiredScope, j.Pins.DesiredHash = 1, f.r.DesiredHash
	testutil.Must(t, f.d.WriteJSON("operation.json", j, true))
	f.r.Desired = cloneSharedPlanPolicy(t, f.r.Desired)
	off := false
	f.r.Desired.Notification.Enabled = &off
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
	_, handled, err := reconcileSharedActivity(context.Background(), f.r, f.paths, sharedActivityLifecycle{inspect: f.inspect})
	testutil.Must(t, err)
	if handled || !reflect.DeepEqual(j, f.journal(t)) {
		t.Fatal("a declaration adopted an unbound gateway as shared activity")
	}
}
