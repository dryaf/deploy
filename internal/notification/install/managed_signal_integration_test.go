//go:build notificationintegration

package install

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
)

func TestManagedSignalNativeExecutable(t *testing.T) {
	spec, i, f, p, before := managedSignalFixture(t)
	build := exec.Command("go", "build", "-o", spec.BinaryFile, "./cmd/deploy")
	build.Dir = "../../.."
	if out, e := process.LimitedOutput(build, time.Minute, 16384); e != nil {
		t.Fatal(e, string(out))
	}
	if err := os.Chmod(spec.BinaryFile, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(raw)
	endpoint := exec.Command(spec.BinaryFile, "notification-worker", "signal-verification-serve", "--local", "--root", p.Spec.Root, "--manifest-hash", p.ManifestHash)
	log, err := os.OpenFile(filepath.Join(filepath.Dir(spec.Root), "endpoint-output"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	endpoint.Stdout = log
	endpoint.Stderr = log
	if err = endpoint.Start(); err != nil {
		t.Fatal(err)
	}
	endpointDone := make(chan error, 1)
	go func() { endpointDone <- endpoint.Wait() }()
	t.Cleanup(func() { _ = endpoint.Process.Kill() })
	waitSignalVerification(t, signalVerificationHTTP(filepath.Join(p.Spec.Root, "api.sock")), endpointDone)
	native := &executableManagedServices{fakeManagedServices: f, root: spec.Root}
	i.services = native
	i.probe = probeManagedBinary
	i.signalProbe = nil
	i.recoveryProbe = nil
	i.signalCheck = nil
	t.Cleanup(func() {
		if native.cmd != nil {
			_ = native.cmd.Process.Kill()
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
	m, err := i.prepare(ctx, "adopt-signal", spec)
	if err != nil {
		out, _ := os.ReadFile(filepath.Join(filepath.Dir(spec.Root), "runtime-output"))
		t.Fatal(err, string(out))
	}
	if m.Identity != before.Identity {
		t.Fatal("gateway identity changed")
	}
	api := client.Client{Socket: m.Paths.Socket, Identity: m.Identity}
	var h worker.Health
	if _, err = api.Call(ctx, protocol.Request{Operation: "system.status"}, &h); err != nil {
		t.Fatal(err)
	}
	if !h.VerificationOnly || h.SignalVerificationHash != p.ManifestHash || h.Loops["signal-verification"].Condition != "ok" {
		t.Fatal(h)
	}
	if _, err = i.resume(ctx, spec.Root); err != nil {
		t.Fatal("live completed resume", err)
	}
	if err = native.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err = native.EnableStart(ctx); err != nil {
		t.Fatal(err)
	}
	if err = native.Verify(ctx, m); err != nil {
		t.Fatal("restart", err)
	}
	// Ending the separate endpoint must terminate the gateway's verification
	// runtime, rather than leave the old process reporting readiness.
	if err = endpoint.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-endpointDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("endpoint did not stop")
	}
	select {
	case err = <-native.done:
		if err == nil {
			t.Fatal("gateway silently ignored endpoint loss")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("gateway retained stale endpoint readiness")
	}
	native.cmd = nil
	native.log.Close()
	native.log = nil
	if _, err = i.resume(ctx, spec.Root); err == nil {
		t.Fatal("completed resume accepted missing endpoint")
	}
}
