//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	deployconfig "github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupRetirementReconcilesAlertsAndMixedBatches(t *testing.T) {
	for _, wholeSource := range []bool{false, true} {
		t.Run(fmt.Sprintf("whole-source=%v", wholeSource), func(t *testing.T) {
			a, s, c, r := healthyWorkerFixture(t)
			now := time.Now().UTC()
			key := instanceKey(r.Header.Attempt.SourceID, r.Header.Attempt.Service)
			testutil.Must(t, s.observeIncident("freshness", key, "missing", now.Add(-24*time.Hour), c.Policy.Notifications))
			testutil.Must(t, s.observeIncident("freshness", key, "missing", now, c.Policy.Notifications))
			testutil.Must(t, s.observeIncident("discovery", r.Header.Attempt.SourceID, "offline", now, c.Policy.Notifications))
			for i := 0; i < 8; i++ {
				testutil.Must(t, s.observeIncident("capture", fmt.Sprintf("unrelated-%d", i), "offline", now, c.Policy.Notifications))
			}
			testutil.Must(t, coalesceNotificationBacklog(s, now))
			config := filepath.Join(t.TempDir(), "runtime.json")
			body, err := json.Marshal(c)
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(config, body, 0600))
			args := []string{"retire", "--local", "--config", config, "--state-dir", s.directory, "--archive", a.dir.File().Name(), "--source", r.Header.Attempt.SourceID, "--reason", "decommissioned"}
			if !wholeSource {
				args = append(args, "--instance", r.Header.Attempt.Service)
			}
			dir, err := safefs.Open(s.directory)
			testutil.Must(t, err)
			busy, err := dir.Lock("notifier.lock", true, true)
			testutil.Must(t, err)
			if err = runWorkerOperation(args); err == nil || !strings.Contains(err.Error(), "busy") {
				t.Fatalf("retirement raced in-flight delivery: %v", err)
			}
			busy.Close()
			dir.Close()
			var before ArchiveControl
			testutil.Must(t, s.get("control", &before))
			if before.Instances[key].Retired {
				t.Fatal("busy retirement changed control")
			}
			testutil.Must(t, runWorkerOperation(args))
			testutil.Must(t, runWorkerOperation(args)) // Idempotent retry.
			testutil.Must(t, s.observeIncident("freshness", key, "late stale observer", now, c.Policy.Notifications))
			testutil.Must(t, s.resolveIncident("freshness", key, now, c.Policy.Notifications))
			var active, recoveries, dispositions int
			testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM incidents WHERE instance=? AND resolved IS NULL", key).Scan(&active))
			testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='recovery'").Scan(&recoveries))
			testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM meta WHERE key LIKE 'incident_resolution/%' AND json_extract(value,'$.disposition')='retired-by-operator'").Scan(&dispositions))
			want := 1
			if wholeSource {
				want = 2
			}
			if active != 0 || recoveries != 0 || dispositions != want {
				t.Fatalf("retirement: active=%d recoveries=%d dispositions=%d", active, recoveries, dispositions)
			}
			var sent []notification.NotificationEvent
			testutil.Must(t, drainNotificationOutbox(context.Background(), s, c, func(_ context.Context, _ string, _ deployconfig.WorkerNotifications, event notification.NotificationEvent) (notification.DeliveryAttempt, error) {
				sent = append(sent, event)
				return notification.DeliveryAttempt{Accepted: true}, nil
			}, now.Add(time.Second)))
			for _, event := range sent {
				if strings.Contains(event.Message, "missing") || strings.Contains(event.Message, "late stale observer") {
					t.Fatalf("obsolete alert delivered: %+v", event)
				}
			}
			var pendingUnrelated int
			testutil.Must(t, s.db.QueryRow(`SELECT count(*) FROM outbox o JOIN incidents i ON json_extract(o.record,'$.incident_id')=i.id WHERE i.instance LIKE 'unrelated-%' AND (o.accepted IS NOT NULL OR o.covered_by IN (SELECT id FROM outbox WHERE accepted IS NOT NULL))`).Scan(&pendingUnrelated))
			if pendingUnrelated != 8 {
				t.Fatalf("unrelated alerts lost: delivered=%d", pendingUnrelated)
			}
		})
	}
}

func TestBackupRetirementResumesAfterControlCommit(t *testing.T) {
	a, s, c, r := healthyWorkerFixture(t)
	now := time.Now().UTC()
	key := instanceKey(r.Header.Attempt.SourceID, r.Header.Attempt.Service)
	testutil.Must(t, s.observeIncident("coverage", key, "absent", now, c.Policy.Notifications))
	testutil.Must(t, a.updateControl(s, func(control *ArchiveControl) error {
		v := control.Instances[key]
		v.Retired, v.RetirementReason = true, "decommissioned"
		control.Instances[key] = v
		return nil
	})) // Simulate interruption before local incident cleanup.
	var sent []notification.NotificationEvent
	testutil.Must(t, drainNotificationOutbox(context.Background(), s, c, func(_ context.Context, _ string, _ deployconfig.WorkerNotifications, event notification.NotificationEvent) (notification.DeliveryAttempt, error) {
		sent = append(sent, event)
		return notification.DeliveryAttempt{Accepted: true}, nil
	}, now.Add(time.Second)))
	if len(sent) != 1 || sent[0].Kind != "retirement" {
		t.Fatalf("interrupted retirement delivered stale alerts: %+v", sent)
	}
}
