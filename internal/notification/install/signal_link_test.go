package install

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func TestSignalLinkCLIValidationDryRunAndStatus(t *testing.T) {
	old := ui.DryRun
	t.Cleanup(func() { ui.DryRun = old })
	ui.DryRun = true
	state := filepath.Join(t.TempDir(), "uncreated-account")
	id, _ := safefs.NewID("n")
	var out bytes.Buffer
	args := []string{"__signal-link", "--local", "--state-dir", state, "--operation", id, "--name", "device"}
	if err := runSignalLinkCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("dry-run touched state", err)
	}
	for _, bad := range [][]string{
		{"__signal-link", "--local", "--state-dir", state, "--name", "device"},
		{"signal-link-resolve", "--local", "--state-dir", state, "--operation", id, "--reason", "reviewed"},
		{"signal-link-status", "--local", "--state-dir", state, "--operation", id},
		{"signal-link-status", "--state-dir", state},
	} {
		if err := runSignalLinkCLI(bad, &out); err == nil {
			t.Fatal("invalid options accepted", bad)
		}
	}
	ui.DryRun = false
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runSignalLinkCLI([]string{"signal-link-status", "--local", "--state-dir", state}, &out); err != nil {
		t.Fatal(err)
	}
	var status signalaccount.Status
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || !status.RuntimeAllowed || status.State != "none" {
		t.Fatal(status, err)
	}
}

func TestSignalServiceStatusDoesNotRequireStoppedRelay(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	lease, err := signalaccount.AcquireRuntime(state)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var out bytes.Buffer
	if err = runSignalLinkCLI([]string{"signal-service-status", "--local", "--state-dir", state}, &out); err != nil {
		t.Fatal(err)
	}
	var status signalaccount.MaintenanceStatus
	if err = json.Unmarshal(out.Bytes(), &status); err != nil || !status.RuntimeAllowed || status.Phase != "none" {
		t.Fatal(status, err)
	}
	if err = runSignalLinkCLI([]string{"signal-service-status", "--local", "--state-dir", state, "--name", "device"}, &out); err == nil {
		t.Fatal("unused name accepted")
	}
}
