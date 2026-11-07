package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
)

func TestBackupPlanDetectsInventoryEndpointChangesWithoutExposingValues(t *testing.T) {
	inv, err := config.ParseInventory("server.yaml", []byte(backupOnlyInventory))
	if err != nil {
		t.Fatal(err)
	}
	desired := *inv.Servers["manager"].Desired
	before := backup.SetupSourceTarget{Alias: "source", Host: "private-old.invalid", User: "olduser", Port: 2200}
	r := ControlRequest{Worker: "backup-worker", Action: "plan", Desired: desired, Facts: testFacts(), SourceTargets: []backup.SetupSourceTarget{before}}
	inspect := func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
		return backup.SetupObservation{Stage: "complete", SourceTargets: []backup.SetupSourceTarget{before}}, &desired, nil
	}
	v, err := planBackupControl(context.Background(), r, inspect)
	if err != nil || v.State != "up_to_date" || len(v.Changes) != 0 {
		t.Fatal(v, err)
	}
	r.SourceTargets[0].Host, r.SourceTargets[0].User, r.SourceTargets[0].Port = "private-new.invalid", "newuser", 2201
	v, err = planBackupControl(context.Background(), r, inspect)
	if err != nil || v.Code != "source_migration_required" || v.State != "needs_action" || !reflect.DeepEqual(v.Changes, []Change{{"servers.source.host", "producer_enrollment"}, {"servers.source.ssh_port", "producer_enrollment"}, {"servers.source.user", "producer_enrollment"}}) {
		t.Fatal(v, err)
	}
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{before.Host, before.User, r.SourceTargets[0].Host, r.SourceTargets[0].User, "2200", "2201"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatal("endpoint value escaped plan output")
		}
	}
	observation, _, _ := inspect(context.Background(), "")
	body, err = json.Marshal(observation)
	if err != nil || strings.Contains(string(body), before.Host) || strings.Contains(string(body), "source_targets") {
		t.Fatal("private planning endpoints escaped status", err)
	}
}

func TestSourcePlanTargetsAreReadOnlyAndComplete(t *testing.T) {
	r := ControlRequest{Worker: "backup-worker", Action: "plan", Desired: config.DesiredWorkers{Backup: &config.BackupDeclaration{Sources: []string{"source"}}}, SourceTargets: []backup.SetupSourceTarget{{Alias: "source", Host: "host.invalid", User: "app", Port: 22}}}
	if err := validateSourceTargets(r); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"plan", "status", "run", "activate", "qualify-begin", "qualify-observe", "qualify-commit"} {
		check := r
		check.Action = action
		if err := validateSourceTargets(check); err != nil {
			t.Fatal(action, err)
		}
	}
	for _, modify := range []func(*ControlRequest){
		func(r *ControlRequest) { r.Action = "apply" },
		func(r *ControlRequest) { r.Action = "uninstall" },
		func(r *ControlRequest) { r.Worker = "notification-worker" },
		func(r *ControlRequest) { r.SourceTargets[0].Alias = "other" },
		func(r *ControlRequest) { r.SourceTargets[0].Host = "-flag" },
		func(r *ControlRequest) { r.SourceTargets[0].User = "root" },
		func(r *ControlRequest) { r.SourceTargets[0].Port = 0 },
		func(r *ControlRequest) { r.SourceTargets = append(r.SourceTargets, r.SourceTargets[0]) },
	} {
		next := r
		next.SourceTargets = append([]backup.SetupSourceTarget(nil), r.SourceTargets...)
		modify(&next)
		if err := validateSourceTargets(next); err == nil {
			t.Fatal("invalid source planning authority accepted", next)
		}
	}
	if _, err := sourcePlanChanges(r.SourceTargets, nil); err == nil {
		t.Fatal("missing installed endpoints reported unchanged")
	}
	if _, err := sourcePlanChanges(r.SourceTargets, []backup.SetupSourceTarget{r.SourceTargets[0], r.SourceTargets[0]}); err == nil {
		t.Fatal("ambiguous retained source endpoints accepted")
	}
	duplicate := r
	duplicate.Desired.Backup = &config.BackupDeclaration{Sources: []string{"source", "source"}}
	duplicate.SourceTargets = []backup.SetupSourceTarget{r.SourceTargets[0], {Alias: "extra", Host: "extra.invalid", User: "app", Port: 22}}
	if validateSourceTargets(duplicate) == nil {
		t.Fatal("duplicate desired aliases accepted as a complete endpoint set")
	}
	// Added aliases still belong to the ordinary source-list enrollment diff.
	next := append([]backup.SetupSourceTarget(nil), r.SourceTargets...)
	next = append(next, backup.SetupSourceTarget{Alias: "extra", Host: "extra.invalid", User: "app", Port: 22})
	changes, err := sourcePlanChanges(next, r.SourceTargets)
	if err != nil || len(changes) != 0 {
		t.Fatal("new source classified as endpoint replacement", changes, err)
	}
}
