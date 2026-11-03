//go:build linux

package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func TestSharedReportUnitsVerifyWithSystemd(t *testing.T) {
	analyzer, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd analyzer unavailable")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home %worker")
	paths := WorkerUnitPaths{Home: home, Config: filepath.Join(home, "runtime.json"), State: filepath.Join(home, "state with spaces"), Keys: filepath.Join(home, "ssh"), RelayState: filepath.Join(home, "signal")}
	helper := filepath.Join(home, ".local/lib/deploy/backup-worker/deploy")
	testutil.Must(t, os.MkdirAll(filepath.Dir(helper), 0700))
	testutil.Must(t, os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0700))
	policy := configfixture.WorkerPolicy(t)
	extra := filepath.Join(home, "gateway-$token")
	units, err := renderBackupReportUnits(paths, policy, []string{extra})
	testutil.Must(t, err)
	service := units["deploy-backup-report.service"]
	if !strings.Contains(service, "backup-worker __report") || !strings.Contains(service, reportUnitPathQuote("-"+extra)) || strings.Contains(service, "__notify") || len(units) != 3 {
		t.Fatal(service)
	}
	image := "example.test/deploy@sha256:" + strings.Repeat("a", 64)
	legacy, err := renderWorkerUnits(WorkerInstallation{Policy: policy}, paths, image, image)
	testutil.Must(t, err)
	units["deploy-backup-notifier.path"] = legacy["deploy-backup-notifier.path"]
	units["deploy-backup-notifier.service"] = "[Service]\nType=oneshot\nExecStart=" + unitQuote(helper) + "\n"
	var files []string
	for name, body := range units {
		path := filepath.Join(root, name)
		testutil.Must(t, os.WriteFile(path, []byte(body), 0600))
		files = append(files, path)
	}
	cmd := exec.Command(analyzer, append([]string{"--user", "verify", "--man=no"}, files...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("systemd rejected report units: %v\n%s", err, output)
	}
	if _, err = renderBackupReportUnits(paths, policy, []string{home}); err == nil {
		t.Fatal("credential denial hid required runtime paths")
	}
}

func TestSharedReportHealthInspectsSelectedUnitsAndAPIFreshness(t *testing.T) {
	f, _, deps, _ := setupReportFixture(t)
	testutil.Must(t, runSharedBackupReportWith(context.Background(), f.config, f.state, deps))
	bin := t.TempDir()
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	script := "#!/bin/sh\nprintf 'LoadState=loaded\nActiveState=active\nResult=success\nUnitFileState=enabled\nNextElapseUSecRealtime=tomorrow\n'\n"
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700))
	archive := filepath.Join(filepath.Dir(f.state.directory), "archive")
	health := collectWorkerHealth(f.config, archive, f.state)
	if !health.DeliveryHealthy {
		t.Fatal(health.Diagnostics)
	}
	if _, ok := health.Units["deploy-backup-report.timer"]; !ok {
		t.Fatal("report timer omitted")
	}
	if _, ok := health.Units["deploy-backup-notifier.timer"]; ok {
		t.Fatal("legacy notifier required after shared handoff")
	}
	var report backupReportHealth
	testutil.Must(t, f.state.get("shared_report_health", &report))
	report.At = report.At.Add(-3 * time.Minute)
	testutil.Must(t, f.state.put("shared_report_health", report))
	health = collectWorkerHealth(f.config, archive, f.state)
	if health.DeliveryHealthy {
		t.Fatal("stale gateway evidence reported healthy")
	}
}
