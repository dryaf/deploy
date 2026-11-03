//go:build notificationintegration

package worker

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

func TestLEDFaultClearPinnedDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	api := led.Client{Socket: ledfixture.Start(t), Budget: time.Second}
	s, now, _ := fixture(t)
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	must(t, s.Replace(ctx, v))
	r := &LEDReconciler{Store: s, API: api, Budget: time.Second}
	must(t, r.Step(ctx))
	var fault string
	must(t, s.db.QueryRow("SELECT id FROM led_states WHERE condition='critical'").Scan(&fault))
	state := led.State{ID: fault, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true, Message: "changed registry entry"}
	must(t, api.Set(ctx, state))
	must(t, s.Replace(ctx, snapshot(*now, 2, 2)))
	if err := r.Step(ctx); err == nil {
		t.Fatal("runtime cleared mismatched daemon state")
	}
	var pending int
	must(t, s.db.QueryRow("SELECT obsolete FROM led_states WHERE id=?", fault).Scan(&pending))
	if pending != 1 {
		t.Fatal("registry refusal lost cleanup obligation")
	}
	entries, err := api.List(ctx)
	must(t, err)
	found := false
	_, healthy := r.ids()
	for _, entry := range entries {
		if entry.ID == fault {
			found = true
			if entry.Message != state.Message {
				t.Fatal("registry conflict was overwritten")
			}
		}
		if entry.ID == healthy {
			t.Fatal("registry conflict retained healthy lease")
		}
	}
	if !found {
		t.Fatal("conflicting fault removed from real registry")
	}
	state.Message = "Notification state: critical"
	must(t, api.Set(ctx, state))
	must(t, r.Step(ctx))
	if err = s.db.QueryRow("SELECT obsolete FROM led_states WHERE id=?", fault).Scan(&pending); err != sql.ErrNoRows {
		t.Fatal("acknowledged cleanup not retired", err)
	}
	entries, err = api.List(ctx)
	must(t, err)
	found = false
	for _, entry := range entries {
		if entry.ID == fault {
			t.Fatal("fault clear did not reach daemon")
		}
		found = found || entry.ID == healthy
	}
	if !found {
		t.Fatal("fresh resolution did not renew bounded healthy lease")
	}
}
