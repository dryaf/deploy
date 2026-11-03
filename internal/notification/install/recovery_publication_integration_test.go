//go:build notificationintegration

package install

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
)

func TestRecoveryPublicationNativeExecutable(t *testing.T) {
	state, flags := publicationCLIArguments(t)
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	args := append([]string{"notification-worker", "recovery-publication-prepare"}, flags...)
	run := func() []byte {
		t.Helper()
		out, err := process.LimitedOutput(exec.Command(binary, args...), 30*time.Second, 32768)
		if err != nil {
			t.Fatal(err, string(out))
		}
		return out
	}
	first := run()
	if !bytes.Equal(first, run()) {
		t.Fatal("native image replay changed")
	}
	var v worker.RecoveryPublicationImage
	if err := json.Unmarshal(first, &v); err != nil {
		t.Fatal(err)
	}
	if v.PublicationAllowed || v.ExecutionEnabled || v.DeliveryEnabled || v.ImageHash == "" {
		t.Fatal(v)
	}
	path := filepath.Join(state, "recovery", v.Operation, "checkpoints", v.ReviewHash, "publication.db")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	out, err := process.LimitedOutput(exec.Command(binary, args...), 30*time.Second, 32768)
	if err == nil || bytes.Contains(out, []byte(`"image_hash"`)) {
		t.Fatal("native command rebuilt lost sealed image", err, string(out))
	}
}

func TestRecoveryPublicationVerifyNativeExecutable(t *testing.T) {
	state, flags, expected := publicationVerifyArguments(t)
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	args := append([]string{"notification-worker", "recovery-publication-verify"}, flags...)
	run := func() {
		t.Helper()
		out, err := process.LimitedOutput(exec.Command(binary, args...), 30*time.Second, 32768)
		if err != nil || !bytes.Equal(out, expected) {
			t.Fatal("native verification changed result", err, string(out))
		}
	}
	run()
	var v worker.RecoveryPublicationImage
	if err := json.Unmarshal(expected, &v); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(state, "recovery", v.Operation)
	for _, path := range []string{filepath.Join(state, "state.db"), filepath.Join(state, "identity.json"), filepath.Join(state, "recovery.json"), filepath.Join(stage, "reconciled.db"), filepath.Join(stage, "reconciliation.json"), filepath.Join(stage, "original-source.json")} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	run()
	completion := filepath.Join(stage, "checkpoints", v.ReviewHash, "publication-image.json")
	if err := os.Remove(completion); err != nil {
		t.Fatal(err)
	}
	out, err := process.LimitedOutput(exec.Command(binary, args...), 30*time.Second, 32768)
	if err == nil || bytes.Contains(out, []byte(`"image_hash"`)) {
		t.Fatal("native verification accepted unacknowledged image", err, string(out))
	}
	if _, err := os.Stat(completion); !os.IsNotExist(err) {
		t.Fatal("verification repaired missing completion", err)
	}
}
