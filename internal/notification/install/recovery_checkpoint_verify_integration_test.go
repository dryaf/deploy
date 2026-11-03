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

func TestRecoveryCheckpointVerifyNativeExecutable(t *testing.T) {
	state, flags := checkpointVerifyArguments(t)
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	args := append([]string{"notification-worker", "recovery-checkpoint-verify"}, flags...)
	run := func() []byte {
		t.Helper()
		out, err := process.LimitedOutput(exec.Command(binary, args...), 20*time.Second, 32768)
		if err != nil {
			t.Fatal(err, string(out))
		}
		return out
	}
	first := run()
	var proof worker.RecoveryCheckpointProof
	if err := json.Unmarshal(first, &proof); err != nil {
		t.Fatal(err)
	}
	if proof.ProofHash == "" || proof.PublicationAllowed || proof.ExecutionEnabled || proof.DeliveryEnabled {
		t.Fatal(proof)
	}
	if err := os.Remove(filepath.Join(state, "recovery.json")); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(state, "recovery", proof.Operation)
	for _, name := range []string{"reconciliation.json", "original-source.json"} {
		if err := os.Remove(filepath.Join(stage, name)); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(first, run()) {
		t.Fatal("historical proof depended on live canonical records")
	}
	if err := os.Remove(filepath.Join(stage, "candidate.db")); err != nil {
		t.Fatal(err)
	}
	output, err := process.LimitedOutput(exec.Command(binary, args...), 20*time.Second, 32768)
	if err == nil || bytes.Contains(output, []byte(`"proof_hash"`)) {
		t.Fatal("native verifier accepted lost retained input", err, string(output))
	}
}
