//go:build notificationintegration

package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
)

func TestNativeManagedSignalDeliveryAndCommands(t *testing.T) {
	testNativeManagedSignalDeliveryAndCommands(t, false)
}
func TestNativeLegacySignalConversationDelivery(t *testing.T) {
	testNativeManagedSignalDeliveryAndCommands(t, true)
}
func testNativeManagedSignalDeliveryAndCommands(t *testing.T, legacy bool) {
	var native *nativeDeliveryServices
	var signal *executableSignalService
	spec, i, _, before, service, record := managedSignalDeliveryFixtureEnrolled(t, func(spec *ManagedSpec, s *SignalServiceSpec, i *managedInstaller, f *fakeManagedServices, endpoint SignalVerificationManifest) signalServiceInstaller {
		spec.BinarySHA256 = managedDigest(buildSignalCommandFixture(t, "./cmd/deploy", spec.BinaryFile))
		s.BinarySHA256 = spec.BinarySHA256
		cli := filepath.Join(t.TempDir(), "signal-cli")
		source := filepath.Join(t.TempDir(), "signal.go")
		if err := os.WriteFile(source, []byte(nativeSignalCLIStub), 0600); err != nil {
			t.Fatal(err)
		}
		s.Runtime.CLIFile = cli
		s.Runtime.CLISHA256 = managedDigest(buildSignalCommandFixture(t, source, cli))
		c, err := decodeRuntime(endpoint.OriginalConfig)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(endpoint.Spec.AccountState, "test-account"), []byte(c.Signal.Account), 0600); err != nil {
			t.Fatal(err)
		}
		signal = &executableSignalService{fakeSignalServiceManager: &fakeSignalServiceManager{units: s.UnitDirectory}, t: t}
		t.Cleanup(func() {
			if signal.cmd != nil {
				_ = signal.Stop(context.Background())
			}
		})
		native = &nativeDeliveryServices{executableManagedServices: &executableManagedServices{fakeManagedServices: f, root: spec.Root}, t: t}
		t.Cleanup(func() {
			if native.cmd != nil {
				_ = native.Stop(context.Background())
			}
		})
		i.services = native
		i.deliveryInspect = func(ctx context.Context, m ManagedManifest, stale bool) error {
			_, err := native.inspector(m).inspect(ctx, m, stale)
			return err
		}
		return signalServiceInstaller{manager: signal}
	}, legacy)
	i.probe = probeManagedBinary
	t.Cleanup(func() {
		if t.Failed() {
			for _, path := range []string{filepath.Join(service.Spec.Root, "test-runtime.log"), filepath.Join(filepath.Dir(spec.Root), "runtime-output")} {
				raw, _ := os.ReadFile(path)
				t.Log(path, string(raw))
			}
		}
	})
	i.signalProbe, i.signalProfilesProbe, i.deliveryProbe, i.deliveryRecoveryProbe, i.commandProbe, i.commandRecoveryProbe = nil, nil, nil, nil, nil, nil
	// This spans activation, command admission, restart, drain and revocation.
	// Each runtime RPC keeps its own production deadline; race proof audits make
	// the complete fixture substantially slower than one lifecycle operation.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	m, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(filepath.Dir(spec.Root), "runtime-output"))
		t.Fatal(err, string(log))
	}
	api := client.Client{Socket: m.Paths.Socket, Identity: m.Identity}
	var health worker.Health
	if _, err = api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); err != nil {
		t.Fatal(err)
	}
	if health.VerificationOnly || health.CommandMode != "" || health.SignalProfilesHash != m.SignalProfiles.Hash || health.Loops["signal-verification"].Condition != "ok" {
		t.Fatal(health)
	}
	c := mustRuntime(t, m.Paths.Config)
	producer := c.Worker.Producers[0].Producer
	request := ManagedProducerRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Producer: producer, Generation: "native-signal", Route: "ops"}
	if err = i.withManagedProducer(ctx, request, func(s ManagedProducerSession) error {
		if err := s.Stage(); err != nil {
			return err
		}
		return s.Activate()
	}); err != nil {
		t.Fatal("producer activation", err)
	}
	observed := time.Now().UTC()
	snapshot := protocol.Snapshot{Producer: producer, Sequence: 2, BasisRevision: 2, ConfigRevision: "native", EvaluatedAt: observed, Subjects: []protocol.Subject{{Key: "fixture", Condition: "ok", Reason: "synthetic current evaluation", ValidUntil: observed.Add(time.Minute)}}}
	if _, err = api.Call(ctx, protocol.Request{Operation: "status.replace", Producer: producer, Snapshot: &snapshot}, nil); err != nil {
		t.Fatal(err)
	}
	message := protocol.Message{Producer: producer, Sequence: 1, ID: "native-signal-report", Generation: request.Generation, Subject: "fixture", Kind: "report", Severity: "info", ObservedAt: observed, ExpiresAt: observed.Add(time.Minute), Text: "synthetic recovered Signal report", Route: "ops", ReportKey: "native-report", ReportType: "daily", ReportRevision: 1, BasisRevision: 2}
	var job worker.Job
	if _, err = api.Call(ctx, protocol.Request{Operation: "message.enqueue", Producer: producer, Message: &message}, &job); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; {
		if _, err = api.Call(ctx, protocol.Request{Operation: "message.get", Producer: producer, EventID: message.ID}, &job); err != nil {
			t.Fatal(err)
		}
		if job.State == "provider_accepted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new effective route did not dispatch", job)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err = os.Remove(filepath.Join(service.Endpoint.Spec.AccountState, "test-sent")); err != nil {
		t.Fatal(err)
	}
	m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(record), "read", false)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(filepath.Dir(spec.Root), "runtime-output"))
		t.Fatal(err, string(log))
	}
	c = mustRuntime(t, m.Paths.Config)
	dest := c.Worker.Interaction.Destinations[0]
	now := time.Now().UnixMilli()
	envelope := fmt.Sprintf(`{"account":%q,"envelope":{"sourceUuid":%q,"sourceDevice":1,"timestamp":%d,"dataMessage":{"timestamp":%d,"message":"/workers"}}}`+"\n", c.Signal.Account, dest.NativeID, now, now)
	if err = os.WriteFile(filepath.Join(service.Endpoint.Spec.AccountState, "test-envelope"), []byte(envelope), 0600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(25 * time.Second); ; {
		if _, err = os.Stat(filepath.Join(service.Endpoint.Spec.AccountState, "test-sent")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(filepath.Dir(spec.Root), "runtime-output"))
			t.Fatal("Signal intake did not produce a reply", err, string(log))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err = native.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err = native.EnableStart(ctx); err != nil {
		t.Fatal(err)
	}
	if err = native.Verify(ctx, m); err != nil {
		t.Fatal("advanced inbox restart", err)
	}
	drain, err := i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); err != nil {
		t.Fatal(err)
	}
	if health.CommandMode != "drain" {
		t.Fatal(health)
	}
	if _, ok := health.Loops["signal-intake"]; ok {
		t.Fatal("disabled gateway still polls Signal")
	}
	request.ManifestHash = protocol.Hash(drain)
	request.Revoke = true
	if err = i.withManagedProducer(ctx, request, func(s ManagedProducerSession) error { return s.Revoke(ctx) }); err != nil {
		t.Fatal("producer revocation", err)
	}
	if _, err = i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(drain), "", false); err != nil {
		t.Fatal(err)
	}
}
