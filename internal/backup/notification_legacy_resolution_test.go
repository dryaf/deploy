//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/ui"
)

func legacyResolutionFixture(t *testing.T, attempted bool) (*backupState, WorkerInstallation, backupLegacyResolution, *cutoverFixtureSession) {
	t.Helper()
	home, c, cutover, deps, session := backupCutoverFixture(t)
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	if attempted {
		testutil.Must(t, drainNotificationOutbox(context.Background(), state, c,
			func(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
				return notification.DeliveryAttempt{Ambiguous: true}, errors.New("lost response")
			}, time.Now().UTC().Add(time.Minute)))
	}
	ledgerTx(t, state, func(tx *sql.Tx) error {
		e, err := readBackupCutoverEvidence(tx)
		if err != nil {
			return err
		}
		cutover.EvidenceHash = notify.Hash(e)
		for _, r := range e.Rows {
			cutover.Decisions[r.ID] = backupCutoverDecision{notify.Hash(r), "held", "incomplete old history"}
		}
		return nil
	})
	testutil.Must(t, state.Close())
	testutil.Must(t, runBackupCutover(context.Background(), home, c, cutover, false, deps))
	state, err = openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	t.Cleanup(func() { state.Close() })
	var review backupLegacyReview
	ledgerTx(t, state, func(tx *sql.Tx) error {
		var err error
		review, err = readBackupLegacyReview(tx, c, "legacy-one", time.Now().UTC())
		return err
	})
	return state, c, backupLegacyResolution{Version: 1, Review: review, ReviewHash: notify.Hash(review), Action: "retry", Reason: "operator reviewed the retained uncertain attempt", AckDuplicateRisk: true}, session
}

func TestBackupLegacyResolutionPreservesEvidenceAndRequiresAcceptance(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(map[bool]string{false: "incomplete", true: "ambiguous"}[attempted], func(t *testing.T) {
			s, c, spec, _ := legacyResolutionFixture(t, attempted)
			journal := legacyAttemptRead(t, s)
			var body, original string
			testutil.Must(t, s.db.QueryRow("SELECT record FROM outbox WHERE id='legacy-one'").Scan(&body))
			testutil.Must(t, s.db.QueryRow("SELECT body FROM backup_notification_legacy WHERE source='legacy-one'").Scan(&original))
			testutil.Must(t, resolveBackupLegacy(context.Background(), s, c, spec, spec.Review.ReviewedAt, nil))
			var event notification.NotificationEvent
			testutil.Must(t, notify.Decode([]byte(body), &event, notify.MaxRequest))
			ledgerTx(t, s, func(tx *sql.Tx) error {
				held, err := backupSourceHeld(tx, event)
				if err != nil || held {
					t.Fatal("reviewed source remains held", held, err)
				}
				if err = requireBackupUnsentSource(tx, event); err != nil {
					return err
				}
				var raw, retained, disposition string
				var accepted sql.NullString
				if err = tx.QueryRow("SELECT record,accepted FROM outbox WHERE id='legacy-one'").Scan(&raw, &accepted); err != nil {
					return err
				}
				if err = tx.QueryRow("SELECT body,disposition FROM backup_notification_legacy WHERE source='legacy-one'").Scan(&retained, &disposition); err != nil {
					return err
				}
				if raw != body || retained != original || accepted.Valid || disposition != "held" {
					t.Fatal("rewrote history or manufactured acceptance")
				}
				var receipts int
				if err = tx.QueryRow("SELECT count(*) FROM notification_receipt_history").Scan(&receipts); err != nil {
					return err
				}
				if receipts != 0 {
					t.Fatal("manufactured canonical receipt")
				}
				return nil
			})
			if legacyAttemptRead(t, s).review != journal.review {
				t.Fatal("changed attempt history")
			}
			// Retry after lost CLI response is idempotent even after source acceptance.
			_, err := s.db.Exec("UPDATE outbox SET accepted=? WHERE id='legacy-one'", time.Now().UTC().Format(eventTimeLayout))
			testutil.Must(t, err)
			testutil.Must(t, resolveBackupLegacy(context.Background(), s, c, spec, spec.Review.ReviewedAt.Add(time.Hour), nil))
		})
	}
}

func TestBackupLegacyResolutionRejectsUnsafeDecisions(t *testing.T) {
	for _, name := range []string{"no-ack", "assert-unsent", "expired", "future", "changed-review", "changed-row", "changed-owner", "accepted-attempt"} {
		t.Run(name, func(t *testing.T) {
			s, c, spec, _ := legacyResolutionFixture(t, true)
			now := spec.Review.ReviewedAt
			switch name {
			case "no-ack":
				spec.AckDuplicateRisk = false
			case "assert-unsent":
				spec.Action = "unsent"
			case "expired":
				now = now.Add(6 * time.Minute)
			case "future":
				now = now.Add(-time.Second)
			case "changed-review":
				spec.Review.ConfigHash = notify.Hash("changed")
			case "changed-row":
				_, err := s.db.Exec("UPDATE outbox SET attempts=attempts+1 WHERE id='legacy-one'")
				testutil.Must(t, err)
			case "changed-owner":
				owner := spec.Review.Owner
				owner.Mode = "fenced"
				testutil.Must(t, s.put("notification_owner", owner))
			case "accepted-attempt":
				_, err := s.db.Exec("UPDATE outbox SET record=json_set(record,'$.attempts[0].accepted',json('true')) WHERE id='legacy-one'")
				testutil.Must(t, err)
				ledgerTx(t, s, func(tx *sql.Tx) error {
					var err error
					spec.Review, err = readBackupLegacyReview(tx, c, "legacy-one", now)
					return err
				})
				spec.ReviewHash = notify.Hash(spec.Review)
			}
			if err := resolveBackupLegacy(context.Background(), s, c, spec, now, nil); err == nil {
				t.Fatal("unsafe resolution accepted")
			}
		})
	}
}

func TestBackupLegacyResolutionResumeAndAnchorLoss(t *testing.T) {
	for _, point := range []string{"legacy-resolution-intent", "legacy-resolution-recorded"} {
		t.Run(point, func(t *testing.T) {
			s, c, spec, _ := legacyResolutionFixture(t, false)
			spec.Action = "unsent"
			stop := errors.New("interruption")
			err := resolveBackupLegacy(context.Background(), s, c, spec, spec.Review.ReviewedAt, func(at string) error {
				if at == point {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			altered := spec
			altered.Reason = "different decision"
			if err = resolveBackupLegacy(context.Background(), s, c, altered, spec.Review.ReviewedAt, nil); err == nil {
				t.Fatal("replaced retained decision")
			}
			testutil.Must(t, resolveBackupLegacy(context.Background(), s, c, spec, spec.Review.ReviewedAt.Add(time.Hour), nil))
			testutil.Must(t, os.Remove(filepath.Join(s.directory, legacyResolutionFile(notify.Hash(spec.Review.Original)))))
			if err = requireBackupCutoverOwner(s, spec.Review.Owner); err == nil {
				t.Fatal("missing independent resolution file accepted")
			}
			if err = resolveBackupLegacy(context.Background(), s, c, spec, spec.Review.ReviewedAt, nil); err == nil {
				t.Fatal("silently repaired lost resolution file")
			}
		})
	}
}

func TestBackupLegacyResolutionExportsThroughCanonicalReceipt(t *testing.T) {
	s, c, spec, session := legacyResolutionFixture(t, true)
	testutil.Must(t, resolveBackupLegacy(context.Background(), s, c, spec, spec.Review.ReviewedAt, nil))
	// Make the original legacy retry due without changing any attempt evidence.
	_, err := s.db.Exec("UPDATE outbox SET due=? WHERE id='legacy-one'", spec.Review.ReviewedAt.Add(-time.Second).Format(eventTimeLayout))
	testutil.Must(t, err)
	f := &pipelineFixture{state: s, binding: spec.Review.Owner.Binding, now: time.Now().UTC()}
	messages := f.freeze(t, 1, 1)
	if len(messages) != 1 {
		t.Fatal("released source was not exported", messages)
	}
	m := messages[0]
	_, err = session.store.Enqueue(context.Background(), m)
	testutil.Must(t, err)
	job, granted, err := session.store.Grant(context.Background(), m.Producer, m.ID)
	testutil.Must(t, err)
	if !granted {
		t.Fatal("released source not granted", job)
	}
	testutil.Must(t, session.store.Finish(context.Background(), job, worker.Result{Accepted: true}))
	page, err := session.Receipts(context.Background(), notify.Cursor{Identity: spec.Review.Owner.Binding.Identity, Producer: m.Producer})
	testutil.Must(t, err)
	ledgerTx(t, s, func(tx *sql.Tx) error {
		return importBackupNotificationPage(tx, backupReceiptPage{Receipts: page.Receipts, Cursor: page.Cursor})
	})
	var accepted bool
	testutil.Must(t, s.db.QueryRow("SELECT accepted IS NOT NULL FROM outbox WHERE id='legacy-one'").Scan(&accepted))
	if !accepted {
		t.Fatal("canonical acceptance failed to settle released source")
	}
	if again := f.freeze(t, 2, 1); len(again) != 0 {
		t.Fatal("replayed accepted source", again)
	}
}

func TestBackupLegacyResolutionCrashChild(t *testing.T) {
	directory := os.Getenv("DEPLOY_RESOLUTION_CRASH_DIRECTORY")
	if directory == "" {
		t.Skip("subprocess fixture")
	}
	s, err := openBackupState(directory, false)
	testutil.Must(t, err)
	defer s.Close()
	var c WorkerInstallation
	var spec backupLegacyResolution
	testutil.Must(t, s.get("fixture_resolution_config", &c))
	testutil.Must(t, s.get("fixture_resolution_spec", &spec))
	err = resolveBackupLegacy(context.Background(), s, c, spec, spec.Review.ReviewedAt, func(at string) error {
		if at == os.Getenv("DEPLOY_RESOLUTION_CRASH_BOUNDARY") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("crash boundary not reached", err)
}

func TestBackupLegacyResolutionSIGKILL(t *testing.T) {
	for _, point := range []string{"legacy-resolution-intent", "legacy-resolution-recorded"} {
		t.Run(point, func(t *testing.T) {
			s, c, spec, _ := legacyResolutionFixture(t, true)
			testutil.Must(t, s.put("fixture_resolution_config", c))
			testutil.Must(t, s.put("fixture_resolution_spec", spec))
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackupLegacyResolutionCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_RESOLUTION_CRASH_DIRECTORY="+s.directory, "DEPLOY_RESOLUTION_CRASH_BOUNDARY="+point)
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("child did not die at boundary", err)
			}
			testutil.Must(t, resolveBackupLegacy(context.Background(), s, c, spec, spec.Review.ReviewedAt.Add(time.Hour), nil))
		})
	}
}

func TestBackupLegacyResolutionDryRun(t *testing.T) {
	previous := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = previous }()
	for _, args := range [][]string{{"--local", "--review", "--source", "legacy-one"}, {"--local", "--apply", "--spec", "/absent/spec.json"}} {
		testutil.Must(t, runBackupLegacyResolutionCLI(args, &bytes.Buffer{}))
	}
	for _, args := range [][]string{{"--review", "--source", "legacy-one"}, {"--local", "--review"}, {"--local", "--apply", "--spec", "relative"}, {"--local", "--apply", "--spec", "/spec", "--source", "legacy-one"}} {
		if err := runBackupLegacyResolutionCLI(args, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid mode accepted", args)
		}
	}
}
