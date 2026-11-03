//go:build linux

package backup

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupTakeoverFencesOldStateAndPreservesPins(t *testing.T) {
	a, old, attempt, _, _ := archiveFixture(t)
	if err := a.updateControl(old, func(c *ArchiveControl) error {
		value := c.Instances[instanceKey(attempt.SourceID, attempt.Service)]
		value.LastSuccess = time.Now().UTC()
		c.Instances[instanceKey(attempt.SourceID, attempt.Service)] = value
		c.Pins["historical"] = "preserve"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	control, err := a.checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := openBackupState(filepath.Join(t.TempDir(), "recovered"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err = recovered.put("recovered_control", control); err != nil {
		t.Fatal(err)
	}
	if err = recovered.put("mode", "recovery"); err != nil {
		t.Fatal(err)
	}
	config := WorkerInstallation{WorkerID: testutil.ID(t, "w"), ArchiveID: a.identity.ID, ConfigHash: strings.Repeat("b", 64)}
	if err = a.claimRecoveredWriter(recovered, config); err != nil {
		t.Fatal(err)
	}
	if err = a.commitControl(old); err == nil {
		t.Fatal("old writer was not fenced")
	}
	next, err := a.checkpoint()
	if err != nil || next.Pins["historical"] != "preserve" || next.Epoch != control.Epoch+1 {
		t.Fatal(next, err)
	}
	if !next.Instances[instanceKey(attempt.SourceID, attempt.Service)].LastSuccess.IsZero() {
		t.Fatal("historical success became fresh on takeover")
	}
	if err = a.claimRecoveredWriter(recovered, config); err != nil {
		t.Fatal("lost-ack takeover was not idempotent", err)
	}
}
