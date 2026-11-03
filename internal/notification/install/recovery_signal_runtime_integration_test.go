//go:build notificationintegration

package install

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

func TestSignalRuntimeInstallNativeExecutable(t *testing.T) {
	flags := signalRuntimeCLIArguments(t)
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	args := append([]string{"notification-worker", "recovery-signal-install"}, flags...)
	one, err := process.LimitedOutput(exec.Command(binary, args...), time.Minute, 32768)
	if err != nil {
		t.Fatal(err, string(one))
	}
	two, err := process.LimitedOutput(exec.Command(binary, args...), time.Minute, 32768)
	if err != nil || !bytes.Equal(one, two) || !bytes.Contains(one, []byte(`"intake_enabled":false`)) {
		t.Fatal(err, string(one), string(two))
	}
}
