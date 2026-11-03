//go:build notificationintegration

package install

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
)

func TestManagedRecoveryNativeExecutable(t *testing.T) {
	spec, i, f, _ := managedFixture(t)
	short, err := os.MkdirTemp("", "recovery-native-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(short)
	old := spec.State
	spec.State = filepath.Join(short, "state")
	if err = os.Rename(old, spec.State); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", spec.BinaryFile, "./cmd/deploy")
	build.Dir = "../../.."
	if out, e := process.LimitedOutput(build, time.Minute, 16384); e != nil {
		t.Fatal(e, string(out))
	}
	if err = os.Chmod(spec.BinaryFile, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(b)
	native := &executableManagedServices{fakeManagedServices: f, root: spec.Root}
	i.services = native
	i.probe = probeManagedBinary
	t.Cleanup(func() {
		if native.cmd != nil {
			native.cmd.Process.Kill()
			select {
			case <-native.done:
			case <-time.After(5 * time.Second):
			}
			if native.log != nil {
				native.log.Close()
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	before, err := i.prepare(ctx, "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = native.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	result := publishManagedRecovery(t, spec)
	spec.RecoveryCommitHash = result.Commit.CommitHash
	m, err := i.prepare(ctx, "adopt-recovery", spec)
	if err != nil {
		out, _ := os.ReadFile(filepath.Join(filepath.Dir(spec.Root), "runtime-output"))
		t.Fatal(err, string(out))
	}
	if m.Identity == before.Identity || m.RecoveryCommitHash != result.Commit.CommitHash {
		t.Fatal(m)
	}
	api := client.Client{Socket: m.Paths.Socket, Identity: m.Identity}
	var health worker.Health
	if _, err = api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); err != nil {
		t.Fatal(err)
	}
	if !health.VerificationOnly || health.Identity != m.Identity || health.InstallationRevision != m.Revision || health.RecoveryCommitHash != m.RecoveryCommitHash {
		t.Fatal(health)
	}
	stale := client.Client{Socket: m.Paths.Socket, Identity: before.Identity}
	if _, err = stale.Call(ctx, protocol.Request{Operation: "system.status"}, &health); err == nil {
		t.Fatal("old epoch client accepted recovered runtime")
	}
	// An active process owns the worker lease: completed resume must use bound health.
	if _, err = i.resume(ctx, spec.Root); err != nil {
		t.Fatal(err)
	}
	spec.RecoveryCommitHash = ""
	updated, err := i.prepare(ctx, "update", spec)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RecoveryCommitHash != m.RecoveryCommitHash {
		t.Fatal(updated)
	}
	if _, err = i.uninstall(ctx, spec.Root); err != nil {
		t.Fatal(err)
	}
}
