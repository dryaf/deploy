//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

type backupHandoffFixture struct {
	*backupEvaluationFixture
	options        backupReconciliationOptions
	first, pending interaction.Command
	grant          interaction.Grant
}

func backupHandoffTestFixture(t *testing.T, interactive bool, configure ...func(*worker.Config)) backupHandoffFixture {
	t.Helper()
	ctx := context.Background()
	if !interactive {
		configure = append([]func(*worker.Config){func(c *worker.Config) { c.Interaction = nil }}, configure...)
	}
	f := newBackupEvaluationFixture(t, configure...)
	f.clock = reportHostClock()
	f.refreshProbe(t)
	testutil.Must(t, f.state.put("notification_owner", backupNotificationOwner{Schema: 1, Mode: "shared", Binding: f.binding, Socket: f.client.Socket}))
	f.observe(t, f.subject, "retained backup fault", f.clock.Now)
	_, exports := f.evaluate(t)
	f.transfer(t, f.client)
	fixture := backupHandoffFixture{backupEvaluationFixture: f}
	var ledger *interaction.Ledger
	var in interaction.Inbound
	if interactive {
		d, err := safefs.Open(f.state.directory)
		testutil.Must(t, err)
		lock, err := d.Lock("commands.lock", true, true)
		testutil.Must(t, err)
		lock.Close()
		d.Close()
		ledger, err = interaction.OpenDomainLedger(ctx, f.state.db, f.binding.Producer, f.binding.Identity, true, func(tx *sql.Tx) error {
			_, e := tx.Exec("CREATE TABLE backup_command_binding(singleton INTEGER PRIMARY KEY CHECK(singleton=1),archive_id TEXT NOT NULL,epoch INTEGER NOT NULL); CREATE TABLE handoff_effects(value INTEGER); INSERT INTO handoff_effects VALUES(0)")
			if e != nil {
				return e
			}
			_, e = tx.Exec("INSERT INTO backup_command_binding VALUES(1,?,?)", f.binding.Archive, f.binding.Epoch)
			return e
		})
		testutil.Must(t, err)
		dest := f.gatewayConfig.Interaction.Destinations[0]
		in = interaction.Inbound{Provider: dest.Provider, Account: dest.Account, NativeID: "handoff-executed", Actor: "actor", Conversation: dest.ID, DestinationFingerprint: dest.Fingerprint(), SentAt: time.Now().UTC(), Text: "backup/storage status"}
		admission, err := f.gateway.AdmitCommand(ctx, in)
		testutil.Must(t, err)
		fixture.first = admission.Command
		testutil.Must(t, ledger.Import(ctx, fixture.first))
		testutil.Must(t, f.gateway.ImportCommand(ctx, f.binding.Producer, fixture.first.ID, notify.Hash(fixture.first)))
		fixture.grant, err = f.gateway.GrantCommand(ctx, f.binding.Producer, fixture.first.ID)
		testutil.Must(t, err)
	}
	artifact := filepath.Join(t.TempDir(), "snapshot")
	manifest, err := f.gateway.Snapshot(ctx, artifact)
	testutil.Must(t, err)
	if len(exports) != 1 {
		t.Fatal(exports)
	}
	f.accept(t, exports[0])
	f.transfer(t, f.client)
	if interactive {
		result, err := ledger.Execute(ctx, fixture.first, fixture.grant, "", time.Now().UTC(), func(tx *sql.Tx, c interaction.Command) (interaction.Result, error) {
			_, e := tx.Exec("UPDATE handoff_effects SET value=value+1")
			return interaction.Result{State: "succeeded", Scope: c.Scope, Text: "Retained outcome"}, e
		})
		testutil.Must(t, err)
		receipt, err := f.gateway.SubmitCommandResult(ctx, f.binding.Producer, result)
		testutil.Must(t, err)
		testutil.Must(t, ledger.ImportReceipt(ctx, receipt))
		testutil.Must(t, f.gateway.AckCommandResult(ctx, f.binding.Producer, receipt))
		testutil.Must(t, ledger.Acknowledged(ctx, receipt))
		in.NativeID = "handoff-pending"
		in.SentAt = time.Now().UTC()
		admission, err := f.gateway.AdmitCommand(ctx, in)
		testutil.Must(t, err)
		fixture.pending = admission.Command
		testutil.Must(t, ledger.Import(ctx, fixture.pending))
	}
	f.stopGateway()
	prepared, err := worker.QuarantineRecovery(ctx, f.gatewayPath, artifact, f.gatewayConfig, manifest.ID, manifest.DatabaseHash)
	testutil.Must(t, err)
	if !safefs.DigestPattern.MatchString(prepared.OriginalSourceHash) {
		t.Fatal("original source archive missing")
	}
	session, err := worker.OpenRecoverySession(ctx, f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	summary, err := reconcileBackupNotifications(ctx, f.state, f.config, session)
	testutil.Must(t, err)
	testutil.Must(t, session.Close())
	fixture.options = backupReconciliationOptions{State: f.state.directory, NotificationState: f.gatewayPath, Operation: summary.Challenge.Operation, SummaryHash: notify.Hash(summary)}
	return fixture
}

func assertBackupHandoff(t *testing.T, f backupHandoffFixture, v backupRecoveryHandoffResult, interactive bool) {
	t.Helper()
	if v.Version != 1 || v.Operation != f.options.Operation || v.SummaryHash != f.options.SummaryHash || v.DeliveryEnabled || v.ExecutionEnabled || v.Delivery.After.Identity == f.binding.Identity || v.Delivery.After.Generation == f.binding.Generation {
		t.Fatal(v)
	}
	var owner backupNotificationOwner
	testutil.Must(t, f.state.get("notification_owner", &owner))
	if owner.Mode != "fenced" || owner.Binding != v.Delivery.After {
		t.Fatal(owner)
	}
	var cursor int64
	testutil.Must(t, f.state.db.QueryRow("SELECT cursor FROM notification_producer").Scan(&cursor))
	if cursor != 0 {
		t.Fatal("handoff acknowledged unread delivery receipts")
	}
	if s, err := worker.Open(f.gatewayPath, f.gatewayConfig, false); err == nil {
		s.Close()
		t.Fatal("handoff published recovery")
	}
	if err := transferBackupNotifications(context.Background(), f.state, f.client); err == nil {
		t.Fatal("delivery unfenced")
	}
	if !interactive {
		if v.Commands != nil {
			t.Fatal("invented command evidence")
		}
		return
	}
	if v.Commands == nil || v.Commands.ExecutionEnabled || v.Commands.Commands != 2 || v.Commands.Checkpoint.Summary.Terminal != 1 || v.Commands.Checkpoint.Summary.Uncertain != 1 {
		t.Fatal(v.Commands)
	}
	var calls int
	testutil.Must(t, f.state.db.QueryRow("SELECT value FROM handoff_effects").Scan(&calls))
	if calls != 1 {
		t.Fatal("domain effect replayed", calls)
	}
	current, err := interaction.OpenLedger(context.Background(), f.state.db, f.binding.Producer, v.Delivery.After.Identity, false)
	testutil.Must(t, err)
	if _, err = current.Execute(context.Background(), f.first, f.grant, "", time.Now().UTC(), func(*sql.Tx, interaction.Command) (interaction.Result, error) {
		t.Fatal("recovery executed old command")
		return interaction.Result{}, nil
	}); err == nil {
		t.Fatal("old grant accepted")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.gatewayPath, "recovery", f.options.Operation, "reconciled.db")+"?mode=ro")
	testutil.Must(t, err)
	defer db.Close()
	for id, want := range map[string]string{f.first.ID: "succeeded", f.pending.ID: "outcome_unknown"} {
		var state string
		testutil.Must(t, db.QueryRow("SELECT state FROM commands WHERE id=?", id).Scan(&state))
		if state != want {
			t.Fatal(id, state, want)
		}
	}
	var enabled int
	testutil.Must(t, db.QueryRow("SELECT count(*) FROM commands WHERE grant<>'' OR confirmed<>0 OR token_hash IS NOT NULL OR preparation<>''").Scan(&enabled))
	if enabled != 0 {
		t.Fatal("command authority revived")
	}
}

func TestBackupRecoveryHandoffWorkflow(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "notifications", true: "commands"}[interactive], func(t *testing.T) {
			f := backupHandoffTestFixture(t, interactive)
			var out, retry bytes.Buffer
			testutil.Must(t, runBackupRecoveryHandoff(context.Background(), f.options, f.config, f.gatewayConfig, &out, nil))
			var result backupRecoveryHandoffResult
			testutil.Must(t, json.Unmarshal(out.Bytes(), &result))
			assertBackupHandoff(t, f, result, interactive)
			testutil.Must(t, runBackupRecoveryHandoff(context.Background(), f.options, f.config, f.gatewayConfig, &retry, nil))
			if !bytes.Equal(out.Bytes(), retry.Bytes()) {
				t.Fatal("retry changed recovery checkpoint")
			}
		})
	}
}

func TestBackupRecoveryHandoffRefusals(t *testing.T) {
	for _, kind := range []string{"operation", "summary", "config", "commands-busy", "worker-busy", "archive", "missing-ledger", "missing-lock", "foreign-pin", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := backupHandoffTestFixture(t, true)
			o, c := f.options, f.config
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "operation":
				o.Operation, _ = safefs.NewID("n")
			case "summary":
				o.SummaryHash = notify.Hash("wrong")
			case "config":
				c.ConfigHash = notify.Hash("wrong")
			case "commands-busy":
				d, e := safefs.Open(o.State)
				testutil.Must(t, e)
				defer d.Close()
				l, e := d.Lock("commands.lock", true, false)
				testutil.Must(t, e)
				defer l.Close()
			case "worker-busy":
				s, e := worker.OpenRecoverySession(ctx, o.NotificationState, f.gatewayConfig)
				testutil.Must(t, e)
				defer s.Close()
			case "archive":
				_, e := f.state.db.Exec("UPDATE backup_command_binding SET epoch=epoch+1")
				testutil.Must(t, e)
			case "missing-ledger":
				_, e := f.state.db.Exec("DROP TABLE interactive_identity")
				testutil.Must(t, e)
			case "missing-lock":
				testutil.Must(t, os.Remove(filepath.Join(o.State, "commands.lock")))
			case "foreign-pin":
				_, e := f.state.db.Exec("UPDATE interactive_identity SET body='{}'")
				testutil.Must(t, e)
			case "cancelled":
				cancel()
			}
			if err := runBackupRecoveryHandoff(ctx, o, c, f.gatewayConfig, io.Discard, nil); err == nil {
				t.Fatal("unsafe handoff accepted")
			}
			var owner backupNotificationOwner
			testutil.Must(t, f.state.get("notification_owner", &owner))
			if owner.Binding != f.binding || owner.Mode != "fenced" {
				t.Fatal("failure changed delivery binding", owner)
			}
			var count int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key IN ('notification_recovery_ownership_intent','notification_recovery_commit')").Scan(&count))
			if count != 0 {
				t.Fatal("refused handoff changed ownership")
			}
		})
	}
}

func TestBackupRecoveryHandoffLostResult(t *testing.T) {
	f := backupHandoffTestFixture(t, true)
	for _, point := range []string{"handoff_commands_reconciled", "handoff_ownership", "handoff_delivery_commit", "handoff_command_commit", "handoff_outcomes_installed", "handoff_result"} {
		lost := errors.New("lost handoff response")
		err := runBackupRecoveryHandoff(context.Background(), f.options, f.config, f.gatewayConfig, io.Discard, func(at string) error {
			if at == point {
				return lost
			}
			return nil
		})
		if !errors.Is(err, lost) {
			t.Fatal(point, err)
		}
	}
	var out bytes.Buffer
	testutil.Must(t, runBackupRecoveryHandoff(context.Background(), f.options, f.config, f.gatewayConfig, &out, nil))
	var v backupRecoveryHandoffResult
	testutil.Must(t, json.Unmarshal(out.Bytes(), &v))
	assertBackupHandoff(t, f, v, true)
}

func TestBackupRecoveryHandoffRetainsBothLeases(t *testing.T) {
	f := backupHandoffTestFixture(t, true)
	dir, err := safefs.Open(f.state.directory)
	testutil.Must(t, err)
	defer dir.Close()
	seen := map[string]bool{}
	err = runBackupRecoveryHandoff(context.Background(), f.options, f.config, f.gatewayConfig, io.Discard, func(at string) error {
		if at != "handoff_commands_reconciled" && at != "handoff_ownership" && at != "handoff_delivery_commit" && at != "handoff_command_commit" && at != "handoff_outcomes_installed" {
			return nil
		}
		seen[at] = true
		if other, e := worker.OpenRecoverySession(context.Background(), f.gatewayPath, f.gatewayConfig); e == nil {
			other.Close()
			t.Fatal("worker lease released between stages", at)
		}
		if other, e := dir.Lock("commands.lock", true, false); e == nil {
			other.Close()
			t.Fatal("source execution lease released between stages", at)
		}
		return nil
	})
	testutil.Must(t, err)
	if len(seen) != 5 {
		t.Fatal("missed handoff stages", seen)
	}
}

func TestBackupRecoveryHandoffPreservesLEDCleanupAuthority(t *testing.T) {
	f, o := prepareBackupLEDCLI(t)
	before := f.registry.writes
	options := backupReconciliationOptions{State: f.state.directory, NotificationState: f.gatewayPath, Operation: f.summary.Challenge.Operation, SummaryHash: notify.Hash(f.summary)}
	testutil.Must(t, runBackupRecoveryHandoff(context.Background(), options, f.config, f.gatewayConfig, io.Discard, nil))
	if f.registry.writes != before {
		t.Fatal("identity handoff mutated LED state")
	}
	var out bytes.Buffer
	testutil.Must(t, runBackupLEDRecovery(context.Background(), "recovery-led-clear", o, f.config, f.gatewayConfig, "/private/staged-led.sock", f.registry, &out))
	var result backupLEDCleanupOutput
	testutil.Must(t, json.Unmarshal(out.Bytes(), &result))
	if result.Result.Outcome != "acknowledged" || result.Import == nil || f.registry.writes != before+1 {
		t.Fatal("handoff lost exact cleanup proof", result, f.registry.writes)
	}
}

func TestBackupRecoveryHandoffDamagedCompletionCannotReplay(t *testing.T) {
	for _, kind := range []string{"delivery-intent", "command-proof", "command-capture", "local-outcome", "gateway-install-file", "gateway-outcome"} {
		t.Run(kind, func(t *testing.T) {
			f := backupHandoffTestFixture(t, true)
			testutil.Must(t, runBackupRecoveryHandoff(context.Background(), f.options, f.config, f.gatewayConfig, io.Discard, nil))
			var err error
			switch kind {
			case "delivery-intent":
				_, err = f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_commit_intent'")
			case "command-proof":
				_, err = f.state.db.Exec("UPDATE interactive_recovery_commit SET proof=''")
			case "command-capture":
				_, err = f.state.db.Exec("DELETE FROM interactive_recovery_records WHERE ordinal=1")
			case "local-outcome":
				_, err = f.state.db.Exec("UPDATE interactive_results SET body=json_set(body,'$.text','changed')")
			case "gateway-install-file":
				err = os.Remove(filepath.Join(f.gatewayPath, "recovery", f.options.Operation, "command-install-"+notify.Hash(f.binding.Producer)+".json"))
			case "gateway-outcome":
				db, e := sql.Open("sqlite", "file:"+filepath.Join(f.gatewayPath, "recovery", f.options.Operation, "reconciled.db")+"?mode=rw")
				testutil.Must(t, e)
				_, err = db.Exec("UPDATE commands SET state='failed' WHERE id=?", f.first.ID)
				testutil.Must(t, db.Close())
			}
			testutil.Must(t, err)
			var out bytes.Buffer
			if err = runBackupRecoveryHandoff(context.Background(), f.options, f.config, f.gatewayConfig, &out, nil); err == nil {
				t.Fatal("damaged completion accepted")
			}
			if out.Len() != 0 {
				t.Fatal("failed handoff reported success")
			}
			var calls int
			testutil.Must(t, f.state.db.QueryRow("SELECT value FROM handoff_effects").Scan(&calls))
			if calls != 1 {
				t.Fatal("replayed domain effect")
			}
			if store, e := worker.Open(f.gatewayPath, f.gatewayConfig, false); e == nil {
				store.Close()
				t.Fatal("damaged handoff released quarantine")
			}
		})
	}
}

func TestBackupRecoveryHandoffReview(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "notifications", true: "commands"}[interactive], func(t *testing.T) {
			f := backupHandoffTestFixture(t, interactive)
			var out bytes.Buffer
			testutil.Must(t, runBackupRecoveryHandoff(context.Background(), f.options, f.config, f.gatewayConfig, &out, nil))
			review, err := worker.ReviewRecovery(context.Background(), f.gatewayPath, f.gatewayConfig, f.options.Operation)
			testutil.Must(t, err)
			if review.PublicationAllowed || review.DeliveryEnabled || review.ExecutionEnabled {
				t.Fatal("review enabled source")
			}
			count := 0
			for _, check := range review.Checks {
				if check.Scope != "producer/"+f.binding.Producer.ID {
					continue
				}
				switch check.Stage {
				case "ownership", "generation", "commands", "delivery_reconciliation":
					count++
					if check.State != "complete" {
						t.Fatal("handoff not recognized", check)
					}
					if check.Stage == "delivery_reconciliation" && check.ProofHash != f.options.SummaryHash {
						t.Fatal("review changed summary")
					}
				}
			}
			if count != 4 {
				t.Fatal("review omitted handoff stages", review)
			}
			checkpoint, err := worker.CheckpointRecovery(context.Background(), f.gatewayPath, f.gatewayConfig, f.options.Operation, review.ReviewHash)
			testutil.Must(t, err)
			if checkpoint.ReviewHash != review.ReviewHash || checkpoint.PublicationAllowed || checkpoint.DeliveryEnabled || checkpoint.ExecutionEnabled {
				t.Fatal("checkpoint changed handoff or enabled source", checkpoint)
			}
			replayed, err := worker.ReviewRecovery(context.Background(), f.gatewayPath, f.gatewayConfig, f.options.Operation)
			testutil.Must(t, err)
			if replayed.ReviewHash != review.ReviewHash {
				t.Fatal("checkpoint changed recovered backup evidence")
			}
			proof, err := worker.VerifyRecoveryCheckpoint(context.Background(), f.gatewayPath, f.gatewayConfig, f.options.Operation, review.ReviewHash, checkpoint.CheckpointHash)
			testutil.Must(t, err)
			if proof.CheckpointHash != checkpoint.CheckpointHash || proof.ProofHash == "" || proof.PublicationAllowed || proof.ExecutionEnabled || proof.DeliveryEnabled {
				t.Fatal("invalid retained backup handoff proof", proof)
			}
			image, err := worker.PrepareRecoveryPublication(context.Background(), f.gatewayPath, f.gatewayConfig, f.options.Operation, review.ReviewHash, checkpoint.CheckpointHash, proof.ProofHash)
			testutil.Must(t, err)
			if image.ProofHash != proof.ProofHash || image.ImageHash == "" || image.PublicationAllowed || image.ExecutionEnabled || image.DeliveryEnabled {
				t.Fatal("invalid prepared backup handoff image", image)
			}
			verified, err := worker.VerifyRecoveryPublication(context.Background(), f.gatewayPath, f.gatewayConfig, f.options.Operation, review.ReviewHash, checkpoint.CheckpointHash, proof.ProofHash, image.ImageHash)
			testutil.Must(t, err)
			if verified != image {
				t.Fatal("historical verification changed backup handoff image")
			}
			replayed, err = worker.ReviewRecovery(context.Background(), f.gatewayPath, f.gatewayConfig, f.options.Operation)
			testutil.Must(t, err)
			if replayed.ReviewHash != review.ReviewHash {
				t.Fatal("publication preparation changed backup handoff")
			}
			publication, publishErr := worker.PublishRecovery(context.Background(), f.gatewayPath, f.gatewayConfig, f.options.Operation, review.ReviewHash, checkpoint.CheckpointHash, proof.ProofHash, image.ImageHash)
			if interactive {
				// Domain handoff alone does not establish a provider cursor or the
				// request seal. Publication must not silently waive either stage.
				if publishErr == nil {
					t.Fatal("incomplete interactive intake published")
				}
				if _, e := os.Stat(filepath.Join(f.gatewayPath, "recovery-publication.json")); !os.IsNotExist(e) {
					t.Fatal("unready intake froze recovery", e)
				}
			} else {
				testutil.Must(t, publishErr)
				if !publication.CanonicalInstalled || publication.DeliveryEnabled || publication.ExecutionEnabled {
					t.Fatal(publication)
				}
			}
			var result backupRecoveryHandoffResult
			testutil.Must(t, json.Unmarshal(out.Bytes(), &result))
			assertBackupHandoff(t, f, result, interactive)
		})
	}
}
