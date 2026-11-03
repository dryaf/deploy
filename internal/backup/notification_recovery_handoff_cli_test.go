//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/ui"
)

func TestBackupRecoveryHandoffCLI(t *testing.T) {
	var runtime string
	f := backupHandoffTestFixture(t, true, func(c *worker.Config) { runtime, *c = writeBackupLEDCLIRuntime(t, *c, "") })
	o := f.options
	o.Config = writeBackupLEDCLIInstallation(t, f.config)
	o.NotificationConfig = runtime
	args := append(backupReconciliationCLIArguments(o), "--summary-hash", o.SummaryHash)
	var out bytes.Buffer
	testutil.Must(t, runBackupRecoveryCoordinatorCLI("recovery-handoff", args, &out))
	var v backupRecoveryHandoffResult
	testutil.Must(t, json.Unmarshal(out.Bytes(), &v))
	assertBackupHandoff(t, f, v, true)
	testutil.Must(t, RunCLI(append([]string{"recovery-handoff"}, args...)))
}

func TestBackupRecoveryHandoffCLIDryRunAndFlags(t *testing.T) {
	f := backupHandoffTestFixture(t, true)
	o := f.options
	o.Config = writeBackupLEDCLIInstallation(t, f.config)
	o.NotificationConfig, _ = writeBackupLEDCLIRuntime(t, f.gatewayConfig, "")
	o.State = filepath.Join(t.TempDir(), "missing-source")
	o.NotificationState = filepath.Join(t.TempDir(), "missing-gateway")
	args := append(backupReconciliationCLIArguments(o), "--summary-hash", o.SummaryHash)
	prior := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = prior }()
	var out bytes.Buffer
	testutil.Must(t, runBackupRecoveryCoordinatorCLI("recovery-handoff", args, &out))
	if !bytes.Contains(out.Bytes(), []byte("recovery-handoff")) {
		t.Fatal(out.String())
	}
	for _, bad := range [][]string{{"--summary-hash", ""}, {"--summary-hash", "bad"}, {"--operation", ""}, {"--local=false"}, {"--notification-state", o.State}, {"--activate"}, {"positional"}} {
		if err := runBackupRecoveryCoordinatorCLI("recovery-handoff", append(append([]string{}, args...), bad...), io.Discard); err == nil {
			t.Fatal("invalid flags", bad)
		}
	}
	if err := runBackupReconciliationCLI(append(backupReconciliationCLIArguments(o), "--summary-hash", ""), io.Discard); err == nil {
		t.Fatal("handoff option accepted by reconciliation")
	}
	for _, path := range []string{o.State, o.NotificationState} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("dry-run initialized state", err)
		}
	}
}

func TestBackupRecoveryHandoffRechecksStableSourceLeases(t *testing.T) {
	for _, name := range []string{"commands.lock", "schema.lock"} {
		t.Run(name, func(t *testing.T) {
			f := backupHandoffTestFixture(t, true)
			err := runBackupRecoveryHandoff(context.Background(), f.options, f.config, f.gatewayConfig, io.Discard, func(at string) error {
				if at != "handoff_preflight" {
					return nil
				}
				path := filepath.Join(f.state.directory, name)
				if e := os.Remove(path); e != nil {
					return e
				}
				return os.WriteFile(path, nil, 0600)
			})
			if err == nil {
				t.Fatal("replaced stable source lease accepted")
			}
			var count int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='interactive_recovery'").Scan(&count))
			if count != 0 {
				t.Fatal("captured commands after lease replacement")
			}
		})
	}
}

func TestBackupRecoveryHandoffRejectsForeignCommandChallenge(t *testing.T) {
	f := backupHandoffTestFixture(t, true)
	ctx := context.Background()
	s, err := worker.OpenRecoveryHandoffSession(ctx, f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	ch, err := s.CommandChallenge(ctx, f.binding.Producer)
	testutil.Must(t, err)
	testutil.Must(t, s.Close())
	ch.Operation, err = safefs.NewID("n")
	testutil.Must(t, err)
	// Preserve a structurally valid, fresh challenge under the wrong operation.
	// The coordinator must pin it before CaptureRecovery changes source state.
	body, err := json.Marshal(struct {
		Challenge notify.RecoveryChallenge
		Manifest  any
		Summary   any
	}{Challenge: ch})
	testutil.Must(t, err)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.gatewayPath, "recovery", f.options.Operation, "reconciled.db")+"?mode=rw")
	testutil.Must(t, err)
	_, err = db.Exec("UPDATE reconciliation_command_sessions SET body=? WHERE producer=?", string(body), f.binding.Producer.ID)
	testutil.Must(t, err)
	testutil.Must(t, db.Close())
	if err = runBackupRecoveryHandoff(ctx, f.options, f.config, f.gatewayConfig, io.Discard, nil); err == nil {
		t.Fatal("foreign command challenge accepted")
	}
	var count int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='interactive_recovery'").Scan(&count))
	if count != 0 {
		t.Fatal("foreign challenge fenced source under wrong operation")
	}
}

func TestBackupRecoveryHandoffCannotSkipGatewayCommands(t *testing.T) {
	f := backupHandoffTestFixture(t, false)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.gatewayPath, "recovery", f.options.Operation, "reconciled.db")+"?mode=rw")
	testutil.Must(t, err)
	_, err = db.Exec("INSERT INTO commands(id,request_key,input_hash,producer,body,state,created) VALUES('retained-command','retained-request','hash',?,'{}','outcome_unknown',1)", f.binding.Producer.ID)
	testutil.Must(t, err)
	testutil.Must(t, db.Close())
	if err = runBackupRecoveryHandoff(context.Background(), f.options, f.config, f.gatewayConfig, io.Discard, nil); err == nil {
		t.Fatal("notification-only handoff ignored gateway command history")
	}
	var count int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_ownership_intent'").Scan(&count))
	if count != 0 {
		t.Fatal("rotated ownership before command reconciliation")
	}
}
