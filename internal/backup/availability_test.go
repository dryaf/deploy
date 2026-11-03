//go:build linux

package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupRegressionVerificationLeaseDoesNotReportMissing(t *testing.T) {
	a, s, c, r := healthyWorkerFixture(t)
	// verify holds this exclusive bundle lease across hashing, validation and
	// result persistence; the host probe runs every minute during that work.
	dir, lease, err := a.openBundleLease(r, true)
	testutil.Must(t, err)
	defer dir.Close()
	defer lease.Close()
	probeErr := a.refreshLatestAvailability(s)
	testutil.Must(t, probeErr)
	summary, healthy, err := evaluateWorkerHealth(c, s, time.Now().UTC())
	testutil.Must(t, err)
	if !healthy {
		t.Errorf("an intact snapshot under a verification lease produced a missing/unavailable protection failure: probe=%v summary=%s", probeErr, summary)
	}
}

func TestBackupAvailabilityDetectsLossDuringVerification(t *testing.T) {
	a, s, _, r := healthyWorkerFixture(t)
	dir, lease, err := a.openBundleLease(r, true)
	testutil.Must(t, err)
	defer dir.Close()
	defer lease.Close()
	testutil.Must(t, os.Remove(filepath.Join(a.dir.File().Name(), receiptPath(r), "database.db")))
	if err = a.refreshLatestAvailability(s); err == nil {
		t.Fatal("verification lease hid actual data loss")
	}
}
