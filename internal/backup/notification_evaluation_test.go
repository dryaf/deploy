//go:build linux

package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

type backupEvaluationFixture struct {
	*pipelineFixture
	config  WorkerInstallation
	clock   backupEvaluationClock
	receipt SnapshotReceipt
	subject string
}

func newBackupEvaluationFixture(t *testing.T, configure ...func(*worker.Config)) *backupEvaluationFixture {
	t.Helper()
	archive, state, attempt, header, data := archiveFixture(t)
	receipt, err := archive.receive(state, attempt, "fixture", 1<<20, func(file *os.File) (protocol.SnapshotHeader, error) { _, err := file.Write(data); return header, err }, fixtureValidator)
	testutil.Must(t, err)
	subject := instanceKey(attempt.SourceID, attempt.Service)
	testutil.Must(t, archive.updateControl(state, func(control *ArchiveControl) error {
		instance := control.Instances[subject]
		instance.Generation = header.Generation
		instance.LastSuccess = attempt.RequestedAt
		control.Instances[subject] = instance
		return nil
	}))
	pipeline := newPipelineFixtureFromState(t, state, configure...)
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	c := WorkerInstallation{Schema: 1, WorkerID: control.WriterID, ArchiveID: control.ArchiveID, ConfigHash: control.ConfigHash, Policy: configfixture.WorkerPolicy(t), Qualification: map[string]bool{}}
	for _, gate := range []string{"source_restrictions", "rootless_runtime", "storage_durability", "notification_delivery", "external_monitor"} {
		c.Qualification[gate] = true
	}
	now := time.Now().UTC()
	clock := backupEvaluationClock{now, "fixture-boot", 100}
	testutil.Must(t, state.put("health_probe", WorkerHealthProbe{Schema: 1, ObservedAt: now, BootID: clock.Boot, Uptime: clock.Uptime, ConfigHash: c.ConfigHash, Healthy: true, BackupHealthy: true, DeliveryHealthy: true}))
	testutil.Must(t, state.put("run", WorkerRun{ID: "run-one", ConfigHash: c.ConfigHash, StartedAt: attempt.RequestedAt, FinishedAt: now, Complete: true, Captured: 1}))
	return &backupEvaluationFixture{pipeline, c, clock, receipt, subject}
}

func (f *backupEvaluationFixture) evaluate(t *testing.T) (e backupNotificationEvaluation, messages []notify.Message) {
	t.Helper()
	ledgerTx(t, f.state, func(tx *sql.Tx) error {
		var err error
		e, messages, err = evaluateBackupNotificationPass(tx, f.config, f.clock)
		return err
	})
	return
}

func (f *backupEvaluationFixture) advance(seconds float64) {
	f.clock.Now = f.clock.Now.Add(time.Duration(seconds * float64(time.Second)))
	f.clock.Uptime += seconds
}
func (f *backupEvaluationFixture) refreshProbe(t *testing.T) {
	t.Helper()
	testutil.Must(t, f.state.put("health_probe", WorkerHealthProbe{Schema: 1, ObservedAt: f.clock.Now, BootID: f.clock.Boot, Uptime: f.clock.Uptime, ConfigHash: f.config.ConfigHash, Healthy: true, BackupHealthy: true, DeliveryHealthy: true}))
}

func TestBackupEvaluationHealthyLeaseAndSemanticRevision(t *testing.T) {
	f := newBackupEvaluationFixture(t)
	first, _ := f.evaluate(t)
	if first.Snapshot.Subjects[0].Condition != "ok" {
		t.Fatal(first.Snapshot)
	}
	deadline := first.Snapshot.Subjects[0].ValidUntil
	if !deadline.Equal(f.clock.Now.Add(120 * time.Second)) {
		t.Fatal("probe lease was not bounded", deadline)
	}
	f.advance(30)
	next, _ := f.evaluate(t)
	if next.Snapshot.Sequence != 2 || next.Snapshot.BasisRevision != first.Snapshot.BasisRevision || !next.Snapshot.Subjects[0].ValidUntil.Equal(deadline) {
		t.Fatal("heartbeat altered semantics or extended old evidence", next.Snapshot)
	}
	f.refreshProbe(t)
	refreshed, _ := f.evaluate(t)
	if refreshed.Snapshot.BasisRevision != first.Snapshot.BasisRevision || !refreshed.Snapshot.Subjects[0].ValidUntil.After(deadline) {
		t.Fatal("fresh probe did not refresh only evidence lease", refreshed.Snapshot)
	}
	f.advance(121)
	stale, _ := f.evaluate(t)
	if stale.Snapshot.Subjects[0].Condition != "unknown" || stale.Snapshot.BasisRevision <= refreshed.Snapshot.BasisRevision {
		t.Fatal("stale probe kept healthy", stale.Snapshot)
	}
}

func TestBackupEvaluationFreshnessFailureAndRecoveryAreAtomicExports(t *testing.T) {
	f := newBackupEvaluationFixture(t)
	f.config.Policy.Freshness = "1m"
	first, _ := f.evaluate(t)
	expected := f.receipt.Header.Attempt.RequestedAt.Add(time.Minute)
	if !first.Snapshot.Subjects[0].ValidUntil.Equal(expected) {
		t.Fatal("snapshot deadline did not bound health")
	}
	f.advance(61)
	f.refreshProbe(t)
	failed, messages := f.evaluate(t)
	if failed.Snapshot.Subjects[0].Condition != "critical" || len(messages) != 1 || messages[0].Kind != "failure" || messages[0].Context == nil {
		t.Fatal(failed, messages)
	}
	// A changed valid freshness policy is authoritative only with matching config
	// identity in both installation and the durable control record.
	f.config.Policy.Freshness = "2h"
	f.config.ConfigHash = strings.Repeat("b", 64)
	var control ArchiveControl
	testutil.Must(t, f.state.get("control", &control))
	control.ConfigHash = f.config.ConfigHash
	testutil.Must(t, f.state.put("control", control))
	var run WorkerRun
	testutil.Must(t, f.state.get("run", &run))
	run.ConfigHash = f.config.ConfigHash
	testutil.Must(t, f.state.put("run", run))
	f.refreshProbe(t)
	recovered, messages := f.evaluate(t)
	if recovered.Snapshot.Subjects[0].Condition != "ok" || len(messages) != 1 || messages[0].Kind != "recovery" {
		t.Fatal(recovered, messages)
	}
	if recovered.Snapshot.BasisRevision <= failed.Snapshot.BasisRevision {
		t.Fatal("recovery did not advance semantic basis")
	}
}

func TestBackupEvaluationCurrentReportThroughGateway(t *testing.T) {
	f := newBackupEvaluationFixture(t)
	f.config.Policy.Timezone = "UTC"
	f.config.Policy.Notifications.DailyAt = "00:00"
	testutil.Must(t, f.state.enqueueDaily(f.config, f.clock.Now, "OLD TEXT FROM PRIOR EVALUATION"))
	evaluation, messages := f.evaluate(t)
	if len(messages) != 1 || strings.Contains(messages[0].Text, "OLD TEXT") || !strings.Contains(messages[0].Text, "Current backup evaluation: ok") {
		t.Fatal(evaluation, messages)
	}
	f.transfer(t, f.client)
	f.accept(t, messages[0])
	f.transfer(t, f.client)
	health, err := f.gateway.Health(context.Background())
	testutil.Must(t, err)
	if health.Condition != "ok" {
		t.Fatal("gateway did not receive actual healthy evaluation", health)
	}
	f.observe(t, f.subject, "new actual capture failure", f.clock.Now)
	failed, _ := f.evaluate(t)
	f.transfer(t, f.client)
	if failed.Snapshot.BasisRevision <= evaluation.Snapshot.BasisRevision {
		t.Fatal("new domain evidence did not change basis")
	}
	health, err = f.gateway.Health(context.Background())
	testutil.Must(t, err)
	if health.Condition != "critical" {
		t.Fatal(health)
	}
}

func TestBackupEvaluationIncompleteEvidenceCannotPublishHealthy(t *testing.T) {
	for _, kind := range []string{"new-boot", "unqualified", "paused", "mode", "run", "missing-probe", "missing-latest"} {
		t.Run(kind, func(t *testing.T) {
			f := newBackupEvaluationFixture(t)
			switch kind {
			case "new-boot":
				f.clock.Boot = "different-boot"
			case "unqualified":
				f.config.Qualification["storage_durability"] = false
			case "paused":
				testutil.Must(t, f.state.put("paused_until", f.clock.Now.Add(-time.Minute)))
			case "mode":
				testutil.Must(t, f.state.put("mode", "read"))
			case "run":
				_, err := f.state.db.Exec("DELETE FROM meta WHERE key='run'")
				testutil.Must(t, err)
			case "missing-probe":
				_, err := f.state.db.Exec("DELETE FROM meta WHERE key='health_probe'")
				testutil.Must(t, err)
			case "missing-latest":
				var control ArchiveControl
				testutil.Must(t, f.state.get("control", &control))
				control.LatestSnapshots = nil
				testutil.Must(t, f.state.put("control", control))
			}
			evaluation, _ := f.evaluate(t)
			if evaluation.Snapshot.Subjects[0].Condition == "ok" {
				t.Fatal("insufficient evidence declared healthy", evaluation)
			}
			if kind == "paused" && evaluation.Snapshot.Subjects[0].Condition != "paused" {
				t.Fatal("expired pause was silently resumed")
			}
		})
	}
}

func TestBackupEvaluationActiveRunDeadlineBoundsHealth(t *testing.T) {
	f := newBackupEvaluationFixture(t)
	f.config.Policy.MaxRunTime = "30s"
	testutil.Must(t, f.state.put("run", WorkerRun{ID: "active-run", ConfigHash: f.config.ConfigHash, StartedAt: f.clock.Now.Add(-10 * time.Second)}))
	healthy, _ := f.evaluate(t)
	if healthy.Snapshot.Subjects[0].Condition != "ok" || !healthy.Snapshot.Subjects[0].ValidUntil.Equal(f.clock.Now.Add(20*time.Second)) {
		t.Fatal(healthy)
	}
	f.advance(21)
	failed, _ := f.evaluate(t)
	if failed.Snapshot.Subjects[0].Condition != "critical" {
		t.Fatal("overdue run remained healthy")
	}
}

func TestBackupEvaluationHostClockEntryPoint(t *testing.T) {
	f := newBackupEvaluationFixture(t)
	boot, uptime, err := process.HostClock()
	testutil.Must(t, err)
	f.clock = backupEvaluationClock{time.Now().UTC(), boot, uptime}
	f.refreshProbe(t)
	evaluation, _, err := evaluateAndFreezeBackupNotifications(context.Background(), f.state, f.config, f.clock.Now)
	testutil.Must(t, err)
	if evaluation.Snapshot.Subjects[0].Condition != "ok" {
		t.Fatal(evaluation)
	}
	var sequence uint64
	testutil.Must(t, f.state.db.QueryRow("SELECT max(sequence) FROM backup_notification_snapshots").Scan(&sequence))
	if sequence != evaluation.Snapshot.Sequence {
		t.Fatal("entry point did not commit snapshot")
	}
}

func TestBackupEvaluationRejectsCapacityAndRevisionMismatch(t *testing.T) {
	for _, kind := range []string{"subjects", "revision", "authority"} {
		t.Run(kind, func(t *testing.T) {
			f := newBackupEvaluationFixture(t)
			f.evaluate(t)
			switch kind {
			case "subjects":
				var control ArchiveControl
				testutil.Must(t, f.state.get("control", &control))
				for i := 0; i < 128; i++ {
					control.Instances[fmt.Sprintf("extra-%d", i)] = ExpectedInstance{}
				}
				testutil.Must(t, f.state.put("control", control))
			case "revision":
				var state backupEvaluationState
				testutil.Must(t, f.state.get("notification_evaluation", &state))
				state.Sequence++
				testutil.Must(t, f.state.put("notification_evaluation", state))
			case "authority":
				f.config.ArchiveID = "wrong-archive"
			}
			tx, err := f.state.db.Begin()
			testutil.Must(t, err)
			if _, _, err = evaluateBackupNotificationPass(tx, f.config, f.clock); err == nil {
				t.Fatal("invalid evaluation admitted")
			}
			testutil.Must(t, tx.Rollback())
			var sequence uint64
			testutil.Must(t, f.state.db.QueryRow("SELECT max(sequence) FROM backup_notification_snapshots").Scan(&sequence))
			if sequence != 1 {
				t.Fatal("failed evaluation advanced snapshot")
			}
		})
	}
}

func TestBackupEvaluationPreservesFailuresAndExplicitRetirement(t *testing.T) {
	f := newBackupEvaluationFixture(t)
	f.observe(t, f.subject, "unresolved capture error", f.clock.Now)
	failed, _ := f.evaluate(t)
	if failed.Snapshot.Subjects[0].Condition != "critical" {
		t.Fatal("fresh snapshot hid unresolved capture", failed)
	}
	var control ArchiveControl
	testutil.Must(t, f.state.get("control", &control))
	instance := control.Instances[f.subject]
	instance.Retired = true
	instance.RetirementReason = "operator decommissioned fixture"
	control.Instances[f.subject] = instance
	testutil.Must(t, f.state.put("control", control))
	retired, messages := f.evaluate(t)
	if retired.Snapshot.Subjects[0].Condition != "unknown" || retired.Snapshot.Subjects[1].Condition != "retired" {
		t.Fatal(retired)
	}
	for _, message := range messages {
		if message.Kind == "recovery" {
			t.Fatal("retirement claimed recovery")
		}
	}
	var open int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM incidents WHERE resolved IS NULL").Scan(&open))
	if open != 0 {
		t.Fatal(open)
	}
}

func TestBackupEvaluationClockRollbackSticksAcrossRestart(t *testing.T) {
	f := newBackupEvaluationFixture(t)
	f.evaluate(t)
	f.clock.Now = f.clock.Now.Add(-time.Second)
	f.clock.Uptime++
	uncertain, _ := f.evaluate(t)
	if uncertain.Snapshot.Subjects[0].Condition == "ok" {
		t.Fatal("rollback kept healthy")
	}
	testutil.Must(t, f.state.Close())
	reopened, err := openBackupState(f.state.directory, false)
	testutil.Must(t, err)
	f.state = reopened
	t.Cleanup(func() { reopened.Close() })
	f.advance(3)
	f.refreshProbe(t)
	uncertain, _ = f.evaluate(t)
	if uncertain.Snapshot.Subjects[0].Condition == "ok" {
		t.Fatal("restart erased sticky clock uncertainty")
	}
	testutil.Must(t, f.state.put("clock_suspect", false))
	restored, _ := f.evaluate(t)
	if restored.Snapshot.Subjects[0].Condition != "ok" {
		t.Fatal(restored)
	}
}

func TestBackupEvaluationRollbackAndMalformedEvidence(t *testing.T) {
	f := newBackupEvaluationFixture(t)
	f.config.Policy.Freshness = "1ns"
	tx, err := f.state.db.Begin()
	testutil.Must(t, err)
	_, messages, err := evaluateBackupNotificationPass(tx, f.config, f.clock)
	testutil.Must(t, err)
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	testutil.Must(t, tx.Rollback())
	for _, table := range []string{"incidents", "outbox", "backup_notification_snapshots", "notification_exports", "backup_command_event_context"} {
		var count int
		testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM "+table).Scan(&count))
		if count != 0 {
			t.Fatal("partial evaluation survived", table, count)
		}
	}
	var state backupEvaluationState
	if err = f.state.get("notification_evaluation", &state); err != sql.ErrNoRows {
		t.Fatal("revision survived rollback", err)
	}
	_, err = f.state.db.Exec("UPDATE meta SET value='broken' WHERE key='health_probe'")
	testutil.Must(t, err)
	tx, err = f.state.db.Begin()
	testutil.Must(t, err)
	defer tx.Rollback()
	if _, _, err = evaluateBackupNotificationPass(tx, f.config, f.clock); err == nil {
		t.Fatal("malformed probe accepted")
	}
}

func TestBackupEvaluationMissingAndCorruptLatestSnapshot(t *testing.T) {
	for _, kind := range []string{"missing", "verification", "identity"} {
		t.Run(kind, func(t *testing.T) {
			f := newBackupEvaluationFixture(t)
			switch kind {
			case "missing":
				testutil.Must(t, f.state.put("unavailable/"+f.receipt.BackupID, "bundle missing"))
			case "verification":
				record := ArchiveVerification{ArchiveID: f.receipt.ArchiveID, BackupID: f.receipt.BackupID, SHA256: f.receipt.Header.SHA256, ErrorKind: "checksum"}
				testutil.Must(t, f.state.put("verification/"+f.receipt.BackupID, BackupVerificationStatus{Last: record, Definitive: &record}))
			case "identity":
				receipt := f.receipt
				receipt.Header.Attempt.BindingHash = "wrong"
				body, _ := json.Marshal(receipt)
				_, err := f.state.db.Exec("UPDATE bundles SET record=?", string(body))
				testutil.Must(t, err)
			}
			evaluation, messages := f.evaluate(t)
			if evaluation.Snapshot.Subjects[0].Condition != "critical" || len(messages) == 0 {
				t.Fatal(evaluation, messages)
			}
		})
	}
}

func TestBackupEvaluationRetiresAggregateOnlyAfterEverySource(t *testing.T) {
	f := newBackupEvaluationFixture(t)
	var control ArchiveControl
	testutil.Must(t, f.state.get("control", &control))
	source := control.Instances[f.subject].SourceID
	f.config.Sources = []deploysource.WorkerSource{{SourceID: source}, {SourceID: "still-enrolled"}}
	control.RetiredSources[source] = "Operator retired source"
	testutil.Must(t, f.state.put("control", control))
	partial, _ := f.evaluate(t)
	if partial.Snapshot.Subjects[0].Condition == "retired" {
		t.Fatal("unretired source hidden")
	}
	control.RetiredSources["still-enrolled"] = "Operator retired final source"
	testutil.Must(t, f.state.put("control", control))
	retired, _ := f.evaluate(t)
	for _, subject := range retired.Snapshot.Subjects {
		if subject.Condition != "retired" {
			t.Fatal("aggregate retirement did not settle coverage", subject)
		}
	}
	delete(control.RetiredSources, "still-enrolled")
	testutil.Must(t, f.state.put("control", control))
	restored, _ := f.evaluate(t)
	if restored.Snapshot.Subjects[0].Condition == "retired" {
		t.Fatal("source reactivation retained retired aggregate")
	}
}
