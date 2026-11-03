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

func telegramCursorFixture(t *testing.T) (telegramHistoryFixture, *recoveryReconciler, TelegramDrainSummary) {
	t.Helper()
	f, p := newTelegramDrainFixture(t)
	ctx := context.Background()
	drain, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, nil)
	must(t, err)
	requests, err := OpenRequestRecoverySession(ctx, f.Path, f.Config)
	must(t, err)
	_, err = requests.SealRecoveryRequests(ctx)
	must(t, err)
	must(t, requests.Close())
	session, err := OpenTelegramRecoveryCursorSession(ctx, f.Path, f.Config)
	must(t, err)
	r := session.(*recoveryReconciler)
	t.Cleanup(func() { r.Close() })
	return f, r, drain
}
func setTelegramHandoffClock(r *recoveryReconciler, d TelegramDrainSummary, elapsed time.Duration) {
	r.now = func() time.Time { return d.ObservedAt.Add(elapsed) }
	r.clock = func() (string, float64, error) { return d.BootID, d.Uptime + elapsed.Seconds(), nil }
}
func TestTelegramCursorCommitPreservesEvidenceAndRestartsAtZero(t *testing.T) {
	f, r, drain := telegramCursorFixture(t)
	ctx := context.Background()
	var before string
	must(t, r.db.QueryRow("SELECT json_group_array(json_array(account,id,hash,body,state)) FROM provider_inbox").Scan(&before))
	proof, err := r.CommitTelegramRecoveryCursor(ctx, f.Account)
	must(t, err)
	must(t, proof.Validate())
	if proof.BeforeOffset != 6 || proof.Offset != 0 || proof.Drain != drain || proof.HistoryComplete || proof.IntakeEnabled {
		t.Fatal(proof)
	}
	var offset int64
	must(t, r.db.QueryRow("SELECT offset FROM telegram_cursors WHERE account=?", f.Account).Scan(&offset))
	if offset != 0 {
		t.Fatal("inherited positive provider ACK offset", offset)
	}
	var after string
	must(t, r.db.QueryRow("SELECT json_group_array(json_array(account,id,hash,body,state)) FROM provider_inbox").Scan(&after))
	if before != after {
		t.Fatal("cursor commit rewrote historical inputs")
	}
	again, err := r.CommitTelegramRecoveryCursor(ctx, f.Account)
	must(t, err)
	if again != proof {
		t.Fatal("replay changed cutoff")
	}
	must(t, r.Close())
	session, err := OpenTelegramRecoveryCursorSession(ctx, f.Path, f.Config)
	must(t, err)
	defer session.Close()
	again, err = session.CommitTelegramRecoveryCursor(ctx, f.Account)
	must(t, err)
	if again != proof {
		t.Fatal("restart changed cutoff")
	}
	must(t, session.Close())
	if normal, err := Open(f.Path, f.Config, false); err == nil {
		normal.Close()
		t.Fatal("cursor commit activated intake")
	}
}
func TestTelegramCursorCutoffGuardsProviderAndDirectAdmission(t *testing.T) {
	f, r, _ := telegramCursorFixture(t)
	ctx := context.Background()
	proof, err := r.CommitTelegramRecoveryCursor(ctx, f.Account)
	must(t, err)
	now := proof.NotBefore.Add(time.Second)
	runtime := &Store{db: r.db, dir: r.dir, identity: r.journal.After, config: r.config, now: func() time.Time { return now }}
	offset, err := runtime.TelegramOffset(ctx, f.Account)
	must(t, err)
	if offset != 0 {
		t.Fatal(offset)
	}
	old := f.Pending
	old.NativeID = "update-8"
	old.SentAt = proof.NotBefore
	fresh := old
	fresh.NativeID = "update-9"
	fresh.SentAt = now
	must(t, runtime.PersistTelegram(ctx, provider.IntakePage{Account: f.Account, Next: 10, Events: []provider.IntakeEvent{{ID: 8, Hash: protocol.Hash(old), Inbound: &old}, {ID: 9, Hash: protocol.Hash(fresh), Inbound: &fresh}}}))
	var body, state string
	must(t, r.db.QueryRow("SELECT body,state FROM provider_inbox WHERE account=? AND id=8", f.Account).Scan(&body, &state))
	if body != "" || state != "rejected" {
		t.Fatal("old input escaped cutoff", body, state)
	}
	for _, text := range []string{old.Text, "help", "confirm some-token"} {
		old.Text = text
		handled, err := runtime.AdmitGatewayQuery(ctx, old)
		if !handled {
			_, err = runtime.AdmitCommand(ctx, old)
		}
		var rejection *interaction.Rejection
		if !errors.As(err, &rejection) || !rejection.Silent {
			t.Fatal("old direct input admitted", err)
		}
		must(t, runtime.recordCommandRejection(ctx, old, rejection))
	}
	must(t, runtime.ConvertIntake(ctx))
	var count int
	must(t, r.db.QueryRow("SELECT count(*) FROM command_inbox WHERE request_key=?", old.RequestKey()).Scan(&count))
	if count != 0 {
		t.Fatal("old input acquired a command/query identity")
	}
	must(t, r.db.QueryRow("SELECT count(*) FROM command_inbox WHERE request_key=?", fresh.RequestKey()).Scan(&count))
	if count != 1 {
		t.Fatal("fresh input not admitted")
	}
	offset, err = runtime.TelegramOffset(ctx, f.Account)
	must(t, err)
	if offset != 10 {
		t.Fatal("ordinary cursor did not advance", offset)
	}
}
func TestTelegramCursorFreshnessAndAuditReplay(t *testing.T) {
	for _, mode := range []string{"settling", "expired", "reboot", "wall_backward", "monotonic_backward", "wall_jump", "valid"} {
		t.Run(mode, func(t *testing.T) {
			f, r, d := telegramCursorFixture(t)
			ctx := context.Background()
			proof, err := r.CommitTelegramRecoveryCursor(ctx, f.Account)
			must(t, err)
			setTelegramHandoffClock(r, d, 6*time.Second)
			switch mode {
			case "settling":
				setTelegramHandoffClock(r, d, telegramHandoffSkew)
			case "expired":
				setTelegramHandoffClock(r, d, telegramHandoffLifetime)
			case "reboot":
				r.clock = func() (string, float64, error) { return "another-boot", d.Uptime + 6, nil }
			case "wall_backward":
				r.now = func() time.Time { return d.ObservedAt.Add(-time.Second) }
			case "monotonic_backward":
				r.clock = func() (string, float64, error) { return d.BootID, d.Uptime - 1, nil }
			case "wall_jump":
				r.now = func() time.Time { return d.ObservedAt.Add(time.Minute) }
			}
			_, err = r.ValidateTelegramRecoveryHandoff(ctx, f.Account)
			if (err == nil) != (mode == "valid") {
				t.Fatal(mode, err)
			}
			replay, err := r.CommitTelegramRecoveryCursor(ctx, f.Account)
			must(t, err)
			if replay != proof {
				t.Fatal("audit replay renewed expired authority")
			}
		})
	}
}
func TestTelegramCursorRejectsDamage(t *testing.T) {
	for _, mode := range []string{"intent", "record", "offset", "requests", "drain", "history", "schema"} {
		t.Run(mode, func(t *testing.T) {
			f, r, _ := telegramCursorFixture(t)
			_, err := r.CommitTelegramRecoveryCursor(context.Background(), f.Account)
			must(t, err)
			switch mode {
			case "intent":
				err = os.Remove(filepath.Join(r.stage.File().Name(), telegramCursorFile(f.Account)))
			case "record":
				_, err = r.db.Exec("DELETE FROM reconciliation_telegram_cursors")
			case "offset":
				_, err = r.db.Exec("UPDATE telegram_cursors SET offset=123")
			case "requests":
				_, err = r.db.Exec("DELETE FROM reconciliation_request_seal")
			case "drain":
				_, err = r.db.Exec("DELETE FROM reconciliation_telegram_drain_pages WHERE ordinal=1")
			case "history":
				_, err = r.db.Exec("DELETE FROM reconciliation_telegram_records WHERE ordinal=1")
			case "schema":
				_, err = r.db.Exec("DELETE FROM metadata WHERE key='telegram_cursor_recovery_schema'")
			}
			must(t, err)
			if _, err = r.CommitTelegramRecoveryCursor(context.Background(), f.Account); err == nil {
				t.Fatal("damaged handoff accepted", mode)
			}
		})
	}
}
func TestTelegramCursorLostResponses(t *testing.T) {
	for _, stage := range []string{"telegram_cursor_intent", "telegram_cursor_transaction", "telegram_cursor_commit"} {
		t.Run(stage, func(t *testing.T) {
			f, r, _ := telegramCursorFixture(t)
			r.fault = func(at string) error {
				if at == stage {
					return errors.New("lost response")
				}
				return nil
			}
			if _, err := r.CommitTelegramRecoveryCursor(context.Background(), f.Account); err == nil {
				t.Fatal("fault ignored")
			}
			r.fault = nil
			a, err := r.CommitTelegramRecoveryCursor(context.Background(), f.Account)
			must(t, err)
			b, err := r.CommitTelegramRecoveryCursor(context.Background(), f.Account)
			must(t, err)
			if a != b {
				t.Fatal("lost response renewed cutoff")
			}
		})
	}
}
func TestTelegramCursorCommitRechecksFreshness(t *testing.T) {
	for _, stage := range []string{"telegram_cursor_intent", "telegram_cursor_transaction"} {
		t.Run(stage, func(t *testing.T) {
			f, r, d := telegramCursorFixture(t)
			r.fault = func(at string) error {
				if at == stage {
					setTelegramHandoffClock(r, d, telegramHandoffLifetime)
				}
				return nil
			}
			if _, err := r.CommitTelegramRecoveryCursor(context.Background(), f.Account); err == nil {
				t.Fatal("stale observation committed")
			}
			must(t, r.transaction(context.Background(), func(tx *sql.Tx) error {
				var count, offset int
				if err := tx.QueryRow("SELECT count(*) FROM reconciliation_telegram_cursors").Scan(&count); err != nil {
					return err
				}
				if err := tx.QueryRow("SELECT offset FROM telegram_cursors WHERE account=?", f.Account).Scan(&offset); err != nil {
					return err
				}
				if count != 0 || offset != 6 {
					t.Fatal("partial expired commit", count, offset)
				}
				return nil
			}))
		})
	}
}

func TestTelegramCursorFinalCommitValidation(t *testing.T) {
	f, r, d := telegramCursorFixture(t)
	prepared := false
	r.fault = func(at string) error {
		if at == "telegram_cursor_transaction" {
			prepared = true
		}
		if at == "reconciliation_transaction" && prepared {
			setTelegramHandoffClock(r, d, telegramHandoffLifetime)
		}
		return nil
	}
	if _, err := r.CommitTelegramRecoveryCursor(context.Background(), f.Account); err == nil {
		t.Fatal("clock expiry after inventory preparation committed")
	}
	var offset int
	must(t, r.db.QueryRow("SELECT offset FROM telegram_cursors WHERE account=?", f.Account).Scan(&offset))
	if offset != 6 {
		t.Fatal("expired handoff changed cursor")
	}
}
func TestTelegramCursorRejectsIncompletePrerequisites(t *testing.T) {
	for _, mode := range []string{"unsealed", "undrained", "expired", "unknown_account"} {
		t.Run(mode, func(t *testing.T) {
			f, r, d := telegramCursorFixture(t)
			account := f.Account
			var err error
			switch mode {
			case "unsealed":
				_, err = r.db.Exec("DELETE FROM reconciliation_request_seal")
			case "undrained":
				_, err = r.db.Exec("DELETE FROM reconciliation_telegram_drain_pages")
			case "expired":
				setTelegramHandoffClock(r, d, telegramHandoffLifetime)
			case "unknown_account":
				account = "unknown-bot"
			}
			must(t, err)
			if _, err = r.CommitTelegramRecoveryCursor(context.Background(), account); err == nil {
				t.Fatal("incomplete handoff accepted")
			}
			var n int
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_telegram_cursors").Scan(&n))
			if n != 0 {
				t.Fatal("incomplete handoff recorded")
			}
		})
	}
}
func TestTelegramCursorRuntimeRequiresCommittedHandoff(t *testing.T) {
	for _, mode := range []string{"uncommitted", "corrupt", "requests"} {
		t.Run(mode, func(t *testing.T) {
			f, r, _ := telegramCursorFixture(t)
			ctx := context.Background()
			if mode != "uncommitted" {
				_, err := r.CommitTelegramRecoveryCursor(ctx, f.Account)
				must(t, err)
			}
			var err error
			switch mode {
			case "corrupt":
				_, err = r.db.Exec("UPDATE reconciliation_telegram_cursors SET hash='changed'")
			case "requests":
				_, err = r.db.Exec("DELETE FROM reconciliation_request_seal")
			}
			must(t, err)
			runtime := &Store{db: r.db, dir: r.dir, identity: r.journal.After, config: r.config, now: time.Now}
			if _, err = runtime.TelegramOffset(ctx, f.Account); err == nil {
				t.Fatal("unsafe provider poll offset exposed")
			}
			if err = runtime.PersistTelegram(ctx, provider.IntakePage{Account: f.Account, Next: 6}); err == nil {
				t.Fatal("unsafe provider cursor persisted")
			}
		})
	}
}
func TestTelegramHandoffValidationDoesNotCommit(t *testing.T) {
	f, r, _ := telegramCursorFixture(t)
	if _, err := r.ValidateTelegramRecoveryHandoff(context.Background(), f.Account); err == nil {
		t.Fatal("uncommitted handoff validated")
	}
	var count int
	must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_telegram_cursors").Scan(&count))
	if count != 0 {
		t.Fatal("validation committed a handoff")
	}
}

func TestTelegramCursorCommitRechecksAuthority(t *testing.T) {
	for _, stage := range []string{"telegram_cursor_intent", "telegram_cursor_transaction"} {
		for _, mode := range []string{"lease", "identity", "context"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				f, r, _ := telegramCursorFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				r.fault = func(at string) error {
					if at != stage {
						return nil
					}
					switch mode {
					case "lease":
						lock := filepath.Join(f.Path, "worker.lock")
						must(t, os.Rename(lock, lock+".held"))
						must(t, os.WriteFile(lock, nil, 0600))
					case "identity":
						changed := r.journal.After
						changed.Recovery = "changed"
						must(t, r.dir.WriteJSON("identity.json", changed, true))
					case "context":
						cancel()
					}
					return nil
				}
				if _, err := r.CommitTelegramRecoveryCursor(ctx, f.Account); err == nil {
					t.Fatal("authority loss committed cursor")
				}
				var count, offset int
				must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_telegram_cursors").Scan(&count))
				must(t, r.db.QueryRow("SELECT offset FROM telegram_cursors WHERE account=?", f.Account).Scan(&offset))
				if count != 0 || offset != 6 {
					t.Fatal("partial handoff after authority loss", count, offset)
				}
			})
		}
	}
}

func TestTelegramCursorPendingIntentCannotRenewObservation(t *testing.T) {
	f, r, d := telegramCursorFixture(t)
	r.fault = func(at string) error {
		if at == "telegram_cursor_intent" {
			return errors.New("interrupted")
		}
		return nil
	}
	if _, err := r.CommitTelegramRecoveryCursor(context.Background(), f.Account); err == nil {
		t.Fatal("fault ignored")
	}
	file := filepath.Join(r.stage.File().Name(), telegramCursorFile(f.Account))
	before, err := os.ReadFile(file)
	must(t, err)
	r.fault = nil
	setTelegramHandoffClock(r, d, telegramHandoffLifetime)
	if _, err = r.CommitTelegramRecoveryCursor(context.Background(), f.Account); err == nil {
		t.Fatal("pending stale intent renewed")
	}
	after, err := os.ReadFile(file)
	must(t, err)
	if string(before) != string(after) {
		t.Fatal("stale intent changed")
	}
}
