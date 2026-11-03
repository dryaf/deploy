package install

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func TestRecoveryLEDEpochCLIScopeAndDryRun(t *testing.T) {
	c, config := runtimeFixture(t)
	c.LEDSocket = "/private/unused-led.sock"
	writeRuntime(t, config, c)
	path := filepath.Join(t.TempDir(), "missing-state")
	args := []string{"recovery-led-handoff", "--local", "--config", config, "--state-dir", path, "--adoption-hash", protocol.Hash("reviewed")}
	prior := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = prior }()
	var out bytes.Buffer
	if err := runRecoveryLEDEpochCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("no state opened")) {
		t.Fatal(out.String())
	}
	if err := RunCLI(args); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("dry-run created state", err)
	}
	for _, extra := range [][]string{{"--local=false"}, {"--adoption-hash", ""}, {"--refresh"}, {"--state-dir", "relative"}, {"--inventory", "unused"}} {
		if err := runRecoveryLEDEpochCLI(append(append([]string{}, args...), extra...), &out); err == nil {
			t.Fatal("invalid scope accepted", extra)
		}
	}
}

func TestRecoveryLEDEpochCLIRequiresOfflineQuarantine(t *testing.T) {
	c, config := runtimeFixture(t)
	c.LEDSocket = "/private/unused-led.sock"
	writeRuntime(t, config, c)
	runtime, err := ReadRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir()
	if err = os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := worker.Open(path, runtime.Worker, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	args := []string{"recovery-led-handoff", "--local", "--config", config, "--state-dir", path, "--adoption-hash", protocol.Hash("reviewed")}
	var out bytes.Buffer
	if err = runRecoveryLEDEpochCLI(args, &out); err == nil {
		t.Fatal("live lease bypassed")
	}
	s.Close()
	if err = runRecoveryLEDEpochCLI(args, &out); err == nil {
		t.Fatal("unquarantined state accepted")
	}
	if _, err = os.Stat(filepath.Join(path, "recovery.json")); !os.IsNotExist(err) {
		t.Fatal("command created quarantine", err)
	}
}
