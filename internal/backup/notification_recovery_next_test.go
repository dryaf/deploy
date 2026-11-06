//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/provider"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupRepeatedRecoveryActivation(t *testing.T) { testBackupRepeatedRecovery(t, false) }
func TestBackupRepeatedInteractiveRecoveryActivation(t *testing.T) {
	testBackupRepeatedRecovery(t, true)
}
func testBackupRepeatedRecovery(t *testing.T, interactive bool) {
	testBackupRepeatedRecoveryCycles(t, interactive, false)
}
func TestBackupMigrationCyclesAcrossRepeatedRecovery(t *testing.T) {
	testBackupRepeatedRecoveryCycles(t, false, true)
}
func testBackupRepeatedRecoveryCycles(t *testing.T, interactive, cycles bool) {
	ctx := context.Background()
	f, home, spec, deps, session := backupRecoveryActivationFixtureMode(t, interactive, true)
	testutil.Must(t, runBackupRecoveryActivation(ctx, home, f.config, spec, false, deps))
	var oldest string
	for epoch := 2; epoch <= 3; epoch++ {
		if cycles {
			backupCycleBetweenRecoveries(t, &f, home, spec, deps, session, epoch)
		}
		t.Logf("recover epoch %d", epoch)
		parent := *session.m.DeliveryRecovery
		artifact := filepath.Join(t.TempDir(), "snapshot")
		snapshot, err := session.store.Snapshot(ctx, artifact)
		testutil.Must(t, err)
		testutil.Must(t, session.Revoke(ctx))
		testutil.Must(t, session.store.Close())
		_, err = worker.QuarantineNextManagedDeliveryRecovery(ctx, f.gatewayPath, artifact, f.gatewayConfig, snapshot.ID, snapshot.DatabaseHash, session.m.Revision, parent)
		testutil.Must(t, err)
		remote, err := worker.OpenRecoverySession(ctx, f.gatewayPath, f.gatewayConfig)
		testutil.Must(t, err)
		stop := errors.New("interrupted ancestor publication")
		_, err = reconcileBackupNotificationsWith(ctx, f.state, f.config, remote, reportHostClock, func(at string) error {
			if at == "backup_next_ancestor" {
				return stop
			}
			return nil
		})
		if !errors.Is(err, stop) {
			t.Fatalf("ancestor interruption: %v", err)
		}
		summary, err := reconcileBackupNotifications(ctx, f.state, f.config, remote)
		testutil.Must(t, err)
		testutil.Must(t, remote.Close())
		var journal backupNotificationRecovery
		testutil.Must(t, f.state.get("notification_recovery", &journal))
		if journal.Version != 2 || journal.Ancestor == "" {
			t.Fatal("ancestor not retained")
		}
		if oldest == "" {
			oldest = journal.Ancestor
		}
		if requireNotificationRecoveryInactiveTx(f.state) == nil {
			t.Fatal("new recovery admitted reporting before activation")
		}
		f.options = backupReconciliationOptions{State: f.state.directory, NotificationState: f.gatewayPath, Operation: summary.Challenge.Operation, SummaryHash: notify.Hash(summary)}
		var out bytes.Buffer
		testutil.Must(t, runBackupRecoveryHandoff(ctx, f.options, f.config, f.gatewayConfig, &out, nil))
		var handoff backupRecoveryHandoffResult
		testutil.Must(t, json.Unmarshal(out.Bytes(), &handoff))
		if interactive {
			f.snapshotPath = artifact
			completeBackupRecoveryIntake(t, f)
		}
		op := f.options.Operation
		review, err := worker.ReviewRecovery(ctx, f.gatewayPath, f.gatewayConfig, op)
		testutil.Must(t, err)
		cp, err := worker.CheckpointRecovery(ctx, f.gatewayPath, f.gatewayConfig, op, review.ReviewHash)
		testutil.Must(t, err)
		proof, err := worker.VerifyRecoveryCheckpoint(ctx, f.gatewayPath, f.gatewayConfig, op, review.ReviewHash, cp.CheckpointHash)
		testutil.Must(t, err)
		image, err := worker.PrepareRecoveryPublication(ctx, f.gatewayPath, f.gatewayConfig, op, review.ReviewHash, cp.CheckpointHash, proof.ProofHash)
		testutil.Must(t, err)
		publication, err := worker.PublishRecovery(ctx, f.gatewayPath, f.gatewayConfig, op, review.ReviewHash, cp.CheckpointHash, proof.ProofHash, image.ImageHash)
		testutil.Must(t, err)
		adoption, err := worker.AdoptPublishedRecoveryVerification(ctx, f.gatewayPath, f.gatewayConfig, publication.Commit.CommitHash)
		testutil.Must(t, err)
		next := worker.ManagedDeliveryRecovery{Version: 2, Owner: parent.Owner, Before: adoption.Before, After: adoption.After, CommitHash: adoption.CommitHash}
		s, err := worker.OpenManagedRecoveryVerification(f.gatewayPath, f.gatewayConfig, adoption.After, session.m.Revision, adoption.CommitHash)
		testutil.Must(t, err)
		testutil.Must(t, s.EnrollManagedDeliveryRecovery(next))
		testutil.Must(t, s.Close())
		s, err = worker.OpenManagedRecoveredDelivery(f.gatewayPath, f.gatewayConfig, session.m.Revision, next, notify.Hash(next.Owner), notify.Hash("fixture-activation"))
		testutil.Must(t, err)
		defer s.Close()
		session.store = s
		session.m.Identity = adoption.After
		session.m.DeliveryRecovery = &next
		session.g = handoff.Delivery.After.Generation
		spec.Managed.ManifestHash = notify.Hash(session.m)
		spec.Managed.Generation = session.g
		spec.CommitHash = notify.Hash(handoff.Delivery)
		spec.ConfigHash = notify.Hash(f.config)
		testutil.Must(t, runBackupRecoveryActivation(ctx, home, f.config, spec, false, deps))
		testutil.Must(t, runBackupRecoveryActivation(ctx, home, f.config, spec, false, deps))
		var owner backupNotificationOwner
		testutil.Must(t, f.state.get("notification_owner", &owner))
		testutil.Must(t, requireBackupCutoverOwner(f.state, owner))
		f.binding = owner.Binding
		f.clock = reportHostClock()
		f.refreshProbe(t)
		testutil.Must(t, f.state.resolveIncident("capture", f.subject, f.clock.Now, f.config.Policy.Notifications))
		f.observe(t, f.subject, "fresh failure after later recovery", f.clock.Now)
		_, messages := f.evaluate(t)
		if len(messages) == 0 {
			t.Fatal("new epoch produced no report")
		}
		api := &recoveryReportAPI{s: s}
		testutil.Must(t, transferBackupNotifications(ctx, f.state, api))
	}
	if interactive {
		var effects int
		testutil.Must(t, f.state.db.QueryRow("SELECT value FROM handoff_effects").Scan(&effects))
		if effects != 1 {
			t.Fatal("recovery repeated command effect")
		}
	}
	testutil.Must(t, os.Remove(filepath.Join(f.state.directory, "notification-recovery-history", oldest+".json")))
	if err := requireBackupRecoveryHistory(f.state); err == nil {
		t.Fatal("missing oldest recovery evidence accepted")
	}
}

func backupCycleBetweenRecoveries(t *testing.T, f *backupHandoffFixture, home string, activation backupRecoveryActivationSpec, deps backupCutoverDependencies, session *cutoverFixtureSession, epoch int) {
	t.Helper()
	ctx := context.Background()
	// Supply disposable legacy credential files; all sends remain synthetic.
	f.config.Policy.Notifications.Primary, f.config.Policy.Notifications.Fallback, f.config.Policy.Notifications.Signal = "telegram", "", nil
	f.config.Policy.Notifications.Telegram = &config.WorkerTelegram{TokenFile: filepath.Join(home, "cycle-token"), ChatIDFile: filepath.Join(home, "cycle-chat")}
	testutil.Must(t, os.WriteFile(f.config.Policy.Notifications.Telegram.TokenFile, []byte("123:fixture"), 0600))
	testutil.Must(t, os.WriteFile(f.config.Policy.Notifications.Telegram.ChatIDFile, []byte("456"), 0600))
	testutil.Must(t, safefs.WritePrivateJSON(backupCutoverPaths(home, f.config).Config, f.config))
	prior := deps.Managed
	deps.Managed = func(ctx context.Context, r install.ManagedProducerRequest, fn func(install.ManagedProducerSession) error) error {
		session.g, session.legacy = r.Generation, r.LegacyGeneration
		return prior(ctx, r, fn)
	}
	var owner backupNotificationOwner
	testutil.Must(t, f.state.get("notification_owner", &owner))
	managed := activation.Managed
	managed.Generation, managed.ManifestHash = owner.Binding.Generation, notify.Hash(session.m)
	record, err := runBackupMigrationCycle(ctx, home, f.config, backupMigrationCycleSpec{Version: 1, Managed: managed, HelperHash: activation.HelperHash, ConfigHash: notify.Hash(f.config), OwnerHash: notify.Hash(owner), Reason: "cycle between recovered generations"}, deps)
	testutil.Must(t, err)
	d, err := safefs.Open(f.state.directory)
	testutil.Must(t, err)
	defer d.Close()
	var original backupCutoverIntent
	testutil.Must(t, d.ReadJSON(backupCutoverFile, &original))
	cutover := original.Spec
	cutover.Version, cutover.Predecessor, cutover.Managed = 4, notify.Hash(record), managed
	cutover.HelperHash, cutover.ConfigHash = activation.HelperHash, notify.Hash(f.config)
	rollback := backupLegacyRollbackSpec{Version: 1, Cutover: cutover, Generation: fmt.Sprintf("between-recoveries-legacy-%d", epoch), Reason: "reviewed synthetic rollback", CredentialHash: legacyCredentialPin(t, f.config)}
	testutil.Must(t, runBackupLegacyRollback(ctx, home, f.config, rollback, false, deps))
	testutil.Must(t, runBackupLegacyRollback(ctx, home, f.config, rollback, true, deps))
	rollbackPath, err := currentMigrationArtifact(f.state, legacyRollbackFile)
	testutil.Must(t, err)
	var retained backupLegacyRollbackIntent
	testutil.Must(t, d.ReadJSON(rollbackPath, &retained))
	var evidence backupCutoverEvidence
	testutil.Must(t, backupTransferTx(ctx, f.state, func(tx *sql.Tx) error { var e error; evidence, e = readBackupCutoverEvidence(tx); return e }))
	cutover.Version, cutover.Predecessor, cutover.Managed.Generation = 1, "", fmt.Sprintf("between-recoveries-shared-%d", epoch)
	cutover.EvidenceHash, cutover.Decisions = notify.Hash(evidence), map[string]backupCutoverDecision{}
	for _, row := range evidence.Rows {
		disposition := "unsent"
		if row.Accepted.Valid {
			disposition = "accepted"
		}
		cutover.Decisions[row.ID] = backupCutoverDecision{notify.Hash(row), disposition, "retained source outcome"}
	}
	testutil.Must(t, runBackupRemigration(ctx, home, f.config, backupRemigrationSpec{1, cutover, notify.Hash(retained)}, false, deps))
	testutil.Must(t, f.state.get("notification_owner", &owner))
	testutil.Must(t, requireBackupCutoverOwner(f.state, owner))
	f.binding = owner.Binding
}

// The provider is entirely local and returns no events. Command history comes
// from the retained gateway snapshot and the specialist ledger.
type backupRecoveryIntakeTransport struct{}

func (backupRecoveryIntakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	result := `[]`
	if strings.HasSuffix(r.URL.Path, "getWebhookInfo") {
		result = `{"url":""}`
	}
	if strings.HasSuffix(r.URL.Path, "getMe") {
		result = `{"id":123,"is_bot":true,"username":"ExampleBot"}`
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"ok":true,"result":%s}`, result))), Request: r}, nil
}
func completeBackupRecoveryIntake(t *testing.T, f backupHandoffFixture) {
	t.Helper()
	ctx := context.Background()
	account := f.gatewayConfig.Interaction.Destinations[0].Account
	_, err := worker.ReconcileTelegramHistory(ctx, f.gatewayPath, f.snapshotPath, account, f.gatewayConfig)
	testutil.Must(t, err)
	_, err = worker.DrainTelegramRecovery(ctx, f.gatewayPath, f.gatewayConfig, backupRecoveryPoller(t, f.gatewayConfig))
	testutil.Must(t, err)
	requests, err := worker.OpenRequestRecoverySession(ctx, f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	_, err = requests.SealRecoveryRequests(ctx)
	testutil.Must(t, err)
	testutil.Must(t, requests.Close())
	cursor, err := worker.OpenTelegramRecoveryCursorSession(ctx, f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	_, err = cursor.CommitTelegramRecoveryCursor(ctx, account)
	testutil.Must(t, err)
	testutil.Must(t, cursor.Close())
}

func backupRecoveryPoller(t *testing.T, c worker.Config) provider.TelegramPoller {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	testutil.Must(t, os.WriteFile(token, []byte("123:fixture-token"), 0600))
	return provider.TelegramPoller{Profile: provider.TelegramAccount{Account: c.Interaction.Destinations[0].Account, TokenFile: token, BotName: "ExampleBot"}, Destinations: c.Interaction.Destinations, Transport: backupRecoveryIntakeTransport{}}
}
