//go:build linux

package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupReviewMonitoringCheckIdentity(t *testing.T) {
	base := "https://checks.example/secret?a=1&b=2"
	for _, alias := range []string{base, "https://CHECKS.EXAMPLE.:443/secret/?b=2&a=1", "https://checks.example/%73ecret/fail?a=1&b=2", "https://checks.example/secret/start?a=1&b=2", "https://checks.example/secret/success?a=1&b=2"} {
		t.Run(alias, func(t *testing.T) {
			dir := t.TempDir()
			one, two := filepath.Join(dir, "one"), filepath.Join(dir, "two")
			testutil.Must(t, os.WriteFile(one, []byte(base), 0600))
			testutil.Must(t, os.WriteFile(two, []byte(alias), 0600))
			err := notification.ValidateMonitoringChecks(config.WorkerMonitoring{BackupPingURLFile: one, DailyDeliveryPingURLFile: two})
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("duplicate accepted or secret leaked: %v", err)
			}
			testutil.Must(t, os.WriteFile(two, []byte("https://checks.example/different"), 0600))
			testutil.Must(t, notification.ValidateMonitoringChecks(config.WorkerMonitoring{BackupPingURLFile: one, DailyDeliveryPingURLFile: two}))
		})
	}
}
