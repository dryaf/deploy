package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

type signalRecoveryServiceFixture struct {
	binding     string
	active      bool
	lease       *signalaccount.Lease
	onStop      func() error
	unsupported bool
	stops       int
}

func (s *signalRecoveryServiceFixture) Inspect(context.Context) (signalaccount.ServiceState, error) {
	return signalaccount.ServiceState{Binding: s.binding, Active: s.active}, nil
}
func (s *signalRecoveryServiceFixture) Stop(context.Context) error {
	s.stops++
	s.active = false
	if s.lease != nil {
		s.lease.Close()
		s.lease = nil
	}
	if s.onStop != nil {
		return s.onStop()
	}
	return nil
}
func (s *signalRecoveryServiceFixture) Start(context.Context) error {
	return errors.New("recovery must not start receiver")
}
func (s *signalRecoveryServiceFixture) VerifyRecoverySupport(context.Context) error {
	if s.unsupported {
		return errors.New("old image")
	}
	return nil
}
func signalServiceRecoveryFixture(t *testing.T) (Runtime, SignalRecoverySpec, *signalRecoveryServiceFixture) {
	t.Helper()
	args := signalRuntimeCLIArguments(t)
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
	svc := &signalRecoveryServiceFixture{binding: protocol.Hash("pinned-service-account-image"), active: true, lease: l}
	t.Cleanup(func() {
		if svc.lease != nil {
			svc.lease.Close()
		}
	})
	id, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	return c, SignalRecoverySpec{AccountState: account, GatewayState: args[4], RelaySource: args[8], RelayDestination: args[10], CommitHash: args[6], Operation: id, ServiceBinding: svc.binding}, svc
}
func TestSignalRecoveryServiceInstallation(t *testing.T) {
	c, spec, svc := signalServiceRecoveryFixture(t)
	before, err := os.ReadFile(filepath.Join(spec.GatewayState, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	one, status, err := InstallSignalRecoveryWithService(context.Background(), c, spec, svc)
	if err != nil || status.RuntimeAllowed || status.Phase != "complete" || status.ResultHash != one.InstallationHash || svc.active || one.IntakeEnabled {
		t.Fatal(one, status, err)
	}
	stops := svc.stops
	two, again, err := InstallSignalRecoveryWithService(context.Background(), c, spec, svc)
	if err != nil || protocol.Hash(one) != protocol.Hash(two) || status != again || svc.stops != stops {
		t.Fatal(two, again, err)
	}
	after, err := os.ReadFile(filepath.Join(spec.GatewayState, "state.db"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("gateway changed", err)
	}
	if l, e := signalaccount.AcquireRuntime(spec.AccountState); e == nil {
		l.Close()
		t.Fatal("old account receiver restarted")
	}
	inbox, err := relay.OpenInstalledInboxVerification(context.Background(), spec.RelayDestination, one.InstallationHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = inbox.List(context.Background(), inbox.Cursor()); err == nil {
		t.Fatal("verification listed messages")
	}
	inbox.Close()
	var out bytes.Buffer
	if err = runSignalLinkCLI([]string{"signal-recovery-status", "--local", "--state-dir", spec.AccountState}, &out); err != nil {
		t.Fatal(err)
	}
	var got signalaccount.RecoveryStatus
	if err = json.Unmarshal(out.Bytes(), &got); err != nil || got != status {
		t.Fatal(out.String(), err)
	}
	if err = os.Remove(filepath.Join(spec.RelayDestination, "signal-runtime-install-complete.json")); err != nil {
		t.Fatal(err)
	}
	result, _, err := InstallSignalRecoveryWithService(context.Background(), c, spec, svc)
	if err == nil || result.InstallationHash != "" {
		t.Fatal("lost completed inbox reconstructed", err)
	}
}
func TestSignalRecoveryServiceRejectsWrongAuthority(t *testing.T) {
	for _, kind := range []string{"capability", "commit", "source-identity", "account-overlap", "gateway-loss", "gateway-directory", "gateway-lease", "gateway-database", "service-change", "stop-response-loss"} {
		t.Run(kind, func(t *testing.T) {
			c, spec, svc := signalServiceRecoveryFixture(t)
			switch kind {
			case "capability":
				svc.unsupported = true
			case "commit":
				spec.CommitHash = protocol.Hash("other")
			case "source-identity":
				c.Signal.InboxIdentity.Recovery = "wrong"
			case "account-overlap":
				spec.AccountState = spec.RelaySource
			case "gateway-loss":
				svc.onStop = func() error { return os.Remove(filepath.Join(spec.GatewayState, "identity.json")) }
			case "gateway-directory":
				svc.onStop = func() error {
					old := spec.GatewayState + "-old"
					if e := os.Rename(spec.GatewayState, old); e != nil {
						return e
					}
					t.Cleanup(func() { os.RemoveAll(old) })
					return os.Mkdir(spec.GatewayState, 0700)
				}
			case "gateway-lease", "gateway-database":
				svc.onStop = func() error {
					name := "worker.lock"
					if kind == "gateway-database" {
						name = "state.db"
					}
					path := filepath.Join(spec.GatewayState, name)
					data, e := os.ReadFile(path)
					if e != nil {
						return e
					}
					if e = os.Rename(path, path+".old"); e != nil {
						return e
					}
					return os.WriteFile(path, data, 0600)
				}
			case "service-change":
				svc.onStop = func() error { svc.binding = protocol.Hash("other"); return nil }
			case "stop-response-loss":
				svc.onStop = func() error { return errors.New("lost stop reply") }
			}
			result, status, err := InstallSignalRecoveryWithService(context.Background(), c, spec, svc)
			if err == nil || result.InstallationHash != "" {
				t.Fatal("wrong authority accepted", err)
			}
			if kind == "capability" || kind == "commit" || kind == "source-identity" || kind == "account-overlap" {
				if svc.stops != 0 {
					t.Fatal("preflight stopped service")
				}
			} else if status.RuntimeAllowed {
				t.Fatal("lost fence", status)
			}
			if kind == "stop-response-loss" {
				svc.onStop = nil
				_, status, err = InstallSignalRecoveryWithService(context.Background(), c, spec, svc)
				if err != nil || status.Phase != "complete" {
					t.Fatal(status, err)
				}
			}
		})
	}
}
func TestSignalRecoveryStatusDryRun(t *testing.T) {
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	path := filepath.Join(t.TempDir(), "absent")
	var out bytes.Buffer
	if err := runSignalLinkCLI([]string{"signal-recovery-status", "--local", "--state-dir", path}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("dry run created directory")
	}
	for _, a := range [][]string{{"signal-recovery-status", "--state-dir", path}, {"signal-recovery-status", "--local", "--state-dir", "relative"}, {"signal-recovery-status", "--local", "--state-dir", path, "--operation", "extra"}} {
		if err := runSignalLinkCLI(a, &out); err == nil {
			t.Fatal("invalid flags accepted", a)
		}
	}
}
