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

func TestRecoveryCheckpointNativeExecutable(t *testing.T) {
	state, flags := recoveryCheckpointArguments(t)
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	args := append([]string{"notification-worker", "recovery-checkpoint"}, flags...)
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
		t.Fatal("native checkpoint replay changed")
	}
	var v worker.RecoveryCheckpoint
	if err := json.Unmarshal(first, &v); err != nil {
		t.Fatal(err)
	}
	if v.PublicationAllowed || v.DeliveryEnabled || v.ExecutionEnabled || v.DatabaseBytes <= 0 {
		t.Fatal(v)
	}
	args = append([]string{"notification-worker", "recovery-status"}, flags[:len(flags)-2]...)
	var status worker.RecoveryStatus
	if err := json.Unmarshal(run(), &status); err != nil {
		t.Fatal(err)
	}
	if status.ReviewHash != v.ReviewHash || status.ProjectionHash != v.ProjectionHash {
		t.Fatal("native checkpoint changed reviewed source", v, status)
	}
	path := filepath.Join(state, "recovery", v.Operation, "checkpoints", v.ReviewHash)
	raw, err := os.ReadFile(filepath.Join(path, "evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		CheckpointHash string `json:"checkpoint_hash"`
		ArchiveHash    string `json:"archive_hash"`
		Files          []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err = json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.CheckpointHash != v.CheckpointHash || evidence.ArchiveHash == "" || len(evidence.Files) < 5 {
		t.Fatal("native checkpoint omitted independent proofs")
	}
	if err = os.Remove(filepath.Join(path, "evidence", "00000.json")); err != nil {
		t.Fatal(err)
	}
	args = append([]string{"notification-worker", "recovery-checkpoint"}, flags...)
	output, err := process.LimitedOutput(exec.Command(binary, args...), 20*time.Second, 32768)
	if err == nil || bytes.Contains(output, []byte(`"checkpoint_hash"`)) {
		t.Fatal("native checkpoint accepted lost sealed evidence", err, string(output))
	}
}
