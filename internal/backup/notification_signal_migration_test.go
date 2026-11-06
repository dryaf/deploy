//go:build linux

package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

type signalCutoverFixtureSession struct {
	*cutoverFixtureSession
	migration backupSignalMigration
	installed relay.SignalRuntimeInstallation
}

func (s signalCutoverFixtureSession) SignalMigrationProfile(ctx context.Context, account, bootstrap, selectorHash, primary, fallback string) (provider.Profile, error) {
	if account != s.migration.Adoption.Spec.accountState() || bootstrap != s.migration.Adoption.Recovery.ResultHash || primary != "signal" || fallback != "" {
		return provider.Profile{}, fmt.Errorf("fixture Signal route or account changed")
	}
	if err := relay.RequireLegacyRuntimeAncestor(ctx, s.installed, bootstrap); err != nil {
		return provider.Profile{}, err
	}
	return s.migration.Profile, nil
}

func TestBackupSignalMigrationRollbackAndRemigration(t *testing.T) {
	for _, boundary := range []string{"cutover-fenced", "cutover-shared", "legacy-rollback-canonical"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			adoptionSpec, svc, active := legacySignalAdoptionFixture(t)
			a, err := adoptBackupSignal(ctx, adoptionSpec, svc)
			testutil.Must(t, err)
			handoff, runtime := legacyAdoptionGateway(t, a)
			installed, _, err := handoffBackupSignal(ctx, handoff, runtime, svc)
			testutil.Must(t, err)
			dest := interaction.Destination{ID: "private", Generation: "one", Provider: "signal", Account: a.Spec.Account, NativeID: "22222222-2222-4222-8222-222222222222"}
			profile := provider.Profile{Kind: "signal", Conversation: &dest, Destination: dest.Fingerprint(), Socket: filepath.Join(a.Spec.Home, "shared-signal.sock"), RelayIdentity: runtime.Signal.InboxIdentity}
			home, c, spec, deps, session := backupCutoverFixtureUsing(t, a.Spec.Home, func(c *WorkerInstallation, cfg *worker.Config) {
				c.Policy.Notifications.Primary, c.Policy.Notifications.Telegram = "signal", nil
				c.Policy.Notifications.Signal = svc.installation.Policy.Notifications.Signal
				cfg.Routes["ops"] = worker.Route{Revision: "one", Primary: "signal"}
			})
			selectors, _, err := notification.LegacySignalSelectors(c.Policy.Notifications)
			testutil.Must(t, err)
			spec.Version, spec.Signal = 3, &backupSignalMigration{Adoption: a, Profile: profile, Selectors: selectors, Reason: "reviewed original recipient and enrolled conversation"}
			wrapped := signalCutoverFixtureSession{session, *spec.Signal, installed}
			deps.Managed = func(ctx context.Context, request install.ManagedProducerRequest, fn func(install.ManagedProducerSession) error) error {
				session.g, session.legacy = request.Generation, request.LegacyGeneration
				return fn(wrapped)
			}
			stop := errors.New("migration interrupted")
			deps.Fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			err = runBackupCutover(ctx, home, c, spec, false, deps)
			if boundary != "legacy-rollback-canonical" && !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if boundary == "legacy-rollback-canonical" {
				testutil.Must(t, err)
			}
			deps.Fault = nil
			testutil.Must(t, runBackupCutover(ctx, home, c, spec, false, deps))
			testutil.Must(t, runBackupCutover(ctx, home, c, spec, true, deps))
			pin, err := notification.LegacyCredentialHash(c.Policy.Notifications, spec.signalProfile())
			testutil.Must(t, err)
			rollback := backupLegacyRollbackSpec{Version: 1, Cutover: spec, CredentialHash: pin, Generation: "signal-legacy-restored", Reason: "retain single adopted account owner"}
			if boundary == "legacy-rollback-canonical" {
				deps.Fault = func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				}
				if err = runBackupLegacyRollback(ctx, home, c, rollback, false, deps); !errors.Is(err, stop) {
					t.Fatal(err)
				}
				deps.Fault = nil
			}
			testutil.Must(t, runBackupLegacyRollback(ctx, home, c, rollback, false, deps))
			state, err := openBackupState(backupCutoverPaths(home, c).State, false)
			testutil.Must(t, err)
			calls := 0
			testutil.Must(t, drainNotificationOutbox(ctx, state, c, func(_ context.Context, channel string, _ config.WorkerNotifications, _ notification.NotificationEvent) (notification.DeliveryAttempt, error) {
				if channel != "signal" {
					t.Fatal(channel)
				}
				calls++
				return notification.DeliveryAttempt{Provider: channel, Accepted: true}, nil
			}, time.Now().UTC().Add(time.Minute)))
			if calls != 1 {
				t.Fatal("legacy source not delivered exactly once", calls)
			}
			testutil.Must(t, state.Close())
			testutil.Must(t, runBackupLegacyRollback(ctx, home, c, rollback, true, deps))
			state, err = openBackupState(backupCutoverPaths(home, c).State, false)
			testutil.Must(t, err)
			var evidence backupCutoverEvidence
			testutil.Must(t, backupTransferTx(ctx, state, func(tx *sql.Tx) error { var e error; evidence, e = readBackupCutoverEvidence(tx); return e }))
			d, err := safefs.Open(state.directory)
			testutil.Must(t, err)
			var retained backupLegacyRollbackIntent
			testutil.Must(t, d.ReadJSON(legacyRollbackFile, &retained))
			d.Close()
			testutil.Must(t, state.Close())
			next := spec
			next.Managed.Generation, next.EvidenceHash = "signal-shared-next", notify.Hash(evidence)
			next.Decisions = map[string]backupCutoverDecision{}
			for _, row := range evidence.Rows {
				next.Decisions[row.ID] = backupCutoverDecision{notify.Hash(row), "accepted", "retained legacy Signal acceptance"}
			}
			testutil.Must(t, runBackupRemigration(ctx, home, c, backupRemigrationSpec{1, next, notify.Hash(retained)}, false, deps))
			state, err = openBackupState(backupCutoverPaths(home, c).State, false)
			testutil.Must(t, err)
			var owner backupNotificationOwner
			testutil.Must(t, state.get("notification_owner", &owner))
			testutil.Must(t, state.Close())
			cycle, err := runBackupMigrationCycle(ctx, home, c, backupMigrationCycleSpec{Version: 1, Managed: next.Managed, ConfigHash: notify.Hash(c), HelperHash: next.HelperHash, OwnerHash: notify.Hash(owner), Reason: "repeat adopted Signal rollback"}, deps)
			testutil.Must(t, err)
			rollback.Cutover = spec
			rollback.Cutover.Version, rollback.Cutover.Predecessor, rollback.Cutover.Managed = 4, notify.Hash(cycle), next.Managed
			rollback.Generation = "signal-legacy-second"
			testutil.Must(t, runBackupLegacyRollback(ctx, home, c, rollback, false, deps))
			testutil.Must(t, runBackupLegacyRollback(ctx, home, c, rollback, true, deps))
			d, err = safefs.Open(backupCutoverPaths(home, c).State)
			testutil.Must(t, err)
			testutil.Must(t, d.ReadJSON(migrationArtifact(notify.Hash(cycle), legacyRollbackFile), &retained))
			d.Close()
			next.Managed.Generation = "signal-shared-third"
			testutil.Must(t, runBackupRemigration(ctx, home, c, backupRemigrationSpec{1, next, notify.Hash(retained)}, false, deps))
			if *active {
				t.Fatal("old account service restarted")
			}
			if l, err := signalaccount.AcquireRuntime(a.Spec.accountState()); err == nil {
				l.Close()
				t.Fatal("old receiver fence removed")
			}
			testutil.Must(t, os.Remove(filepath.Join(a.Spec.Inbox, "legacy-account-adoption.json")))
			if err = runBackupRemigration(ctx, home, c, backupRemigrationSpec{1, next, notify.Hash(retained)}, false, deps); err == nil {
				t.Fatal("lost adoption proof accepted")
			}
		})
	}
}
