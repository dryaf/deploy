//go:build linux

package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func sharedCommandReceiverFixture(t *testing.T) CommandReceiverRequest {
	t.Helper()
	r, _ := sharedCommandReceiverGatewayFixture(t)
	return r
}
func sharedCommandReceiverGatewayFixture(t *testing.T) (CommandReceiverRequest, *cutoverFixtureSession) {
	return sharedCommandReceiverModeFixture(t, false)
}
func sharedCommandReceiverModeFixture(t *testing.T, mutations bool) (CommandReceiverRequest, *cutoverFixtureSession) {
	t.Helper()
	version, permissions := 1, []string{"read"}
	if mutations {
		version, permissions = 2, []string{"read", "incident.manage"}
	}
	home := t.TempDir()
	testutil.Must(t, os.Chmod(home, 0700))
	home, c, spec, deps, session := backupCutoverFixtureUsing(t, home, func(_ *WorkerInstallation, c *worker.Config) {
		p := c.Producers[0].Producer
		d := interaction.Destination{ID: "private", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"}
		c.Interaction = &interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "backup/manager", Producer: p, Generation: "cutover-one", Commands: backupCommandManifest(mutations)}}, Destinations: []interaction.Destination{d}, Permissions: []interaction.Permission{{Actor: "789", Conversation: d.ID, Worker: p.ID, Permissions: permissions, Scope: interaction.Scope{Subjects: []string{"a"}}}}}
	})
	testutil.Must(t, runBackupCutover(context.Background(), home, c, spec, false, deps))
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	defer state.Close()
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	owner, err := currentSharedBackupOwner(state)
	testutil.Must(t, err)
	op, err := safefs.NewID("n")
	testutil.Must(t, err)
	destination := interaction.Destination{ID: "private", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"}
	policy := interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "backup/manager", Producer: session.p, Generation: session.g, Commands: backupCommandManifest(mutations)}}, Destinations: []interaction.Destination{destination}, Permissions: []interaction.Permission{{Actor: "789", Conversation: destination.ID, Worker: session.p.ID, Permissions: permissions, Scope: interaction.Scope{Subjects: []string{"a"}}}}}
	return CommandReceiverRequest{Version: version, Operation: op, State: state.directory, Archive: control.ArchiveID, WriterEpoch: control.Epoch, OwnerHash: notify.Hash(*owner), Producer: session.p, Identity: owner.Binding.Identity, Socket: owner.Socket, Policy: policy}, session
}

func TestSharedCommandReceiverCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_COMMAND_RECEIVER_CRASH_INPUT")
	if path == "" {
		t.Skip("child only")
	}
	d, err := safefs.Open(path)
	testutil.Must(t, err)
	defer d.Close()
	var input struct {
		Request  CommandReceiverRequest
		Boundary string
	}
	testutil.Must(t, d.ReadJSON("input.json", &input))
	_, err = stageSharedCommandReceiver(context.Background(), input.Request, func(at string) error {
		if at == input.Boundary {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("child survived", err)
}

func TestSharedCommandReceiverSIGKILL(t *testing.T) {
	for _, boundary := range []string{"intent", "ledger", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			r := sharedCommandReceiverFixture(t)
			path := t.TempDir()
			d, err := safefs.Open(path)
			testutil.Must(t, err)
			defer d.Close()
			testutil.Must(t, d.WriteJSON("input.json", struct {
				Request  CommandReceiverRequest
				Boundary string
			}{r, boundary}, false))
			cmd := exec.Command(os.Args[0], "-test.run=^TestSharedCommandReceiverCrashChild$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_COMMAND_RECEIVER_CRASH_INPUT="+path)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong child exit", string(out))
			}
			first, err := StageSharedCommandReceiver(context.Background(), r)
			testutil.Must(t, err)
			again, err := StageSharedCommandReceiver(context.Background(), r)
			testutil.Must(t, err)
			if first != again {
				t.Fatal("crash retry changed receiver")
			}
		})
	}
}

func TestSharedCommandReceiverResumesWithoutReinitializingLedger(t *testing.T) {
	for _, boundary := range []string{"intent", "ledger", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			r := sharedCommandReceiverFixture(t)
			ctx := context.Background()
			cutover, err := os.ReadFile(filepath.Join(r.State, backupCutoverFile))
			testutil.Must(t, err)
			stop := errors.New("lost receiver reply")
			if _, err = stageSharedCommandReceiver(ctx, r, func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}); !errors.Is(err, stop) {
				t.Fatal("boundary missed", err)
			}
			changed := r
			changed.Operation, _ = safefs.NewID("n")
			if _, err = StageSharedCommandReceiver(ctx, changed); err == nil {
				t.Fatal("retargeted receiver")
			}
			receipt, err := StageSharedCommandReceiver(ctx, r)
			testutil.Must(t, err)
			state, err := openBackupState(r.State, false)
			testutil.Must(t, err)
			// Retained ambiguous results must survive subsequent exact setup retries.
			retainedResult := interaction.Result{CommandID: "retained", CommandHash: notify.Hash("retained-command"), Revision: 1, Phase: "execute", State: "outcome_unknown", Scope: interaction.Scope{Subjects: []string{"a"}}, Text: "Outcome remains unknown."}
			testutil.Must(t, retainedResult.Validate())
			retainedBody, err := json.Marshal(retainedResult)
			testutil.Must(t, err)
			_, err = state.db.Exec("INSERT INTO interactive_results VALUES('retained',1,?,'',0)", string(retainedBody))
			testutil.Must(t, err)
			var evidence int
			testutil.Must(t, state.db.QueryRow("SELECT count(*) FROM backup_command_evidence").Scan(&evidence))
			testutil.Must(t, state.Close())
			for range 2 {
				again, err := StageSharedCommandReceiver(ctx, r)
				testutil.Must(t, err)
				if again != receipt {
					t.Fatal("receiver receipt changed")
				}
			}
			state, err = openBackupState(r.State, false)
			testutil.Must(t, err)
			defer state.Close()
			var outcome string
			testutil.Must(t, state.db.QueryRow("SELECT body FROM interactive_results WHERE id='retained'").Scan(&outcome))
			var remaining int
			testutil.Must(t, state.db.QueryRow("SELECT count(*) FROM backup_command_evidence").Scan(&remaining))
			if outcome != string(retainedBody) || evidence != remaining {
				t.Fatal("setup rewrote command history")
			}
			got, err := os.ReadFile(filepath.Join(r.State, backupCutoverFile))
			testutil.Must(t, err)
			if string(got) != string(cutover) {
				t.Fatal("receiver changed reporting ownership")
			}
		})
	}
}

func TestSharedCommandReceiverRefusesChangedAuthority(t *testing.T) {
	for _, damage := range []string{"receipt", "receipt-and-lock", "sql", "results", "identity", "lock", "archive", "owner", "cutover", "disabled", "pending", "rights"} {
		t.Run(damage, func(t *testing.T) {
			r := sharedCommandReceiverFixture(t)
			ctx := context.Background()
			_, err := StageSharedCommandReceiver(ctx, r)
			testutil.Must(t, err)
			state, err := openBackupState(r.State, false)
			testutil.Must(t, err)
			switch damage {
			case "receipt":
				testutil.Must(t, os.Remove(filepath.Join(r.State, commandReceiverFile)))
			case "receipt-and-lock":
				testutil.Must(t, os.Remove(filepath.Join(r.State, commandReceiverFile)))
				testutil.Must(t, os.Remove(filepath.Join(r.State, "commands.lock")))
			case "lock":
				testutil.Must(t, os.Remove(filepath.Join(r.State, "commands.lock")))
			case "sql":
				_, err = state.db.Exec("DROP TABLE backup_command_receiver")
			case "results":
				_, err = state.db.Exec("DROP TABLE interactive_results")
			case "identity":
				_, err = state.db.Exec("UPDATE interactive_identity SET body='{}'")
			case "archive":
				var control ArchiveControl
				testutil.Must(t, state.get("control", &control))
				control.Epoch++
				err = state.put("control", control)
			case "cutover":
				_, err = state.db.Exec("DELETE FROM meta WHERE key='notification_cutover_complete'")
			case "disabled":
				var control ArchiveControl
				testutil.Must(t, state.get("control", &control))
				control.ReportingDisabled = true
				err = state.put("control", control)
			case "owner":
				err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
					owner, err := readBackupNotificationOwner(tx)
					if err != nil {
						return err
					}
					owner.Mode = "fenced"
					return storeBackupEvaluationMeta(tx, "notification_owner", owner)
				})
			case "pending":
				err = os.WriteFile(filepath.Join(r.State, "installation.pending"), []byte("{}"), 0600)
			case "rights":
				r.Policy.Workers[0].Commands = backupCommandManifest(true)
			}
			testutil.Must(t, err)
			testutil.Must(t, state.Close())
			if _, err = StageSharedCommandReceiver(ctx, r); err == nil {
				t.Fatal("damaged receiver accepted")
			}
			if damage == "receipt-and-lock" {
				if _, err = os.Stat(filepath.Join(r.State, "commands.lock")); !os.IsNotExist(err) {
					t.Fatal("lost receiver lock recreated", err)
				}
			}
		})
	}
}

func TestSharedCommandReceiverRunsReadCommands(t *testing.T) {
	r, session := sharedCommandReceiverGatewayFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receipt, err := StageSharedCommandReceiver(ctx, r)
	testutil.Must(t, err)
	done := make(chan error, 1)
	go func() { done <- session.store.Serve(ctx, r.Socket) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("gateway failed to stop")
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.Dial("unix", r.Socket)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway unavailable")
		}
		time.Sleep(5 * time.Millisecond)
	}
	destination := r.Policy.Destinations[0]
	inbound := interaction.Inbound{Provider: destination.Provider, Account: destination.Account, NativeID: "first", Actor: "789", Conversation: destination.ID, DestinationFingerprint: destination.Fingerprint(), SentAt: time.Now().UTC(), Text: "backup/manager status"}
	view, err := session.store.AdmitCommand(ctx, inbound)
	testutil.Must(t, err)
	scheduled, err := openBackupState(r.State, false)
	testutil.Must(t, err)
	for range 2 {
		testutil.Must(t, runScheduledSharedCommands(ctx, scheduled, StepSharedCommandReceiver))
	}
	testutil.Must(t, scheduled.Close())
	state, err := openBackupState(r.State, false)
	testutil.Must(t, err)
	var body string
	var acked int
	testutil.Must(t, state.db.QueryRow("SELECT body,acked FROM interactive_results WHERE id=?", view.Command.ID).Scan(&body, &acked))
	var result interaction.Result
	testutil.Must(t, notify.Decode([]byte(body), &result, notify.MaxResponse))
	if result.State != "succeeded" || !strings.Contains(result.Text, "does not establish current backup freshness") || acked != 1 || !result.Scope.Contains("a") || result.Scope.Contains("other") {
		t.Fatal("incorrect read result", result, acked)
	}
	testutil.Must(t, state.Close())
	if again, err := StageSharedCommandReceiver(ctx, r); err != nil || again != receipt {
		t.Fatal("receiver setup changed after processing", err)
	}
	if err = StepSharedCommandReceiver(ctx, r.State, notify.Hash("wrong-receipt")); err == nil {
		t.Fatal("unreviewed receiver ran")
	}
	// A missing authority file cannot be repaired by the run path.
	testutil.Must(t, os.Remove(filepath.Join(r.State, commandReceiverFile)))
	if err = StepSharedCommandReceiver(ctx, r.State, receipt); err == nil {
		t.Fatal("receiver ignored missing intent")
	}
	if _, err = os.Stat(filepath.Join(r.State, commandReceiverFile)); !os.IsNotExist(err) {
		t.Fatal("runner recreated intent")
	}
}

func TestSharedCommandReceiverRechecksAuthorityInExecutionTransaction(t *testing.T) {
	for _, damage := range []string{"owner", "archive", "receipt", "cutover", "disabled"} {
		t.Run(damage, func(t *testing.T) {
			r, session := sharedCommandReceiverGatewayFixture(t)
			ctx := context.Background()
			_, err := StageSharedCommandReceiver(ctx, r)
			testutil.Must(t, err)
			d := r.Policy.Destinations[0]
			view, err := session.store.AdmitCommand(ctx, interaction.Inbound{Provider: d.Provider, Account: d.Account, NativeID: "first", Actor: "789", Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Now().UTC(), Text: "backup/manager status"})
			testutil.Must(t, err)
			state, err := openBackupState(r.State, false)
			testutil.Must(t, err)
			defer state.Close()
			handlers, err := backupCommandHandlers(r.Policy, r.Producer, false, nil)
			testutil.Must(t, err)
			handler := commandReceiverHandler{CommandHandler: handlers["backup.status"], request: r}
			tx, err := state.db.BeginTx(ctx, nil)
			testutil.Must(t, err)
			defer tx.Rollback()
			switch damage {
			case "owner":
				owner, e := readBackupNotificationOwner(tx)
				testutil.Must(t, e)
				owner.Mode = "fenced"
				err = storeBackupEvaluationMeta(tx, "notification_owner", owner)
			case "archive":
				var control ArchiveControl
				testutil.Must(t, backupEvaluationMeta(tx, "control", &control))
				control.Epoch++
				err = storeBackupEvaluationMeta(tx, "control", control)
			case "receipt":
				_, err = tx.Exec("UPDATE backup_command_receiver SET request_hash='changed'")
			case "cutover":
				_, err = tx.Exec("DELETE FROM meta WHERE key='notification_cutover_complete'")
			case "disabled":
				var control ArchiveControl
				testutil.Must(t, backupEvaluationMeta(tx, "control", &control))
				control.ReportingDisabled = true
				err = storeBackupEvaluationMeta(tx, "control", control)
			}
			testutil.Must(t, err)
			if _, err = handler.Apply(tx, view.Command); err == nil {
				t.Fatal("executed using stale authority")
			}
		})
	}
}

func TestSharedCommandReceiverRejectsInitialDriftBeforeIntent(t *testing.T) {
	for _, change := range []string{"owner", "archive", "rights", "foreign-ledger", "busy-schema", "busy-commands"} {
		t.Run(change, func(t *testing.T) {
			r := sharedCommandReceiverFixture(t)
			d, err := safefs.Open(r.State)
			testutil.Must(t, err)
			defer d.Close()
			switch change {
			case "owner":
				r.OwnerHash = notify.Hash("foreign")
			case "archive":
				r.WriterEpoch++
			case "rights":
				r.Policy.Workers[0].Commands = backupCommandManifest(true)
			case "foreign-ledger":
				state, err := openBackupState(r.State, false)
				testutil.Must(t, err)
				_, err = state.db.Exec("CREATE TABLE interactive_identity(singleton INTEGER PRIMARY KEY, body TEXT NOT NULL)")
				testutil.Must(t, err)
				testutil.Must(t, state.Close())
			case "busy-schema", "busy-commands":
				name := "schema.lock"
				if change == "busy-commands" {
					name = "commands.lock"
				}
				lock, err := d.Lock(name, true, true)
				testutil.Must(t, err)
				defer lock.Close()
			}
			if _, err = StageSharedCommandReceiver(context.Background(), r); err == nil {
				t.Fatal("invalid initial receiver accepted")
			}
			if _, err = os.Stat(filepath.Join(r.State, commandReceiverFile)); !os.IsNotExist(err) {
				t.Fatal("published invalid intent", err)
			}
		})
	}
}
