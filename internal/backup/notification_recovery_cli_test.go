//go:build linux

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/testutil"
)

func backupLEDCLIOptions(f *backupLEDAdapterFixture) backupLEDRecoveryOptions {
	return backupLEDRecoveryOptions{State: f.state.directory, NotificationState: f.gatewayPath, Inventory: f.inventory.InventoryID, InventoryHash: f.inventory.InventoryHash, Binding: "backups"}
}

func prepareBackupLEDCLI(t *testing.T) (*backupLEDAdapterFixture, backupLEDRecoveryOptions) {
	t.Helper()
	f := newBackupLEDAdapterFixture(t)
	testutil.Must(t, f.session.Close())
	o := backupLEDCLIOptions(f)
	var out bytes.Buffer
	ctx := context.Background()
	testutil.Must(t, runBackupLEDRecovery(ctx, "recovery-led-evidence", o, f.config, f.gatewayConfig, "/private/staged-led.sock", f.registry, &out))
	var evidence notify.RecoveryLEDEvidence
	testutil.Must(t, json.Unmarshal(out.Bytes(), &evidence))
	if evidence.Challenge.InventoryID != o.Inventory {
		t.Fatal(evidence.Challenge)
	}
	v, err := worker.ReviewRecoveryLED(ctx, f.gatewayPath, f.gatewayConfig, "/private/staged-led.sock", nil, false)
	testutil.Must(t, err)
	p, err := worker.PreserveRecoveryLED(ctx, f.gatewayPath, f.gatewayConfig, "/private/staged-led.sock", o.Inventory, o.InventoryHash, f.registry)
	testutil.Must(t, err)
	a, err := worker.AdoptRecoveryLED(ctx, f.gatewayPath, f.gatewayConfig, "/private/staged-led.sock", worker.RecoveryLEDAdoptionRequest{InventoryID: o.Inventory, InventoryHash: o.InventoryHash, PreservationHash: p.RecordHash}, f.registry)
	testutil.Must(t, err)
	epoch, err := worker.HandoffRecoveryLED(ctx, f.gatewayPath, f.gatewayConfig, "/private/staged-led.sock", a.RecordHash)
	testutil.Must(t, err)
	proposal := v.Resolutions["backups"]
	if !proposal.DomainResolved || len(proposal.ProposedFaultIDs) == 0 {
		t.Fatal(proposal)
	}
	o.Fault, o.EvidenceHash, o.EpochHash = proposal.ProposedFaultIDs[0], proposal.EvidenceHash, epoch.RecordHash
	return f, o
}

func TestBackupLEDRecoveryLocalWorkflow(t *testing.T) {
	f, o := prepareBackupLEDCLI(t)
	ctx := context.Background()
	writes := f.registry.writes
	sequence := backupLEDSequence(t, f.state)
	var out bytes.Buffer
	testutil.Must(t, runBackupLEDRecovery(ctx, "recovery-led-clear", o, f.config, f.gatewayConfig, "/private/staged-led.sock", f.registry, &out))
	var result backupLEDCleanupOutput
	testutil.Must(t, json.Unmarshal(out.Bytes(), &result))
	if result.Result.Outcome != "acknowledged" || result.ResultHash != notify.Hash(result.Result) || result.Import == nil || result.Import.State != "committed" || result.Import.FaultID != o.Fault {
		t.Fatal(result)
	}
	if f.registry.writes != writes+1 || sequence != backupLEDSequence(t, f.state) {
		t.Fatal("cleanup replayed or committed speculative source evaluation")
	}
	// An exact retry reads historical completion even if the source is now paused.
	testutil.Must(t, f.state.put("paused_until", time.Now().Add(time.Hour)))
	out.Reset()
	testutil.Must(t, runBackupLEDRecovery(ctx, "recovery-led-clear", o, f.config, f.gatewayConfig, "/private/staged-led.sock", f.registry, &out))
	var retry backupLEDCleanupOutput
	testutil.Must(t, json.Unmarshal(out.Bytes(), &retry))
	if notify.Hash(result) != notify.Hash(retry) || f.registry.writes != writes+1 {
		t.Fatal("retry changed outcome or sent another clear")
	}
	if live, err := worker.Open(f.gatewayPath, f.gatewayConfig, false); err == nil {
		live.Close()
		t.Fatal("workflow published recovery")
	}
}

func TestBackupLEDRecoveryLocalScopeAndFreshness(t *testing.T) {
	for _, damage := range []string{"inventory", "inventory-hash", "epoch", "binding", "fault", "evidence", "pause", "incident", "probe", "worker-busy", "backup-busy"} {
		t.Run(damage, func(t *testing.T) {
			f, o := prepareBackupLEDCLI(t)
			switch damage {
			case "inventory":
				o.Inventory = "n-00000000000000000000000000000000"
			case "inventory-hash":
				o.InventoryHash = notify.Hash("other")
			case "epoch":
				o.EpochHash = notify.Hash("other")
			case "binding":
				o.Binding = "$worker"
			case "fault":
				o.Fault = "deploy.f." + string(bytes.Repeat([]byte("0"), 48))
			case "evidence":
				o.EvidenceHash = notify.Hash("other")
			case "pause":
				testutil.Must(t, f.state.put("paused_until", time.Now().Add(time.Hour)))
			case "incident":
				testutil.Must(t, f.state.observeIncident("capture", f.subject, "new failure", time.Now(), f.config.Policy.Notifications))
			case "probe":
				_, err := f.state.db.Exec("DELETE FROM meta WHERE key='health_probe'")
				testutil.Must(t, err)
			case "worker-busy":
				s, err := worker.OpenRecoveryLEDEvidenceSession(context.Background(), f.gatewayPath, f.gatewayConfig)
				testutil.Must(t, err)
				defer s.Close()
			case "backup-busy":
				_, release, err := lockBackupRecovery(context.Background(), f.state)
				testutil.Must(t, err)
				defer release()
			}
			writes := f.registry.writes
			var out bytes.Buffer
			if err := runBackupLEDRecovery(context.Background(), "recovery-led-clear", o, f.config, f.gatewayConfig, "/private/staged-led.sock", f.registry, &out); err == nil {
				t.Fatal("unsafe cleanup accepted", damage)
			}
			if writes != f.registry.writes {
				t.Fatal("unsafe cleanup reached daemon", damage)
			}
		})
	}
}

type lostBackupCLIClear struct{ worker.LEDRecoveryCleaner }

func (a lostBackupCLIClear) Clear(ctx context.Context, id string) error {
	if err := a.LEDRecoveryCleaner.Clear(ctx, id); err != nil {
		return err
	}
	return context.DeadlineExceeded
}

func TestBackupLEDRecoveryLocalUncertainResult(t *testing.T) {
	f, o := prepareBackupLEDCLI(t)
	writes := f.registry.writes
	var out bytes.Buffer
	api := lostBackupCLIClear{f.registry}
	if err := runBackupLEDRecovery(context.Background(), "recovery-led-clear", o, f.config, f.gatewayConfig, "/private/staged-led.sock", api, &out); err == nil {
		t.Fatal("lost acknowledgement accepted")
	}
	var result backupLEDCleanupOutput
	testutil.Must(t, json.Unmarshal(out.Bytes(), &result))
	if result.Result.Outcome != "uncertain" || result.Import != nil || result.ResultHash != notify.Hash(result.Result) {
		t.Fatal(result)
	}
	out.Reset()
	testutil.Must(t, runBackupLEDRecovery(context.Background(), "recovery-led-clear", o, f.config, f.gatewayConfig, "/private/staged-led.sock", api, &out))
	if f.registry.writes != writes+1 {
		t.Fatal("uncertain clear replayed")
	}
	v, err := worker.ReviewRecoveryLED(context.Background(), f.gatewayPath, f.gatewayConfig, "/private/staged-led.sock", nil, false)
	testutil.Must(t, err)
	if len(v.CleanupImports) != 0 {
		t.Fatal("uncertain result imported")
	}
}

func TestBackupLEDRecoveryLocalRequiresExistingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent")
	var out bytes.Buffer
	if err := runBackupLEDRecovery(context.Background(), "recovery-led-evidence", backupLEDRecoveryOptions{State: path}, WorkerInstallation{}, worker.Config{}, "/private/led.sock", nil, &out); err == nil {
		t.Fatal("missing state accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing state created", err)
	}
}
