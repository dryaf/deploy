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
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func providerScopeFixture(t *testing.T, enrollment ...bool) (string, VerificationNotificationScopeChange, WorkerSetupJournal, *providerScopeDependencies, *credentialReportServices, *install.ManagedReportProfile) {
	return providerScopeFixtureWithSources(t, nil, enrollment...)
}
func providerScopeFixtureWithSources(t *testing.T, aliases []string, enrollment ...bool) (string, VerificationNotificationScopeChange, WorkerSetupJournal, *providerScopeDependencies, *credentialReportServices, *install.ManagedReportProfile) {
	t.Helper()
	n := &config.NotificationDeclaration{Providers: config.ProviderDeclarations{"telegram": {TokenFile: "~/.config/old-token", ChatIDFile: "~/.config/old-chat"}}}
	if len(enrollment) > 0 && enrollment[0] {
		n.Providers = config.ProviderDeclarations{"signal": {AccountFile: "~/.config/signal-account", RecipientFile: "~/.config/signal-recipient"}}
	}
	return providerScopeFixtureWithDeclaration(t, n, aliases)
}
func providerScopeFixtureWithDeclaration(t *testing.T, n *config.NotificationDeclaration, aliases []string) (string, VerificationNotificationScopeChange, WorkerSetupJournal, *providerScopeDependencies, *credentialReportServices, *install.ManagedReportProfile) {
	t.Helper()
	home, before, _, _ := sharedSourceExpansionFixtureWithNotification(t, n, aliases...)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var j WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &j))
	raw, err := json.Marshal(before.Declaration.Desired)
	testutil.Must(t, err)
	var desired config.DesiredWorkers
	testutil.Must(t, json.Unmarshal(raw, &desired))
	p := desired.Notification.Providers["telegram"]
	p.TokenFile, p.ChatIDFile = filepath.Join(home, "next-token"), filepath.Join(home, "next-chat")
	desired.Notification.Providers["telegram"] = p
	c, err := loadInstalledWorker(home)
	testutil.Must(t, err)
	profile := j.Shared.Profile
	credentials := append(slices.Clone(profile.Credentials), p.TokenFile, p.ChatIDFile)
	slices.Sort(credentials)
	credentials = slices.Compact(credentials)
	units, err := renderBackupReportUnits(backupCutoverPaths(home, c), c.Policy, profile.Credentials)
	testutil.Must(t, err)
	services := &credentialReportServices{t: t, paths: backupCutoverPaths(home, c), loaded: units, active: true}
	deps := &providerScopeDependencies{credentials: credentials, profile: func(root string, identity notify.Identity, producer notify.Producer, route string) (install.ManagedReportProfile, error) {
		if root != j.Shared.GatewayRoot || identity != j.Shared.Owner.Binding.Identity || producer != j.Producer.Producer || route != j.Declaration.NotificationRoute {
			t.Fatal("report profile inspection lost owner pins")
		}
		return profile, nil
	}, services: func(string, WorkerInstallation) reportCredentialServices { return services }}
	r := VerificationNotificationScopeChange{Operation: testutil.ID(t, "n"), BeforeScope: j.Declaration.NotificationDesiredHash, BeforeGateway: profile.ManifestHash, Desired: desired}
	return home, r, j, deps, services, &profile
}

func TestDeliveryProviderScopeResumesWithReportsFenced(t *testing.T) {
	for _, point := range []string{"signal:scope-intent", "signal:report-stopped", "signal:report-files", "signal:gateway-complete", "signal:scope-receipt", "signal:scope-complete", "signal-destination:scope-intent", "signal-destination:report-stopped", "signal-destination:gateway-complete", "signal-destination:scope-receipt", "scope-intent", "backup-gated", "report-stopped", "report-files", "report-reloaded", "gateway-complete", "scope-receipt", "scope-complete", "limits:scope-intent", "limits:backup-gated", "limits:report-stopped", "limits:gateway-complete", "limits:scope-receipt", "limits:scope-complete", "producer:scope-intent", "producer:backup-gated", "producer:report-stopped", "producer:report-files", "producer:report-reloaded", "producer:gateway-complete", "producer:scope-receipt", "producer:scope-complete", "routes:scope-intent", "routes:backup-gated", "routes:report-stopped", "routes:report-files", "routes:report-reloaded", "routes:gateway-complete", "routes:scope-receipt", "routes:scope-complete", "destination:scope-intent", "destination:backup-gated", "destination:report-stopped", "destination:report-files", "destination:report-reloaded", "destination:gateway-complete", "destination:scope-receipt", "destination:scope-complete", "enrollment:scope-intent", "enrollment:backup-gated", "enrollment:report-stopped", "enrollment:report-files", "enrollment:report-reloaded", "enrollment:gateway-complete", "enrollment:scope-receipt", "enrollment:scope-complete"} {
		t.Run(point, func(t *testing.T) {
			home, r, before, deps, services, profile := providerScopeFixture(t, strings.HasPrefix(point, "enrollment:") || strings.HasPrefix(point, "signal"))
			version := 16
			if strings.HasPrefix(point, "signal") {
				if strings.HasPrefix(point, "signal-destination:") {
					version = 20
				}
				point = strings.TrimPrefix(strings.TrimPrefix(point, "signal:"), "signal-destination:")
				p := before.Declaration.Desired.Notification.Providers["signal"]
				p.AccountFile, p.RecipientFile = filepath.Join(home, "next-account"), filepath.Join(home, "next-recipient")
				r.Desired.Notification.Providers = config.ProviderDeclarations{"signal": p}
				deps.credentials = append(slices.Clone(profile.Credentials), filepath.Join(home, ".config/signal-account"), filepath.Join(home, ".config/signal-recipient"), p.AccountFile, p.RecipientFile)
				slices.Sort(deps.credentials)
				deps.credentials = slices.Compact(deps.credentials)
			}
			if strings.HasPrefix(point, "enrollment:") {
				version = 21
				point = strings.TrimPrefix(point, "enrollment:")

			}

			if strings.HasPrefix(point, "destination:") {
				version = 20
				point = strings.TrimPrefix(point, "destination:")
			}
			if strings.HasPrefix(point, "routes:") {
				version = 19
				point = strings.TrimPrefix(point, "routes:")
				r.Desired.Notification.Providers = before.Declaration.Desired.Notification.Providers
				r.Desired.Notification.Routes = map[string]config.NotificationRoute{"alerts": {Primary: "telegram"}, "audit": {Primary: "telegram"}}
				deps.credentials = slices.Clone(profile.Credentials)
			}
			if strings.HasPrefix(point, "producer:") {
				version = 18
				point = strings.TrimPrefix(point, "producer:")
				r.Desired.Notification.Providers = before.Declaration.Desired.Notification.Providers
				r.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {Routes: []string{"alerts"}, MaxMessages: 1000, MaxSilenceSeconds: 300, MaxEvidenceSeconds: 172800, PolicyHealth: true}}
				deps.credentials = slices.Clone(profile.Credentials)
			}
			if strings.HasPrefix(point, "limits:") {
				version = 17
				point = strings.TrimPrefix(point, "limits:")
				r.Desired.Notification.Providers = before.Declaration.Desired.Notification.Providers
				r.Desired.Notification.Limits.MaxMessages = 9000
				deps.credentials = slices.Clone(profile.Credentials)
			}

			ctx := context.Background()
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			cutoverPath := filepath.Join(home, ".local/state/deploy/backup-worker/state", backupCutoverFile)
			original, err := os.ReadFile(cutoverPath)
			testutil.Must(t, err)
			gateway := notify.Hash("rotated verification gateway")
			migrate := func(p ProducerReservation) (string, error) {
				if p != *before.Producer || !services.stopped || services.active {
					t.Fatal("gateway changed before draining reporting")
				}
				if state, e := openInstalledBackupState(home, false); e == nil {
					state.Close()
					t.Fatal("runtime gate did not fence backup readers")
				}
				profile.ManifestHash, profile.Credentials = gateway, slices.Clone(deps.credentials)
				return gateway, nil
			}
			lost := errors.New("lost credential scope reply")
			_, err = reconcileNotificationScopeReports(ctx, home, r, migrate, version, deps, func(at string) error {
				if at == point {
					return lost
				}
				return nil
			})
			if !errors.Is(err, lost) {
				t.Fatal("missed scope boundary", point, err)
			}
			scope, err := r.Desired.NotificationFingerprint()
			testutil.Must(t, err)
			if _, done, e := InspectVerificationNotificationScope(home, r.Operation, r.BeforeGateway, scope); e != nil || done {
				t.Fatal("pending report scope appeared complete", done, e)
			}
			for range 2 {
				receipt, e := reconcileNotificationScopeReports(ctx, home, r, migrate, version, deps)
				testutil.Must(t, e)
				if receipt != gateway {
					t.Fatal("receipt changed")
				}
			}
			var after WorkerSetupJournal
			testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
			if !services.active || after.Producer == nil || *after.Producer != *before.Producer || !reflect.DeepEqual(before.SourceKeys, after.SourceKeys) || after.Shared.Owner != before.Shared.Owner || !reflect.DeepEqual(after.Shared.Profile, *profile) {
				t.Fatal("credential rotation changed reporting/source authority")
			}
			for path, digest := range after.OwnedFiles {
				raw, e := readBackupOwnedFile(root, path, 8<<20)
				testutil.Must(t, e)
				if sharedSetupDigest(raw) != digest {
					t.Fatal("owned file pin lost", path)
				}
			}
			state, e := openInstalledBackupState(home, false)
			testutil.Must(t, e)
			testutil.Must(t, requireWorkerSetupOwner(state, &after))
			state.Close()
			checked, done, e := InspectVerificationNotificationScope(home, r.Operation, r.BeforeGateway, scope)
			testutil.Must(t, e)
			if !done || checked != gateway {
				t.Fatal("completed report scope unavailable")
			}
			retained, e := os.ReadFile(cutoverPath)
			testutil.Must(t, e)
			if string(retained) != string(original) {
				t.Fatal("rewrote original reporting handoff")
			}
			calls := services.stops + services.starts + services.reloads
			_, e = reconcileNotificationScopeReports(ctx, home, r, func(ProducerReservation) (string, error) {
				t.Fatal("replayed completed gateway change")
				return "", nil
			}, version, deps)
			testutil.Must(t, e)
			if calls != services.stops+services.starts+services.reloads {
				t.Fatal("completed scope restarted reporting")
			}
		})
	}
}

func TestDeliveryProviderScopeRejectsChangedRetryAndUnit(t *testing.T) {
	for _, damage := range []string{"plan", "unit", "backup", "profile"} {
		t.Run(damage, func(t *testing.T) {
			home, r, _, deps, services, profile := providerScopeFixture(t)
			lost := errors.New("pause before gateway")
			_, err := reconcileNotificationScopeReports(context.Background(), home, r, func(ProducerReservation) (string, error) { return "", lost }, 16, deps)
			if !errors.Is(err, lost) {
				t.Fatal(err)
			}
			switch damage {
			case "plan":
				deps.credentials = append(deps.credentials, filepath.Join(home, "unreviewed"))
			case "unit":
				testutil.Must(t, os.WriteFile(filepath.Join(services.paths.unitDirectory(), "deploy-backup-report.service"), []byte("changed"), 0600))
			case "backup":
				r.Desired.Backup.Schedule = "weekly"
			case "profile":
				profile.Identity.Recovery = testutil.ID(t, "r")
			}
			called := false
			_, err = reconcileNotificationScopeReports(context.Background(), home, r, func(ProducerReservation) (string, error) { called = true; return notify.Hash("gateway"), nil }, 16, deps)
			if err == nil {
				t.Fatal("changed credential scope accepted")
			}
			if damage != "profile" && called {
				t.Fatal("changed scope reached gateway")
			}
			if services.active {
				t.Fatal("failed scope restarted reporting")
			}
		})
	}
}
