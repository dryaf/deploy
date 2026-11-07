//go:build notificationintegration

package install

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

type executableFreshSignalService struct {
	*fakeFreshSignalManager
	manifest FreshSignalManifest
	cmd      *exec.Cmd
	done     chan error
	log      *os.File
}

func (f *executableFreshSignalService) manager(m FreshSignalManifest) userFreshSignalManager {
	return userFreshSignalManager{userSignalServiceManager{run: func(_ context.Context, program string, args ...string) ([]byte, error) {
		return freshSignalManagerOutput(m, freshSignalManagerProperties(m, f.state), program, args...)
	}}}
}
func (f *executableFreshSignalService) Inspect(ctx context.Context, m FreshSignalManifest) (signalServiceState, error) {
	f.manifest = m
	return f.manager(m).Inspect(ctx, m)
}
func (f *executableFreshSignalService) Start(context.Context) error {
	m := f.manifest
	log, err := os.OpenFile(filepath.Join(m.Spec.Root, "test-runtime.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	args := m.commandArgs()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		log.Close()
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	f.cmd, f.log, f.done = cmd, log, done
	f.starts++
	f.state = signalServiceState{Loaded: true, Enabled: true, Active: true, PID: uint32(cmd.Process.Pid)}
	return nil
}
func (f *executableFreshSignalService) Verify(ctx context.Context, m FreshSignalManifest) error {
	return f.manager(m).Verify(ctx, m)
}

// Actual deploy runtime, pinned child supervisor, leases and authenticated Unix
// health; manager state and the Signal executable alone are simulated.
func TestFreshSignalNativeService(t *testing.T)        { testFreshSignalNativeService(t, false) }
func TestFreshSignalNativeProviderUpdate(t *testing.T) { testFreshSignalNativeService(t, true) }
func testFreshSignalNativeService(t *testing.T, updating bool) {
	binary := freshSignalTestBinary(t)
	s, _, manager := freshSignalFixture(t, binary)
	s.Socket = filepath.Join(s.Root, "custom.sock")
	s.BinaryFile = filepath.Join(t.TempDir(), "deploy")
	s.BinarySHA256 = managedDigest(buildSignalCommandFixture(t, "./cmd/deploy", s.BinaryFile))
	service := &executableFreshSignalService{fakeFreshSignalManager: manager}
	t.Cleanup(func() {
		if service.cmd != nil {
			service.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-service.done:
			case <-time.After(10 * time.Second):
				service.cmd.Process.Kill()
				<-service.done
			}
			service.log.Close()
		}
	})
	i := freshSignalInstaller{manager: service}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	m, err := i.setup(ctx, s)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(s.Root, "test-runtime.log"))
		t.Fatal(err, string(log))
	}
	_, err = i.setup(ctx, s)
	testutil.Must(t, err)
	if service.starts != 1 {
		t.Fatal("live retry restarted service")
	}
	// Explicit resume restarts only this stopped process and retains the linked
	// account, canonical inbox and installation reference.
	testutil.Must(t, service.cmd.Process.Signal(syscall.SIGTERM))
	select {
	case <-service.done:
	case <-time.After(10 * time.Second):
		t.Fatal("native service did not stop")
	}
	testutil.Must(t, service.log.Close())
	service.cmd = nil
	service.state.Active, service.state.PID = false, 0
	resumed, e := i.restart(ctx, m.reference())
	testutil.Must(t, e)
	if protocol.Hash(resumed) != protocol.Hash(m) || service.starts != 2 {
		t.Fatal("native restart changed ownership")
	}
	pid := service.cmd.Process.Pid
	_, e = i.restart(ctx, m.reference())
	testutil.Must(t, e)
	if service.starts != 2 || service.cmd.Process.Pid != pid {
		t.Fatal("native restart replayed")
	}
	health, err := freshSignalHealth(m)
	testutil.Must(t, err)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", s.socket())
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	body := fmt.Sprintf(`{"id":"fixture","message":"synthetic direct alert","destination":%q}`, health.Destination)
	response, err := client.Post("http://localhost/send", "application/json", strings.NewReader(body))
	testutil.Must(t, err)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("synthetic delivery failed", response.StatusCode)
	}
	// Start an actual managed gateway bound to the fresh service. No recovery
	// records, provider sends or delivery activation are needed for verification.
	spec, gatewayInstaller, gatewayManager, _ := managedFixture(t)
	spec.FreshSignal = &FreshSignalReference{Root: m.Spec.Root, ManifestHash: m.ManifestHash}
	spec.UnitDirectory = s.UnitDirectory
	gatewayManager.units = s.UnitDirectory
	spec.BinaryFile = s.BinaryFile
	spec.BinarySHA256 = s.BinarySHA256
	raw, err := os.ReadFile(spec.RuntimeFile)
	testutil.Must(t, err)
	var input RuntimeInput
	testutil.Must(t, safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes))
	input.Providers = map[string]provider.Profile{"signal": {Kind: "signal", Socket: s.socket(), Destination: health.Destination}}
	for name, route := range input.Worker.Routes {
		route.Primary = "signal"
		route.Fallback = ""
		input.Worker.Routes[name] = route
	}
	if updating {
		input.Bootstrap, input.Worker.Producers = true, nil
		p := provider.Profile{Kind: "telegram", TokenFile: filepath.Join(t.TempDir(), "token"), ChatFile: filepath.Join(t.TempDir(), "chat")}
		testutil.Must(t, os.WriteFile(p.TokenFile, []byte("123:offline_native_fixture"), 0600))
		testutil.Must(t, os.WriteFile(p.ChatFile, []byte("456"), 0600))
		p.Destination = provider.TelegramIdentity("123:offline_native_fixture", "456")
		input.Providers["telegram"] = p
		for name, route := range input.Worker.Routes {
			route.Primary, route.Fallback = "telegram", "signal"
			input.Worker.Routes[name] = route
		}
	}
	config, err := input.Resolve()
	testutil.Must(t, err)
	raw, err = input.Bytes()
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
	spec.State = filepath.Join(filepath.Dir(spec.State), "fresh-state")
	if updating {
		_, err = worker.BootstrapGateway(ctx, spec.State, testutil.ID(t, "n"), config.Worker)
		testutil.Must(t, err)
	} else {
		testutil.Must(t, os.Mkdir(spec.State, 0700))
		store, err := worker.Open(spec.State, config.Worker, true)
		testutil.Must(t, err)
		for _, producer := range config.Worker.Producers {
			testutil.Must(t, store.Stage(producer.Producer, "one"))
		}
		testutil.Must(t, store.Close())
	}

	gateway := &executableManagedServices{fakeManagedServices: gatewayManager, root: spec.Root}
	t.Cleanup(func() {
		if gateway.cmd != nil {
			gateway.cmd.Process.Kill()
			<-gateway.done
			gateway.log.Close()
		}
	})
	gatewayInstaller.services = gateway
	gatewayInstaller.probe = probeManagedBinary
	gatewayManifest, err := gatewayInstaller.ensureVerification(ctx, spec)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(filepath.Dir(spec.Root), "runtime-output"))
		t.Fatal("fresh gateway startup", err, string(log))
	}
	if gatewayManifest.RecoveryCommitHash != "" {
		t.Fatal("fresh gateway fabricated recovery")
	}
	_, err = gatewayInstaller.ensureVerification(ctx, spec)
	testutil.Must(t, err)
	if updating {
		before := gatewayManifest
		changeBootstrapProviderInput(t, spec, true)
		lost := errors.New("lost live gateway migration reply")
		gatewayInstaller.fault = func(at string) error {
			if at == "start-returned" {
				return lost
			}
			return nil
		}
		_, err = gatewayInstaller.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "bootstrap-providers")
		if !errors.Is(err, lost) {
			t.Fatal("provider update", err)
		}
		gatewayInstaller.fault = nil
		gatewayPID, gatewayStarts := gateway.cmd.Process.Pid, gateway.starts
		for range 2 {
			gatewayManifest, err = gatewayInstaller.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "bootstrap-providers")
			testutil.Must(t, err)
			if gateway.cmd.Process.Pid != gatewayPID || gateway.starts != gatewayStarts {
				t.Fatal("migration replay restarted live gateway")
			}
		}
		if gatewayManifest.Identity != before.Identity || *gatewayManifest.FreshSignal != *before.FreshSignal || service.cmd.Process.Pid != pid || service.starts != 2 {
			t.Fatal("Telegram update changed native lifetime or ownership")
		}
	}
	// Select the synthetic native provider explicitly in the combined case;
	// no Telegram request is ever made by this offline process fixture.
	diagnostic, err := TestDeliveryOnceFor(ctx, spec.Root, protocol.Hash(gatewayManifest), "signal")
	testutil.Must(t, err)
	if diagnostic.Status != "provider_accepted" || diagnostic.Provider != "signal" || diagnostic.Qualified {
		t.Fatal("owned Signal diagnostic failed", diagnostic)
	}
	repeated, err := TestDeliveryOnceFor(ctx, spec.Root, protocol.Hash(gatewayManifest), "signal")
	testutil.Must(t, err)
	if repeated != diagnostic {
		t.Fatal("Signal diagnostic outcome changed on retry")
	}
	// Losing publication makes the actual runtime close its socket and release its
	// account lease; no configuration edit can keep the old process admitted.
	testutil.Must(t, os.Remove(filepath.Join(s.Root, "ready.json")))
	select {
	case err = <-service.done:
		if err == nil {
			t.Fatal("lost publication reported success")
		}
		service.cmd = nil
		service.log.Close()
	case <-time.After(15 * time.Second):
		t.Fatal("runtime ignored withdrawn authority")
	}
	select {
	case err = <-gateway.done:
		if err == nil {
			t.Fatal("gateway ignored lost fresh service")
		}
		gateway.cmd = nil
		gateway.log.Close()
	case <-time.After(15 * time.Second):
		t.Fatal("gateway stayed ready after fresh service lost authority")
	}
}
