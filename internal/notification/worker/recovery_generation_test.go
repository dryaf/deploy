package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func generationFixture(t *testing.T) (reconcileFixture, RecoveryDisposition, RecoveryGeneration) {
	t.Helper()
	f, d := applicationFixture(t, "retry")
	ctx := context.Background()
	must(t, f.r.initializeRetryBindings(ctx))
	g, err := f.r.PrepareRecoveryGeneration(ctx, producer)
	must(t, err)
	_, err = f.r.ApplyRecoveryMessage(ctx, producer, d.MessageID)
	must(t, err)
	return f, d, g
}

func TestRecoveryGenerationAndRetryPreserveEnvelopeAndProofs(t *testing.T) {
	f, d, g := generationFixture(t)
	ctx := context.Background()
	var before string
	must(t, f.r.db.QueryRow("SELECT body FROM messages WHERE id=?", d.MessageID).Scan(&before))
	b, err := f.r.BindRecoveryRetry(ctx, producer, d.MessageID)
	must(t, err)
	if b.Generation != g || b.OriginalGeneration != f.records[1].Message.Generation || b.DeliveryEnabled {
		t.Fatal(b)
	}
	var body, hash, generation, state string
	must(t, f.r.db.QueryRow("SELECT body,hash,generation,state FROM messages WHERE id=?", d.MessageID).Scan(&body, &hash, &generation, &state))
	if before != body || hash != d.MessageHash || generation != g.Generation || state != "fenced" {
		t.Fatal("binding rewrote immutable evidence", generation, state)
	}
	var receipts int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM receipts").Scan(&receipts))
	if receipts != 1 {
		t.Fatal("binding minted receipt", receipts)
	}
	owner := readOwner(t, &Store{dir: f.r.dir})
	if owner.Active != "" || owner.Generations[g.Generation] != "staged" || owner.Generations[b.OriginalGeneration] != "revoked" {
		t.Fatal(owner)
	}
	proof, err := f.r.PrepareRecoveryOwnership(ctx, producer)
	must(t, err)
	if proof != g.Ownership {
		t.Fatal("staging changed migration proof")
	}
	_, err = f.r.ApplyRecoveryMessage(ctx, producer, d.MessageID)
	must(t, err)
	_, err = f.r.DecideRecoveryMessage(ctx, d)
	must(t, err)
	review, err := f.r.ReviewRecoveryMessage(ctx, producer, d.MessageID)
	must(t, err)
	must(t, review.Validate())
	if review.RetryBindingState != "bound" || protocol.Hash(review.RetryBinding) != protocol.Hash(b) {
		t.Fatal(review)
	}
	must(t, f.r.Close())
	session, err := OpenRecoveryGenerationSession(ctx, f.path, configuration())
	must(t, err)
	again, err := session.PrepareRecoveryGeneration(ctx, producer)
	must(t, err)
	if again != g {
		t.Fatal("restart allocated new generation")
	}
	rebound, err := session.BindRecoveryRetry(ctx, producer, d.MessageID)
	must(t, err)
	if protocol.Hash(rebound) != protocol.Hash(b) {
		t.Fatal("binding changed on retry")
	}
	must(t, session.Close())
	if normal, err := Open(f.path, configuration(), false); err == nil {
		normal.Close()
		t.Fatal("binding enabled service")
	}
}

// Exercise dispatch separately from managed activation, which remains disabled.
// These scoped stores borrow the isolated candidate only inside the test.
func retryDispatchStore(f reconcileFixture, now time.Time) *Store {
	return &Store{db: f.r.db, dir: f.r.dir, identity: f.r.journal.After, config: f.r.config, session: "dispatch-fixture", now: func() time.Time { return now }}
}

func TestRecoveryRetryDispatchUsesNewGenerationAndDrain(t *testing.T) {
	f, d, g := generationFixture(t)
	ctx := context.Background()
	_, err := f.r.BindRecoveryRetry(ctx, producer, d.MessageID)
	must(t, err)
	s := retryDispatchStore(f, f.now)
	_, granted, err := s.Grant(ctx, producer, d.MessageID)
	must(t, err)
	if granted {
		t.Fatal("staged generation granted attempt")
	}
	must(t, s.Replace(ctx, snapshot(f.now, 2, 1)))
	must(t, s.Activate(producer, g.Generation))
	job, granted, err := s.Grant(ctx, producer, d.MessageID)
	must(t, err)
	if !granted {
		t.Fatal("approved retry was not eligible", job)
	}
	if protocol.Hash(job.Message) != d.MessageHash || job.Message.Generation == g.Generation {
		t.Fatal("provider received a rewritten envelope")
	}
	if err = s.Revoke(ctx, producer, g.Generation); err == nil {
		t.Fatal("new generation ignored in-flight retry")
	}
	must(t, s.Finish(ctx, job, Result{Accepted: true}))
	must(t, s.Revoke(ctx, producer, g.Generation))
}

func TestRecoveryRetryStillRequiresCurrentValidityAndRoute(t *testing.T) {
	for _, mode := range []string{"expired", "stale", "route", "unapproved_column"} {
		t.Run(mode, func(t *testing.T) {
			f, d, g := generationFixture(t)
			ctx := context.Background()
			if mode != "unapproved_column" {
				_, err := f.r.BindRecoveryRetry(ctx, producer, d.MessageID)
				must(t, err)
			} else {
				_, err := f.r.db.Exec("UPDATE messages SET generation=? WHERE id=?", g.Generation, d.MessageID)
				must(t, err)
			}
			now := f.now
			if mode == "expired" {
				now = now.Add(time.Hour)
			}
			s := retryDispatchStore(f, now)
			must(t, s.Activate(producer, g.Generation))
			if mode != "stale" && mode != "unapproved_column" {
				must(t, s.Replace(ctx, snapshot(now, 2, 1)))
			}
			if mode == "route" {
				s.config.Routes = map[string]Route{"operations": {Revision: "changed", Primary: "signal"}}
			}
			_, granted, err := s.Grant(ctx, producer, d.MessageID)
			if mode == "unapproved_column" {
				if err == nil {
					t.Fatal("column override accepted")
				}
			} else {
				must(t, err)
			}
			if granted {
				t.Fatal("recovery bypassed current guard", mode)
			}
		})
	}
}

func TestRecoveryGenerationRejectsMissingOrChangedAuthority(t *testing.T) {
	for _, mode := range []string{"no_migration", "old_generation", "lost_plan", "changed_plan", "changed_summary", "foreign_stage", "history", "busy", "lock"} {
		t.Run(mode, func(t *testing.T) {
			f, d := dispositionFixture(t, "retry")
			ctx := context.Background()
			if mode != "no_migration" {
				_, err := f.r.PrepareRecoveryOwnership(ctx, producer)
				must(t, err)
			}
			if mode == "lost_plan" || mode == "changed_plan" || mode == "old_generation" {
				_, err := f.r.PrepareRecoveryGeneration(ctx, producer)
				must(t, err)
			}
			switch mode {
			case "lost_plan":
				must(t, os.Remove(filepath.Join(f.r.stage.File().Name(), generationFile(producer))))
			case "changed_plan", "old_generation":
				var plan recoveryGenerationIntent
				must(t, f.r.stage.ReadJSON(generationFile(producer), &plan))
				if mode == "old_generation" {
					plan.Generation = "generation-one"
				} else {
					plan.Ownership.SummaryHash = protocol.Hash("changed")
				}
				must(t, f.r.stage.WriteJSON(generationFile(producer), plan, true))
			case "changed_summary":
				_, err := f.r.db.Exec("UPDATE reconciliation_producers SET completion=json_set(completion,'$.Ambiguous',0)")
				must(t, err)
			case "foreign_stage":
				s := &Store{dir: f.r.dir, identity: f.r.journal.After, config: f.r.config}
				must(t, s.Stage(producer, "foreign"))
			case "history":
				must(t, os.Remove(filepath.Join(f.path, ownershipHistoryPath(producer, 1))))
			case "busy":
				lease, err := f.r.dir.Lock(ownershipName(producer)+".lock", true, false)
				must(t, err)
				defer lease.Close()
			case "lock":
				must(t, os.Remove(filepath.Join(f.path, ownershipName(producer)+".lock")))
			}
			if _, err := f.r.PrepareRecoveryGeneration(ctx, producer); err == nil {
				t.Fatal("invalid generation staged", mode, d.MessageID)
			}
		})
	}
}

func TestRecoveryGenerationAndBindingAuthorityLossAtCommit(t *testing.T) {
	for _, boundary := range []string{"generation_intent", "generation_ownership_history", "retry_binding_intent", "retry_binding_transaction"} {
		for _, loss := range []string{"cancel", "replaced_lock"} {
			t.Run(boundary+"/"+loss, func(t *testing.T) {
				f, d := applicationFixture(t, "retry")
				must(t, f.r.initializeRetryBindings(context.Background()))
				binding := boundary == "retry_binding_intent" || boundary == "retry_binding_transaction"
				if binding {
					_, err := f.r.PrepareRecoveryGeneration(context.Background(), producer)
					must(t, err)
					_, err = f.r.ApplyRecoveryMessage(context.Background(), producer, d.MessageID)
					must(t, err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				lockPath := filepath.Join(f.path, ownershipName(producer)+".lock")
				reached := false
				f.r.fault = func(stage string) error {
					if stage == boundary && !reached {
						reached = true
						if loss == "cancel" {
							cancel()
						} else {
							must(t, os.Rename(lockPath, lockPath+".held"))
							must(t, os.WriteFile(lockPath, nil, 0600))
						}
					}
					return nil
				}
				var err error
				if binding {
					_, err = f.r.BindRecoveryRetry(ctx, producer, d.MessageID)
				} else {
					_, err = f.r.PrepareRecoveryGeneration(ctx, producer)
				}
				if !reached || err == nil {
					t.Fatal("authority loss did not interrupt commit", reached, err)
				}
				if binding {
					var count int
					must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_retry_bindings").Scan(&count))
					if count != 0 {
						t.Fatal("binding committed after authority loss")
					}
				}
				if loss == "replaced_lock" {
					must(t, os.Rename(lockPath+".held", lockPath))
				}
				f.r.fault = nil
				if binding {
					_, err = f.r.BindRecoveryRetry(context.Background(), producer, d.MessageID)
				} else {
					_, err = f.r.PrepareRecoveryGeneration(context.Background(), producer)
				}
				must(t, err)
			})
		}
	}
}

func TestRecoveryRetryRejectsLostEvidence(t *testing.T) {
	for _, mode := range []string{"retry_file", "generation_file", "application_file", "receipt", "row", "schema", "rollback", "column", "summary", "history"} {
		t.Run(mode, func(t *testing.T) {
			f, d, g := generationFixture(t)
			ctx := context.Background()
			b, err := f.r.BindRecoveryRetry(ctx, producer, d.MessageID)
			must(t, err)
			switch mode {
			case "retry_file":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), retryBindingFile(producer, d.MessageID)))
			case "generation_file":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), generationFile(producer)))
			case "application_file":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), applicationFile(producer, d.MessageID)))
			case "receipt":
				_, err = f.r.db.Exec("DELETE FROM receipts")
			case "row":
				_, err = f.r.db.Exec("UPDATE reconciliation_retry_bindings SET hash='changed'")
			case "schema":
				_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='recovery_retries_schema'")
			case "rollback":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_retry_bindings")
			case "column":
				_, err = f.r.db.Exec("UPDATE messages SET generation=? WHERE id=?", b.OriginalGeneration, d.MessageID)
			case "summary":
				_, err = f.r.db.Exec("UPDATE reconciliation_producers SET completion=''")
			case "history":
				err = os.Remove(filepath.Join(f.path, ownershipHistoryPath(producer, g.Revision)))
			}
			must(t, err)
			if _, err = f.r.BindRecoveryRetry(ctx, producer, d.MessageID); err == nil {
				t.Fatal("lost evidence rebound")
			}
		})
	}
}

func TestRecoveryRetryRequiresAppliedConsent(t *testing.T) {
	for _, mode := range []string{"undecided", "unapplied", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			action := "retry"
			if mode == "cancel" {
				action = "cancel"
			}
			f, d := dispositionFixture(t, action)
			ctx := context.Background()
			must(t, f.r.initializeApplications(ctx))
			must(t, f.r.initializeRetryBindings(ctx))
			if mode != "undecided" {
				_, err := f.r.DecideRecoveryMessage(ctx, d)
				must(t, err)
			}
			_, err := f.r.PrepareRecoveryOwnership(ctx, producer)
			must(t, err)
			_, err = f.r.PrepareRecoveryGeneration(ctx, producer)
			must(t, err)
			if mode == "cancel" {
				_, err = f.r.ApplyRecoveryMessage(ctx, producer, d.MessageID)
				must(t, err)
			}
			if _, err = f.r.BindRecoveryRetry(ctx, producer, d.MessageID); err == nil {
				t.Fatal("unapproved retry bound")
			}
		})
	}
}

func TestRecoveryGenerationCrashChild(t *testing.T) {
	path := os.Getenv("RECOVERY_GENERATION_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	session, err := OpenRecoveryGenerationSession(context.Background(), path, configuration())
	must(t, err)
	defer session.Close()
	r := session.(*recoveryReconciler)
	r.fault = func(at string) error {
		if at == os.Getenv("RECOVERY_GENERATION_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	if os.Getenv("RECOVERY_GENERATION_BIND") == "1" {
		_, err = session.BindRecoveryRetry(context.Background(), producer, "second")
	} else {
		_, err = session.PrepareRecoveryGeneration(context.Background(), producer)
	}
	must(t, err)
	t.Fatal("fault missing")
}

func TestRecoveryGenerationAndBindingSIGKILL(t *testing.T) {
	for _, stage := range []string{"generation_intent", "generation_ownership_intent", "generation_ownership_history", "generation_ownership_owner", "retry_binding_intent", "retry_binding_transaction", "retry_binding_commit"} {
		t.Run(stage, func(t *testing.T) {
			f, d := applicationFixture(t, "retry")
			ctx := context.Background()
			must(t, f.r.initializeRetryBindings(ctx))
			_, err := f.r.ApplyRecoveryMessage(ctx, producer, d.MessageID)
			must(t, err)
			bind := "0"
			if stage[:5] == "retry" {
				bind = "1"
				_, err = f.r.PrepareRecoveryGeneration(ctx, producer)
				must(t, err)
			}
			must(t, f.r.Close())
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryGenerationCrashChild$")
			cmd.Env = append(os.Environ(), "RECOVERY_GENERATION_PATH="+f.path, "RECOVERY_GENERATION_STAGE="+stage, "RECOVERY_GENERATION_BIND="+bind)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			must(t, cmd.Start())
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			})
			scan := bufio.NewScanner(pipe)
			if !scan.Scan() || scan.Text() != "READY" {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("fault missing", stderr.String())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			session, err := OpenRecoveryGenerationSession(ctx, f.path, configuration())
			must(t, err)
			defer session.Close()
			var plan recoveryGenerationIntent
			must(t, session.(*recoveryReconciler).stage.ReadJSON(generationFile(producer), &plan))
			g, err := session.PrepareRecoveryGeneration(ctx, producer)
			must(t, err)
			if g.Generation != plan.Generation {
				t.Fatal("restart allocated a second generation")
			}
			if bind == "1" {
				review, err := session.ReviewRecoveryMessage(ctx, producer, d.MessageID)
				must(t, err)
				want := "intent"
				if stage == "retry_binding_commit" {
					want = "bound"
				}
				if review.RetryBindingState != want {
					t.Fatal(review)
				}
			}
			b, err := session.BindRecoveryRetry(ctx, producer, d.MessageID)
			must(t, err)
			again, err := session.BindRecoveryRetry(ctx, producer, d.MessageID)
			must(t, err)
			if protocol.Hash(b) != protocol.Hash(again) || b.Generation != g {
				t.Fatal("binding replay changed")
			}
			must(t, session.Close())
			if normal, err := Open(f.path, configuration(), false); err == nil {
				normal.Close()
				t.Fatal("recovery enabled service")
			}
		})
	}
}

func TestRecoveryRetryLostCommitResponse(t *testing.T) {
	f, d, _ := generationFixture(t)
	ctx := context.Background()
	f.r.fault = func(at string) error {
		if at == "retry_binding_commit" {
			return errors.New("lost response")
		}
		return nil
	}
	_, err := f.r.BindRecoveryRetry(ctx, producer, d.MessageID)
	if err == nil {
		t.Fatal("fault missing")
	}
	f.r.fault = nil
	_, err = f.r.BindRecoveryRetry(ctx, producer, d.MessageID)
	must(t, err)
	var receipts int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM receipts").Scan(&receipts))
	if receipts != 1 {
		t.Fatal("binding minted receipts")
	}
}

func TestRecoveryRetryRestoresSemanticFloorsWithoutRegressing(t *testing.T) {
	for _, existing := range []uint64{0, 5} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			f := newReconcileFixture(t)
			ctx := context.Background()
			m := &f.records[1].Message
			m.Incidents = []protocol.Incident{{Subject: "backup", ID: "incident", Revision: 1}}
			newer := *m
			newer.ID = "third"
			newer.Sequence = 3
			newer.ReportRevision = 2
			newer.Incidents = []protocol.Incident{{Subject: "backup", ID: "incident", Revision: 2}}
			f.records = append(f.records, producerRecoveryRecord{Message: newer})
			f.manifest.HighestIssued = 3
			ch := beginReconciliation(t, f)
			must(t, f.r.Import(ctx, ch, f.records))
			summary, err := f.r.Finish(ctx, ch, snapshot(f.now, 2, 1))
			must(t, err)
			must(t, f.r.initializeDispositions(ctx))
			must(t, f.r.initializeApplications(ctx))
			must(t, f.r.initializeRetryBindings(ctx))
			id, err := safefs.NewID("n")
			must(t, err)
			d := RecoveryDisposition{ID: id, Operation: ch.Operation, Identity: ch.Identity, Producer: producer, MessageID: m.ID, MessageHash: protocol.Hash(*m), SummaryHash: protocol.Hash(summary), Action: "retry", AcceptDuplicateRisk: true, Actor: "operator", Reason: "Reviewed"}
			_, err = f.r.DecideRecoveryMessage(ctx, d)
			must(t, err)
			_, err = f.r.PrepareRecoveryOwnership(ctx, producer)
			must(t, err)
			_, err = f.r.ApplyRecoveryMessage(ctx, producer, d.MessageID)
			must(t, err)
			g, err := f.r.PrepareRecoveryGeneration(ctx, producer)
			must(t, err)
			if existing != 0 {
				_, err = f.r.db.Exec("INSERT INTO reports VALUES(?,?,?)", producer.ID, m.ReportKey, existing)
				must(t, err)
				_, err = f.r.db.Exec("INSERT INTO incidents(producer,subject,id,revision) VALUES(?,?,?,?)", producer.ID, "backup", "incident", existing)
				must(t, err)
			}
			_, err = f.r.BindRecoveryRetry(ctx, producer, d.MessageID)
			must(t, err)
			var reportFloor, incidentFloor uint64
			must(t, f.r.db.QueryRow("SELECT revision FROM reports WHERE producer=? AND key=?", producer.ID, m.ReportKey).Scan(&reportFloor))
			must(t, f.r.db.QueryRow("SELECT revision FROM incidents WHERE producer=? AND subject='backup' AND id='incident'", producer.ID).Scan(&incidentFloor))
			want := uint64(2)
			if existing > want {
				want = existing
			}
			if reportFloor != want || incidentFloor != want {
				t.Fatal(reportFloor, incidentFloor, want)
			}
			s := retryDispatchStore(f, f.now)
			must(t, s.Activate(producer, g.Generation))
			must(t, s.Replace(ctx, snapshot(f.now, 2, 1)))
			_, granted, err := s.Grant(ctx, producer, d.MessageID)
			must(t, err)
			if granted {
				t.Fatal("obsolete report revived")
			}
		})
	}
}

func TestRecoveryRetryDispatchRejectsDamagedProof(t *testing.T) {
	for _, mode := range []string{"intent", "application", "receipt", "record", "schema", "generation", "history"} {
		t.Run(mode, func(t *testing.T) {
			f, d, g := generationFixture(t)
			ctx := context.Background()
			_, err := f.r.BindRecoveryRetry(ctx, producer, d.MessageID)
			must(t, err)
			s := retryDispatchStore(f, f.now)
			must(t, s.Activate(producer, g.Generation))
			must(t, s.Replace(ctx, snapshot(f.now, 2, 1)))
			switch mode {
			case "intent":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), retryBindingFile(producer, d.MessageID)))
			case "application":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), applicationFile(producer, d.MessageID)))
			case "receipt":
				_, err = f.r.db.Exec("DELETE FROM receipts")
			case "record":
				_, err = f.r.db.Exec("UPDATE reconciliation_retry_bindings SET hash='changed'")
			case "schema":
				_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='recovery_retries_schema'")
			case "generation":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), generationFile(producer)))
			case "history":
				err = os.Remove(filepath.Join(f.path, ownershipHistoryPath(producer, g.Revision)))
			}
			must(t, err)
			_, granted, err := s.Grant(ctx, producer, d.MessageID)
			if err == nil || granted {
				t.Fatal("damaged proof authorized attempt", mode)
			}
		})
	}
}

func TestRecoveryGenerationReviewRejectsChangedBinding(t *testing.T) {
	f, d, _ := generationFixture(t)
	ctx := context.Background()
	_, err := f.r.BindRecoveryRetry(ctx, producer, d.MessageID)
	must(t, err)
	v, err := f.r.ReviewRecoveryMessage(ctx, producer, d.MessageID)
	must(t, err)
	raw, err := json.Marshal(v)
	must(t, err)
	for _, mode := range []string{"phase", "application", "generation", "generation_history", "message"} {
		var altered RecoveryMessageReview
		must(t, json.Unmarshal(raw, &altered))
		switch mode {
		case "phase":
			altered.RetryBindingState = "active"
		case "application":
			altered.Application = nil
		case "generation":
			altered.RetryBinding.Generation.DeliveryEnabled = true
		case "generation_history":
			altered.RetryBinding.Generation.History = altered.RetryBinding.Generation.Ownership.History
		case "message":
			altered.RetryBinding.OriginalGeneration = "changed"
		}
		if err = altered.Validate(); err == nil {
			t.Fatal("changed review accepted", mode)
		}
	}
}
