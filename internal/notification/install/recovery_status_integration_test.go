//go:build notificationintegration

package install

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
)

func TestRecoveryStatusNativeExecutable(t *testing.T) {
	_, flags := recoveryStatusArguments(t)
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	args := append([]string{"notification-worker", "recovery-status"}, flags...)
	run := func() []byte {
		t.Helper()
		out, err := process.LimitedOutput(exec.Command(binary, args...), 20*time.Second, 32768)
		if err != nil {
			t.Fatal(err, string(out))
		}
		return out
	}
	first := run()
	if !bytes.Equal(first, run()) {
		t.Fatal("native status replay changed")
	}
	var v worker.RecoveryStatus
	if err := json.Unmarshal(first, &v); err != nil {
		t.Fatal(err)
	}
	if v.PublicationAllowed || v.DeliveryEnabled || v.ExecutionEnabled || v.PendingStages == 0 {
		t.Fatal(v)
	}
}
