//go:build linux

package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func (*cutoverFixtureServices) PrepareRemigration(context.Context, WorkerUnitPaths, string) error {
	return nil
}
func (s *cutoverFixtureSession) RequireRevokedPredecessors(shared, legacy string) error {
	for _, generation := range []string{shared, legacy} {
		v, err := s.store.OwnershipStatus(s.p, generation)
		if err != nil {
			return err
		}
		if v.Active || v.State != "revoked" || generation == s.g {
			return fmt.Errorf("predecessor is not a distinct revoked generation")
		}
	}
	return nil
}

func remigrationFixture(t *testing.T, outcome string, originalHeld ...bool) (string, WorkerInstallation, backupRemigrationSpec, backupCutoverDependencies, *cutoverFixtureSession, notify.Message) {
	t.Helper()
	home, c, cutover, deps, session := backupCutoverFixture(t)
	ctx := context.Background()
	heldInitially := len(originalHeld) > 0 && originalHeld[0]
	if heldInitially {
		for id, d := range cutover.Decisions {
			d.Disposition = "held"
			cutover.Decisions[id] = d
		}
	}
	testutil.Must(t, runBackupCutover(ctx, home, c, cutover, false, deps))
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	var owner backupNotificationOwner
	testutil.Must(t, state.get("notification_owner", &owner))
	if heldInitially {
		var review backupLegacyReview
		ledgerTx(t, state, func(tx *sql.Tx) error {
			var e error
			review, e = readBackupLegacyReview(tx, c, "legacy-one", time.Now().UTC())
			return e
		})
		resolution := backupLegacyResolution{Version: 1, Review: review, ReviewHash: notify.Hash(review), Action: "unsent", Reason: "initial unattempted source review"}
		testutil.Must(t, resolveBackupLegacy(ctx, state, c, resolution, review.ReviewedAt, nil))
	}
	f := &pipelineFixture{state: state, binding: owner.Binding, now: time.Now().UTC()}
	messages := f.freeze(t, 1, 1)
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	_, err = session.store.Enqueue(ctx, messages[0])
	testutil.Must(t, err)
	testutil.Must(t, state.Close())
	testutil.Must(t, runBackupCutover(ctx, home, c, cutover, true, deps))
	rollback := backupLegacyRollbackSpec{Version: 1, Cutover: cutover, CredentialHash: legacyCredentialPin(t, c), Generation: "legacy-between", Reason: "synthetic rollback"}
	testutil.Must(t, runBackupLegacyRollback(ctx, home, c, rollback, false, deps))
	state, err = openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	if outcome != "unsent" {
		testutil.Must(t, drainNotificationOutbox(ctx, state, c, func(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
			if outcome == "accepted" {
				return notification.DeliveryAttempt{Accepted: true}, nil
			}
			return notification.DeliveryAttempt{Ambiguous: true}, errors.New("lost synthetic response")
		}, time.Now().UTC().Add(time.Minute)))
	}
	testutil.Must(t, state.Close())
	testutil.Must(t, runBackupLegacyRollback(ctx, home, c, rollback, true, deps))
	state, err = openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	defer state.Close()
	d, err := safefs.Open(state.directory)
	testutil.Must(t, err)
	defer d.Close()
	var prior backupLegacyRollbackIntent
	testutil.Must(t, d.ReadJSON(legacyRollbackFile, &prior))
	var evidence backupCutoverEvidence
	ledgerTx(t, state, func(tx *sql.Tx) error { var err error; evidence, err = readBackupCutoverEvidence(tx); return err })
	next := cutover
	next.Managed.Generation = "cutover-two"
	next.EvidenceHash = notify.Hash(evidence)
	next.Decisions = map[string]backupCutoverDecision{}
	for _, row := range evidence.Rows {
		disposition := outcome
		if disposition == "ambiguous" {
			disposition = "held"
		}
		next.Decisions[row.ID] = backupCutoverDecision{notify.Hash(row), disposition, "reviewed native legacy outcome"}
	}
	oldManaged := deps.Managed
	deps.Managed = func(ctx context.Context, r install.ManagedProducerRequest, fn func(install.ManagedProducerSession) error) error {
		session.g = r.Generation
		return oldManaged(ctx, r, fn)
	}
	return home, c, backupRemigrationSpec{1, next, notify.Hash(prior)}, deps, session, messages[0]
}

func TestBackupRemigrationPreservesDeliveryOutcomes(t *testing.T) {
	for _, outcome := range []string{"accepted", "unsent", "ambiguous"} {
		t.Run(outcome, func(t *testing.T) {
			home, c, spec, deps, session, old := remigrationFixture(t, outcome)
			ctx := context.Background()
			testutil.Must(t, runBackupRemigration(ctx, home, c, spec, false, deps))
			testutil.Must(t, runBackupRemigration(ctx, home, c, spec, false, deps))
			if _, ok, err := session.store.Grant(ctx, old.Producer, old.ID); err != nil || ok {
				t.Fatal("revoked export granted", ok, err)
			}
			state, err := openBackupState(backupCutoverPaths(home, c).State, false)
			testutil.Must(t, err)
			defer state.Close()
			var owner backupNotificationOwner
			testutil.Must(t, state.get("notification_owner", &owner))
			testutil.Must(t, requireBackupCutoverOwner(state, owner))
			if owner.Mode != "shared" || owner.Binding.Generation != spec.Cutover.Managed.Generation {
				t.Fatal(owner)
			}
			if err = requireLegacyNotificationOwner(state); err == nil {
				t.Fatal("legacy sender unfenced")
			}
			f := &pipelineFixture{state: state, binding: owner.Binding, now: time.Now().UTC()}
			fresh := f.freeze(t, 2, 2)
			if outcome == "unsent" {
				if len(fresh) != 1 || fresh[0].Sequence <= old.Sequence || fresh[0].ID == old.ID || fresh[0].Generation != owner.Binding.Generation {
					t.Fatal("unsent successor missing", fresh)
				}
				_, err = session.store.Enqueue(ctx, fresh[0])
				testutil.Must(t, err)
			} else if len(fresh) != 0 {
				t.Fatal("legacy accepted/ambiguous source resent", fresh)
			}
			var receipts int
			testutil.Must(t, state.db.QueryRow("SELECT count(*) FROM notification_receipt_history WHERE state='provider_accepted'").Scan(&receipts))
			if receipts != 0 {
				t.Fatal("invented shared acceptance")
			}
			state.Close()
			testutil.Must(t, runBackupRemigration(ctx, home, c, spec, true, deps))
			if err = runBackupRemigration(ctx, home, c, spec, false, deps); err == nil {
				t.Fatal("revoked generation reused")
			}
		})
	}
}

func TestBackupRemigrationInterruptedAndMissingAnchors(t *testing.T) {
	for _, point := range []string{"remigration-intent", "remigration-sql", "remigration-staged", "remigration-active", "remigration-file", "remigration-shared"} {
		t.Run(point, func(t *testing.T) {
			home, c, spec, deps, _, _ := remigrationFixture(t, "accepted")
			stop := errors.New("interrupted")
			deps.Fault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			if err := runBackupRemigration(context.Background(), home, c, spec, false, deps); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			deps.Fault = nil
			testutil.Must(t, runBackupRemigration(context.Background(), home, c, spec, false, deps))
			testutil.Must(t, os.Remove(filepath.Join(backupCutoverPaths(home, c).State, remigrationFile)))
			if err := runBackupRemigration(context.Background(), home, c, spec, false, deps); err == nil {
				t.Fatal("lost remigration intent regenerated")
			}
		})
	}
}

func TestBackupRemigrationRefusesLostCompletionAndChangedEvidence(t *testing.T) {
	for _, kind := range []string{"completion-file", "source-sql", "owner-schema", "legacy-file", "unsafe-unsent", "cancel-before-sql"} {
		t.Run(kind, func(t *testing.T) {
			home, c, spec, deps, _, _ := remigrationFixture(t, "ambiguous")
			ctx := context.Background()
			if kind == "unsafe-unsent" {
				for id, decision := range spec.Cutover.Decisions {
					decision.Disposition = "unsent"
					spec.Cutover.Decisions[id] = decision
				}
				if err := runBackupRemigration(ctx, home, c, spec, false, deps); err == nil {
					t.Fatal("attempted source declared unsent")
				}
				return
			}
			if kind == "cancel-before-sql" {
				deps.Fault = func(at string) error {
					if at == "remigration-intent" {
						return errors.New("stop")
					}
					return nil
				}
				if err := runBackupRemigration(ctx, home, c, spec, false, deps); err == nil {
					t.Fatal("interruption missed")
				}
				deps.Fault = nil
				testutil.Must(t, runBackupRemigration(ctx, home, c, spec, true, deps))
				testutil.Must(t, runBackupRemigration(ctx, home, c, spec, true, deps))
				if err := runBackupRemigration(ctx, home, c, spec, false, deps); err == nil {
					t.Fatal("cancelled generation activated")
				}
				return
			}
			testutil.Must(t, runBackupRemigration(ctx, home, c, spec, false, deps))
			path := backupCutoverPaths(home, c).State
			switch kind {
			case "completion-file":
				testutil.Must(t, os.Remove(filepath.Join(path, remigrationFile+".complete")))
			case "legacy-file":
				testutil.Must(t, os.Remove(filepath.Join(path, legacyRollbackFile)))
			default:
				state, err := openBackupState(path, false)
				testutil.Must(t, err)
				if kind == "source-sql" {
					_, err = state.db.Exec("DELETE FROM meta WHERE key LIKE 'notification_remigration_source/%'")
				} else {
					_, err = state.db.Exec("UPDATE meta SET value=json_set(value,'$.Schema',1) WHERE key='notification_owner'")
				}
				testutil.Must(t, err)
				state.Close()
			}
			if err := runBackupRemigration(ctx, home, c, spec, false, deps); err == nil {
				t.Fatal("lost remigration authority repaired")
			}
		})
	}
}

func TestBackupRemigrationHeldResolution(t *testing.T) {
	for _, loss := range []string{"none", "file", "sql", "prior-approval"} {
		t.Run(loss, func(t *testing.T) {
			home, c, spec, deps, _, old := remigrationFixture(t, "ambiguous", loss == "prior-approval")
			ctx := context.Background()
			testutil.Must(t, runBackupRemigration(ctx, home, c, spec, false, deps))
			state, err := openBackupState(backupCutoverPaths(home, c).State, false)
			testutil.Must(t, err)
			defer state.Close()
			var owner backupNotificationOwner
			testutil.Must(t, state.get("notification_owner", &owner))
			ledgerTx(t, state, func(tx *sql.Tx) error {
				held, e := backupHasHeldSources(tx)
				if e != nil {
					return e
				}
				if !held {
					t.Fatal("ambiguous native work reported healthy")
				}
				return nil
			})
			var review backupLegacyReview
			ledgerTx(t, state, func(tx *sql.Tx) error {
				var e error
				review, e = readBackupLegacyReview(tx, c, "legacy-one", time.Now().UTC())
				return e
			})
			if review.RemigrationHash == "" || legacyResolutionHash(review) == notify.Hash(review.Original) {
				t.Fatal("missing independent remigration approval epoch")
			}
			resolution := backupLegacyResolution{Version: 1, Review: review, ReviewHash: notify.Hash(review), Action: "retry", Reason: "reviewed uncertain native attempt", AckDuplicateRisk: true}
			unsafe := resolution
			unsafe.Action = "unsent"
			if resolveBackupLegacy(ctx, state, c, unsafe, review.ReviewedAt, nil) == nil {
				t.Fatal("uncertain native attempt marked unsent")
			}
			testutil.Must(t, resolveBackupLegacy(ctx, state, c, resolution, review.ReviewedAt, nil))
			testutil.Must(t, resolveBackupLegacy(ctx, state, c, resolution, review.ReviewedAt.Add(time.Hour), nil))
			testutil.Must(t, requireBackupCutoverOwner(state, owner))
			ledgerTx(t, state, func(tx *sql.Tx) error {
				held, e := backupHasHeldSources(tx)
				if e != nil {
					return e
				}
				if held {
					t.Fatal("approved native work still held")
				}
				return nil
			})
			if loss == "prior-approval" {
				var n int
				testutil.Must(t, state.db.QueryRow("SELECT count(*) FROM meta WHERE key LIKE 'notification_legacy_resolution/%'").Scan(&n))
				if n != 2 {
					t.Fatal("earlier approval overwritten", n)
				}
			}
			if loss == "file" {
				testutil.Must(t, os.Remove(filepath.Join(state.directory, legacyResolutionFile(legacyResolutionHash(review)))))
				if requireBackupCutoverOwner(state, owner) == nil {
					t.Fatal("lost resolution file authorized sending")
				}
				return
			}
			if loss == "sql" {
				_, err = state.db.Exec("DELETE FROM meta WHERE key=?", legacyResolutionPrefix+legacyResolutionHash(review))
				testutil.Must(t, err)
			}
			f := &pipelineFixture{state: state, binding: owner.Binding, now: time.Now().UTC().Add(time.Hour)}
			fresh := f.freeze(t, 2, 2)
			if loss == "sql" {
				if len(fresh) != 0 {
					t.Fatal("lost SQL approval authorized sending", fresh)
				}
				return
			}
			if len(fresh) != 1 || fresh[0].ID == old.ID || fresh[0].Sequence <= old.Sequence || fresh[0].Generation != owner.Binding.Generation {
				t.Fatal("reviewed successor missing", fresh)
			}
		})
	}
}

func TestBackupRemigrationAcceptedBatchCoverage(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			f := newPipelineFixture(t)
			for n := 0; n < 9; n++ {
				f.observe(t, fmt.Sprintf("batch-%d", n), "retained incident", f.now)
			}
			f.freeze(t, 1, 1)
			testutil.Must(t, coalesceNotificationBacklog(f.state, f.now))
			_, err := f.state.db.Exec("UPDATE outbox SET accepted=? WHERE kind='batch'", f.now.Format(eventTimeLayout))
			testutil.Must(t, err)
			var record backupRemigration
			record.Before.Binding = f.binding
			record.Spec.Cutover.Decisions = map[string]backupCutoverDecision{}
			ledgerTx(t, f.state, func(tx *sql.Tx) error {
				var e error
				record.Evidence, e = readBackupCutoverEvidence(tx)
				return e
			})
			for _, row := range record.Evidence.Rows {
				disposition := "unsent"
				if row.Accepted.Valid {
					disposition = "accepted"
				}
				record.Spec.Cutover.Decisions[row.ID] = backupCutoverDecision{notify.Hash(row), disposition, "reviewed batch"}
			}
			if broken {
				for n := range record.Evidence.Rows {
					if record.Evidence.Rows[n].CoveredBy.Valid {
						record.Evidence.Rows[n].CoveredBy.String = "different-batch"
						break
					}
				}
			}
			tx, err := f.state.db.Begin()
			testutil.Must(t, err)
			defer tx.Rollback()
			err = installRemigrationSources(tx, record)
			if broken {
				if err == nil {
					t.Fatal("accepted batch lost member provenance")
				}
				return
			}
			testutil.Must(t, err)
			testutil.Must(t, tx.Commit())
			var accepted, open, receipts int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='legacy_accepted'").Scan(&accepted))
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_receipt_history").Scan(&receipts))
			if accepted != 9 || open != 0 || receipts != 0 {
				t.Fatal("batch coverage or historical acceptance changed", accepted, open, receipts)
			}
		})
	}
}
