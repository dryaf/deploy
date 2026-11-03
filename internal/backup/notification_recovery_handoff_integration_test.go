//go:build linux && notificationintegration

package backup

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupRecoveryHandoffNativeWorkflow(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal("build", err, string(out))
	}
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "notifications", true: "commands"}[interactive], func(t *testing.T) {
			var runtime string
			f := backupHandoffTestFixture(t, interactive, func(c *worker.Config) { runtime, *c = writeBackupLEDCLIRuntime(t, *c, "") })
			o := f.options
			o.Config = writeBackupLEDCLIInstallation(t, f.config)
			o.NotificationConfig = runtime
			reconcile := append([]string{"backup-worker", "recovery-notifications"}, backupReconciliationCLIArguments(o)...)
			out, err := process.LimitedOutput(exec.Command(binary, reconcile...), 15*time.Second, 32768)
			if err != nil {
				t.Fatal("native reconciliation", err, string(out))
			}
			var review backupReconciliationOutput
			testutil.Must(t, json.Unmarshal(out, &review))
			if review.SummaryHash != o.SummaryHash {
				t.Fatal("native summary digest changed")
			}
			args := append([]string{"backup-worker", "recovery-handoff"}, backupReconciliationCLIArguments(o)...)
			args = append(args, "--summary-hash", review.SummaryHash)
			run := func() []byte {
				out, err := process.LimitedOutput(exec.Command(binary, args...), 20*time.Second, 32768)
				if err != nil {
					t.Fatal("native handoff", err, string(out))
				}
				return out
			}
			first := run()
			if !bytes.Equal(first, run()) {
				t.Fatal("native replay changed")
			}
			var result backupRecoveryHandoffResult
			testutil.Must(t, json.Unmarshal(first, &result))
			assertBackupHandoff(t, f, result, interactive)
			reviewed, err := process.LimitedOutput(exec.Command(binary, "notification-worker", "recovery-status", "--local", "--config", runtime, "--state-dir", f.gatewayPath, "--operation", o.Operation), 20*time.Second, 32768)
			if err != nil {
				t.Fatal("native recovery status", err, string(reviewed))
			}
			var status worker.RecoveryStatus
			testutil.Must(t, json.Unmarshal(reviewed, &status))
			if status.PublicationAllowed || status.DeliveryEnabled || status.ExecutionEnabled || status.Operation != o.Operation {
				t.Fatal(status)
			}
		})
	}
}
