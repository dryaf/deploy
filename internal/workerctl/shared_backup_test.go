package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type sharedSetupFixture struct {
	calls       []string
	reserved    backup.WorkerSetupResponse
	plan        backup.SetupPlan
	failGateway bool
	admitted    bool
	installed   string
	setupCount  int
	failSetup   bool
	gatewayHash string
}

func (f *sharedSetupFixture) Reserve(ctx context.Context, p backup.SetupPlan) (backup.WorkerSetupResponse, error) {
	f.calls = append(f.calls, "reserve")
	f.plan = p
	r := f.reserved
	r.PolicyUpdateRequired = f.installed != "" && f.installed != p.Request.Declaration.DesiredHash
	return r, nil
}
func (f *sharedSetupFixture) Setup(ctx context.Context, p backup.SetupPlan) (backup.WorkerSetupResponse, error) {
	f.calls = append(f.calls, "setup")
	if !reflect.DeepEqual(p.Request, f.plan.Request) {
		return backup.WorkerSetupResponse{}, fmt.Errorf("reservation request retargeted")
	}
	if f.failSetup {
		return backup.WorkerSetupResponse{}, fmt.Errorf("backup update interrupted before publication")
	}
	r := f.reserved
	r.Stage = "complete"
	r.Unchanged = f.installed == p.Request.Declaration.DesiredHash
	if !r.Unchanged {
		f.setupCount++
		f.installed = p.Request.Declaration.DesiredHash
	}
	return r, nil
}
func (f *sharedSetupFixture) Apply(ctx context.Context, s config.ServerConfig, a Artifact, r ControlRequest) (ControlResult, error) {
	f.calls = append(f.calls, "gateway")
	if f.failGateway {
		return ControlResult{}, fmt.Errorf("gateway interrupted")
	}
	if r.DesiredHash != f.plan.Request.Declaration.NotificationDesiredHash || r.DesiredScope != 1 || r.Worker != "notification-worker" {
		return ControlResult{}, fmt.Errorf("dependency pin differs")
	}
	state := "verification"
	if f.admitted {
		if f.gatewayHash != r.DesiredHash {
			return ControlResult{}, fmt.Errorf("gateway changed during ordinary backup update")
		}
		full, err := r.Desired.Fingerprint()
		if err != nil {
			return ControlResult{}, err
		}
		state = "delivery_admitted"
		if full == f.installed {
			state = "reporting_connected"
		}
	}
	f.gatewayHash = r.DesiredHash
	return ControlResult{Version: 1, State: state, Code: "ok", Identity: &protocol.Identity{Worker: "gateway-fixture", Recovery: "initial"}}, nil
}

func TestShortSharedBackupReservesThenEnrollsThenInstalls(t *testing.T) {
	dir := t.TempDir()
	artifact := elfFixture(t, "arm64")
	raw, err := os.ReadFile(artifact.File)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "arm64"), raw, 0700); err != nil {
		t.Fatal(err)
	}
	artifact.File = "arm64"
	x86 := elfFixture(t, "amd64")
	raw, err = os.ReadFile(x86.File)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "amd64"), raw, 0700); err != nil {
		t.Fatal(err)
	}
	x86.File = "amd64"
	image := "example.test/worker@sha256:" + strings.Repeat("a", 64)
	catalog := Catalog{Version: 1, Release: "fixture", Linux: map[string]ArchitectureArtifacts{"arm64": {Helper: artifact, BackupImage: image}, "amd64": {Helper: x86, BackupImage: image}}}
	raw, _ = json.Marshal(catalog)
	catalogFile := filepath.Join(dir, "catalog.json")
	if err = os.WriteFile(catalogFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(backupOnlyInventory, "    backup_worker:", "    notification_worker:\n      providers:\n        telegram:\n          token_file: ~/.config/deploy/token\n          chat_id_file: ~/.config/deploy/chat\n    backup_worker:", 1)
	yaml = strings.Replace(yaml, "sources: [source]", "sources: [source]\n      freshness: 26h", 1)
	inventory := filepath.Join(dir, "server.yaml")
	if err = os.WriteFile(inventory, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	w, _ := safefs.NewID("w")
	a, _ := safefs.NewID("a")
	e, _ := safefs.NewID("n")
	g, _ := safefs.NewID("g")
	f := &sharedSetupFixture{reserved: backup.WorkerSetupResponse{Schema: 1, Stage: "reserved", WorkerID: w, ArchiveID: a, Producer: &backup.ProducerReservation{Version: 1, Producer: protocol.Producer{ID: w, Epoch: e}, Generation: g}}, failGateway: true}
	var out bytes.Buffer
	cli := CLI{Inspector: backupTestInspector{}, BackupInstaller: f, Controller: f, Out: &out, SourceTrust: func(context.Context, config.ServerConfig) (string, error) { return "verified fixture key", nil }}
	opts := Options{Inventory: inventory, Catalog: catalogFile, NonInteractive: true, JSON: true}
	if err = cli.Run(context.Background(), "backup-worker", []string{"manager"}, opts); err == nil {
		t.Fatal("gateway failure hidden")
	}
	if !reflect.DeepEqual(f.calls, []string{"reserve", "gateway"}) {
		t.Fatal("source install happened before enrollment", f.calls)
	}
	f.failGateway = false
	for range 2 {
		f.calls = nil
		out.Reset()
		if err = cli.Run(context.Background(), "backup-worker", []string{"manager"}, opts); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(f.calls, []string{"reserve", "gateway", "setup"}) {
			t.Fatal("wrong dependency order", f.calls)
		}
		var result BackupResult
		if err = json.Unmarshal(out.Bytes(), &result); err != nil || result.State != "qualification_required" || result.WorkerID != w {
			t.Fatal(out.String(), err)
		}
		p := f.plan.Request
		if p.Policy.Notifications.Mode != "shared" || p.Policy.Notifications.Telegram != nil || p.Declaration.NotificationRoot == "" || p.Declaration.NotificationRoute != "alerts" {
			t.Fatal("shared intent lost", p.Declaration, p.Policy.Notifications)
		}
	}
	// Reconcile a specialist-only policy change through the same short command.
	// Keep freshness explicit: changing a derived gateway evidence bound is a
	// separate producer-policy migration rather than a backup timer update.
	f.admitted = true
	before := f.plan.Request.Declaration.DesiredHash
	updated := strings.Replace(yaml, "sources: [source]", "sources: [source]\n      schedule: daily\n      retention: {keep_last: 12}", 1)
	if err = os.WriteFile(inventory, []byte(updated), 0600); err != nil {
		t.Fatal(err)
	}
	f.failSetup = true
	f.calls = nil
	out.Reset()
	if err = cli.Run(context.Background(), "backup-worker", []string{"manager"}, opts); err == nil || !reflect.DeepEqual(f.calls, []string{"reserve", "setup"}) {
		t.Fatal("pending update reached gateway admission before backup completion", f.calls, err)
	}
	f.failSetup = false
	for attempt := range 2 {
		f.calls = nil
		out.Reset()
		if err = cli.Run(context.Background(), "backup-worker", []string{"manager"}, opts); err != nil {
			t.Fatal(err)
		}
		want := []string{"reserve", "setup", "reserve", "gateway"}
		state := "qualification_required"
		if attempt == 1 {
			want, state = []string{"reserve", "gateway", "setup"}, "unchanged"
		}
		var result BackupResult
		if err = json.Unmarshal(out.Bytes(), &result); err != nil || result.State != state || result.WorkerID != w || result.ArchiveID != a || !reflect.DeepEqual(f.calls, want) {
			t.Fatal("shared update ordering or outcome changed", f.calls, out.String(), err)
		}
		if f.plan.Request.Declaration.DesiredHash == before || f.setupCount != 2 || f.plan.Request.Policy.Schedule != "daily" || f.plan.Request.Policy.Retention.KeepLast != 12 {
			t.Fatal("changed policy not applied exactly once", f.setupCount, f.plan.Request)
		}
	}
	// Source additions do not change this gateway's routing/freshness scope.
	// Finish the backup's enrollment transaction before observing that dependency.
	expanded := strings.Replace(updated, "sources: [source]", "sources: [source, extra]", 1) + "  extra: {host: extra.invalid, user: deploy}\n"
	if err = os.WriteFile(inventory, []byte(expanded), 0600); err != nil {
		t.Fatal(err)
	}
	beforeGateway, beforeProducer := f.gatewayHash, *f.reserved.Producer
	f.calls = nil
	out.Reset()
	if err = cli.Run(context.Background(), "backup-worker", []string{"manager"}, opts); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.calls, []string{"reserve", "setup", "reserve", "gateway"}) || f.gatewayHash != beforeGateway || *f.reserved.Producer != beforeProducer || len(f.plan.Request.Sources) != 2 || f.setupCount != 3 {
		t.Fatal("source addition changed shared identity or admission order", f.calls, f.setupCount)
	}
}
