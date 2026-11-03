//go:build linux && notificationintegration

package backup

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupReconciliationNativeWorkflow(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal("build", err, string(out))
	}
	var runtimeFile string
	f, o := backupReconciliationCLIFixture(t, func(c *worker.Config) { runtimeFile, *c = writeBackupLEDCLIRuntime(t, *c, "") })
	o.Config, o.NotificationConfig = writeBackupLEDCLIInstallation(t, f.config), runtimeFile
	args := append([]string{"backup-worker", "recovery-notifications"}, backupReconciliationCLIArguments(o)...)
	run := func() []byte {
		out, err := process.LimitedOutput(exec.Command(binary, args...), 15*time.Second, 32768)
		if err != nil {
			t.Fatal("native reconciliation", err, string(out))
		}
		return out
	}
	first := run()
	var summary notify.RecoverySummary
	testutil.Must(t, json.Unmarshal(first, &summary))
	if summary.Challenge.Operation != o.Operation || summary.Manifest.HighestIssued != 2 || summary.Manifest.HighestAcknowledged != 1 || summary.Ambiguous != 1 {
		t.Fatal("native summary lost retained evidence", summary)
	}
	testutil.Must(t, f.state.put("paused", true))
	if !bytes.Equal(first, run()) {
		t.Fatal("native retry renewed recovery evidence")
	}
	assertRecoveryStillFenced(t, f, summary)
}
