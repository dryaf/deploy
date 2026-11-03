package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
)

type ledImportCrashInput struct {
	Config               Config
	Point, Epoch, Result string
}

func TestRecoveryLEDImportCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_LED_IMPORT_CRASH")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(filepath.Join(path, "test-import-input.json"))
	must(t, err)
	var input ledImportCrashInput
	must(t, json.Unmarshal(raw, &input))
	r, err := openRecoveryReconciler(context.Background(), path, input.Config, nil)
	must(t, err)
	defer r.Close()
	r.fault = func(at string) error {
		if at == input.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	_, err = r.importLEDCleanup(context.Background(), migrationSocket, input.Epoch, input.Result)
	t.Fatal("crash boundary missed", err)
}

func TestRecoveryLEDImportSIGKILL(t *testing.T) {
	for _, point := range []string{"led_import_intent", "led_import_file", "led_import_rows", "led_import_transaction", "led_import_commit"} {
		t.Run(point, func(t *testing.T) {
			f, epoch, result := importFixture(t, "acknowledged")
			raw, err := json.Marshal(ledImportCrashInput{f.config, point, epoch.RecordHash, protocol.Hash(result)})
			must(t, err)
			must(t, os.WriteFile(filepath.Join(f.path, "test-import-input.json"), raw, 0600))
			must(t, f.r.Close())
			cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryLEDImportCrashHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_LED_IMPORT_CRASH="+f.path)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("child did not crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected child exit", cmd.ProcessState, string(out))
			}
			r, err := openRecoveryReconciler(context.Background(), f.path, f.config, nil)
			must(t, err)
			defer r.Close()
			var intent recoveryLEDImport
			must(t, r.stage.ReadJSON(ledImportHead, &intent))
			var rows, imports int
			must(t, r.db.QueryRow("SELECT count(*) FROM led_states WHERE id=?", f.old).Scan(&rows))
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_led_imports").Scan(&imports))
			if rows+imports != 1 || (imports == 1) != (point == "led_import_commit") {
				t.Fatal("non-atomic fault retirement", rows, imports)
			}
			v, err := r.importLEDCleanup(context.Background(), migrationSocket, epoch.RecordHash, protocol.Hash(result))
			must(t, err)
			if v.RecordHash != protocol.Hash(intent) {
				t.Fatal("crash replaced import intent")
			}
		})
	}
}
