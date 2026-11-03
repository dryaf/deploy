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

type publicationCrashInput struct {
	Path, Operation, Review, Checkpoint, Proof, Point string
	Config                                            Config
}

func TestRecoveryPublicationCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_PUBLICATION_CRASH")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	must(t, err)
	var in publicationCrashInput
	must(t, json.Unmarshal(raw, &in))
	r, err := openRecoveryStatus(context.Background(), in.Path, in.Config, in.Operation)
	must(t, err)
	defer r.Close()
	r.fault = func(at string) error {
		if at == in.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	_, err = r.preparePublication(context.Background(), in.Review, in.Checkpoint, in.Proof)
	t.Fatal("crash boundary missed", err)
}

func TestRecoveryPublicationSIGKILL(t *testing.T) {
	for _, point := range []string{"publication_intent", "publication_copied", "publication_anchor_transaction", "publication_anchor_commit", "publication_copy", "publication_sealed", "publication_image", "publication_ready"} {
		t.Run(point, func(t *testing.T) {
			r, review, cp, proof := publicationFixture(t)
			in := publicationCrashInput{r.dir.File().Name(), r.journal.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash, point, r.config}
			must(t, r.Close())
			raw, err := json.Marshal(in)
			must(t, err)
			path := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(path, raw, 0600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryPublicationCrashHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_PUBLICATION_CRASH="+path)
			output, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(output))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected child exit", cmd.ProcessState, string(output))
			}
			one, err := PrepareRecoveryPublication(context.Background(), in.Path, in.Config, in.Operation, in.Review, in.Checkpoint, in.Proof)
			must(t, err)
			two, err := PrepareRecoveryPublication(context.Background(), in.Path, in.Config, in.Operation, in.Review, in.Checkpoint, in.Proof)
			must(t, err)
			if one != two || one.PublicationAllowed || one.ExecutionEnabled || one.DeliveryEnabled {
				t.Fatal(one, two)
			}
		})
	}
}
