//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDeliveryDerivedFreshnessResumesPublication(t *testing.T) {
	for _, point := range []string{"scope-intent", "backup-gated", "capture-stopped", "report-stopped", "gateway-complete", "capture-staged", "archive-checkpoint", "runtime-0", "runtime-1", "scope-receipt", "scope-complete"} {
		t.Run(point, func(t *testing.T) {
			home, r, before, deps, services, profile := providerScopeFixture(t)
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			original, err := loadInstalledWorker(home)
			testutil.Must(t, err)
			paths, err := declarationPaths(home, before.Declaration)
			testutil.Must(t, err)
			container := original
			container.Sources = slices.Clone(original.Sources)
			container.Policy = containerNotificationPolicy(container.Policy)
			for i := range container.Sources {
				container.Sources[i].KeyFile = "/keys/" + container.Sources[i].Alias + ".key"
				container.Sources[i].KnownHosts = "/keys/" + container.Sources[i].Alias + ".known_hosts"
			}
			raw, err := json.Marshal(container)
			testutil.Must(t, err)
			name := paths.OwnedPath(paths.ContainerRuntime())
			testutil.Must(t, root.Mkdir(filepath.Dir(name)))
			testutil.Must(t, os.WriteFile(filepath.Join(home, name), raw, 0600))
			before.OwnedFiles[name] = sharedSetupDigest(raw)
			for _, name := range []string{".config/containers/systemd/deploy-backup.container", ".config/systemd/user/deploy-backup.timer", ".config/systemd/user/deploy-backup-health.timer", ".config/systemd/user/deploy-backup-health.service", ".config/systemd/user/deploy-backup-maintenance.timer", ".config/systemd/user/deploy-backup-maintenance.service"} {
				if _, exists := before.OwnedFiles[name]; exists {
					continue
				}
				body := []byte("synthetic owned " + name)
				testutil.Must(t, root.Mkdir(filepath.Dir(name)))
				testutil.Must(t, os.WriteFile(filepath.Join(home, name), body, 0600))
				before.OwnedFiles[name] = sharedSetupDigest(body)
			}
			testutil.Must(t, root.WriteJSON(backupSetupFile, before, true))
			r.Desired.Notification.Providers = before.Declaration.Desired.Notification.Providers
			r.Desired.Backup.Freshness = "48h"
			deps.credentials = slices.Clone(profile.Credentials)
			stopped := false
			units, err := freshnessCaptureUnits(before)
			testutil.Must(t, err)
			deps.capture = backupActivityDependencies{
				inspect: func(_ context.Context, _ string, got []backupActivityUnit) (bool, error) {
					if !reflect.DeepEqual(got, units) {
						t.Fatal("capture ownership changed")
					}
					return stopped, nil
				},
				stop: func(context.Context, string, []backupActivityUnit) error { stopped = true; return nil },
			}
			cutover, err := os.ReadFile(filepath.Join(paths.State, backupCutoverFile))
			testutil.Must(t, err)
			gateway := notify.Hash("revised evidence deadline")
			migrate := func(p ProducerReservation) (string, error) {
				if p != *before.Producer || !stopped || services.active {
					t.Fatal("gateway changed before capture/report fence")
				}
				if s, e := openInstalledBackupState(home, false); e == nil {
					s.Close()
					t.Fatal("runtime gate missing")
				}
				profile.ManifestHash = gateway
				return gateway, nil
			}
			lost := errors.New("lost freshness reply")
			_, err = reconcileNotificationScopeReports(context.Background(), home, r, migrate, 23, deps, func(at string) error {
				if at == point {
					return lost
				}
				return nil
			})
			if !errors.Is(err, lost) {
				t.Fatal("boundary missed", point, err)
			}
			scope, err := r.Desired.NotificationFingerprint()
			testutil.Must(t, err)
			if _, done, e := InspectRetainedDeliveryPolicyScope(home, r.Operation, r.BeforeGateway, scope); e != nil || done {
				t.Fatal("partial scope completed", done, e)
			}
			for range 2 {
				_, err = reconcileNotificationScopeReports(context.Background(), home, r, migrate, 23, deps)
				testutil.Must(t, err)
			}
			var after WorkerSetupJournal
			testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
			current, err := loadInstalledWorker(home)
			testutil.Must(t, err)
			if current.ConfigHash == original.ConfigHash || current.Policy.Freshness != "48h" || !reflect.DeepEqual(current.Sources, original.Sources) || current.WorkerID != original.WorkerID || current.ArchiveID != original.ArchiveID || !reflect.DeepEqual(before.Producer, after.Producer) || before.Shared.Owner != after.Shared.Owner || !services.active || !stopped {
				t.Fatal("freshness lost identity, reporting, or staged capture")
			}
			for name, digest := range after.OwnedFiles {
				body, e := readBackupOwnedFile(root, name, safefs.MaxRecordBytes)
				testutil.Must(t, e)
				if sharedSetupDigest(body) != digest {
					t.Fatal("lost owned file", name)
				}
			}
			state, err := openInstalledBackupState(home, false)
			testutil.Must(t, err)
			var activity string
			testutil.Must(t, state.get("installation", &activity))
			testutil.Must(t, requireWorkerSetupOwner(state, &after))
			state.Close()
			if activity != "staged-awaiting-qualification" {
				t.Fatal("capture was reactivated")
			}
			_, err = commandScopeOrigin(root, after, before.Declaration.NotificationDesiredHash)
			testutil.Must(t, err)
			projected, err := reportingScopeRuntime(root, after, current, before.Declaration.NotificationDesiredHash)
			testutil.Must(t, err)
			if !reflect.DeepEqual(projected, original) {
				t.Fatal("reporting projection changed original runtime")
			}
			changed := current
			changed.Policy.Schedule = "weekly"
			if _, e := reportingScopeRuntime(root, after, changed, before.Declaration.NotificationDesiredHash); e == nil {
				t.Fatal("unrelated runtime edit accepted as freshness")
			}
			retained, err := os.ReadFile(filepath.Join(paths.State, backupCutoverFile))
			testutil.Must(t, err)
			if string(retained) != string(cutover) {
				t.Fatal("original cutover rewritten")
			}
			if receipt, done, e := InspectRetainedDeliveryPolicyScope(home, r.Operation, r.BeforeGateway, scope); e != nil || !done || receipt != gateway {
				t.Fatal("completed scope unavailable", done, e)
			}
		})
	}
}
