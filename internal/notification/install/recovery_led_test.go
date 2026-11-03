package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func TestRecoveryLEDCLIDryRunAndScope(t *testing.T) {
	c, config := runtimeFixture(t)
	c.LEDSocket = "/private/unused-led.sock"
	writeRuntime(t, config, c)
	state := filepath.Join(t.TempDir(), "absent-state")
	args := []string{"recovery-led-review", "--config", config, "--state-dir", state}
	var out bytes.Buffer
	if err := runRecoveryLEDCLI(args, &out); err == nil {
		t.Fatal("missing --local accepted")
	}
	prior := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = prior }()
	if err := runRecoveryLEDCLI(append(args, "--local", "--refresh"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no registry requests or recovery files written") {
		t.Fatal(out.String())
	}
	if err := RunCLI(append(args, "--local")); err != nil {
		t.Fatal("CLI dispatch missing", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("dry-run touched worker state", err)
	}
	c.LEDSocket = ""
	writeRuntime(t, config, c)
	if err := runRecoveryLEDCLI(append(args, "--local"), &out); err == nil {
		t.Fatal("implicit LED endpoint accepted")
	}
}

func TestRecoveryLEDCLIRequiresQuarantineAndLease(t *testing.T) {
	c, config := runtimeFixture(t)
	c.LEDSocket = "/private/unused-led.sock"
	writeRuntime(t, config, c)
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := ReadRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	s, err := worker.Open(state, runtime.Worker, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	args := []string{"recovery-led-review", "--local", "--config", config, "--state-dir", state}
	var out bytes.Buffer
	if err := runRecoveryLEDCLI(args, &out); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatal("live worker not protected by lease", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runRecoveryLEDCLI(args, &out); err == nil {
		t.Fatal("non-quarantined worker accepted")
	}
	if _, err := os.Stat(filepath.Join(state, "recovery.json")); !os.IsNotExist(err) {
		t.Fatal("review quarantined a live installation", err)
	}
}

func TestRecoveryLEDPreservationCLIScopeAndDryRun(t *testing.T) {
	c, config := runtimeFixture(t)
	c.LEDSocket = "/private/unused-led.sock"
	writeRuntime(t, config, c)
	state := filepath.Join(t.TempDir(), "absent-state")
	old := ui.DryRun
	ui.DryRun = true
	t.Cleanup(func() { ui.DryRun = old })
	id, _ := safefs.NewID("n")
	args := []string{"recovery-led-preserve", "--local", "--config", config, "--state-dir", state, "--inventory", id, "--inventory-hash", strings.Repeat("a", 64)}
	var out bytes.Buffer
	if err := runRecoveryLEDCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if err := RunCLI(args); err != nil {
		t.Fatal("dispatch missing", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("dry-run created state", err)
	}
	for _, bad := range [][]string{append(append([]string{}, args...), "--refresh"), args[:len(args)-2], {"recovery-led-review", "--local", "--config", config, "--state-dir", state, "--inventory", id}} {
		if err := runRecoveryLEDCLI(bad, &out); err == nil {
			t.Fatal("ambiguous preservation options accepted", bad)
		}
	}
	ui.DryRun = false
	if err := runRecoveryLEDCLI(args, &out); err == nil {
		t.Fatal("absent quarantine initialized")
	}
}

func TestRecoveryLEDAdoptionCLIScopeAndDryRun(t *testing.T) {
	c, config := runtimeFixture(t)
	c.LEDSocket = "/private/unused-led.sock"
	writeRuntime(t, config, c)
	state := filepath.Join(t.TempDir(), "absent-state")
	old := ui.DryRun
	ui.DryRun = true
	t.Cleanup(func() { ui.DryRun = old })
	id, _ := safefs.NewID("n")
	args := []string{"recovery-led-adopt", "--local", "--config", config, "--state-dir", state, "--inventory", id, "--inventory-hash", strings.Repeat("a", 64), "--preservation-hash", strings.Repeat("b", 64)}
	var out bytes.Buffer
	if err := runRecoveryLEDCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if err := RunCLI(args); err != nil {
		t.Fatal("dispatch missing", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("dry-run created state", err)
	}
	for _, bad := range [][]string{
		append(append([]string{}, args...), "--refresh=false"), args[:len(args)-2],
		append([]string{"recovery-led-preserve"}, args[1:]...),
		append([]string{"recovery-led-review"}, args[1:]...),
		append(append([]string{}, args...), "--preservation-hash", "invalid"),
	} {
		if err := runRecoveryLEDCLI(bad, &out); err == nil {
			t.Fatal("ambiguous adoption options accepted", bad)
		}
	}
	ui.DryRun = false
	if err := runRecoveryLEDCLI(args, &out); err == nil {
		t.Fatal("absent quarantine initialized")
	}
}
