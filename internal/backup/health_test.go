//go:build linux

package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupHealthIncludesMaintenanceUnits(t *testing.T) {
	for _, scenario := range []struct {
		name, unit, properties string
		healthy                bool
	}{
		{"idle-service", "service", "LoadState=loaded\nActiveState=inactive\nResult=success\nUnitFileState=static\n", true},
		{"running-service", "service", "LoadState=loaded\nActiveState=activating\nResult=success\nUnitFileState=static\n", true},
		{"failed-service", "service", "LoadState=loaded\nActiveState=failed\nResult=exit-code\nUnitFileState=static\n", false},
		{"missing-service", "service", "LoadState=not-found\nActiveState=inactive\n", false},
		{"disabled-timer", "timer", "LoadState=loaded\nActiveState=active\nUnitFileState=disabled\nNextElapseUSecRealtime=tomorrow\n", false},
		{"stopped-timer", "timer", "LoadState=loaded\nActiveState=inactive\nUnitFileState=enabled\nNextElapseUSecRealtime=tomorrow\n", false},
		{"missing-timer", "timer", "LoadState=not-found\nActiveState=inactive\n", false},
		{"unarmed-timer", "timer", "LoadState=loaded\nActiveState=active\nUnitFileState=enabled\nNextElapseUSecRealtime=n/a\n", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			a, state, c, _ := healthyWorkerFixture(t)
			bin := t.TempDir()
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			t.Setenv("DEPLOY_TEST_UNIT", "deploy-backup-maintenance."+scenario.unit)
			t.Setenv("DEPLOY_TEST_PROPERTIES", scenario.properties)
			script := `#!/bin/sh
for unit in "$@"; do :; done
if [ "$unit" = "$DEPLOY_TEST_UNIT" ]; then
    printf '%s' "$DEPLOY_TEST_PROPERTIES"
else
    printf 'LoadState=loaded\nActiveState=active\nResult=success\nUnitFileState=enabled\nNextElapseUSecRealtime=tomorrow\n'
fi
`
			testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700))
			c.Policy.Storage.RequireMount = "/proc"
			c.Policy.Storage.MinFreeBytes = 1
			probe := collectWorkerHealth(c, a.dir.File().Name(), state)
			if probe.Healthy != scenario.healthy || probe.BackupHealthy != scenario.healthy || !probe.DeliveryHealthy {
				t.Fatalf("incorrect maintenance health: %+v", probe)
			}
			for _, suffix := range []string{"timer", "service"} {
				if _, ok := probe.Units["deploy-backup-maintenance."+suffix]; !ok {
					t.Fatalf("maintenance %s not inspected", suffix)
				}
			}
			if !scenario.healthy && !strings.Contains(strings.Join(probe.Diagnostics, "\n"), "deploy-backup-maintenance."+scenario.unit) {
				t.Fatalf("maintenance failure omitted from diagnostics: %+v", probe)
			}
		})
	}
}
