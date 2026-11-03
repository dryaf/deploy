//go:build linux

package configfixture

import (
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil"
)

func WorkerPolicy(t *testing.T) config.BackupWorkerConfig {
	t.Helper()
	file := testutil.WriteFixture(t, t.TempDir(), "server.yaml", testutil.ServerInventory)
	inventory, err := config.ReadInventory(file)
	if err != nil {
		t.Fatal(err)
	}
	return *inventory.Servers["home-backup"].BackupWorker
}
