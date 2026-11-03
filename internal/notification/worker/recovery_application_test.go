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

func applicationFixture(t *testing.T, action string) (reconcileFixture, RecoveryDisposition) {
	t.Helper()
	f, d := dispositionFixture(t, action)
	_, err := f.r.DecideRecoveryMessage(context.Background(), d)
	must(t, err)
	_, err = f.r.PrepareRecoveryOwnership(context.Background(), producer)
	must(t, err)
	must(t, f.r.initializeApplications(context.Background()))
	return f, d
}

func TestRecoveryApplicationAppliesWithoutDeliveryAndPreservesAudit(t *testing.T) {
	for _, action := range []string{"retry", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f, d := applicationFixture(t, action)
			ctx := context.Background()
			var original string
			must(t, f.r.db.QueryRow("SELECT body FROM messages WHERE id=?", d.MessageID).Scan(&original))
			got, err := f.r.ApplyRecoveryMessage(ctx, producer, d.MessageID)
			must(t, err)
			want := "cancelled"
			if action == "retry" {
				want = "fenced"
			}
			if got.Application.State != want || got.Application.Decision.Decision != d || got.ReceiptPosition != 1 || got.Application.DeliveryEnabled {
				t.Fatal(got)
			}
			var body, state, hash, receiptBody string
			must(t, f.r.db.QueryRow("SELECT body,state,hash FROM messages WHERE id=?", d.MessageID).Scan(&body, &state, &hash))
			must(t, f.r.db.QueryRow("SELECT body FROM receipts WHERE position=?", got.ReceiptPosition).Scan(&receiptBody))
			var receipt protocol.Receipt
			must(t, json.Unmarshal([]byte(receiptBody), &receipt))
			if body != original || hash != d.MessageHash || state != want || receipt.State != want || receipt.Acceptance != nil || receipt.AttemptID != "" || receipt.Provider != "" {
				t.Fatal("application changed immutable evidence or fabricated acceptance", state, receipt)
			}
			must(t, f.r.db.QueryRow("SELECT state FROM messages WHERE id='first'").Scan(&state))
			if state != "provider_accepted" {
				t.Fatal("retained acceptance changed")
			}
			summary, err := f.r.Summary(ctx, producer)
			must(t, err)
			if protocol.Hash(summary) != d.SummaryHash || summary.Ambiguous != 1 {
				t.Fatal("original summary rewritten")
			}
			f.r.now = func() time.Time { return f.now.Add(24 * time.Hour) }
			again, err := f.r.ApplyRecoveryMessage(ctx, producer, d.MessageID)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(got) {
				t.Fatal("retry changed audit time or receipt")
			}
			decision, err := f.r.DecideRecoveryMessage(ctx, d)
			must(t, err)
			if protocol.Hash(decision) != protocol.Hash(got.Application.Decision) {
				t.Fatal("original decision changed")
			}
			changed := d
			changed.Reason = "replacement"
			if _, err = f.r.DecideRecoveryMessage(ctx, changed); err == nil {
				t.Fatal("applied decision replaced")
			}
			must(t, f.r.Close())
			session, err := OpenRecoveryApplicationSession(ctx, f.path, configuration())
			must(t, err)
			read, err := session.RecoveryMessageApplication(ctx, producer, d.MessageID)
			must(t, err)
			if protocol.Hash(read) != protocol.Hash(got) {
				t.Fatal("application changed on reopen")
			}
			review, err := session.ReviewRecoveryMessage(ctx, producer, d.MessageID)
			must(t, err)
			must(t, review.Validate())
			if review.ApplicationState != "applied" || review.Application == nil || review.RequestID != d.ID {
				t.Fatal(review)
			}
			must(t, session.Close())
			if normal, err := Open(f.path, configuration(), false); err == nil {
				normal.Close()
				t.Fatal("application removed recovery fence")
			}
		})
	}
}

func TestRecoveryApplicationRequiresRecordedDecisionAndRevokedOwnership(t *testing.T) {
	for _, mode := range []string{"no_decision", "decision_intent", "old_ownership", "pending_ownership", "live_generation", "unknown_generation", "sequence", "attempt", "clock", "busy", "missing_lock", "history_tail"} {
		t.Run(mode, func(t *testing.T) {
			f, d := dispositionFixture(t, "retry")
			ctx := context.Background()
			must(t, f.r.initializeApplications(ctx))
			if mode != "no_decision" {
				if mode == "decision_intent" {
					f.r.fault = func(s string) error {
						if s == "disposition_intent" {
							return errors.New("stop")
						}
						return nil
					}
				}
				_, err := f.r.DecideRecoveryMessage(ctx, d)
				if mode == "decision_intent" {
					if err == nil {
						t.Fatal("fault missing")
					}
				} else {
					must(t, err)
				}
				f.r.fault = nil
			}
			if mode != "old_ownership" {
				if mode == "pending_ownership" {
					f.r.fault = func(s string) error {
						if s == "recovery_ownership_intent" {
							return errors.New("stop")
						}
						return nil
					}
				}
				_, err := f.r.PrepareRecoveryOwnership(ctx, producer)
				if mode == "pending_ownership" {
					if err == nil {
						t.Fatal("fault missing")
					}
				} else {
					must(t, err)
				}
				f.r.fault = nil
			}
			switch mode {
			case "live_generation":
				s := &Store{dir: f.r.dir, identity: f.r.journal.After, config: f.r.config}
				must(t, s.Stage(producer, "another"))
				must(t, s.Activate(producer, "another"))
			case "unknown_generation":
				_, err := f.r.db.Exec("UPDATE messages SET generation='unknown' WHERE id=?", d.MessageID)
				must(t, err)
			case "sequence":
				_, err := f.r.db.Exec("UPDATE messages SET seq=99 WHERE id=?", d.MessageID)
				must(t, err)
			case "attempt":
				_, err := f.r.db.Exec("UPDATE messages SET attempt='live' WHERE id=?", d.MessageID)
				must(t, err)
			case "clock":
				f.r.now = func() time.Time { return f.now.Add(-time.Second) }
			case "busy":
				lease, err := f.r.dir.Lock(ownershipName(producer)+".lock", true, false)
				must(t, err)
				defer lease.Close()
			case "missing_lock":
				must(t, os.Remove(filepath.Join(f.path, ownershipName(producer)+".lock")))
			case "history_tail":
				s := &Store{dir: f.r.dir}
				owner := readOwner(t, s)
				must(t, s.dir.WriteJSON(ownershipHistoryPath(producer, owner.Revision+2), "foreign", false))
			}
			if _, err := f.r.ApplyRecoveryMessage(ctx, producer, d.MessageID); err == nil {
				t.Fatal("missing authority applied")
			}
			var count int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM receipts").Scan(&count))
			if count != 0 {
				t.Fatal("receipt created without authority")
			}
		})
	}
}

func TestRecoveryApplicationRejectsLostOrChangedEvidence(t *testing.T) {
	for _, mode := range []string{"intent", "decision", "receipt_missing", "receipt_changed", "record", "schema", "table", "state", "summary", "partial_rollback"} {
		t.Run(mode, func(t *testing.T) {
			f, d := applicationFixture(t, "cancel")
			ctx := context.Background()
			_, err := f.r.ApplyRecoveryMessage(ctx, producer, d.MessageID)
			must(t, err)
			switch mode {
			case "intent":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), applicationFile(producer, d.MessageID)))
			case "decision":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), dispositionFile(producer, d.MessageID)))
			case "receipt_missing":
				_, err = f.r.db.Exec("DELETE FROM receipts")
			case "receipt_changed":
				_, err = f.r.db.Exec("UPDATE receipts SET body=json_set(body,'$.state','provider_accepted')")
			case "record":
				_, err = f.r.db.Exec("UPDATE reconciliation_applications SET hash='changed'")
			case "schema":
				_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='recovery_applications_schema'")
			case "table":
				_, err = f.r.db.Exec("DROP TABLE reconciliation_applications")
			case "state":
				_, err = f.r.db.Exec("UPDATE messages SET state='ambiguous' WHERE id=?", d.MessageID)
			case "summary":
				_, err = f.r.db.Exec("UPDATE reconciliation_producers SET completion=''")
			case "partial_rollback":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_applications; UPDATE messages SET state='ambiguous' WHERE id='second';")
			}
			must(t, err)
			if _, err = f.r.ApplyRecoveryMessage(ctx, producer, d.MessageID); err == nil {
				t.Fatal("damaged evidence reapplied")
			}
			if _, err = f.r.RecoveryMessageApplication(ctx, producer, d.MessageID); err == nil {
				t.Fatal("damaged evidence returned proof")
			}
			if _, err = f.r.ReviewRecoveryMessage(ctx, producer, d.MessageID); err == nil {
				t.Fatal("damaged evidence reviewed")
			}
		})
	}
}

func TestRecoveryApplicationPendingIntentsCanResumeInDifferentOrder(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	// Neither retained message has terminal evidence in this exchange.
	f.records[0].Receipt = nil
	ch := beginReconciliation(t, f)
	must(t, f.r.Import(ctx, ch, f.records))
	summary, err := f.r.Finish(ctx, ch, snapshot(f.now, 2, 1))
	must(t, err)
	must(t, f.r.initializeDispositions(ctx))
	must(t, f.r.initializeApplications(ctx))
	_, err = f.r.PrepareRecoveryOwnership(ctx, producer)
	must(t, err)
	for _, record := range f.records {
		id, err := safefs.NewID("n")
		must(t, err)
		d := RecoveryDisposition{ID: id, Operation: ch.Operation, Identity: ch.Identity, Producer: producer, MessageID: record.Message.ID, MessageHash: protocol.Hash(record.Message), SummaryHash: protocol.Hash(summary), Action: "cancel", Actor: "operator", Reason: "Reviewed"}
		_, err = f.r.DecideRecoveryMessage(ctx, d)
		must(t, err)
	}
	f.r.fault = func(stage string) error {
		if stage == "application_intent" {
			return errors.New("lost response")
		}
		return nil
	}
	if _, err = f.r.ApplyRecoveryMessage(ctx, producer, "first"); err == nil {
		t.Fatal("fault missing")
	}
	f.r.fault = nil
	review, err := f.r.ReviewRecoveryMessage(ctx, producer, "first")
	must(t, err)
	if review.ApplicationState != "intent" || review.Application.ReceiptPosition != 0 {
		t.Fatal(review)
	}
	second, err := f.r.ApplyRecoveryMessage(ctx, producer, "second")
	must(t, err)
	first, err := f.r.ApplyRecoveryMessage(ctx, producer, "first")
	must(t, err)
	if second.ReceiptPosition != 1 || first.ReceiptPosition != 2 {
		t.Fatal("pending intents reserved receipt positions", first, second)
	}
}

func TestRecoveryApplicationScopesMessageToProducer(t *testing.T) {
	f, d := applicationFixture(t, "cancel")
	other := f.records[1].Message
	other.Producer.ID = "another-producer"
	raw, err := json.Marshal(other)
	must(t, err)
	// Give the other producer the earlier physical row, sharing the message ID.
	_, err = f.r.db.Exec("UPDATE messages SET producer=?,hash=?,body=? WHERE producer=? AND id=?", other.Producer.ID, protocol.Hash(other), string(raw), producer.ID, d.MessageID)
	must(t, err)
	raw, err = json.Marshal(f.records[1].Message)
	must(t, err)
	_, err = f.r.db.Exec("INSERT INTO messages(producer,id,seq,hash,body,generation,route_revision,state) SELECT ?,id,seq,?,?,generation,route_revision,state FROM messages WHERE producer=? AND id=?", producer.ID, d.MessageHash, string(raw), other.Producer.ID, d.MessageID)
	must(t, err)
	_, err = f.r.ApplyRecoveryMessage(context.Background(), producer, d.MessageID)
	must(t, err)
	var state string
	must(t, f.r.db.QueryRow("SELECT state FROM messages WHERE producer=? AND id=?", other.Producer.ID, d.MessageID).Scan(&state))
	if state != "ambiguous" {
		t.Fatal("other producer changed")
	}
}

func TestRecoveryApplicationRechecksAuthorityAtCommitBoundaries(t *testing.T) {
	for _, stage := range []string{"application_intent", "application_transaction"} {
		for _, mode := range []string{"producer_lock", "identity", "context"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				f, d := applicationFixture(t, "cancel")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				lock := filepath.Join(f.path, ownershipName(producer)+".lock")
				f.r.fault = func(at string) error {
					if at != stage {
						return nil
					}
					switch mode {
					case "producer_lock":
						must(t, os.Rename(lock, lock+".held"))
						must(t, os.WriteFile(lock, nil, 0600))
					case "identity":
						changed := f.r.journal.After
						changed.Recovery = "changed"
						must(t, f.r.dir.WriteJSON("identity.json", changed, true))
					case "context":
						cancel()
					}
					return nil
				}
				if _, err := f.r.ApplyRecoveryMessage(ctx, producer, d.MessageID); err == nil {
					t.Fatal("lost authority committed")
				}
				f.r.fault = nil
				switch mode {
				case "producer_lock":
					must(t, os.Remove(lock))
					must(t, os.Rename(lock+".held", lock))
				case "identity":
					must(t, f.r.dir.WriteJSON("identity.json", f.r.journal.After, true))
				}
				var n int
				must(t, f.r.db.QueryRow("SELECT count(*) FROM receipts").Scan(&n))
				if n != 0 {
					t.Fatal("receipt survived rollback")
				}
				applied, err := f.r.ApplyRecoveryMessage(context.Background(), producer, d.MessageID)
				must(t, err)
				if applied.ReceiptPosition != 1 {
					t.Fatal(applied)
				}
			})
		}
	}
}

func TestRecoveryApplicationCrashChild(t *testing.T) {
	input := os.Getenv("RECOVERY_APPLICATION_INPUT")
	if input == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(input)
	must(t, err)
	var v dispositionCrashInput
	must(t, json.Unmarshal(raw, &v))
	session, err := OpenRecoveryApplicationSession(context.Background(), v.Path, configuration())
	must(t, err)
	defer session.Close()
	r := session.(*recoveryReconciler)
	r.now = func() time.Time { return v.Now }
	r.fault = func(stage string) error {
		if stage == os.Getenv("RECOVERY_APPLICATION_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	_, err = r.ApplyRecoveryMessage(context.Background(), producer, v.Decision.MessageID)
	must(t, err)
	t.Fatal("fault missing")
}

func TestRecoveryApplicationSIGKILL(t *testing.T) {
	for _, action := range []string{"retry", "cancel"} {
		for _, stage := range []string{"application_intent", "application_transaction", "application_commit"} {
			t.Run(action+"/"+stage, func(t *testing.T) {
				f, d := applicationFixture(t, action)
				must(t, f.r.Close())
				input := filepath.Join(t.TempDir(), "input.json")
				raw, err := json.Marshal(dispositionCrashInput{Path: f.path, Decision: d, Now: f.now})
				must(t, err)
				must(t, os.WriteFile(input, raw, 0600))
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryApplicationCrashChild$")
				cmd.Env = append(os.Environ(), "RECOVERY_APPLICATION_INPUT="+input, "RECOVERY_APPLICATION_STAGE="+stage)
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
				session, err := OpenRecoveryApplicationSession(ctx, f.path, configuration())
				must(t, err)
				defer session.Close()
				review, err := session.ReviewRecoveryMessage(ctx, producer, d.MessageID)
				must(t, err)
				want := "intent"
				if stage == "application_commit" {
					want = "applied"
				}
				if review.ApplicationState != want {
					t.Fatal(review)
				}
				applied, err := session.ApplyRecoveryMessage(ctx, producer, d.MessageID)
				must(t, err)
				again, err := session.ApplyRecoveryMessage(ctx, producer, d.MessageID)
				must(t, err)
				if protocol.Hash(applied) != protocol.Hash(again) || applied.ReceiptPosition != 1 || !applied.Application.AppliedAt.Equal(f.now) {
					t.Fatal("restart changed application", applied, again)
				}
				var receipts int
				must(t, session.(*recoveryReconciler).db.QueryRow("SELECT count(*) FROM receipts").Scan(&receipts))
				if receipts != 1 {
					t.Fatal("duplicate receipt", receipts)
				}
				must(t, session.Close())
				if normal, err := Open(f.path, configuration(), false); err == nil {
					normal.Close()
					t.Fatal("restart enabled delivery")
				}
			})
		}
	}
}
