package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/ui"
)

func TestManagedSignalCLIDryRun(t *testing.T) {
	spec, _, _, p, _ := managedSignalFixture(t)
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	spec.UnitDirectory = filepath.Join(home, "systemd", "user")
	path := filepath.Join(t.TempDir(), "adopt.json")
	write := func() {
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	before, err := os.ReadFile(filepath.Join(spec.Root, "lifecycle.json"))
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := os.ReadFile(filepath.Join(p.Spec.Root, signalVerificationComplete))
	if err != nil {
		t.Fatal(err)
	}
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	if err = RunCLI([]string{"adopt-signal", "--local", "--spec", path}); err != nil {
		t.Fatal(err)
	}
	if err = RunCLI([]string{"adopt-signal", "--spec", path}); err == nil {
		t.Fatal("local flag missing")
	}
	if err = RunCLI([]string{"update", "--local", "--spec", path}); err == nil {
		t.Fatal("ordinary update accepted adoption flag")
	}
	if err = RunCLI([]string{"__managed-signal-verification-probe", "--local"}); err == nil {
		t.Fatal("probe accepted flag")
	}
	spec.RuntimeFile = p.Spec.GatewayConfig
	write()
	if err = RunCLI([]string{"adopt-signal", "--local", "--spec", path}); err == nil {
		t.Fatal("dry-run accepted unstaged configuration")
	}
	after, err := os.ReadFile(filepath.Join(spec.Root, "lifecycle.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("dry-run changed lifecycle", err)
	}
	after, err = os.ReadFile(filepath.Join(p.Spec.Root, signalVerificationComplete))
	if err != nil || string(endpoint) != string(after) {
		t.Fatal("dry-run changed endpoint", err)
	}
	if _, err = os.Stat(spec.UnitDirectory); !os.IsNotExist(err) {
		t.Fatal("dry-run created unit directory", err)
	}
}
