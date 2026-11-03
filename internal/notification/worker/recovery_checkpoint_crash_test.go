package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

type checkpointCrashInput struct {
	Path, Operation, Review, Point string
	Config                         Config
}

func TestRecoveryCheckpointCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_CHECKPOINT_CRASH")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	must(t, err)
	var input checkpointCrashInput
	must(t, json.Unmarshal(raw, &input))
	r, err := openRecoveryStatus(context.Background(), input.Path, input.Config, input.Operation)
	must(t, err)
	defer r.Close()
	r.fault = func(at string) error {
		if at == input.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	_, err = r.checkpointRecovery(context.Background(), input.Review)
	t.Fatal("crash boundary missed", err)
}

func TestRecoveryCheckpointSIGKILL(t *testing.T) {
	for _, point := range []string{"checkpoint_intent", "checkpoint_copy_step", "checkpoint_copy", "checkpoint_sealed", "checkpoint_database", "checkpoint_ready", "checkpoint_evidence_intent", "checkpoint_evidence_copy", "checkpoint_evidence_file", "checkpoint_evidence_sealed", "checkpoint_evidence_ready"} {
		t.Run(point, func(t *testing.T) {
			r, review := checkpointFixture(t)
			// Ensure a page-step kill interrupts a multi-step copy, rather
			// than only killing after a tiny database is already complete.
			_, err := r.db.Exec("INSERT INTO metadata VALUES('copy-padding',hex(zeroblob(600000)))")
			must(t, err)
			review, err = r.reviewRecovery(context.Background())
			must(t, err)
			input := checkpointCrashInput{r.dir.File().Name(), r.journal.Operation, review.ReviewHash, point, r.config}
			must(t, r.Close())
			raw, err := json.Marshal(input)
			must(t, err)
			file := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(file, raw, 0600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryCheckpointCrashHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_CHECKPOINT_CRASH="+file)
			output, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(output))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected child exit", cmd.ProcessState, string(output))
			}
			first, err := CheckpointRecovery(context.Background(), input.Path, input.Config, input.Operation, input.Review)
			must(t, err)
			again, err := CheckpointRecovery(context.Background(), input.Path, input.Config, input.Operation, input.Review)
			must(t, err)
			if first != again || first.PublicationAllowed || first.ExecutionEnabled || first.DeliveryEnabled {
				t.Fatal(first, again)
			}
		})
	}
}
