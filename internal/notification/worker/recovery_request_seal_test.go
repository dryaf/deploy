package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

func requestSealFixture(t *testing.T) (telegramHistoryFixture, *recoveryReconciler) {
	t.Helper()
	f := newTelegramHistoryFixture(t)
	_, err := ReconcileTelegramHistory(context.Background(), f.Path, f.History, f.Account, f.Config)
	must(t, err)
	s, err := OpenRequestRecoverySession(context.Background(), f.Path, f.Config)
	must(t, err)
	r := s.(*recoveryReconciler)
	t.Cleanup(func() { r.Close() })
	must(t, r.initializeCommandRecovery(context.Background()))
	return f, r
}
func TestRequestRecoverySealPreservesMappingsAndBlocksReplay(t *testing.T) {
	f, r := requestSealFixture(t)
	ctx := context.Background()
	var before string
	must(t, r.db.QueryRow("SELECT json_group_array(json_array(request_key,input_hash,command_id)) FROM command_inbox").Scan(&before))
	summary, err := r.SealRecoveryRequests(ctx)
	must(t, err)
	if summary.Mapped != 5 || summary.Unmapped != 2 || summary.Requests != 7 || summary.Opaque != 1 || summary.InboxRows != 7 || summary.IntakeEnabled {
		t.Fatal(summary)
	}
	var after string
	must(t, r.db.QueryRow("SELECT json_group_array(json_array(request_key,input_hash,command_id)) FROM command_inbox").Scan(&after))
	if before != after {
		t.Fatal("sealing remapped requests")
	}
	again, err := r.SealRecoveryRequests(ctx)
	must(t, err)
	if summary != again {
		t.Fatal("seal changed on replay")
	}
	// Borrow a candidate to exercise future runtime admission checks in isolation.
	// This starts no service and does not activate the restored installation.
	_, err = r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	runtime := &Store{db: r.db, dir: r.dir, identity: r.journal.After, config: r.config, now: time.Now}
	for _, in := range []interaction.Inbound{f.Executed, f.Confirmation} {
		v, err := runtime.AdmitCommand(ctx, in)
		must(t, err)
		if v.Command.ID != f.Command.ID || v.Command.Deadline != f.Command.Deadline {
			t.Fatal("old command identity or deadline refreshed")
		}
	}
	for _, text := range []string{f.Pending.Text, "help", "confirm new-token"} {
		in := f.Pending
		in.Text = text
		handled, err := runtime.AdmitGatewayQuery(ctx, in)
		if !handled {
			_, err = runtime.AdmitCommand(ctx, in)
		}
		var rejected *interaction.Rejection
		if !errors.As(err, &rejected) || !rejected.Silent {
			t.Fatal("unmapped input admitted or replied to", err)
		}
		must(t, runtime.recordCommandRejection(ctx, in, rejected))
	}
	must(t, r.db.QueryRow("SELECT json_group_array(json_array(request_key,input_hash,command_id)) FROM command_inbox").Scan(&after))
	if before != after {
		t.Fatal("quarantined replay acquired a new mapping")
	}
	if _, err = runtime.GrantCommand(ctx, producer, f.Command.ID); err == nil {
		t.Fatal("retained gateway-only command became executable")
	}
	must(t, r.Close())
	reopened, err := OpenRequestRecoverySession(ctx, f.Path, f.Config)
	must(t, err)
	defer reopened.Close()
	again, err = reopened.SealRecoveryRequests(ctx)
	must(t, err)
	if again != summary {
		t.Fatal("restart changed inventory")
	}
	must(t, reopened.Close())
	if normal, err := Open(f.Path, f.Config, false); err == nil {
		normal.Close()
		t.Fatal("request sealing activated worker")
	}
}

func TestRequestRecoverySealAllowsNewRequestIdentity(t *testing.T) {
	f, r := requestSealFixture(t)
	ctx := context.Background()
	_, err := r.SealRecoveryRequests(ctx)
	must(t, err)
	state, err := r.openTelegramDrain(ctx, f.Account)
	must(t, err)
	state, err = r.commitTelegramDrain(ctx, state, 0, provider.IntakePage{Account: f.Account}, nil)
	must(t, err)
	must(t, r.stage.WriteJSON(telegramDrainFile(f.Account), state, true))
	must(t, r.initializeTelegramCursorRecovery(ctx))
	cursor, err := r.CommitTelegramRecoveryCursor(ctx, f.Account)
	must(t, err)
	now := cursor.NotBefore.Add(time.Second)
	_, err = r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	runtime := &Store{db: r.db, dir: r.dir, identity: r.journal.After, config: r.config, now: func() time.Time { return now }}
	fresh := f.Pending
	fresh.NativeID = "update-99999"
	fresh.SentAt = now
	v, err := runtime.AdmitCommand(ctx, fresh)
	must(t, err)
	if v.Command.ID == f.Pending.CommandID() || v.Command.RequestKey != fresh.RequestKey() {
		t.Fatal("fresh request reused historical identity")
	}
}

func TestRequestRecoverySealRejectsDamagedEvidence(t *testing.T) {
	for _, mode := range []string{"mapping", "command", "query", "inbox", "state", "fence", "completion", "intent", "schema"} {
		t.Run(mode, func(t *testing.T) {
			f, r := requestSealFixture(t)
			_, err := r.SealRecoveryRequests(context.Background())
			must(t, err)
			switch mode {
			case "mapping":
				_, err = r.db.Exec("DELETE FROM command_inbox WHERE request_key=?", f.Executed.RequestKey())
			case "command":
				_, err = r.db.Exec("UPDATE commands SET body=json_set(body,'$.deadline','2030-01-01T00:00:00Z') WHERE id=?", f.Command.ID)
			case "query":
				_, err = r.db.Exec("UPDATE gateway_queries SET body=json_set(body,'$.kind','workers')")
			case "inbox":
				_, err = r.db.Exec("DELETE FROM provider_inbox WHERE id=400")
			case "state":
				_, err = r.db.Exec("UPDATE provider_inbox SET state='pending' WHERE id=400")
			case "fence":
				_, err = r.db.Exec("DELETE FROM reconciliation_request_fences WHERE key=?", f.Pending.RequestKey())
			case "completion":
				_, err = r.db.Exec("DELETE FROM reconciliation_request_seal")
			case "intent":
				err = os.Remove(filepath.Join(r.stage.File().Name(), "request-seal.json"))
			case "schema":
				_, err = r.db.Exec("DELETE FROM metadata WHERE key='request_recovery_schema'")
			}
			must(t, err)
			if _, err = r.SealRecoveryRequests(context.Background()); err == nil {
				t.Fatal("damaged evidence accepted", mode)
			}
		})
	}
}
func TestRequestRecoverySealPreventsLaterImports(t *testing.T) {
	f, r := requestSealFixture(t)
	ctx := context.Background()
	_, err := r.SealRecoveryRequests(ctx)
	must(t, err)
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO command_inbox VALUES(?,?,?)", protocol.Hash("late"), protocol.Hash("body"), f.Command.ID)
		return err
	})
	if err == nil {
		t.Fatal("late mapping changed sealed inventory")
	}
	var count int
	must(t, r.db.QueryRow("SELECT count(*) FROM command_inbox WHERE request_key=?", protocol.Hash("late")).Scan(&count))
	if count != 0 {
		t.Fatal("late mutation survived failed seal check")
	}
}
func TestRequestRecoverySealLostResponses(t *testing.T) {
	for _, boundary := range []string{"request_seal_intent", "request_seal_transaction", "request_seal_commit"} {
		t.Run(boundary, func(t *testing.T) {
			_, r := requestSealFixture(t)
			r.fault = func(at string) error {
				if at == boundary {
					return errors.New("lost response")
				}
				return nil
			}
			if _, err := r.SealRecoveryRequests(context.Background()); err == nil {
				t.Fatal("fault ignored")
			}
			r.fault = nil
			a, err := r.SealRecoveryRequests(context.Background())
			must(t, err)
			b, err := r.SealRecoveryRequests(context.Background())
			must(t, err)
			if a != b {
				t.Fatal("replay changed seal")
			}
		})
	}
}
func TestRequestRecoverySealWithRetainedSignalHistory(t *testing.T) {
	f := newSignalRecoveryFixture(t)
	ctx := context.Background()
	must(t, f.r.initializeRequestRecovery(ctx))
	if _, err := f.r.SealRecoveryRequests(ctx); err == nil {
		t.Fatal("redacted Signal position mistaken for a native request ID")
	}
	_, err := interaction.ReconcileSignal(ctx, f.source, f.r)
	must(t, err)
	v, err := f.r.SealRecoveryRequests(ctx)
	must(t, err)
	if v.Unmapped < 2 || v.Opaque == 0 {
		t.Fatal(v)
	}
	runtime := &Store{db: f.r.db, dir: f.r.dir, identity: f.r.journal.After, config: f.r.config, now: time.Now}
	_, err = runtime.AdmitCommand(ctx, f.unknown)
	var rejection *interaction.Rejection
	if !errors.As(err, &rejection) || !rejection.Silent {
		t.Fatal("Signal replay admitted", err)
	}
}

func TestRequestRecoverySealRechecksAuthority(t *testing.T) {
	for _, stage := range []string{"request_seal_intent", "request_seal_transaction"} {
		for _, mode := range []string{"lease", "context"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				f, r := requestSealFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				r.fault = func(at string) error {
					if at != stage {
						return nil
					}
					if mode == "context" {
						cancel()
					} else {
						lock := filepath.Join(f.Path, "worker.lock")
						must(t, os.Rename(lock, lock+".held"))
						must(t, os.WriteFile(lock, nil, 0600))
					}
					return nil
				}
				if _, err := r.SealRecoveryRequests(ctx); err == nil {
					t.Fatal("lost authority committed inventory")
				}
				var count int
				must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_request_seal").Scan(&count))
				if count != 0 {
					t.Fatal("seal committed after authority loss")
				}
			})
		}
	}
}

func TestRequestRecoverySealCoexistsWithCommandOutcomeInstallation(t *testing.T) {
	f, _ := commandInstallFixture(t, false)
	ctx := context.Background()
	must(t, f.r.initializeRequestRecovery(ctx))
	before, err := f.r.SealRecoveryRequests(ctx)
	must(t, err)
	_, err = interaction.InstallCommandRecovery(ctx, f.ledger, f.r)
	must(t, err)
	after, err := f.r.SealRecoveryRequests(ctx)
	must(t, err)
	if before != after {
		t.Fatal("outcome installation changed request identity inventory")
	}
}

func TestRequestRecoveryAdmissionRefusesIncompleteOrDamagedSeal(t *testing.T) {
	for _, mode := range []string{"unsealed", "missing_row", "missing_completion", "target", "mapping", "opaque_replay"} {
		t.Run(mode, func(t *testing.T) {
			f, r := requestSealFixture(t)
			ctx := context.Background()
			if mode != "unsealed" {
				_, err := r.SealRecoveryRequests(ctx)
				must(t, err)
			}
			in := f.Executed
			var err error
			switch mode {
			case "missing_row":
				_, err = r.db.Exec("DELETE FROM reconciliation_request_fences WHERE key=?", in.RequestKey())
			case "missing_completion":
				_, err = r.db.Exec("DELETE FROM reconciliation_request_seal")
			case "target":
				_, err = r.db.Exec("UPDATE commands SET body=json_set(body,'$.deadline','2030-01-01T00:00:00Z') WHERE id=?", in.CommandID())
			case "mapping":
				_, err = r.db.Exec("DELETE FROM command_inbox WHERE request_key=?", in.RequestKey())
			case "opaque_replay":
				in.NativeID = "update-95"
			}
			must(t, err)
			runtime := &Store{db: r.db, dir: r.dir, identity: r.journal.After, config: r.config, now: time.Now}
			if _, err = runtime.AdmitCommand(ctx, in); err == nil {
				t.Fatal("unsafe replay accepted")
			}
			if mode == "missing_row" || mode == "missing_completion" {
				if err = runtime.ImportCommand(ctx, producer, f.Command.ID, protocol.Hash(f.Command)); err == nil {
					t.Fatal("direct command API bypassed damaged request seal")
				}
			}
		})
	}
}

func TestRequestRecoverySealReservesDatabaseCapacity(t *testing.T) {
	_, r := requestSealFixture(t)
	_, err := r.db.Exec("CREATE TABLE capacity_padding(body BLOB);INSERT INTO capacity_padding VALUES(zeroblob(?))", r.config.MaxDatabaseBytes)
	must(t, err)
	if _, err = r.SealRecoveryRequests(context.Background()); err == nil {
		t.Fatal("full candidate accepted more fences")
	}
	_, err = os.Stat(filepath.Join(r.stage.File().Name(), "request-seal.json"))
	if !os.IsNotExist(err) {
		t.Fatal("capacity failure published intent", err)
	}
	_, err = r.db.Exec("DROP TABLE capacity_padding;VACUUM")
	must(t, err)
	_, err = r.SealRecoveryRequests(context.Background())
	must(t, err)
}

func TestRequestRecoverySealEmptyInventory(t *testing.T) {
	s, _, path := fixture(t)
	cfg := s.config
	artifact := filepath.Join(t.TempDir(), "snapshot")
	ctx := context.Background()
	_, err := s.Snapshot(ctx, artifact)
	must(t, err)
	must(t, s.Close())
	must(t, PrepareRecoveryQuarantine(ctx, path, artifact, cfg))
	session, err := OpenRequestRecoverySession(ctx, path, cfg)
	must(t, err)
	defer session.Close()
	a, err := session.SealRecoveryRequests(ctx)
	must(t, err)
	b, err := session.SealRecoveryRequests(ctx)
	must(t, err)
	if a != b || a.Requests != 0 || a.InboxRows != 0 || a.IntakeEnabled {
		t.Fatal(a, b)
	}
}

func TestRequestRecoverySealRejectsChangedSignalNativeIdentity(t *testing.T) {
	f := newSignalRecoveryFixture(t)
	ctx := context.Background()
	_, err := interaction.ReconcileSignal(ctx, f.source, f.r)
	must(t, err)
	must(t, f.r.initializeRequestRecovery(ctx))
	_, err = f.r.db.Exec("UPDATE provider_inbox SET body=json_set(body,'$.native_id','changed') WHERE CASE WHEN body='' THEN '' ELSE json_extract(body,'$.native_id') END=?", f.unknown.NativeID)
	must(t, err)
	if _, err = f.r.SealRecoveryRequests(ctx); err == nil {
		t.Fatal("Signal native identity changed before seal")
	}
}
