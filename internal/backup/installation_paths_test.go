package backup

import (
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
)

func TestBackupInstallationPathsResolveAndRenderTargetOverrides(t *testing.T) {
	desired := config.WorkerPaths{Config: "~/worker config", Runtime: "~/inputs/worker.json", State: "/srv/backup/state", Data: "/srv/backup/data", Installation: "~/worker artifacts", UnitDirectory: "~/custom units", DeniedPaths: []string{"~/private $notes", "~/private $notes"}}
	// Shell expressions in policy remain invalid even though emitted unit values
	// also have their own quoting rules.
	if _, err := ResolveInstallationPaths("/home/pi", "/home/pi/cfg", "/home/pi/state", "/home/pi/data", desired); err == nil {
		t.Fatal("accepted shell expression in YAML path")
	}
	desired.DeniedPaths = []string{"~/private notes", "~/private notes"}
	p, err := ResolveInstallationPaths("/home/pi", "/home/pi/cfg", "/home/pi/state", "/home/pi/data", desired)
	if err != nil {
		t.Fatal(err)
	}
	if p.Config != "/home/pi/worker config" || p.Runtime != "/home/pi/inputs/worker.json" || p.State != "/srv/backup/state" || p.QuadletDirectory != "/home/pi/cfg/containers/systemd" || len(p.DeniedPaths) != 1 {
		t.Fatal("wrong target layout", p)
	}
	if err = p.MatchesDesired(desired); err != nil {
		t.Fatal(err)
	}
	paths, err := p.UnitPaths("/srv/backup/data/archive")
	if err != nil {
		t.Fatal(err)
	}
	policy := config.BackupWorkerConfig{Storage: config.WorkerStorage{Mode: "home", Path: paths.Archive}, Schedule: "hourly", Timezone: "UTC", Notifications: config.WorkerNotifications{Mode: "disabled"}}
	units, err := renderWorkerUnits(WorkerInstallation{Policy: policy}, paths, "example.test/backup@sha256:"+strings.Repeat("a", 64), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(units["deploy-backup.container"], `"/home/pi/worker config/container.json:/config/runtime.json:ro"`) {
		t.Fatal("container config incorrectly follows runtime override", units["deploy-backup.container"])
	}
	for _, name := range []string{"deploy-backup-health.service", "deploy-backup-maintenance.service"} {
		body := units[name]
		for _, want := range []string{`"/home/pi/worker artifacts/deploy"`, `--config "/home/pi/inputs/worker.json"`, `--state-dir "/srv/backup/state"`, `InaccessiblePaths="-/home/pi/private notes"`} {
			if !strings.Contains(body, want) {
				t.Fatal("native unit lost resolved path", name, want, body)
			}
		}
	}
	reports, err := renderBackupReportUnits(paths, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reports["deploy-backup-report.service"], `"/home/pi/worker artifacts/deploy"`) || !strings.Contains(reports["deploy-backup-report.service"], `InaccessiblePaths="-/home/pi/private notes"`) {
		t.Fatal("report renderer lost layout")
	}
	if p.OwnedPath(p.Runtime) != "inputs/worker.json" || p.OwnedPath(p.State) != p.State {
		t.Fatal("ownership paths retargeted")
	}
}

func TestBackupInstallationPathsPreserveLegacyDefaultsAndRejectOverlap(t *testing.T) {
	p, err := ResolveInstallationPaths("/home/pi", "", "", "", config.WorkerPaths{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Runtime != "/home/pi/.config/deploy/backup-worker/runtime.json" || p.State != "/home/pi/.local/state/deploy/backup-worker/state" || p.Helper() != "/home/pi/.local/lib/deploy/backup-worker/deploy" {
		t.Fatal("legacy layout changed", p)
	}
	for _, override := range []config.WorkerPaths{
		{Config: "~/"}, {State: "~/.local/state/deploy/backup-worker"}, {State: "~/.local/state/deploy/backup-worker/history"},
		{Runtime: "~/.config/deploy/backup-worker/container.json"}, {Runtime: "~/.config/deploy/backup-worker/ssh/key"},
		{Installation: "~/.local/state/deploy/backup-worker"}, {Config: "~/.local/share/deploy/backup-worker"},
		{DeniedPaths: []string{"~/.config"}}, {UnitDirectory: "~/.config/deploy/backup-worker/units"},
	} {
		if _, err := ResolveInstallationPaths("/home/pi", "", "", "", override); err == nil {
			t.Fatal("unsafe layout", override)
		}
	}
	for _, archive := range []string{p.State, p.Config, p.Installation, p.Home, p.Keys(), p.RelaySocket()} {
		if _, err = p.UnitPaths(archive); err == nil {
			t.Fatal("archive shadows runtime", archive)
		}
	}
	p.State = "/other/state"
	if err := p.MatchesDesired(config.WorkerPaths{}); err == nil {
		t.Fatal("retargeted declared layout accepted")
	}
}
