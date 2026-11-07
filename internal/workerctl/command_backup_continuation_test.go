package workerctl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestCompletedCommandAdmissionInspectsAppliedBackupChanges(t *testing.T) {
	for _, condition := range []string{"applied", "unapplied", "pending-backup", "pending-admission", "inspector-missing"} {
		t.Run(condition, func(t *testing.T) {
			f := newCommandActivityFixture(t, "read")
			old := f.journal(t)
			before := f.r.Desired
			b := *before.Backup
			b.Artifacts.Image = "fixture.invalid/backup@sha256:" + protocol.Hash("new image")
			f.r.Desired.Backup = &b
			l := f.lifecycle.notificationCommandLifecycle
			l.inspect = func(context.Context, install.CommandPolicyRequest) (install.ManagedManifest, bool, error) {
				return f.manifest, true, nil
			}
			l.ensure = func(context.Context, install.CommandPolicyRequest) (install.ManagedManifest, error) {
				t.Fatal("completed admission restarted")
				return install.ManagedManifest{}, nil
			}
			l.receiverStage = func(context.Context, backup.CommandReceiverRequest) (string, error) {
				t.Fatal("completed receiver restaged")
				return "", nil
			}
			connected := false
			l.connect = func(_ context.Context, _ string, desired, _ string) error {
				want, _ := f.r.Desired.Fingerprint()
				if desired != want {
					t.Fatal("continuation used old backup policy")
				}
				connected = true
				return nil
			}
			l.backupReview = func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
				actual := f.r.Desired
				stage := "complete"
				if condition == "unapplied" {
					actual = before
				}
				if condition == "pending-backup" {
					stage = "helper_upgrade_pending"
				}
				return backup.SetupObservation{Stage: stage, SharedReportingConnected: true}, &actual, nil
			}
			if condition == "inspector-missing" {
				l.backupReview = nil
			}
			if condition == "pending-admission" {
				old.Complete = false
				testutil.Must(t, f.d.WriteJSON("operation.json", old, true))
			}
			tree := planTree(t, f.paths.Control)
			_, err := advanceNotificationCommands(context.Background(), f.r, f.paths, f.d, old, ControlResult{}, l)
			if (err == nil) != (condition == "applied") || connected != (condition == "applied") {
				t.Fatal(condition, connected, err)
			}
			if !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
				t.Fatal("backup continuation rewrote original command admission")
			}
		})
	}
}

func TestCommandActivityAfterAppliedBackupChange(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "unapplied", true: "applied"}[applied], func(t *testing.T) {
			f := newCommandActivityFixture(t, "read")
			actual := f.r.Desired
			b := *actual.Backup
			b.Artifacts.HelperSHA256 = protocol.Hash("new helper")
			f.r.Desired.Backup = &b
			if applied {
				actual.Backup = &b
			}
			f.lifecycle.backupReview = func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
				return backup.SetupObservation{Stage: "complete", SharedReportingConnected: true}, &actual, nil
			}
			f.toggle(false)
			tree := planTree(t, f.paths.Control)
			result, handled, err := reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle)
			if !handled || (err == nil) != applied {
				t.Fatal(result, handled, err)
			}
			if !applied {
				if !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) || f.effects != 0 {
					t.Fatal("unapplied backup change mutated command authority")
				}
				return
			}
			testutil.Must(t, verifyCommandActivityReceipt(context.Background(), f.d, f.journal(t), f.manifest, f.lifecycle))
			_, _, err = reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle)
			testutil.Must(t, err)
			if f.effects != 1 {
				t.Fatal("command continuation repeated its effect", f.effects)
			}
			f.toggle(true)
			for range 2 {
				result, handled, err = reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle)
				if err != nil || !handled || result.State != "commands_admitted" {
					t.Fatal(result, handled, err)
				}
				testutil.Must(t, verifyCommandActivityReceipt(context.Background(), f.d, f.journal(t), f.manifest, f.lifecycle))
			}
			if f.effects != 2 {
				t.Fatal("reenable repeated its effect", f.effects)
			}

		})
	}
}

func TestCompletedCommandActivityInspectsAppliedBackupChanges(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, applied := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%t/applied=%t", enabled, applied), func(t *testing.T) {
				f := newCommandActivityFixture(t, "read")
				f.toggle(false)
				_, _, err := reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle)
				testutil.Must(t, err)
				if enabled {
					f.toggle(true)
					_, _, err = reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle)
					testutil.Must(t, err)
				}
				old, effects := f.journal(t), f.effects
				actual := f.r.Desired
				b := *actual.Backup
				b.Artifacts.HelperSHA256 = protocol.Hash("later helper")
				f.r.Desired.Backup = &b
				if applied {
					actual = f.r.Desired
				}
				f.lifecycle.backupReview = func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
					return backup.SetupObservation{Stage: "complete", SharedReportingConnected: true}, &actual, nil
				}
				tree := planTree(t, f.paths.Control)
				for range 2 {
					_, handled, err := reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle)
					if !handled || (err == nil) != applied {
						t.Fatal(handled, err)
					}
				}
				if old.Digest() != f.journal(t).Digest() || f.effects != effects || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
					t.Fatal("backup continuation rewrote completed command activity")
				}
			})
		}
	}
}

func TestCommandDeclarationKeepsFullPolicyAuthority(t *testing.T) {
	for _, condition := range []string{"legacy", "full", "stale-scope", "corrupt-full", "wrong-scope"} {
		t.Run(condition, func(t *testing.T) {
			f := newCommandActivityFixture(t, "read")
			j := f.journal(t)
			if condition != "legacy" {
				testutil.Must(t, retainDeclaration(f.d, f.r.Desired, j.Pins.Artifacts["command-backup-policy"]))
			}
			changed := f.r.Desired
			b := *changed.Backup
			b.Artifacts.HelperSHA256 = protocol.Hash("changed helper")
			changed.Backup = &b
			switch condition {
			case "stale-scope":
				testutil.Must(t, os.Remove(filepath.Join(f.paths.Control, "declarations", j.Pins.Artifacts["command-backup-policy"]+".json")))
				testutil.Must(t, f.d.WriteJSON("declarations/"+j.Pins.DesiredHash+".json", changed, true))
			case "corrupt-full":
				testutil.Must(t, f.d.WriteJSON("declarations/"+j.Pins.Artifacts["command-backup-policy"]+".json", changed, true))
			case "wrong-scope":
				j.Pins.DesiredHash = protocol.Hash("different scope")
			}
			tree := planTree(t, f.paths.Control)
			_, err := readCommandDeclaration(f.d, j)
			if (err == nil) != (condition == "legacy" || condition == "full") {
				t.Fatal(condition, err)
			}
			if !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
				t.Fatal("declaration review wrote files")
			}
		})
	}
}
