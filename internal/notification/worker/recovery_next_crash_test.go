package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

type nextRecoveryCrashInput struct {
	Path, Snapshot, Revision, Boundary string
	Config                             Config
	Manifest                           SnapshotManifest
	Parent                             ManagedDeliveryRecovery
}

func TestNextRecoveryCrashChild(t *testing.T) {
	input := os.Getenv("DEPLOY_NEXT_RECOVERY_CRASH_INPUT")
	if input == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(input)
	must(t, err)
	var in nextRecoveryCrashInput
	must(t, json.Unmarshal(raw, &in))
	err = prepareNextManagedRecovery(context.Background(), in.Path, in.Snapshot, in.Config, in.Manifest, in.Parent, in.Revision, func(phase string) error {
		if phase == in.Boundary {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatalf("crash boundary not reached: %v", err)
}

func TestNextRecoverySIGKILL(t *testing.T) {
	for _, point := range []string{"next_intent", "next_identity", "next_retire_" + publicationExchange, "next_journal", "committed", "quarantined"} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			path, c, revision, parent := recoveredDeliveryFixture(t, false, true)
			s, err := OpenManagedRecoveryVerification(path, c, parent.After, revision, parent.CommitHash)
			must(t, err)
			must(t, s.EnrollManagedDeliveryRecovery(parent))
			artifact := filepath.Join(t.TempDir(), "snapshot")
			m, err := s.Snapshot(ctx, artifact)
			must(t, err)
			must(t, s.Close())
			raw, err := json.Marshal(nextRecoveryCrashInput{path, artifact, revision, point, c, m, parent})
			must(t, err)
			input := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(input, raw, 0600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestNextRecoveryCrashChild$", "-test.timeout=1m")
			cmd.Env = append(os.Environ(), "DEPLOY_NEXT_RECOVERY_CRASH_INPUT="+input)
			out, err := process.LimitedOutput(cmd, 65*time.Second, 4096)
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("expected SIGKILL: %v %s", err, out)
			}
			if opened, err := OpenManagedRecoveryVerification(path, c, parent.After, revision, parent.CommitHash); err == nil {
				opened.Close()
				t.Fatal("old runtime admitted after process death")
			}
			must(t, prepareNextManagedRecovery(ctx, path, artifact, c, m, parent, revision, nil))
			r, err := openRecoveryReconciler(ctx, path, c, nil)
			must(t, err)
			defer r.Close()
			_, err = r.preserveOriginal(ctx)
			must(t, err)
		})
	}
}
