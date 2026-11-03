//go:build notificationintegration

package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
)

func TestRecoveryFenceNativeExecutable(t *testing.T) {
	config, path, review := recoveryCLIFixture(t)
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = repo
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	if err = os.Chmod(binary, 0700); err != nil {
		t.Fatal(err)
	}
	args := []string{"notification-worker", "recovery-fence-producer", "--local", "--config", config, "--state-dir", path, "--producer", review.Message.Producer.ID}
	first, err := process.LimitedOutput(exec.Command(binary, args...), 30*time.Second, 16384)
	if err != nil {
		t.Fatal(err, string(first))
	}
	var proof worker.RecoveryOwnershipFence
	if err = json.Unmarshal(first, &proof); err != nil {
		t.Fatal(err, string(first))
	}
	if !proof.ReconciliationPending || proof.DeliveryEnabled || proof.Producer != review.Message.Producer {
		t.Fatal(proof)
	}
	second, err := process.LimitedOutput(exec.Command(binary, args...), 30*time.Second, 16384)
	if err != nil {
		t.Fatal(err, string(second))
	}
	if !bytes.Equal(first, second) {
		t.Fatal("native retry changed fence")
	}
	c, err := ReadRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	session, err := worker.OpenRecoveryOwnershipSession(context.Background(), path, c.Worker)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := session.PrepareRecoveryOwnership(context.Background(), proof.Producer)
	session.Close()
	if err != nil {
		t.Fatal(err)
	}
	if completed.PreviousHistory != proof.History {
		t.Fatal("reconciliation lost native fence")
	}
	if _, err = process.LimitedOutput(exec.Command(binary, args...), 30*time.Second, 16384); err == nil {
		t.Fatal("native command refenced completed producer")
	}
	if normal, err := worker.Open(path, c.Worker, false); err == nil {
		normal.Close()
		t.Fatal("native command published quarantine")
	}
}
