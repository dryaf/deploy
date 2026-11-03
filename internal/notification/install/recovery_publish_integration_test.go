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

func TestRecoveryPublishNativeExecutable(t *testing.T) {
	_, flags := publishCLIArguments(t)
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	args := append([]string{"notification-worker", "recovery-publish"}, flags...)
	one, err := process.LimitedOutput(exec.Command(binary, args...), 30*time.Second, 32768)
	if err != nil {
		t.Fatal(err, string(one))
	}
	two, err := process.LimitedOutput(exec.Command(binary, args...), 30*time.Second, 32768)
	if err != nil || !bytes.Equal(one, two) || !bytes.Contains(one, []byte(`"canonical_installed":true`)) {
		t.Fatal("native publication failed", err, string(one), string(two))
	}
	args[1] = "recovery-publication-verify"
	out, err := process.LimitedOutput(exec.Command(binary, args...), 30*time.Second, 32768)
	if err != nil || !bytes.Contains(out, []byte(`"publication_allowed":false`)) {
		t.Fatal("publication invalidated historical image", err, string(out))
	}
}
