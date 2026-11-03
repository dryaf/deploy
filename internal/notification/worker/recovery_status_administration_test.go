package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func administrativeRecoveryFixture(t *testing.T, pending bool) (*recoveryReconciler, MessageDecision) {
	t.Helper()
	s, _, path, decision := administrationFixture(t)
	ctx := context.Background()
	artifact := filepath.Join(t.TempDir(), "snapshot")
	manifest, err := s.Snapshot(ctx, artifact)
	must(t, err)
	if pending {
		must(t, s.dir.Mkdir("administration"))
		must(t, s.dir.WriteJSON(messageIntentFile(decision.Review.ID), decision, false))
	} else {
		_, err = s.DecideMessage(ctx, decision)
		must(t, err)
	}
	c := s.config
	must(t, s.Close())
	_, err = QuarantineRecovery(ctx, path, artifact, c, manifest.ID, manifest.DatabaseHash)
	must(t, err)
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	must(t, err)
	t.Cleanup(func() { r.Close() })
	return r, decision
}

func TestRecoveryEvidenceRetainsAdministrativeAudit(t *testing.T) {
	for _, state := range []string{"completed", "missing-acknowledgment", "pending-intent"} {
		t.Run(state, func(t *testing.T) {
			r, decision := administrativeRecoveryFixture(t, state == "pending-intent")
			id := decision.Review.ID
			if state == "missing-acknowledgment" {
				must(t, os.Remove(filepath.Join(r.dir.File().Name(), messageResultFile(id))))
			}
			ctx := context.Background()
			review, err := r.reviewRecovery(ctx)
			must(t, err)
			statusCheck(t, review, "worker", "administrative_audit", "complete")
			if state == "missing-acknowledgment" {
				statusCheck(t, review, "worker", "administrative_acknowledgments", "pending")
			}
			if state == "pending-intent" {
				statusCheck(t, review, "worker", "administrative_intents", "retained_fenced")
			}
			cp, err := r.checkpointRecovery(ctx, review.ReviewHash)
			must(t, err)
			d, err := r.stage.Sub("checkpoints/" + review.ReviewHash)
			must(t, err)
			defer d.Close()
			v, err := readCheckpointEvidence(ctx, d, cp, review, r.config.MaxDatabaseBytes)
			must(t, err)
			seen := map[string]bool{}
			for _, f := range v.Files {
				seen[f.Path] = true
			}
			if !seen["worker/"+messageIntentFile(id)] || seen["worker/"+messageResultFile(id)] != (state == "completed") {
				t.Fatal("administrative file presence changed", seen)
			}
			if state != "completed" {
				if _, err := os.Stat(filepath.Join(r.dir.File().Name(), messageResultFile(id))); !os.IsNotExist(err) {
					t.Fatal("checkpoint repaired or executed a decision", err)
				}
			}
			var receipts int
			must(t, r.db.QueryRow("SELECT count(*) FROM receipts").Scan(&receipts))
			if receipts != 0 {
				t.Fatal("old administrative decision created a receipt")
			}
		})
	}
}

func TestRecoveryStatusRejectsLostAdministrativeAuthority(t *testing.T) {
	for _, kind := range []string{"intent", "result", "sql", "source-ack", "late-intent"} {
		t.Run(kind, func(t *testing.T) {
			r, decision := administrativeRecoveryFixture(t, false)
			var err error
			switch kind {
			case "intent":
				err = os.Remove(filepath.Join(r.dir.File().Name(), messageIntentFile(decision.Review.ID)))
			case "result":
				err = r.dir.WriteJSON(messageResultFile(decision.Review.ID), map[string]string{}, true)
			case "sql":
				_, err = r.db.Exec("DELETE FROM metadata WHERE key LIKE 'message-decision-archive-v1:%'")
			case "source-ack":
				_, err = r.db.Exec("DELETE FROM metadata WHERE key=?", "message-decision-recovery-source-v1:"+r.journal.Operation)
			case "late-intent":
				r.fault = func(at string) error {
					if at == "recovery_status_observed" {
						changed := decision
						changed.Reason = "changed after observation"
						return r.dir.WriteJSON(messageIntentFile(decision.Review.ID), changed, true)
					}
					return nil
				}
			}
			must(t, err)
			if _, err = r.reviewRecovery(context.Background()); err == nil {
				t.Fatal("lost administrative authority accepted")
			}
		})
	}
}

func TestRecoveryEvidenceHashPinsPendingAdministrativeIntent(t *testing.T) {
	r, decision := administrativeRecoveryFixture(t, true)
	first, err := r.reviewRecovery(context.Background())
	must(t, err)
	decision.Reason = "another still-pending operator request"
	must(t, r.dir.WriteJSON(messageIntentFile(decision.Review.ID), decision, true))
	second, err := r.reviewRecovery(context.Background())
	must(t, err)
	if first.EvidenceHash == second.EvidenceHash || first.ReviewHash == second.ReviewHash || first.ProjectionHash != second.ProjectionHash {
		t.Fatal("review failed to distinguish independent administrative intent change")
	}
	if protocol.Hash(first.Checks) != protocol.Hash(second.Checks) {
		t.Fatal("pending intent became an execution grant")
	}
}
