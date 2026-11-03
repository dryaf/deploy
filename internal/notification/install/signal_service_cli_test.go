package install

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func TestSignalServiceCLIDryRun(t *testing.T) {
	s, _, _ := signalServiceFixture(t)
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	s.UnitDirectory = filepath.Join(config, "systemd", "user")
	dir := t.TempDir()
	d, err := safefs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.WriteJSON("spec.json", s, false); err != nil {
		t.Fatal(err)
	}
	before := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = before }()
	var out bytes.Buffer
	args := []string{"signal-service-setup", "--local", "--spec", filepath.Join(dir, "spec.json")}
	if err = runSignalServiceCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[DRY]") {
		t.Fatal(out.String())
	}
	names, err := os.ReadDir(s.Root)
	if err != nil || len(names) != 0 {
		t.Fatal("dry-run changed installation", err)
	}
	if _, err = os.Stat(s.UnitDirectory); !os.IsNotExist(err) {
		t.Fatal("dry-run created unit directory", err)
	}
	if _, err = os.Stat(filepath.Join(s.Endpoint.Root, signalServiceEndpointBinding)); !os.IsNotExist(err) {
		t.Fatal("dry-run bound endpoint", err)
	}
	if err = runSignalServiceCLI([]string{"signal-service-setup", "--spec", filepath.Join(dir, "spec.json")}, &out); err == nil {
		t.Fatal("missing local accepted")
	}
	if err = runSignalServiceCLI([]string{"signal-service-resume", "--local", "--root", s.Root, "--spec", "unused"}, &out); err == nil {
		t.Fatal("cross-command flag accepted")
	}
	if err = runSignalServiceCLI([]string{"__signal-service-serve", "--local", "--root", s.Root}, &out); err == nil {
		t.Fatal("missing operation accepted")
	}
	out.Reset()
	if err = runSignalServiceCLI([]string{"__signal-verification-service-probe"}, &out); err != nil {
		t.Fatal(err)
	}
	var cap managedCapabilities
	if err = json.Unmarshal(out.Bytes(), &cap); err != nil || cap.SignalService != 1 {
		t.Fatal(cap, err)
	}
}
