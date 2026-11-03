//go:build linux

package backup

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func TestBackupWorkerUnitsUseRealGeneratorAndSeparateCredentials(t *testing.T) {
	policy := configfixture.WorkerPolicy(t)
	paths := WorkerUnitPaths{Home: "/home/backup", Config: "/home/backup/.config/deploy/backup-worker/runtime.json", State: "/home/backup/.local/state/deploy/backup-worker/state", Archive: policy.Storage.Path, Keys: "/home/backup/.config/deploy/backup-worker/ssh", RelayState: "/home/backup/.local/share/deploy/backup-worker/signal", RelaySocket: "/home/backup/.local/state/deploy/backup-worker/relay"}
	image := "example.test/deploy@sha256:" + strings.Repeat("a", 64)
	units, err := renderWorkerUnits(WorkerInstallation{Policy: policy}, paths, image, image)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(units["deploy-backup.container"], "/secrets/") || strings.Contains(units["deploy-backup-notifier.container"], "/keys:") || strings.Contains(units["deploy-backup-notifier.container"], ":/archive") {
		t.Fatal("credential or archive access crossed component boundary")
	}
	if strings.Contains(units["deploy-backup-signal.container"], "[Install]") {
		t.Fatal("staged Signal relay would start automatically after reboot")
	}
	if !strings.Contains(units["deploy-backup-signal.container"], "Tmpfs=/tmp:rw,size=256m,nosuid,nodev") {
		t.Fatal("Signal native-library temporary space is insufficient")
	}
	for _, name := range []string{"deploy-backup-notifier.container", "deploy-backup-test-signal.container"} {
		if !strings.Contains(units[name], "Wants=deploy-backup-signal.service\nAfter=deploy-backup-signal.service") {
			t.Fatalf("%s does not start its relay dependency", name)
		}
	}
	for _, name := range workerActivationUnits {
		if strings.HasSuffix(name, ".service") {
			t.Fatal("generated Quadlet service cannot be enabled with systemctl")
		}
	}
	generator := "/usr/lib/systemd/system-generators/podman-system-generator"
	if _, err = os.Stat(generator); err != nil {
		t.Skip("installed generator unavailable")
	}
	directory := t.TempDir()
	for name, unit := range units {
		if strings.HasSuffix(name, ".container") {
			if err = os.WriteFile(filepath.Join(directory, name), []byte(unit), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cmd := exec.Command(generator, "--user", "--dryrun")
	cmd.Env = append(os.Environ(), "QUADLET_UNIT_DIRS="+directory)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(output))
	}
	for name := range units {
		if strings.HasSuffix(name, ".container") && !strings.Contains(string(output), "---"+strings.TrimSuffix(name, ".container")+".service---") {
			t.Fatalf("generator omitted %s: %s", name, output)
		}
	}
	var roundTrip WorkerInstallation
	data, _ := json.Marshal(WorkerInstallation{Policy: containerNotificationPolicy(policy)})
	if err = json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if policy.Notifications.Signal.AccountFile == roundTrip.Policy.Notifications.Signal.AccountFile {
		t.Fatal("host and container credential paths not separated")
	}
}
