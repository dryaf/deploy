//go:build linux

package backup

import (
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func TestBackupScheduleAcrossDST(t *testing.T) {
	policy := configfixture.WorkerPolicy(t)
	policy.Schedule = "daily"
	zone, _ := time.LoadLocation("Europe/Vienna")
	now := time.Date(2026, 3, 29, 0, 0, 1, 0, zone)
	next, err := nextBackupOccurrence(policy, now)
	if err != nil || next.Sub(now) != 23*time.Hour-time.Second {
		t.Fatal(next, err)
	}
	policy.Schedule = "hourly"
	next, err = nextBackupOccurrence(policy, now)
	if err != nil || next.Sub(now) > time.Hour {
		t.Fatal(next, err)
	}
}
