package install

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

type fakeFreshSignalManager struct {
	units           string
	state           signalServiceState
	starts, reloads int
	stops, disables int
}

func (f *fakeFreshSignalManager) Stop(context.Context) error {
	f.stops++
	f.state.Active, f.state.PID = false, 0
	return nil
}
func (f *fakeFreshSignalManager) Disable(context.Context) error {
	f.disables++
	f.state.Enabled = false
	return nil
}

func (f *fakeFreshSignalManager) Inspect(context.Context, FreshSignalManifest) (signalServiceState, error) {
	return f.state, nil
}
func (f *fakeFreshSignalManager) Reload(context.Context) error {
	f.reloads++
	_, err := os.Stat(filepath.Join(f.units, freshSignalUnit))
	if os.IsNotExist(err) {
		f.state = signalServiceState{}
		return nil
	}
	if err != nil {
		return err
	}
	f.state.Loaded = true
	return nil
}
func (f *fakeFreshSignalManager) Start(context.Context) error {
	f.starts++
	f.state = signalServiceState{Loaded: true, Active: true, Enabled: true, PID: 123}
	return nil
}
func (f *fakeFreshSignalManager) Verify(_ context.Context, m FreshSignalManifest) error {
	if !f.state.Active || !f.state.Enabled {
		return errors.New("synthetic service inactive")
	}
	return signalaccount.InspectNativeService(m.Spec.AccountState, m.binding())
}
func freshSignalTestBinary(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "main.go")
	testutil.Must(t, os.WriteFile(source, []byte(`package main
import("fmt";"os";"slices")
func main(){if slices.Contains(os.Args,"listAccounts") {fmt.Print(`+"`"+`[{"number":"+10000000000","aci":"11111111-1111-4111-8111-111111111111"}]`+"`"+`);return};if slices.Contains(os.Args,"--version") {fmt.Println("signal-cli 0.14.9");return};fmt.Println("synthetic-link-uri-no-network")}
`), 0600))
	binary := filepath.Join(root, "fake-cli")
	if out, err := exec.Command("go", "build", "-o", binary, source).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	testutil.Must(t, os.Chmod(binary, 0700))
	return binary
}
func freshSignalFixture(t *testing.T, binary string) (FreshSignalSpec, freshSignalInstaller, *fakeFreshSignalManager) {
	t.Helper()
	// Short owned root leaves room for Unix sockets, even with long test names.
	base, err := os.MkdirTemp("", "fresh-service-")
	testutil.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(base) })
	directory := func(name string) string {
		path := filepath.Join(base, name)
		testutil.Must(t, os.Mkdir(path, 0700))
		return path
	}
	root, units, account, parent := directory("service"), directory("units"), directory("account-state"), directory("inbox-parent")
	raw, err := os.ReadFile(binary)
	testutil.Must(t, err)
	native := signalaccount.NativeCLI{Path: binary, SHA256: managedDigest(raw), Version: "0.14.9"}
	linked, err := signalaccount.LinkNative(context.Background(), account, "fixture", testutil.ID(t, "n"), native, io.Discard)
	testutil.Must(t, err)
	inbox, err := relay.BootstrapFreshInbox(context.Background(), filepath.Join(parent, "inbox"), testutil.ID(t, "n"), "11111111-1111-4111-8111-111111111111")
	testutil.Must(t, err)
	selector, recipient := filepath.Join(base, "selector"), filepath.Join(base, "recipient")
	testutil.Must(t, os.WriteFile(selector, []byte("+10000000000"), 0600))
	testutil.Must(t, os.WriteFile(recipient, []byte("22222222-2222-4222-8222-222222222222"), 0600))
	spec := FreshSignalSpec{Version: 1, Operation: testutil.ID(t, "n"), Root: root, UnitDirectory: units, BinaryFile: binary, BinarySHA256: native.SHA256, CLIFile: binary, CLISHA256: native.SHA256, CLIVersion: native.Version, AccountState: account, AccountFile: selector, AccountSHA256: managedDigest([]byte("+10000000000")), RecipientFile: recipient, RecipientSHA256: managedDigest([]byte("22222222-2222-4222-8222-222222222222")), LinkOperation: linked.Operation, LinkHash: linked.RecordHash, Inbox: inbox}
	manager := &fakeFreshSignalManager{units: units}
	installer := freshSignalInstaller{manager: manager, probe: func(context.Context, *safefs.Directory, FreshSignalManifest) error { return nil }}
	return spec, installer, manager
}
func TestFreshSignalServiceLifecycle(t *testing.T) {
	binary := freshSignalTestBinary(t)
	s, i, f := freshSignalFixture(t, binary)
	ctx := context.Background()
	m, err := i.setup(ctx, s)
	testutil.Must(t, err)
	got, err := InspectFreshSignal(ctx, m.reference(), false)
	testutil.Must(t, err)
	if protocol.Hash(got) != protocol.Hash(m) {
		t.Fatal("inspection changed manifest")
	}
	starts, reloads := f.starts, f.reloads
	got, err = i.setup(ctx, s)
	testutil.Must(t, err)
	if protocol.Hash(got) != protocol.Hash(m) || f.starts != starts || f.reloads != reloads {
		t.Fatal("completed retry changed service")
	}
	if l, err := signalaccount.AcquireRuntime(s.AccountState); err == nil {
		l.Close()
		t.Fatal("ordinary relay bypassed native service owner")
	}
	for _, inspect := range []func() (bool, error){func() (bool, error) {
		x, e := signalaccount.InspectRecovery(s.AccountState)
		return x.RuntimeAllowed, e
	}, func() (bool, error) {
		x, e := signalaccount.InspectMaintenance(s.AccountState)
		return x.RuntimeAllowed, e
	}} {
		allowed, err := inspect()
		testutil.Must(t, err)
		if allowed {
			t.Fatal("status admitted legacy account runtime")
		}
	}
	// No recovered-account evidence is created to admit this fresh service.
	names, err := os.ReadDir(s.AccountState)
	testutil.Must(t, err)
	for _, name := range names {
		if strings.Contains(name.Name(), "recovery") {
			t.Fatal("fresh service fabricated recovery history")
		}
	}
	f.state.Active = false
	f.state.PID = 0
	if _, err = i.setup(ctx, s); err == nil || f.starts != starts {
		t.Fatal("completed stopped service restarted without review", err)
	}
}
func TestFreshSignalServiceCrashBoundaries(t *testing.T) {
	binary := freshSignalTestBinary(t)
	for _, boundary := range []string{"intent", "prepared", "staged", "verified", "binding-returned", "bound", "unit-returned", "unit", "published", "start-returned", "started", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			s, i, f := freshSignalFixture(t, binary)
			hit := false
			i.fault = func(at string) error {
				if at == boundary {
					hit = true
					return errors.New("synthetic interruption")
				}
				return nil
			}
			m, err := i.setup(context.Background(), s)
			if err == nil || !hit {
				t.Fatal("boundary not reached", err)
			}
			i.fault = nil
			completed, err := i.setup(context.Background(), s)
			testutil.Must(t, err)
			if completed.ManifestHash != m.ManifestHash || f.starts != 1 {
				t.Fatal("retry changed service identity or restarted", f.starts)
			}
			_, err = InspectFreshSignal(context.Background(), completed.reference(), false)
			testutil.Must(t, err)
		})
	}
}
func TestFreshSignalServiceRejectsLostAuthority(t *testing.T) {
	binary := freshSignalTestBinary(t)
	for _, file := range []string{"operation.lock", "installation.lock", "intent.json", "lifecycle.json", "ready.json", "complete.json", "deploy", "signal-cli", "account", "recipient"} {
		t.Run(file, func(t *testing.T) {
			s, i, f := freshSignalFixture(t, binary)
			_, err := i.setup(context.Background(), s)
			testutil.Must(t, err)
			starts, reloads := f.starts, f.reloads
			testutil.Must(t, os.Remove(filepath.Join(s.Root, file)))
			if _, err = i.setup(context.Background(), s); err == nil {
				t.Fatal("lost authority reconstructed")
			}
			if f.starts != starts || f.reloads != reloads {
				t.Fatal("damaged completed service mutated manager")
			}
		})
	}
}
func TestFreshSignalServiceRejectsForeignUnitBeforeBinding(t *testing.T) {
	binary := freshSignalTestBinary(t)
	s, i, f := freshSignalFixture(t, binary)
	testutil.Must(t, os.WriteFile(filepath.Join(s.UnitDirectory, freshSignalUnit), []byte("foreign"), 0600))
	if _, err := i.setup(context.Background(), s); err == nil {
		t.Fatal("foreign unit accepted")
	}
	lease, err := signalaccount.AcquireRuntime(s.AccountState)
	testutil.Must(t, err)
	lease.Close()
	if f.starts != 0 || f.reloads != 0 {
		t.Fatal("foreign unit touched")
	}
}

func TestFreshSignalServiceProbesIdentityBeforePermanentBinding(t *testing.T) {
	binary := freshSignalTestBinary(t)
	s, i, f := freshSignalFixture(t, binary)
	// An unchanged native link with the wrong selector must not acquire permanent
	// service ownership, publish a unit or start a process.
	testutil.Must(t, os.WriteFile(s.AccountFile, []byte("+19999999999"), 0600))
	s.AccountSHA256 = managedDigest([]byte("+19999999999"))
	if _, err := i.setup(context.Background(), s); err == nil {
		t.Fatal("unrelated account selector bound")
	}
	lease, err := signalaccount.AcquireRuntime(s.AccountState)
	testutil.Must(t, err)
	lease.Close()
	if f.starts != 0 || f.reloads != 0 {
		t.Fatal("identity mismatch changed manager")
	}
	if _, err = os.Stat(filepath.Join(s.UnitDirectory, freshSignalUnit)); !os.IsNotExist(err) {
		t.Fatal("identity mismatch published unit", err)
	}
}
