//go:build linux

package backup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	deploysource "github.com/dryaf/deploy/internal/backup/source"
	notifyinstall "github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/ui"
)

func writeBackupLEDCLIInstallation(t *testing.T, c WorkerInstallation) string {
	t.Helper()
	c.Sources = nil
	for _, alias := range c.Policy.Sources {
		id, err := safefs.NewID("s")
		testutil.Must(t, err)
		c.Sources = append(c.Sources, deploysource.WorkerSource{Alias: alias, SourceID: id, Host: "localhost", User: "backup", Port: 22, KeyFile: "/private/unused-key", KnownHosts: "/private/unused-known-hosts"})
	}
	testutil.Must(t, validateWorkerInstallation(c))
	dir := t.TempDir()
	testutil.Must(t, os.Chmod(dir, 0700))
	path := filepath.Join(dir, "backup.json")
	raw, err := json.Marshal(c)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(path, raw, 0600))
	return path
}

func writeBackupLEDCLIRuntime(t *testing.T, c worker.Config, socket string) (string, worker.Config) {
	t.Helper()
	dir := t.TempDir()
	testutil.Must(t, os.Chmod(dir, 0700))
	path := filepath.Join(dir, "notification.json")
	runtime := notifyinstall.Runtime{Version: 1, Worker: c, LEDSocket: socket, Providers: map[string]provider.Profile{"telegram": {Kind: "telegram", TokenFile: "/private/unused-token", ChatFile: "/private/unused-chat", Destination: notify.Hash("offline-destination")}}}
	raw, err := json.Marshal(runtime)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(path, raw, 0600))
	resolved, err := notifyinstall.ReadRuntime(path)
	testutil.Must(t, err)
	return path, resolved.Worker
}

func backupLEDCLIArguments(op string, o backupLEDRecoveryOptions) []string {
	args := []string{op, "--local", "--config", o.Config, "--state-dir", o.State, "--notification-config", o.NotificationConfig, "--notification-state", o.NotificationState, "--inventory", o.Inventory, "--inventory-hash", o.InventoryHash}
	if op == "recovery-led-clear" {
		args = append(args, "--binding", o.Binding, "--fault", o.Fault, "--evidence-hash", o.EvidenceHash, "--epoch-hash", o.EpochHash)
	}
	return args
}

func TestBackupLEDRecoveryCLIFlagsAndDryRun(t *testing.T) {
	f, o := prepareBackupLEDCLI(t)
	o.Config = writeBackupLEDCLIInstallation(t, f.config)
	o.NotificationConfig, _ = writeBackupLEDCLIRuntime(t, f.gatewayConfig, "/private/staged-led.sock")
	o.State = filepath.Join(t.TempDir(), "absent-backup")
	o.NotificationState = filepath.Join(t.TempDir(), "absent-notification")
	prior := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = prior }()
	for _, op := range []string{"recovery-led-evidence", "recovery-led-clear"} {
		args := backupLEDCLIArguments(op, o)
		var out bytes.Buffer
		testutil.Must(t, runBackupLEDRecoveryCLI(args, &out))
		if !bytes.Contains(out.Bytes(), []byte("no state opened")) {
			t.Fatal(out.String())
		}
		testutil.Must(t, RunCLI(args))
		bad := [][]string{{"--local=false"}, {"--inventory-hash", ""}, {"--notification-state", o.State}, {"--config", "relative"}, {"--refresh"}, {"some-worker"}}
		if op == "recovery-led-evidence" {
			bad = append(bad, []string{"--fault", ""})
		} else {
			bad = append(bad, []string{"--epoch-hash", ""}, []string{"--binding", "$worker"}, []string{"--evidence-hash", ""}, []string{"--fault", "deploy.f." + string(bytes.Repeat([]byte("z"), 48))})
		}
		for _, extra := range bad {
			if err := runBackupLEDRecoveryCLI(append(append([]string{}, args...), extra...), &out); err == nil {
				t.Fatal("invalid CLI scope", extra)
			}
		}
	}
	for _, path := range []string{o.State, o.NotificationState} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("dry-run created state", path, err)
		}
	}
}
