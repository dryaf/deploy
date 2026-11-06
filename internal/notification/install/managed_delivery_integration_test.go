//go:build notificationintegration

package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
)

type nativeDeliveryServices struct {
	*executableManagedServices
	t *testing.T
}

func (f *nativeDeliveryServices) inspector(m ManagedManifest) userDeliveryInspector {
	return userDeliveryInspector{run: func(_ context.Context, program string, _ ...string) ([]byte, error) {
		pid := uint32(0)
		if f.cmd != nil {
			pid = uint32(f.cmd.Process.Pid)
		}
		if program == "busctl" {
			return deliveryExecOutput(m), nil
		}
		p := deliveryManagerProperties(m, pid)
		unit, err := os.ReadFile(m.UnitDirectory + "/" + managedUnit)
		if err != nil {
			return nil, err
		}
		if strings.Contains(string(unit), "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6") {
			p["RestrictAddressFamilies"] = "AF_UNIX AF_INET AF_INET6"
		} else {
			p["RestrictAddressFamilies"] = "AF_UNIX"
		}
		return nativeSignalManagerOutput(p), nil
	}}
}
func (f *nativeDeliveryServices) Verify(ctx context.Context, m ManagedManifest) error {
	u := f.inspector(m)
	return (userManagedServices{deliveryInspector: &u}).Verify(ctx, m)
}

func TestManagedDeliveryNativeExecutable(t *testing.T) {
	spec, i, f, _ := managedFixture(t)
	qualificationFixtureInputs(t, spec.RuntimeFile)
	build := exec.Command("go", "build", "-o", spec.BinaryFile, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	raw, err := os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	// Stage an independent private input rather than a possible build-cache link.
	if err := os.Remove(spec.BinaryFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.BinaryFile, raw, 0700); err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(raw)
	native := &nativeDeliveryServices{executableManagedServices: &executableManagedServices{fakeManagedServices: f, root: spec.Root}, t: t}
	t.Cleanup(func() {
		if native.cmd != nil {
			native.cmd.Process.Kill()
			<-native.done
			native.log.Close()
		}
	})
	i.services = native
	i.probe = probeManagedBinary
	i.deliveryInspect = func(ctx context.Context, m ManagedManifest, stale bool) error {
		_, err := native.inspector(m).inspect(ctx, m, stale)
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	before, err := i.prepare(ctx, "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runQualificationCLI([]string{"qualification-template", "--local", "--root", spec.Root}, &out); err != nil {
		t.Fatal(err)
	}
	var report QualificationReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	qualificationPass(&report)
	status, err := qualifyFixture(t, spec.Root, report)
	if err != nil {
		t.Fatal(err)
	}
	active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(*status.Record), false)
	if err != nil {
		log, _ := os.ReadFile(spec.Root + "/../runtime-output")
		t.Fatal(err, string(log))
	}
	api := client.Client{Socket: active.Paths.Socket, Identity: active.Identity, PeerPID: uint32(native.cmd.Process.Pid)}
	var health worker.Health
	if _, err := api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); err != nil || health.VerificationOnly || health.DeliveryActivationHash != active.DeliveryActivation.Hash {
		t.Fatal(health, err)
	}
	wrong := api
	wrong.PeerPID++
	if _, err := wrong.Call(ctx, protocol.Request{Operation: "system.status"}, &health); err == nil {
		t.Fatal("wrong socket peer PID accepted")
	}
	if out, err := process.LimitedOutput(exec.CommandContext(ctx, "systemd-analyze", "--user", "verify", "--man=no", active.UnitDirectory+"/"+managedUnit), 15*time.Second, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	if _, err := i.resume(ctx, spec.Root); err != nil {
		t.Fatal("active resume", err)
	}
	c := mustRuntime(t, active.Paths.Config)
	// Exercise the real immutable binary/helper probes and runtime restart after
	// schema-5 ownership publication. Managers remain simulated; no provider I/O.
	legacy := ManagedProducerRequest{Root: spec.Root, ManifestHash: protocol.Hash(active), Producer: c.Worker.Producers[0].Producer, Route: c.Worker.Producers[0].Routes[0], Generation: "one"}
	if err := i.withManagedProducer(ctx, legacy, func(s ManagedProducerSession) error { return s.Activate() }); err != nil {
		t.Fatal("native active producer restart", err)
	}
	legacy.Revoke = true
	if err := i.withManagedProducer(ctx, legacy, func(s ManagedProducerSession) error { return s.Revoke(ctx) }); err != nil {
		t.Fatal("native producer revocation", err)
	}
	legacy.LegacyGeneration = "native-legacy-one"
	var legacySession ManagedProducerSession
	if err := i.withManagedProducer(ctx, legacy, func(s ManagedProducerSession) error { legacySession = s; return s.RestoreLegacy() }); err != nil {
		log, _ := os.ReadFile(spec.Root + "/../runtime-output")
		t.Fatal("native legacy activation", err, string(log))
	}
	if err := legacySession.RestoreLegacy(); err == nil {
		t.Fatal("closed session retained legacy authority")
	}
	check, release, err := worker.AcquireLegacyAuthority(active.Paths.State, active.Identity, legacy.Producer, legacy.LegacyGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	release()
	legacy.LegacyRevoke = true
	if err := i.withManagedProducer(ctx, legacy, func(s ManagedProducerSession) error { return s.RevokeLegacy() }); err != nil {
		t.Fatal("native legacy revocation", err)
	}
	if err := os.Remove(c.Providers["telegram"].TokenFile); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-native.done:
		if err == nil {
			t.Fatal("credential loss did not fail admission")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("delivery runtime ignored credential loss")
	}
	native.cmd = nil
	native.log.Close()
	native.log = nil
	f.state.Active = "inactive"
	deactivated, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(active), "", false)
	if err != nil || deactivated.Mode != "verification" {
		t.Fatal("credential-free deactivation", err)
	}
	if err := native.Verify(ctx, deactivated); err != nil {
		t.Fatal("verification restart", err)
	}
	updatedBinary := append(raw, []byte("\nmanaged-delivery-update\n")...)
	if err := os.WriteFile(spec.BinaryFile, updatedBinary, 0700); err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(updatedBinary)
	updated, err := i.prepare(ctx, "update", spec)
	if err != nil || updated.DeliveryOwnerHash != active.DeliveryOwnerHash {
		t.Fatal("enrolled verification update", err)
	}
	rolled, err := i.rollback(ctx, spec.Root, before.Revision)
	if err != nil || rolled.BinaryHash != before.BinaryHash || rolled.DeliveryOwnerHash != active.DeliveryOwnerHash || rolled.Mode != "verification" {
		t.Fatal("enrolled verification rollback", err)
	}
	if _, err := i.uninstall(ctx, spec.Root); err != nil {
		t.Fatal(err)
	}
	if native.cmd != nil {
		t.Fatal("runtime leaked after uninstall")
	}
}
