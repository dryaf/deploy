//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
	"github.com/dryaf/deploy/internal/ui"
)

func legacyAttemptFixture(t *testing.T) (*backupState, WorkerInstallation, time.Time) {
	t.Helper()
	_, s, _, _, _ := archiveFixture(t)
	c := WorkerInstallation{Policy: configfixture.WorkerPolicy(t)}
	c.Policy.Monitoring = config.WorkerMonitoring{}
	c.Policy.Notifications.Fallback = ""
	now := time.Now().UTC()
	ledgerTx(t, s, func(tx *sql.Tx) error {
		return enqueueNotification(tx, notification.NotificationEvent{ID: "legacy-one", Kind: "test", Message: "original envelope"}, now)
	})
	return s, c, now
}

func legacyAttemptRead(t *testing.T, s *backupState) legacyDeliveryJournal {
	t.Helper()
	var j legacyDeliveryJournal
	testutil.Must(t, backupTransferTx(context.Background(), s, func(tx *sql.Tx) error {
		var err error
		j, err = readLegacyDeliveryJournal(tx)
		return err
	}))
	return j
}

func legacyAccept(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
	return notification.DeliveryAttempt{Accepted: true}, nil
}

func TestLegacyDeliveryReservationRecovery(t *testing.T) {
	for _, boundary := range []string{"legacy-attempt-reserved", "legacy-provider-returned", "legacy-attempt-settled"} {
		t.Run(boundary, func(t *testing.T) {
			s, c, now := legacyAttemptFixture(t)
			calls := 0
			send := func(ctx context.Context, provider string, policy config.WorkerNotifications, event notification.NotificationEvent) (notification.DeliveryAttempt, error) {
				calls++
				j := legacyAttemptRead(t, s)
				if j.pending == nil || j.pending.Event.Message != event.Message || j.review.Unsettled != 1 {
					t.Fatal("provider called without durable exact reservation")
				}
				return legacyAccept(ctx, provider, policy, event)
			}
			stop := errors.New("interruption")
			err := drainNotificationOutboxWith(context.Background(), s, c, send, now, func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			before := calls
			testutil.Must(t, drainNotificationOutbox(context.Background(), s, c, send, now.Add(time.Second)))
			if calls != before {
				t.Fatal("restart blindly repeated an accepted or unsettled attempt")
			}
			j := legacyAttemptRead(t, s)
			if j.pending != nil || j.review.Attempts != 1 || j.review.CutoverAllowed || j.review.HistoricalEvidenceComplete {
				t.Fatal(j.review)
			}
			var accepted bool
			testutil.Must(t, s.db.QueryRow("SELECT accepted IS NOT NULL FROM outbox WHERE id='legacy-one'").Scan(&accepted))
			if boundary == "legacy-attempt-settled" {
				if !accepted || j.review.Accepted != 1 || j.review.Ambiguous != 0 {
					t.Fatal("atomic acceptance lost", j.review)
				}
			} else {
				if accepted || j.review.Ambiguous != 1 || j.review.Accepted != 0 {
					t.Fatal("unknown outcome manufactured acceptance", j.review)
				}
				// Existing legacy retry policy still applies, but a new reservation
				// retains the older uncertainty rather than rewriting it as unsent.
				testutil.Must(t, drainNotificationOutbox(context.Background(), s, c, send, now.Add(2*time.Minute)))
				j = legacyAttemptRead(t, s)
				if j.review.Attempts != 2 || j.review.Ambiguous != 1 || j.review.Accepted != 1 {
					t.Fatal(j.review)
				}
			}
		})
	}
}

func TestLegacyDeliveryAtomicAcceptanceAndImmutableEnvelope(t *testing.T) {
	s, c, now := legacyAttemptFixture(t)
	c.Policy.Notifications.Fallback = "telegram"
	calls := 0
	send := func(_ context.Context, provider string, _ config.WorkerNotifications, event notification.NotificationEvent) (notification.DeliveryAttempt, error) {
		calls++
		// Simulate a domain writer refreshing the current report during I/O.
		_, err := s.db.Exec("UPDATE outbox SET record=json_set(record,'$.message','new current text') WHERE id=?", event.ID)
		testutil.Must(t, err)
		return notification.DeliveryAttempt{Provider: provider, Accepted: provider == "telegram", Ambiguous: provider != "telegram"}, nil
	}
	testutil.Must(t, drainNotificationOutbox(context.Background(), s, c, send, now))
	j := legacyAttemptRead(t, s)
	if calls != 2 || j.review.Accepted != 1 || j.review.Ambiguous != 1 {
		t.Fatal(calls, j.review)
	}
	var raw, text string
	testutil.Must(t, s.db.QueryRow("SELECT record,json_extract(record,'$.message') FROM outbox WHERE id='legacy-one'").Scan(&raw, &text))
	var event notification.NotificationEvent
	testutil.Must(t, json.Unmarshal([]byte(raw), &event))
	if text != "new current text" || len(event.Attempts) != 2 || !event.Attempts[0].Ambiguous || !event.Attempts[1].Accepted {
		t.Fatal("settlement overwrote current domain data or previous fallback evidence", event)
	}
	var original string
	testutil.Must(t, s.db.QueryRow("SELECT json_extract(body,'$.intent.event.message') FROM backup_legacy_delivery_journal WHERE sequence=1").Scan(&original))
	if original != "original envelope" {
		t.Fatal("sent payload was not retained")
	}
}

func TestLegacyDeliveryJournalLossAndLeaseReplacement(t *testing.T) {
	for _, mode := range []string{"head", "table", "row", "body", "lease", "replacement", "context", "fence"} {
		t.Run(mode, func(t *testing.T) {
			s, c, now := legacyAttemptFixture(t)
			called := false
			send := func(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
				called = true
				return notification.DeliveryAttempt{Accepted: true}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := drainNotificationOutboxWith(ctx, s, c, send, now, func(at string) error {
				if at != "legacy-attempt-reserved" {
					return nil
				}
				if mode == "replacement" {
					path := filepath.Join(s.directory, "legacy-delivery.lock")
					testutil.Must(t, os.Rename(path, path+".old"))
					testutil.Must(t, os.WriteFile(path, nil, 0600))
					return nil
				}
				if mode == "context" {
					cancel()
					return nil
				}
				if mode == "fence" {
					return s.put("notification_owner", "fenced")
				}
				return errors.New("stop before send")
			})
			if err == nil || called {
				t.Fatal("changed authority reached provider", err)
			}
			var mutation string
			switch mode {
			case "head":
				mutation = "DELETE FROM meta WHERE key='legacy_delivery_journal'"
			case "table":
				mutation = "DROP TABLE backup_legacy_delivery_journal"
			case "row":
				mutation = "DELETE FROM backup_legacy_delivery_journal"
			case "body":
				mutation = "UPDATE backup_legacy_delivery_journal SET body='{}'"
			case "lease":
				testutil.Must(t, os.Remove(filepath.Join(s.directory, "legacy-delivery.lock")))
			case "replacement":
				// Even a later process must reject the substituted lock inode.
			default:
				return
			}
			if mutation != "" {
				_, err := s.db.Exec(mutation)
				testutil.Must(t, err)
			}
			if err := drainNotificationOutbox(context.Background(), s, c, send, now.Add(time.Hour)); err == nil || called {
				t.Fatal("lost journal/lease silently reinitialized", err)
			}
		})
	}
}

func TestLegacyDeliveryAttemptCannotBecomeUnsentBatchOrExport(t *testing.T) {
	s, c, now := legacyAttemptFixture(t)
	stop := errors.New("lost response")
	if err := drainNotificationOutboxWith(context.Background(), s, c, legacyAccept, now, func(at string) error {
		if at == "legacy-provider-returned" {
			return stop
		}
		return nil
	}); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	// A reservation itself blocks shared export, even before recovery has copied
	// its uncertain result into the outbox's bounded attempt summary.
	ledgerTx(t, s, func(tx *sql.Tx) error {
		event := notification.NotificationEvent{ID: "legacy-one"}
		if requireBackupUnsentSource(tx, event) == nil {
			t.Fatal("unsettled legacy event authorized as a fresh shared export")
		}
		return nil
	})
	testutil.Must(t, drainNotificationOutbox(context.Background(), s, c, legacyAccept, now.Add(time.Second)))
	ledgerTx(t, s, func(tx *sql.Tx) error {
		for n := 0; n < 8; n++ {
			if err := enqueueNotification(tx, notification.NotificationEvent{ID: fmt.Sprintf("new-%d", n), Kind: "test", Message: "new"}, now); err != nil {
				return err
			}
		}
		return nil
	})
	testutil.Must(t, coalesceNotificationBacklog(s, now.Add(time.Hour)))
	var covered bool
	testutil.Must(t, s.db.QueryRow("SELECT covered_by IS NOT NULL FROM outbox WHERE id='legacy-one'").Scan(&covered))
	if covered {
		t.Fatal("ambiguous event became an apparently unsent batch member")
	}
}

func TestLegacyDeliveryReservationMustSurviveProviderBoundary(t *testing.T) {
	for _, boundary := range []string{"legacy-attempt-reserved", "legacy-provider-returned"} {
		t.Run(boundary, func(t *testing.T) {
			s, c, now := legacyAttemptFixture(t)
			calls := 0
			err := drainNotificationOutboxWith(context.Background(), s, c, func(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
				calls++
				return notification.DeliveryAttempt{Accepted: true}, nil
			}, now, func(at string) error {
				if at == boundary {
					_, err := s.db.Exec("DELETE FROM backup_legacy_delivery_journal")
					return err
				}
				return nil
			})
			if err == nil || boundary == "legacy-attempt-reserved" && calls != 0 || boundary == "legacy-provider-returned" && calls != 1 {
				t.Fatal("lost reservation crossed provider boundary", err, calls)
			}
			var accepted bool
			testutil.Must(t, s.db.QueryRow("SELECT accepted IS NOT NULL FROM outbox WHERE id='legacy-one'").Scan(&accepted))
			if accepted {
				t.Fatal("acceptance committed without its retained reservation")
			}
		})
	}
}

func TestLegacyDeliveryReviewDoesNotRepairOrAuthorize(t *testing.T) {
	s, c, now := legacyAttemptFixture(t)
	var out bytes.Buffer
	args := []string{"--local", "--state-dir", s.directory}
	testutil.Must(t, runBackupDeliveryReviewCLI(args, &out))
	var review legacyDeliveryReview
	testutil.Must(t, json.Unmarshal(out.Bytes(), &review))
	if review.JournalPresent || review.CutoverAllowed || review.HistoricalEvidenceComplete {
		t.Fatal(review)
	}
	stop := errors.New("interrupted")
	if err := drainNotificationOutboxWith(context.Background(), s, c, legacyAccept, now, func(at string) error {
		if at == "legacy-attempt-reserved" {
			return stop
		}
		return nil
	}); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	head := legacyAttemptRead(t, s).head
	out.Reset()
	testutil.Must(t, runBackupDeliveryReviewCLI(args, &out))
	testutil.Must(t, json.Unmarshal(out.Bytes(), &review))
	if review.Head != head || review.Unsettled != 1 || review.Ambiguous != 0 || legacyAttemptRead(t, s).head != head || strings.Contains(out.String(), "original envelope") {
		t.Fatal("review changed evidence or exposed message payload", out.String())
	}
	for _, extra := range [][]string{{"--apply"}, {"--local=false"}, {"--state-dir", "relative"}, {"worker"}} {
		if runBackupDeliveryReviewCLI(append(append([]string{}, args...), extra...), &out) == nil {
			t.Fatal("invalid review flags", extra)
		}
	}
	prior := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = prior }()
	absent := filepath.Join(t.TempDir(), "absent")
	testutil.Must(t, runBackupDeliveryReviewCLI([]string{"--local", "--state-dir", absent}, &out))
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatal("dry run opened state", err)
	}
}

func TestLegacyDeliveryCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_LEGACY_DELIVERY_CRASH_STATE")
	if path == "" {
		t.Skip("subprocess only")
	}
	s, err := openBackupState(path, false)
	testutil.Must(t, err)
	defer s.Close()
	c := WorkerInstallation{Policy: configfixture.WorkerPolicy(t)}
	c.Policy.Monitoring = config.WorkerMonitoring{}
	c.Policy.Notifications.Fallback = ""
	testutil.Must(t, drainNotificationOutboxWith(context.Background(), s, c, legacyAccept, time.Now().UTC(), func(at string) error {
		if at == os.Getenv("DEPLOY_LEGACY_DELIVERY_CRASH_BOUNDARY") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}))
	t.Fatal("crash boundary not reached")
}

func TestLegacyDeliverySIGKILL(t *testing.T) {
	for _, boundary := range []string{"legacy-attempt-reserved", "legacy-provider-returned", "legacy-attempt-settled"} {
		t.Run(boundary, func(t *testing.T) {
			s, c, now := legacyAttemptFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLegacyDeliveryCrashHelper$", "-test.timeout=25s")
			cmd.Env = append(os.Environ(), "DEPLOY_LEGACY_DELIVERY_CRASH_STATE="+s.directory, "DEPLOY_LEGACY_DELIVERY_CRASH_BOUNDARY="+boundary)
			out, err := cmd.CombinedOutput()
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || ctx.Err() != nil {
				t.Fatal("wrong crash", err, string(out))
			}
			calls := 0
			testutil.Must(t, drainNotificationOutbox(context.Background(), s, c, func(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
				calls++
				return notification.DeliveryAttempt{Accepted: true}, nil
			}, now.Add(time.Second)))
			j := legacyAttemptRead(t, s)
			if calls != 0 || j.pending != nil || j.review.Accepted+j.review.Ambiguous != 1 {
				t.Fatal(calls, j.review)
			}
		})
	}
}

func TestLegacyDeliveryJournalReservesCompletionCapacity(t *testing.T) {
	s, c, now := legacyAttemptFixture(t)
	j := legacyDeliveryJournal{head: legacyDeliveryHead{Sequence: maxLegacyDeliveryRecords - 1, Hash: notify.Hash("full")}}
	if _, _, err := j.reserve(context.Background(), s, c, "signal", notification.NotificationEvent{ID: "legacy-one"}); err == nil {
		t.Fatal("attempt reserved without room for its outcome")
	}
	// A live lease blocks competing direct dispatch, independent of notifier.lock.
	d, err := safefs.Open(s.directory)
	testutil.Must(t, err)
	defer d.Close()
	l, err := d.Lock("legacy-delivery.lock", true, true)
	testutil.Must(t, err)
	defer l.Close()
	if err := drainNotificationOutbox(context.Background(), s, c, legacyAccept, now); err == nil {
		t.Fatal("concurrent dispatch acquired a second reservation")
	}
}
