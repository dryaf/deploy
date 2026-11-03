//go:build linux && notificationintegration

package backup

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

func TestBackupLEDRecoveryNativeWorkflow(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal("build", err, string(out))
	}
	socket := ledfixture.Start(t)
	api := led.Client{Socket: socket, Budget: time.Second}
	var runtimeFile string
	f := newBackupLEDAdapterFixtureAt(t, socket, func(c *worker.Config) { runtimeFile, *c = writeBackupLEDCLIRuntime(t, *c, socket) })
	testutil.Must(t, f.session.Close())
	ctx := context.Background()
	for _, state := range f.registry.states {
		testutil.Must(t, api.Set(ctx, state))
	}
	testutil.Must(t, api.Set(ctx, led.State{ID: "other.application", LED: "green", Pattern: "blink-fast", Priority: 100, Persistent: true}))
	view, err := worker.ReviewRecoveryLED(ctx, f.gatewayPath, f.gatewayConfig, socket, api, true)
	testutil.Must(t, err)
	f.inventory = view
	o := backupLEDCLIOptions(f)
	o.Config = writeBackupLEDCLIInstallation(t, f.config)
	o.NotificationConfig = runtimeFile
	run := func(op string) []byte {
		args := append([]string{"backup-worker"}, backupLEDCLIArguments(op, o)...)
		out, err := process.LimitedOutput(exec.Command(binary, args...), 15*time.Second, 32768)
		if err != nil {
			t.Fatal(op, err, string(out))
		}
		return out
	}
	var evidence notify.RecoveryLEDEvidence
	testutil.Must(t, json.Unmarshal(run("recovery-led-evidence"), &evidence))
	view, err = worker.ReviewRecoveryLED(ctx, f.gatewayPath, f.gatewayConfig, socket, nil, false)
	testutil.Must(t, err)
	proposal := view.Resolutions["backups"]
	if !proposal.DomainResolved || len(proposal.ProposedFaultIDs) == 0 {
		t.Fatal(proposal)
	}
	preserved, err := worker.PreserveRecoveryLED(ctx, f.gatewayPath, f.gatewayConfig, socket, o.Inventory, o.InventoryHash, api)
	testutil.Must(t, err)
	adopted, err := worker.AdoptRecoveryLED(ctx, f.gatewayPath, f.gatewayConfig, socket, worker.RecoveryLEDAdoptionRequest{InventoryID: o.Inventory, InventoryHash: o.InventoryHash, PreservationHash: preserved.RecordHash}, api)
	testutil.Must(t, err)
	epoch, err := worker.HandoffRecoveryLED(ctx, f.gatewayPath, f.gatewayConfig, socket, adopted.RecordHash)
	testutil.Must(t, err)
	o.Fault, o.EvidenceHash, o.EpochHash = proposal.ProposedFaultIDs[0], proposal.EvidenceHash, epoch.RecordHash
	before, err := api.List(ctx)
	testutil.Must(t, err)
	var result, retry backupLEDCleanupOutput
	testutil.Must(t, json.Unmarshal(run("recovery-led-clear"), &result))
	if result.Result.Outcome != "acknowledged" || result.Import == nil || result.Import.State != "committed" {
		t.Fatal(result)
	}
	testutil.Must(t, json.Unmarshal(run("recovery-led-clear"), &retry))
	if notify.Hash(result) != notify.Hash(retry) {
		t.Fatal("native retry changed completion")
	}
	after, err := api.List(ctx)
	testutil.Must(t, err)
	if len(after) != len(before)-1 {
		t.Fatal("wrong registry change", before, after)
	}
	seen := map[string]string{}
	for _, entry := range after {
		seen[entry.ID] = notify.Hash(entry.State)
	}
	for _, entry := range before {
		if entry.ID == o.Fault {
			if _, ok := seen[entry.ID]; ok {
				t.Fatal("target fault not cleared")
			}
			continue
		}
		if seen[entry.ID] != notify.Hash(entry.State) {
			t.Fatal("unrelated registry state changed", entry.ID)
		}
	}
	if live, err := worker.Open(f.gatewayPath, f.gatewayConfig, false); err == nil {
		live.Close()
		t.Fatal("native workflow activated worker")
	}
}
