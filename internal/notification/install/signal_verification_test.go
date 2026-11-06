package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func signalVerificationFixture(t *testing.T) SignalVerificationSpec {
	return signalVerificationFixtureWithGateway(t, nil)
}
func signalVerificationFixtureWithGateway(t *testing.T, beforeRecovery func(string, string)) SignalVerificationSpec {
	return signalVerificationFixtureProviders(t, beforeRecovery, false)
}
func signalVerificationFixtureProviders(t *testing.T, beforeRecovery func(string, string), telegram bool) SignalVerificationSpec {
	return signalVerificationFixtureConfigured(t, beforeRecovery, telegram, nil)
}
func signalVerificationFixtureConfigured(t *testing.T, beforeRecovery func(string, string), telegram bool, configure func(*Runtime), producerRecovery ...signalProducerRecoveryFixture) SignalVerificationSpec {
	return signalVerificationFixtureConfiguredBootstrap(t, beforeRecovery, telegram, configure, false, producerRecovery...)
}
func signalVerificationFixtureConfiguredBootstrap(t *testing.T, beforeRecovery func(string, string), telegram bool, configure func(*Runtime), bootstrap bool, producerRecovery ...signalProducerRecoveryFixture) SignalVerificationSpec {
	t.Helper()
	args := signalRuntimeCLIArgumentsConfiguredBootstrap(t, beforeRecovery, telegram, configure, bootstrap, producerRecovery...)
	c, err := ReadRuntime(args[2])
	if err != nil {
		t.Fatal(err)
	}
	account := t.TempDir()
	if err = os.Chmod(account, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := signalaccount.AcquireRuntime(account)
	if err != nil {
		t.Fatal(err)
	}
	svc := &signalRecoveryServiceFixture{binding: protocol.Hash("verification-fixture-service"), active: true, lease: l}
	t.Cleanup(func() {
		if svc.lease != nil {
			svc.lease.Close()
		}
	})
	op, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	spec := SignalRecoverySpec{AccountState: account, GatewayState: args[4], RelaySource: args[8], RelayDestination: args[10], CommitHash: args[6], Operation: op, ServiceBinding: svc.binding}
	_, status, err := InstallSignalRecoveryWithService(context.Background(), c, spec, svc)
	if err != nil {
		t.Fatal(err)
	}
	// Keep Unix socket paths below sun_path's bound even with long subtest names.
	root, err := os.MkdirTemp("", "signal-verifier-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	raw, err := os.ReadFile(args[2])
	if err != nil {
		t.Fatal(err)
	}
	op, err = safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	return SignalVerificationSpec{Version: 1, Operation: op, Root: root, GatewayConfig: args[2], GatewaySHA256: managedDigest(raw), GatewayState: args[4], CommitHash: args[6], AccountState: account, InboxDestination: args[10], Recovery: status}
}
func TestSignalVerificationPublication(t *testing.T) {
	s := signalVerificationFixture(t)
	before, err := os.ReadFile(s.GatewayConfig)
	if err != nil {
		t.Fatal(err)
	}
	one, err := PrepareSignalVerification(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	two, err := PrepareSignalVerification(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(one) != protocol.Hash(two) {
		t.Fatal("endpoint retry diverged")
	}
	staged, err := ReadRuntime(filepath.Join(s.Root, signalVerificationGateway))
	if err != nil {
		t.Fatal(err)
	}
	original, err := ReadRuntime(s.GatewayConfig)
	if err != nil {
		t.Fatal(err)
	}
	if staged.Signal.Socket != filepath.Join(s.Root, "api.sock") || staged.Signal.InboxIdentity != one.Installation.Cursor.Handoff.Target.Identity {
		t.Fatal("wrong staged Signal target")
	}
	staged.Signal = original.Signal
	if protocol.Hash(staged) != protocol.Hash(original) {
		t.Fatal("staging changed other configuration or resolved revisions twice")
	}
	after, err := os.ReadFile(s.GatewayConfig)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("changed installed gateway configuration", err)
	}
	if _, err = os.Stat(filepath.Join(s.Root, "api.sock")); !os.IsNotExist(err) {
		t.Fatal("prepare bound socket", err)
	}
	if l, e := signalaccount.AcquireRuntime(s.AccountState); e == nil {
		l.Close()
		t.Fatal("prepare released account fence")
	}
	v, err := openSignalEndpointVerifier(context.Background(), s.Root, one.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	defer v.close()
	if _, err = PrepareSignalVerification(context.Background(), s); err == nil {
		t.Fatal("prepare entered live verification")
	}
	// Gateway authority is only needed during publication, not held by the server.
	d, err := safefs.Open(s.GatewayState)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	lease, err := d.Lock("worker.lock", true, false)
	if err != nil {
		t.Fatal("gateway cannot run alongside verifier", err)
	}
	lease.Close()
}
func TestSignalVerificationLostEvidence(t *testing.T) {
	for _, name := range []string{signalVerificationIntent, signalVerificationGateway, signalVerificationLock, "destination-receipt"} {
		t.Run(name, func(t *testing.T) {
			s := signalVerificationFixture(t)
			m, err := PrepareSignalVerification(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.Root, name)
			if name == "destination-receipt" {
				path = filepath.Join(s.InboxDestination, "signal-verification-endpoint.json")
			}
			if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if _, err = PrepareSignalVerification(context.Background(), s); err == nil {
				t.Fatal("completed evidence silently repaired")
			}
			if v, e := openSignalEndpointVerifier(context.Background(), s.Root, m.ManifestHash); e == nil {
				v.close()
				t.Fatal("incomplete verification served")
			}
			if _, err = os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("lost evidence recreated", err)
			}
		})
	}
}
func TestSignalVerificationIndependentBinding(t *testing.T) {
	s := signalVerificationFixture(t)
	stop := errors.New("stop")
	_, err := prepareSignalVerification(context.Background(), s, func(at string) error {
		if at == "endpoint_destination_bound" {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(s.Root, signalVerificationIntent)); err != nil {
		t.Fatal(err)
	}
	if _, err = PrepareSignalVerification(context.Background(), s); err == nil {
		t.Fatal("lost intent recreated")
	}
	s.Root, err = os.MkdirTemp("", "signal-other-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(s.Root)
	s.Operation, _ = safefs.NewID("n")
	if _, err = PrepareSignalVerification(context.Background(), s); err == nil {
		t.Fatal("destination rebound to new root")
	}
}
func TestSignalVerificationRejectsWrongPins(t *testing.T) {
	s := signalVerificationFixture(t)
	for _, mutate := range []func(*SignalVerificationSpec){func(s *SignalVerificationSpec) { s.CommitHash = protocol.Hash("wrong") }, func(s *SignalVerificationSpec) { s.GatewaySHA256 = protocol.Hash("wrong") }, func(s *SignalVerificationSpec) { s.Recovery.Target = protocol.Hash("wrong") }, func(s *SignalVerificationSpec) { s.Recovery.ResultHash = protocol.Hash("wrong") }, func(s *SignalVerificationSpec) { s.Root = s.AccountState }} {
		bad := s
		mutate(&bad)
		if _, err := PrepareSignalVerification(context.Background(), bad); err == nil {
			t.Fatal("bad pin accepted")
		}
	}
	if _, err := os.Stat(filepath.Join(s.Root, signalVerificationIntent)); !os.IsNotExist(err) {
		t.Fatal("wrong pins published intent", err)
	}
}
func TestSignalVerificationCLIAndDryRun(t *testing.T) {
	s := signalVerificationFixture(t)
	path := filepath.Join(t.TempDir(), "spec.json")
	raw, _ := json.Marshal(s)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	if err = runSignalVerificationPrepareCLI([]string{"--local", "--spec", path}, &out); err != nil {
		t.Fatal(err)
	}
	if err = runSignalVerificationServeCLI([]string{"--local", "--root", s.Root, "--manifest-hash", protocol.Hash("dry")}, &out); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(s.Root)
	if err != nil || len(before) != len(after) {
		t.Fatal("dry-run changed root", err)
	}
	ui.DryRun = false
	if err = runSignalVerificationPrepareCLI([]string{"--spec", path}, &out); err == nil {
		t.Fatal("missing local accepted")
	}
	out.Reset()
	if err = runSignalVerificationPrepareCLI([]string{"--local", "--spec", path}, &out); err != nil {
		t.Fatal(err)
	}
	var m SignalVerificationManifest
	if err = json.Unmarshal(out.Bytes(), &m); err != nil || m.validate() != nil {
		t.Fatal("bad CLI output", err)
	}
}

func signalVerificationHTTP(socket string) *http.Client {
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}
func waitSignalVerification(t *testing.T, c *http.Client, done <-chan error) SignalVerificationHealth {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatal("verifier exited", err)
		default:
		}
		r, err := c.Get("http://unix/health")
		if err == nil {
			var h SignalVerificationHealth
			e := json.NewDecoder(r.Body).Decode(&h)
			r.Body.Close()
			if e == nil && r.StatusCode == http.StatusOK {
				return h
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("verification did not become ready")
	return SignalVerificationHealth{}
}
func TestSignalVerificationServer(t *testing.T) {
	s := signalVerificationFixture(t)
	m, err := PrepareSignalVerification(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := map[string][]byte{}
	for _, path := range []string{filepath.Join(s.InboxDestination, "inbox.db"), filepath.Join(m.Installation.Source, "inbox.db"), filepath.Join(s.GatewayState, "state.db")} {
		snapshot[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		done := make(chan error, 1)
		go func() { done <- ServeSignalVerification(ctx, s.Root, m.ManifestHash) }()
		client := signalVerificationHTTP(filepath.Join(s.Root, "api.sock"))
		h := waitSignalVerification(t, client, done)
		if h.Version != 1 || !h.VerificationOnly || h.IntakeEnabled || h.ManifestHash != m.ManifestHash || h.Signal.InboxIdentity != m.Installation.Cursor.Handoff.Target.Identity {
			t.Fatal(h)
		}
		for _, path := range []string{"/send", "/receive", "/inbox", "/list", "/ack", "/health?extra=1"} {
			r, e := client.Get("http://unix" + path)
			if e != nil {
				t.Fatal(e)
			}
			r.Body.Close()
			if r.StatusCode != http.StatusForbidden {
				t.Fatal(path, r.StatusCode)
			}
		}
		r, e := client.Post("http://unix/health", "application/json", bytes.NewBufferString("{}"))
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Fatal("POST health accepted")
		}
		for _, pair := range [][2]string{{s.AccountState, "deploy-account.lock"}, {s.InboxDestination, "inbox.lock"}, {m.Installation.Source, "inbox.lock"}} {
			d, e := safefs.Open(pair[0])
			if e != nil {
				t.Fatal(e)
			}
			l, e := d.Lock(pair[1], true, false)
			d.Close()
			if e == nil {
				l.Close()
				t.Fatal("server did not retain lease", pair)
			}
		}
		cancel()
		select {
		case e := <-done:
			if !errors.Is(e, context.Canceled) {
				t.Fatal(e)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("verifier did not stop")
		}
		if _, e = os.Stat(filepath.Join(s.Root, "api.sock")); !os.IsNotExist(e) {
			t.Fatal("socket not cleaned up", e)
		}
	}
	for path, before := range snapshot {
		after, e := os.ReadFile(path)
		if e != nil || !bytes.Equal(before, after) {
			t.Fatal("verifier changed durable database", path, e)
		}
	}
}
func TestSignalVerificationServerFailsOnAuthorityLoss(t *testing.T) {
	for _, target := range []string{"receipt", "database", "socket"} {
		t.Run(target, func(t *testing.T) {
			s := signalVerificationFixture(t)
			m, err := PrepareSignalVerification(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- serveSignalVerification(ctx, s.Root, m.ManifestHash, 20*time.Millisecond) }()
			client := signalVerificationHTTP(filepath.Join(s.Root, "api.sock"))
			waitSignalVerification(t, client, done)
			var replacement net.Listener
			switch target {
			case "receipt":
				err = os.Remove(filepath.Join(s.InboxDestination, "signal-verification-endpoint.json"))
			case "database":
				path := filepath.Join(s.InboxDestination, "inbox.db")
				raw, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				if err = os.Rename(path, path+".old"); err == nil {
					err = os.WriteFile(path, raw, 0600)
				}
			case "socket":
				path := filepath.Join(s.Root, "api.sock")
				if err = os.Remove(path); err == nil {
					replacement, err = net.Listen("unix", path)
				}
				if replacement != nil {
					defer replacement.Close()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case e := <-done:
				if e == nil {
					t.Fatal("authority loss reported success")
				}
			case <-time.After(20 * time.Second):
				t.Fatal("authority loss not detected")
			}
			if replacement != nil {
				if _, e := os.Lstat(filepath.Join(s.Root, "api.sock")); e != nil {
					t.Fatal("removed replacement socket", e)
				}
			}
		})
	}
}
