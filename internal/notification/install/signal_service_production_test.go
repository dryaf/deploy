package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func productionSignalFixture(t *testing.T) (SignalServiceSpec, signalServiceInstaller, *fakeSignalServiceManager) {
	t.Helper()
	s, i, f := signalServiceFixture(t)
	raw, err := os.ReadFile(s.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	p, err := inspectSignalVerificationPublication(s.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	c, err := decodeRuntime(p.OriginalConfig)
	if err != nil {
		t.Fatal(err)
	}
	selector := filepath.Join(t.TempDir(), "account")
	account := []byte(c.Signal.Account + "\n")
	if err = os.WriteFile(selector, account, 0600); err != nil {
		t.Fatal(err)
	}
	s.Runtime = &SignalServiceRuntime{CLIFile: s.BinaryFile, CLISHA256: managedDigest(raw), CLIVersion: "0.14.9", AccountFile: selector, AccountSHA256: managedDigest(account)}
	return s, i, f
}

func TestSignalProductionEnrollmentLifecycle(t *testing.T) {
	s, i, f := productionSignalFixture(t)
	m, err := i.setup(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	health, err := signalServiceHealth(m)
	if err != nil {
		t.Fatal(err)
	}
	if health.VerificationOnly || !health.IntakeEnabled {
		t.Fatal(health)
	}
	unit, err := renderSignalService(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"AF_UNIX AF_INET AF_INET6", systemdPath(m.Endpoint.Spec.AccountState), systemdPath(m.Endpoint.Spec.InboxDestination), "KillMode=control-group"} {
		if !strings.Contains(unit, part) {
			t.Fatal("missing production unit binding", part)
		}
	}
	if _, err = i.resume(context.Background(), s.Root); err != nil {
		t.Fatal(err)
	}
	if f.starts != 1 {
		t.Fatal("resume restarted admitted service")
	}
	if _, err = i.uninstall(context.Background(), s.Root); err != nil {
		t.Fatal(err)
	}
	starts := f.starts
	if _, err = i.setup(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if f.starts != starts {
		t.Fatal("removed production enrollment restarted")
	}
	if _, err = os.Stat(filepath.Join(s.Root, "signal-cli")); err != nil {
		t.Fatal("lost pinned runtime", err)
	}
}

func TestSignalProductionEvidenceLoss(t *testing.T) {
	for _, name := range []string{"signal-cli", "account", "complete.json", "intent.json"} {
		t.Run(name, func(t *testing.T) {
			s, i, _ := productionSignalFixture(t)
			if _, err := i.setup(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(s.Root, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := i.resume(context.Background(), s.Root); err == nil {
				t.Fatal("missing completed evidence repaired")
			}
		})
	}
}

func TestSignalProductionInterruptions(t *testing.T) {
	for _, phase := range []string{"prepared", "bound", "unit", "published", "start-returned", "complete"} {
		t.Run(phase, func(t *testing.T) {
			s, i, _ := productionSignalFixture(t)
			stop := errors.New("interrupted")
			i.fault = func(at string) error {
				if at == phase {
					return stop
				}
				return nil
			}
			if _, err := i.setup(context.Background(), s); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			if _, err := i.resume(context.Background(), s.Root); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSignalProductionInputRefusedBeforeIntent(t *testing.T) {
	s, i, _ := productionSignalFixture(t)
	s.Runtime.CLISHA256 = strings.Repeat("0", 64)
	if _, err := i.setup(context.Background(), s); err == nil {
		t.Fatal("changed CLI accepted")
	}
	if _, err := os.Stat(filepath.Join(s.Root, "intent.json")); !os.IsNotExist(err) {
		t.Fatal("bad input journaled", err)
	}
}
