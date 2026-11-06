//go:build notificationintegration

package install

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
)

// Real executable/runtime/helper probes and socket peer checks; all managers
// and renewal observations are synthetic. Commands stay off, so this executable
// has no reason to poll a provider or send a message.
func TestManagedRecoveredTelegramDeliveryNative(t *testing.T) {
	spec, i, _, active, _ := recoveredTelegramCommandsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(active), "", false); err != nil {
		t.Fatal(err)
	}
	if err := i.services.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", spec.BinaryFile, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	raw, err := os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(spec.BinaryFile); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(spec.BinaryFile, raw, 0700); err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256, spec.RecoveryCommitHash = managedDigest(raw), ""
	f := i.services.(*fakeDeliveryServices).fakeManagedServices
	native := &nativeDeliveryServices{executableManagedServices: &executableManagedServices{fakeManagedServices: f, root: spec.Root}, t: t}
	t.Cleanup(func() {
		if native.cmd != nil {
			native.cmd.Process.Kill()
			<-native.done
			native.log.Close()
		}
	})
	i.services, i.probe = native, probeManagedBinary
	i.commandProbe, i.deliveryProbe, i.deliveryRecoveryProbe, i.telegramHandoffProbe, i.telegramProbe, i.recoveryProbe, i.commandRecoveryProbe = nil, nil, nil, nil, nil, nil, nil
	i.deliveryInspect = func(ctx context.Context, m ManagedManifest, stale bool) error {
		_, err := native.inspector(m).inspect(ctx, m, stale)
		return err
	}
	m, err := i.prepare(ctx, "upgrade-helper", spec)
	if err != nil {
		t.Fatal("native recovered helper upgrade", err)
	}
	record := freshDeliveryQualification(t, spec.Root)
	m, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(record), false)
	if err != nil {
		t.Fatal("native recovered interactive delivery", err)
	}
	api := client.Client{Socket: m.Paths.Socket, Identity: m.Identity, PeerPID: uint32(native.cmd.Process.Pid)}
	var health worker.Health
	if _, err = api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); err != nil || health.VerificationOnly || health.CommandMode != "" || health.TelegramRenewalID != m.TelegramRenewalID {
		t.Fatal(health, err)
	}
	if err = native.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err = native.EnableStart(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = i.resume(ctx, spec.Root); err != nil {
		t.Fatal("native recovered restart", err)
	}
	if _, err = i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(m), "", false); err != nil {
		t.Fatal(err)
	}
	if _, err = i.uninstall(ctx, spec.Root); err != nil {
		t.Fatal(err)
	}
}
