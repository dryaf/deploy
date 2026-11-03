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
	"github.com/dryaf/deploy/internal/safefs"
)

func TestRecoveryQuarantineNativeExecutable(t *testing.T) {
	state, c, flags := quarantineCLIArguments(t)
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal("build", err, string(out))
	}
	args := append([]string{"notification-worker", "recovery-prepare"}, flags...)
	run := func() []byte {
		out, err := process.LimitedOutput(exec.Command(binary, args...), 20*time.Second, 32768)
		if err != nil {
			t.Fatal("native quarantine", err, string(out))
		}
		return out
	}
	// A running worker keeps its exclusive lease. The CLI must not stop it or
	// begin quarantine behind it.
	live, err := worker.Open(state, c, false)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if out, err := process.LimitedOutput(exec.Command(binary, args...), 20*time.Second, 32768); err == nil {
		t.Fatal("native preparation bypassed worker lease", string(out))
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	first := run()
	var view worker.RecoveryQuarantineView
	if err := json.Unmarshal(first, &view); err != nil {
		t.Fatal(err)
	}
	if view.Phase != "quarantined" || view.Before == view.After || view.DeliveryEnabled || !safefs.DigestPattern.MatchString(view.OriginalSourceHash) {
		t.Fatal(view)
	}
	if !bytes.Equal(first, run()) {
		t.Fatal("native resume changed quarantine")
	}
	if normal, err := worker.Open(state, c, false); err == nil {
		normal.Close()
		t.Fatal("native preparation published recovery")
	}
}
