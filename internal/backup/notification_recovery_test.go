//go:build linux

package backup

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func backupRecoveryFixture(t *testing.T) (*pipelineFixture, notify.RecoveryChallenge, backupEvaluationClock) {
	t.Helper()
	f := newPipelineFixture(t)
	testutil.Must(t, f.state.put("notification_owner", backupNotificationOwner{Schema: 1, Mode: "shared", Binding: f.binding, Socket: f.client.Socket}))
	ch := notify.RecoveryChallenge{Operation: testutil.ID(t, "n"), Identity: notify.Identity{Worker: f.binding.Identity.Worker, Recovery: testutil.ID(t, "n")}, Producer: f.binding.Producer,
		Nonce: testutil.ID(t, "n"), IssuedAt: f.now, ExpiresAt: f.now.Add(10 * time.Minute), BootID: "recovery-boot", Uptime: 100}
	return f, ch, backupEvaluationClock{Now: f.now, Boot: ch.BootID, Uptime: 100}
}

func TestBackupRecoveryExportPreservesEvidenceAndObligations(t *testing.T) {
	f, ch, clock := backupRecoveryFixture(t)
	ctx := context.Background()
	f.observe(t, "a", "First failure", f.now)
	messages := f.freeze(t, 1, 1)
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	f.transfer(t, f.client)
	f.accept(t, messages[0])
	f.transfer(t, f.client)
	f.now = f.now.Add(time.Second)
	f.observe(t, "b", "Unsent failure", f.now)
	messages = f.freeze(t, 2, 2)
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	var cursor int64
	testutil.Must(t, f.state.db.QueryRow("SELECT cursor FROM notification_producer").Scan(&cursor))
	journal, err := beginBackupNotificationRecoveryWith(ctx, f.state, ch, clock, nil)
	testutil.Must(t, err)
	if journal.Records != 2 || journal.Manifest.HighestAcknowledged != 1 || journal.Manifest.ImportedReceipts.Position != cursor || journal.MissingTerminalReceipts != 0 {
		t.Fatal(journal)
	}
	again, err := beginBackupNotificationRecoveryWith(ctx, f.state, ch, clock, nil)
	testutil.Must(t, err)
	if notify.Hash(journal) != notify.Hash(again) {
		t.Fatal("replay changed frozen export")
	}
	page, err := readBackupNotificationRecoveryPage(ctx, f.state, ch, 0, clock)
	testutil.Must(t, err)
	if !page.Done || page.Next != 2 || len(page.Records) != 2 {
		t.Fatal(page)
	}
	proof := page.Records[0].Receipt
	if proof == nil || proof.Identity != f.binding.Identity || proof.Receipt.State != "provider_accepted" || proof.Receipt.Position == 0 || proof.Receipt.AttemptID == "" || proof.Receipt.At.IsZero() || proof.Receipt.Acceptance == nil {
		t.Fatal("lost original terminal receipt", proof)
	}
	if page.Records[1].Receipt != nil || notify.Hash(page.Records[1].Message) != notify.Hash(messages[0]) {
		t.Fatal("fabricated receipt or changed frozen message")
	}
	var open, satisfied int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='satisfied'").Scan(&satisfied))
	if open != 1 || satisfied != 1 {
		t.Fatal("recovery changed obligations", open, satisfied)
	}
	ledgerTx(t, f.state, func(tx *sql.Tx) error {
		b, err := readBackupNotificationBinding(tx)
		if err != nil {
			return err
		}
		if b != f.binding {
			t.Fatal("active identity changed prematurely")
		}
		owner, err := readBackupNotificationOwner(tx)
		if err != nil {
			return err
		}
		if owner.Mode != "fenced" {
			t.Fatal(owner)
		}
		return nil
	})
	// Domain changes remain possible while delivery is fenced.
	f.observe(t, "c", "New failure during recovery", f.now.Add(2*time.Second))
	if err = transferBackupNotifications(ctx, f.state, f.client); err == nil {
		t.Fatal("fenced transfer succeeded")
	}
	if err = backupTransferTx(ctx, f.state, func(tx *sql.Tx) error { return importNotificationReceipts(tx, journal.Manifest.ImportedReceipts, nil) }); err == nil {
		t.Fatal("old stream advanced during recovery")
	}
	if err = backupTransferTx(ctx, f.state, func(tx *sql.Tx) error { _, err := freezeNotificationExport(tx, messages[0], nil); return err }); err == nil {
		t.Fatal("fenced export succeeded")
	}
	if err = requireLegacyNotificationOwner(f.state); err == nil {
		t.Fatal("legacy delivery reenabled")
	}
	var activeCursor int64
	testutil.Must(t, f.state.db.QueryRow("SELECT cursor FROM notification_producer").Scan(&activeCursor))
	if activeCursor != cursor {
		t.Fatal("cursor changed")
	}
}

func TestBackupRecoveryLegacyEvidenceIsNotSynthesized(t *testing.T) {
	f, ch, clock := backupRecoveryFixture(t)
	f.observe(t, "a", "Failure", f.now)
	messages := f.freeze(t, 1, 1)
	f.transfer(t, f.client)
	f.accept(t, messages[0])
	f.transfer(t, f.client)
	// Model an installation that imported outcomes before full receipt retention.
	_, err := f.state.db.Exec("DROP TABLE notification_receipt_history")
	testutil.Must(t, err)
	// A lost enqueue response must not hide the acknowledgement subsequently
	// established by importing a receipt, even without retained full provenance.
	_, err = f.state.db.Exec("UPDATE backup_notification_transfers SET submitted=0")
	testutil.Must(t, err)
	journal, err := beginBackupNotificationRecoveryWith(context.Background(), f.state, ch, clock, nil)
	testutil.Must(t, err)
	if journal.MissingTerminalReceipts != 1 || journal.Manifest.HighestAcknowledged != 1 {
		t.Fatal(journal)
	}
	page, err := readBackupNotificationRecoveryPage(context.Background(), f.state, ch, 0, clock)
	testutil.Must(t, err)
	if page.Records[0].Receipt != nil {
		t.Fatal("invented pre-upgrade provenance")
	}
}

func TestBackupRecoveryWaitsForNotifierLease(t *testing.T) {
	f, ch, clock := backupRecoveryFixture(t)
	lease, err := backupNotifierLease(f.state)
	testutil.Must(t, err)
	defer lease.Close()
	if _, err = beginBackupNotificationRecoveryWith(context.Background(), f.state, ch, clock, nil); err == nil {
		t.Fatal("capture overlapped active notifier")
	}
	ledgerTx(t, f.state, func(tx *sql.Tx) error { return requireNotificationRecoveryInactive(tx) })
}

func TestBackupRecoveryRejectsClockRollbackAfterCapture(t *testing.T) {
	f, ch, clock := backupRecoveryFixture(t)
	clock.Now = clock.Now.Add(time.Minute)
	clock.Uptime += 60
	_, err := beginBackupNotificationRecoveryWith(context.Background(), f.state, ch, clock, nil)
	testutil.Must(t, err)
	rollback := clock
	rollback.Now = rollback.Now.Add(-time.Second)
	if _, err = readBackupNotificationRecoveryPage(context.Background(), f.state, ch, 0, rollback); err == nil {
		t.Fatal("wall rollback accepted")
	}
	rollback = clock
	rollback.Uptime--
	if _, err = beginBackupNotificationRecoveryWith(context.Background(), f.state, ch, rollback, nil); err == nil {
		t.Fatal("monotonic rollback accepted")
	}
}

func TestBackupRecoveryReceiptAndCursorAreAtomic(t *testing.T) {
	s, id, p := notificationLedger(t)
	var m notify.Message
	ledgerTx(t, s, func(tx *sql.Tx) (err error) {
		m, err = freezeNotificationExport(tx, exportedReport(p, "report", "report", "daily", 1), []notificationObligation{{"daily", "daily"}})
		return
	})
	r := notify.Receipt{Position: 1, EventID: m.ID, Sequence: m.Sequence, State: "expired", At: time.Now().UTC()}
	ctx := context.Background()
	cursor := notify.Cursor{Identity: id, Producer: p, Position: 2}
	if err := backupTransferTx(ctx, s, func(tx *sql.Tx) error { return importNotificationReceipts(tx, cursor, []notify.Receipt{r}) }); err == nil {
		t.Fatal("skipped cursor accepted")
	}
	var count int
	testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM notification_receipt_history").Scan(&count))
	if count != 0 {
		t.Fatal("receipt survived rolled back cursor")
	}
	cursor.Position = 1
	ledgerTx(t, s, func(tx *sql.Tx) error { return importNotificationReceipts(tx, cursor, []notify.Receipt{r}) })
	ledgerTx(t, s, func(tx *sql.Tx) error { return importNotificationReceipts(tx, cursor, []notify.Receipt{r}) })
	r.At = r.At.Add(time.Second)
	if err := backupTransferTx(ctx, s, func(tx *sql.Tx) error { return importNotificationReceipts(tx, cursor, []notify.Receipt{r}) }); err == nil {
		t.Fatal("changed receipt replay accepted")
	}
}

func TestBackupRecoveryRejectsInvalidChallenges(t *testing.T) {
	f, ch, clock := backupRecoveryFixture(t)
	cases := map[string]func(*notify.RecoveryChallenge, *backupEvaluationClock){
		"worker":              func(c *notify.RecoveryChallenge, _ *backupEvaluationClock) { c.Identity.Worker = "different" },
		"old epoch":           func(c *notify.RecoveryChallenge, _ *backupEvaluationClock) { c.Identity = f.binding.Identity },
		"producer":            func(c *notify.RecoveryChallenge, _ *backupEvaluationClock) { c.Producer.Epoch = "different" },
		"operation":           func(c *notify.RecoveryChallenge, _ *backupEvaluationClock) { c.Operation = "bad" },
		"nonce":               func(c *notify.RecoveryChallenge, _ *backupEvaluationClock) { c.Nonce = "bad" },
		"expired":             func(c *notify.RecoveryChallenge, k *backupEvaluationClock) { k.Now = c.ExpiresAt },
		"wall backwards":      func(c *notify.RecoveryChallenge, k *backupEvaluationClock) { k.Now = c.IssuedAt.Add(-time.Second) },
		"reboot":              func(_ *notify.RecoveryChallenge, k *backupEvaluationClock) { k.Boot = "different" },
		"monotonic expired":   func(c *notify.RecoveryChallenge, k *backupEvaluationClock) { k.Uptime = c.Uptime + 600 },
		"monotonic backwards": func(c *notify.RecoveryChallenge, k *backupEvaluationClock) { k.Uptime = c.Uptime - 1 },
		"nonfinite":           func(_ *notify.RecoveryChallenge, k *backupEvaluationClock) { k.Uptime = math.Inf(1) },
		"overlong":            func(c *notify.RecoveryChallenge, _ *backupEvaluationClock) { c.ExpiresAt = c.IssuedAt.Add(time.Hour) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			bad, k := ch, clock
			change(&bad, &k)
			if _, err := beginBackupNotificationRecoveryWith(context.Background(), f.state, bad, k, nil); err == nil {
				t.Fatal("invalid challenge accepted")
			}
		})
	}
	ledgerTx(t, f.state, func(tx *sql.Tx) error {
		owner, err := readBackupNotificationOwner(tx)
		if err == nil && owner.Mode != "shared" {
			t.Fatal("invalid challenge fenced owner")
		}
		return err
	})
}

func TestBackupRecoveryRenewalAndCorruption(t *testing.T) {
	f, ch, clock := backupRecoveryFixture(t)
	f.observe(t, "a", "Failure", f.now)
	f.freeze(t, 1, 1)
	ctx := context.Background()
	first, err := beginBackupNotificationRecoveryWith(ctx, f.state, ch, clock, nil)
	testutil.Must(t, err)
	renewed := ch
	renewed.Nonce = testutil.ID(t, "n")
	renewed.IssuedAt = ch.ExpiresAt
	renewed.ExpiresAt = renewed.IssuedAt.Add(10 * time.Minute)
	renewed.Uptime += 600
	later := backupEvaluationClock{Now: renewed.IssuedAt, Boot: renewed.BootID, Uptime: renewed.Uptime}
	next, err := beginBackupNotificationRecoveryWith(ctx, f.state, renewed, later, nil)
	testutil.Must(t, err)
	if notify.Hash(first.Manifest) != notify.Hash(next.Manifest) || next.CapturedAt != first.CapturedAt {
		t.Fatal("renewal recaptured export")
	}
	if _, err = readBackupNotificationRecoveryPage(ctx, f.state, ch, 0, clock); err == nil {
		t.Fatal("superseded nonce accepted")
	}
	if _, err = beginBackupNotificationRecoveryWith(ctx, f.state, ch, clock, nil); err == nil {
		t.Fatal("challenge regressed")
	}
	changed := renewed
	changed.Operation = testutil.ID(t, "n")
	if _, err = beginBackupNotificationRecoveryWith(ctx, f.state, changed, later, nil); err == nil {
		t.Fatal("parallel recovery accepted")
	}
	page, err := readBackupNotificationRecoveryPage(ctx, f.state, renewed, 0, later)
	testutil.Must(t, err)
	if len(page.Records) != 1 {
		t.Fatal(page)
	}
	_, err = f.state.db.Exec("UPDATE backup_notification_recovery_records SET hash='damaged'")
	testutil.Must(t, err)
	if _, err = readBackupNotificationRecoveryPage(ctx, f.state, renewed, 0, later); err == nil {
		t.Fatal("damaged page accepted")
	}
	_, err = f.state.db.Exec("DELETE FROM backup_notification_recovery_records")
	testutil.Must(t, err)
	if _, err = beginBackupNotificationRecoveryWith(ctx, f.state, renewed, later, nil); err == nil {
		t.Fatal("missing export silently rebuilt")
	}
}

func TestBackupRecoveryBoundedPagesAndMissingHistory(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			f, ch, clock := backupRecoveryFixture(t)
			ledgerTx(t, f.state, func(tx *sql.Tx) error {
				for i := 0; i < 140; i++ {
					m := notify.Message{Producer: f.binding.Producer, ID: fmt.Sprintf("test-%d", i), Generation: f.binding.Generation, Route: f.binding.Route, Subject: "backup", Kind: "test", Severity: "info", Text: strings.Repeat("界", 1000), ObservedAt: f.now}
					m, err := freezeNotificationExport(tx, m, []notificationObligation{{m.ID, "alert"}})
					if err != nil {
						return err
					}
					if _, err = tx.Exec("INSERT INTO backup_notification_transfers(event) VALUES(?)", m.ID); err != nil {
						return err
					}
				}
				return nil
			})
			if missing {
				_, err := f.state.db.Exec("DELETE FROM notification_exports WHERE seq=13")
				testutil.Must(t, err)
			}
			ctx := context.Background()
			_, err := beginBackupNotificationRecoveryWith(ctx, f.state, ch, clock, nil)
			if missing {
				if err == nil {
					t.Fatal("lost history converted to compacted floor")
				}
				ledgerTx(t, f.state, func(tx *sql.Tx) error { return requireNotificationRecoveryInactive(tx) })
				return
			}
			testutil.Must(t, err)
			after := uint64(0)
			pages := 0
			for {
				page, err := readBackupNotificationRecoveryPage(ctx, f.state, ch, after, clock)
				testutil.Must(t, err)
				raw, err := json.Marshal(page.Records)
				testutil.Must(t, err)
				if len(raw)+4096 > notify.MaxRequest || len(page.Records) > 128 || page.Next <= after {
					t.Fatal("unbounded or stalled page")
				}
				after = page.Next
				pages++
				if page.Done {
					break
				}
			}
			if after != 140 || pages < 4 {
				t.Fatal(after, pages)
			}
		})
	}
}

func TestBackupRecoveryCaptureCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_BACKUP_RECOVERY_STATE")
	if path == "" {
		t.Skip("subprocess helper")
	}
	s, err := openBackupState(path, false)
	testutil.Must(t, err)
	defer s.Close()
	var input struct {
		Challenge notify.RecoveryChallenge
		Clock     backupEvaluationClock
	}
	raw, err := os.ReadFile(os.Getenv("DEPLOY_BACKUP_RECOVERY_INPUT"))
	testutil.Must(t, err)
	testutil.Must(t, json.Unmarshal(raw, &input))
	_, err = beginBackupNotificationRecoveryWith(context.Background(), s, input.Challenge, input.Clock, func(stage string) error {
		if stage == os.Getenv("DEPLOY_BACKUP_RECOVERY_BOUNDARY") {
			fmt.Println("READY")
			select {}
		}
		return nil
	})
	testutil.Must(t, err)
	t.Fatal("crash boundary was not reached")
}

func TestBackupRecoveryCaptureSIGKILL(t *testing.T) {
	for _, boundary := range []string{"backup_recovery_transaction", "backup_recovery_commit"} {
		t.Run(boundary, func(t *testing.T) {
			f, ch, clock := backupRecoveryFixture(t)
			f.observe(t, "a", "Failure", f.now)
			f.freeze(t, 1, 1)
			path := f.state.directory
			testutil.Must(t, f.state.Close())
			input := filepath.Join(t.TempDir(), "input.json")
			raw, err := json.Marshal(struct {
				Challenge notify.RecoveryChallenge
				Clock     backupEvaluationClock
			}{ch, clock})
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(input, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackupRecoveryCaptureCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_RECOVERY_STATE="+path, "DEPLOY_BACKUP_RECOVERY_INPUT="+input, "DEPLOY_BACKUP_RECOVERY_BOUNDARY="+boundary)
			pipe, err := cmd.StdoutPipe()
			testutil.Must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			testutil.Must(t, cmd.Start())
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			})
			scanner := bufio.NewScanner(pipe)
			if !scanner.Scan() || scanner.Text() != "READY" {
				t.Fatal("child did not reach boundary", scanner.Text(), stderr.String())
			}
			testutil.Must(t, cmd.Process.Kill())
			if err = cmd.Wait(); err == nil {
				t.Fatal("expected SIGKILL")
			}
			s, err := openBackupState(path, false)
			testutil.Must(t, err)
			defer s.Close()
			var saved backupNotificationRecovery
			err = s.get("notification_recovery", &saved)
			if boundary == "backup_recovery_transaction" {
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("uncommitted journal survived", err)
				}
			} else {
				testutil.Must(t, err)
			}
			j, err := beginBackupNotificationRecoveryWith(context.Background(), s, ch, clock, nil)
			testutil.Must(t, err)
			page, err := readBackupNotificationRecoveryPage(context.Background(), s, ch, 0, clock)
			testutil.Must(t, err)
			if !page.Done || page.Next != 1 || j.Manifest.HighestAcknowledged != 0 {
				t.Fatal(j, page)
			}
		})
	}
}
