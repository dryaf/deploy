package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func planTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	testutil.Must(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files[path] = info.Mode().String()
		if info.Mode().IsRegular() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] += inputDigest(body)
		}
		return nil
	}))
	return files
}

func TestNotificationPlanUsesRetainedScopeAndNeverWrites(t *testing.T) {
	r, paths := bootstrapControlFixture(t)
	r.DesiredScope = 1
	r.DesiredHash, _ = r.Desired.NotificationFingerprint()
	r.Action, r.Inputs = "plan", nil
	d, err := ensureConfigDirectory(paths.Control)
	testutil.Must(t, err)
	defer d.Close()
	testutil.Must(t, retainDeclaration(d, r.Desired, r.DesiredHash, 1))
	lock, err := d.Lock("operation.lock", true, true)
	testutil.Must(t, err)
	lock.Close()
	m := install.ManagedManifest{Mode: "verification", Identity: protocol.Identity{Worker: "gateway", Recovery: "initial"}}
	j := OperationJournal{Version: 1, ID: testutil.ID(t, "n"), Complete: true,
		Pins:     OperationPins{Worker: r.Worker, Action: "apply", DesiredScope: 1, DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Artifacts: map[string]string{"helper": r.HelperSHA256}, Installations: map[string]string{"notification-worker": paths.Installation, "state": paths.State, "runtime": paths.Runtime}},
		Children: []ChildReference{{Name: "notification-bootstrap", Operation: testutil.ID(t, "n"), Complete: true, Receipt: protocol.Hash(m)}}}
	testutil.Must(t, d.WriteJSON("operation.json", j, false))
	inspect := func(context.Context, string) (install.InstallationObservation, error) {
		return install.InstallationObservation{Manifest: m, Phase: "complete", Unbound: true}, nil
	}
	before := planTree(t, r.Facts.Home)
	r.Desired.Backup.Retention = &config.RetentionConfig{KeepLast: 9}
	result, err := planNotificationControl(context.Background(), r, paths, inspect)
	testutil.Must(t, err)
	if result.State != "up_to_date" || len(result.Changes) != 0 {
		t.Fatal("backup-only change invalidated gateway plan", result)
	}
	p := r.Desired.Notification.Providers["telegram"]
	p.TokenInput = "/operator/private/new-token"
	r.Desired.Notification.Providers["telegram"] = p
	result, err = planNotificationControl(context.Background(), r, paths, inspect)
	testutil.Must(t, err)
	if result.Code != "policy_changed" || !reflect.DeepEqual(result.Changes, []Change{{"notification_worker.providers.telegram.token_input", "provider_review"}}) {
		t.Fatal("missing concrete migration diff", result)
	}
	raw, err := json.Marshal(result)
	testutil.Must(t, err)
	if bytes.Contains(raw, []byte(p.TokenInput)) || bytes.Contains(raw, []byte("fixture-secret")) {
		t.Fatal("plan leaked input values", string(raw))
	}
	if !reflect.DeepEqual(before, planTree(t, r.Facts.Home)) {
		t.Fatal("read-only plan changed retained files")
	}
	m.Revision = "unexpected-drift"
	if _, err = planNotificationControl(context.Background(), r, paths, inspect); err == nil {
		t.Fatal("manual installation drift accepted for planning")
	}
	m.Revision = ""
	testutil.Must(t, os.Remove(filepath.Join(paths.Control, "declarations", j.Pins.DesiredHash+".json")))
	before = planTree(t, r.Facts.Home)
	if _, err = planNotificationControl(context.Background(), r, paths, inspect); err == nil {
		t.Fatal("missing snapshot silently reconstructed from desired YAML")
	}
	if !reflect.DeepEqual(before, planTree(t, r.Facts.Home)) {
		t.Fatal("missing policy was recreated")
	}
}

func TestBackupPlanClassifiesDisabledAndRetainedPolicy(t *testing.T) {
	inv, err := config.ParseInventory("server.yaml", []byte(backupOnlyInventory))
	testutil.Must(t, err)
	before := *inv.Servers["manager"].Desired
	body, err := json.Marshal(before)
	testutil.Must(t, err)
	var after config.DesiredWorkers
	testutil.Must(t, json.Unmarshal(body, &after))
	after.Backup.Storage.Path = "/new/archive"
	after.Backup.Monitoring.BackupPingURLFile = "/private/monitor"
	after.Backup.Notifications.RepeatAfter = "8h"
	disabled := false
	after.Backup.Enabled = &disabled
	r := ControlRequest{Desired: after, Facts: testFacts()}
	inspect := func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
		return backup.SetupObservation{Stage: "complete"}, &before, nil
	}
	result, err := planBackupControl(context.Background(), r, inspect)
	testutil.Must(t, err)
	want := []Change{{"backup_worker.enabled", "activity_transition"}, {"backup_worker.monitoring.backup_ping_url_file", "monitor_review"}, {"backup_worker.notifications.repeat_after", "backup_update"}, {"backup_worker.storage.path", "path_migration"}}
	if !reflect.DeepEqual(result.Changes, want) {
		t.Fatal(result.Changes)
	}
	result, err = planBackupControl(context.Background(), r, func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
		return backup.SetupObservation{Stage: "complete"}, nil, nil
	})
	testutil.Must(t, err)
	if result.Code != "policy_snapshot_unavailable" {
		t.Fatal("legacy snapshot inferred", result)
	}
}

func TestDisablingBackupPlanDoesNotClaimGatewayRemoval(t *testing.T) {
	inv, err := config.ParseInventory("server.yaml", []byte(cliInventory))
	testutil.Must(t, err)
	before := *inv.Servers["manager"].Desired
	after := before
	b := *before.Backup
	after.Backup = &b
	disabled := false
	b.Enabled = &disabled
	r := ControlRequest{Desired: after, Facts: testFacts()}
	result, err := planBackupControl(context.Background(), r, func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
		return backup.SetupObservation{Stage: "complete"}, &before, nil
	})
	testutil.Must(t, err)
	if !reflect.DeepEqual(result.Changes, []Change{{"backup_worker.enabled", "activity_transition"}}) {
		t.Fatal("backup scoping advertised unrelated gateway removal", result.Changes)
	}
}

func TestBackupPlanExplainsCredentialReferenceMovesWithoutReadingSecrets(t *testing.T) {
	for _, which := range []string{"token", "chat", "both", "mixed"} {
		t.Run(which, func(t *testing.T) {
			inv, err := config.ParseInventory("server.yaml", []byte(backupOnlyInventory))
			testutil.Must(t, err)
			before := *inv.Servers["manager"].Desired
			before.Backup.Notifications.Telegram = &config.WorkerTelegram{TokenFile: "/private/old-token", ChatIDFile: "/private/old-chat"}
			after := before
			b := *before.Backup
			after.Backup = &b
			profile := *b.Notifications.Telegram
			b.Notifications.Telegram = &profile
			if which != "chat" {
				profile.TokenFile = "/private/new-token"
			}
			if which != "token" {
				profile.ChatIDFile = "/private/new-chat"
			}
			if which == "mixed" {
				b.Schedule = "daily"
			}
			result, err := planBackupControl(context.Background(), ControlRequest{Desired: after, Facts: testFacts()}, func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
				return backup.SetupObservation{Stage: "complete"}, &before, nil
			})
			testutil.Must(t, err)
			if strings.Contains(result.Next, "all old and new files") != (which != "mixed") {
				t.Fatal("reference-only guidance did not match changes", result)
			}
			raw, err := json.Marshal(result)
			testutil.Must(t, err)
			if bytes.Contains(raw, []byte("/private/")) {
				t.Fatal("reference paths leaked from plan")
			}
		})
	}
}

func TestBackupPlanExplainsSourceExpansionAndPendingAuthorization(t *testing.T) {
	inv, err := config.ParseInventory("server.yaml", []byte(backupOnlyInventory))
	testutil.Must(t, err)
	before := *inv.Servers["manager"].Desired
	after := before
	b := *before.Backup
	after.Backup = &b
	b.Sources = append(append([]string(nil), b.Sources...), "extra")
	r := ControlRequest{Desired: after, Facts: testFacts()}
	stage := "complete"
	inspect := func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
		return backup.SetupObservation{Stage: stage}, &before, nil
	}
	result, err := planBackupControl(context.Background(), r, inspect)
	testutil.Must(t, err)
	if result.Code != "policy_changed" || !strings.Contains(result.Next, "enrolls added sources") || !reflect.DeepEqual(result.Changes, []Change{{"backup_worker.sources", "producer_enrollment"}}) {
		t.Fatal("missing source-addition workflow", result)
	}
	stage = "source_enrollment_pending"
	result, err = planBackupControl(context.Background(), r, inspect)
	testutil.Must(t, err)
	if result.State != "resumable" || result.Code != stage || !strings.Contains(result.Next, "exact source-addition declaration") {
		t.Fatal("pending authorization hidden", result)
	}
	stage = "complete"
	b.Sources = []string{"extra"}
	result, err = planBackupControl(context.Background(), r, inspect)
	testutil.Must(t, err)
	if strings.Contains(result.Next, "enrolls added sources") {
		t.Fatal("source replacement advertised as addition", result)
	}
}

type planningInspector struct {
	countingInspector
	statusCalls int
}

func (i *planningInspector) Status(context.Context, config.ServerConfig, string, ResolvedPaths) (TargetStatus, error) {
	i.statusCalls++
	return TargetStatus{LoadState: "loaded", ActiveState: "inactive", Paths: []PathObservation{{Name: "manifest", Exists: true, Safe: true}, {Name: "state", Exists: true, Safe: true}}}, nil
}

type planningController struct{ calls int }

func (c *planningController) Apply(_ context.Context, _ config.ServerConfig, _ Artifact, r ControlRequest) (ControlResult, error) {
	c.calls++
	if r.Action != "plan" || len(r.Inputs) != 0 || r.LocalSignal != nil || r.LocalLED != nil {
		return ControlResult{}, os.ErrPermission
	}
	return ControlResult{Version: 1, State: "planned", Code: "policy_changed", Changes: []Change{{"notification_worker.limits.max_database_bytes", "budget_review"}}}, nil
}

func TestLivePlanUsesInspectionAndDryRunDoesNot(t *testing.T) {
	artifact := elfFixture(t, "arm64")
	inventory := inventoryFixture(t)
	raw, err := os.ReadFile(inventory)
	testutil.Must(t, err)
	yaml := strings.Replace(string(raw), "providers: [telegram]", "artifacts:\n        helper: "+artifact.File+"\n        helper_sha256: "+artifact.SHA256+"\n      providers:\n        telegram: {token_file: ~/.config/token, chat_id_file: ~/.config/chat}", 1)
	testutil.Must(t, os.WriteFile(inventory, []byte(yaml), 0600))
	var out bytes.Buffer
	inspector, controller := &planningInspector{}, &planningController{}
	cli := CLI{Inspector: inspector, Controller: controller, Out: &out}
	for _, dry := range []bool{true, false} {
		out.Reset()
		testutil.Must(t, cli.Run(context.Background(), "notification-worker", []string{"plan", "manager"}, Options{Inventory: inventory, NonInteractive: true, DryRun: dry}))
		if dry && (inspector.Calls != 0 || inspector.statusCalls != 0 || controller.calls != 0) {
			t.Fatal("dry-run contacted target")
		}
		if !dry && (inspector.Calls != 1 || inspector.statusCalls != 1 || controller.calls != 1 || !strings.Contains(out.String(), "Change: notification_worker.limits.max_database_bytes (budget_review)")) {
			t.Fatal("live plan did not expose semantic diff", out.String())
		}
	}
}

func TestBackupMonitoringPlanExplainsQualificationWithoutExposingReferences(t *testing.T) {
	inv, err := config.ParseInventory("server.yaml", []byte(cliInventory))
	testutil.Must(t, err)
	before := *inv.Servers["manager"].Desired
	after := before
	b := *before.Backup
	after.Backup = &b
	b.Monitoring.BackupPingURLFile = "/private/replacement-monitor"
	r := ControlRequest{Desired: after, Facts: testFacts()}
	result, err := planBackupControl(context.Background(), r, func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
		return backup.SetupObservation{Stage: "complete"}, &before, nil
	})
	testutil.Must(t, err)
	if result.Code != "policy_changed" || !strings.Contains(result.Next, "without sending heartbeats") || !strings.Contains(result.Next, "reactivate") {
		t.Fatal("monitoring update lacks concrete next step", result)
	}
	oldScope, err := before.NotificationFingerprint()
	testutil.Must(t, err)
	newScope, err := after.NotificationFingerprint()
	testutil.Must(t, err)
	if oldScope != newScope {
		t.Fatal("backup monitor changed gateway policy")
	}
	raw, err := json.Marshal(result)
	testutil.Must(t, err)
	if bytes.Contains(raw, []byte(b.Monitoring.BackupPingURLFile)) {
		t.Fatal("monitor reference exposed in normal plan")
	}
}

func TestBackupPlanExplainsPendingNotificationPolicy(t *testing.T) {
	inv, err := config.ParseInventory("server.yaml", []byte(backupOnlyInventory))
	testutil.Must(t, err)
	r := ControlRequest{Server: "manager", Desired: *inv.Servers["manager"].Desired, Facts: testFacts()}
	result, err := planBackupControl(context.Background(), r, func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
		return backup.SetupObservation{Stage: "notification_policy_pending"}, nil, nil
	})
	testutil.Must(t, err)
	if result.State != "resumable" || result.Code != "notification_policy_pending" || !strings.Contains(result.Next, "deploy notification-worker resume manager") {
		t.Fatal(result)
	}
}
