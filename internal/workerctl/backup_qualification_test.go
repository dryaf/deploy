package workerctl

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestBackupQualificationControlKeepsExactPinsAndSeparateActivation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(file, []byte(backupOnlyInventory), 0600); err != nil {
		t.Fatal(err)
	}
	inv, err := config.ReadInventory(file)
	if err != nil {
		t.Fatal(err)
	}
	p, err := DeclarationPlan(inv, "backup-worker", "manager", "qualify")
	if err != nil {
		t.Fatal(err)
	}
	r := ControlRequest{Version: 1, Worker: "backup-worker", Server: "manager", Action: "qualify-begin", Desired: *inv.Servers["manager"].Desired, DesiredHash: p.DesiredHash, HelperSHA256: strings.Repeat("a", 64), Facts: TargetFacts{OS: "linux", Arch: "arm64", UID: 1000, User: "deploy", Home: "/home/deploy", Linger: true}}
	id, _ := safefs.NewID("n")
	calls := 0
	qualify := func(_ context.Context, home, desired, helper, action string, o install.QualificationObservation) (install.QualificationSession, error) {
		calls++
		if home != r.Facts.Home || desired != r.DesiredHash || helper != r.HelperSHA256 || action != "begin" || o != (install.QualificationObservation{}) {
			t.Fatal("changed backup qualification binding")
		}
		return install.QualificationSession{ID: id, Recorded: true, EvidenceComplete: true}, nil
	}
	activated := 0
	activate := func(_ context.Context, home, desired, helper string) (backup.ActivationResult, error) {
		activated++
		if home != r.Facts.Home || desired != r.DesiredHash || helper != r.HelperSHA256 {
			t.Fatal("changed activation binding")
		}
		return backup.ActivationResult{State: "active", Unchanged: true}, nil
	}
	result, err := operateBackupControl(context.Background(), r, qualify, activate)
	if err != nil || result.State != "qualification_recorded" || calls != 1 || activated != 0 || !strings.Contains(result.Next, "activate manager") {
		t.Fatal(result, err)
	}
	r.Action = "activate"
	result, err = operateBackupControl(context.Background(), r, qualify, activate)
	if err != nil || result.State != "unchanged" || calls != 1 || activated != 1 {
		t.Fatal(result, err)
	}
	r.Facts.Linger = false
	if _, err = operateBackupControl(context.Background(), r, qualify, activate); err == nil || activated != 1 {
		t.Fatal("activated without lingering")
	}
}

func TestBackupGuidedOfflineCommands(t *testing.T) {
	file := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(file, []byte(backupOnlyInventory), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := CLI{Out: &out}
	for _, op := range []string{"qualify", "activate"} {
		out.Reset()
		if err := c.Run(context.Background(), "backup-worker", []string{op, "manager", "--dry-run", "--json", "--servers", file}, Options{}); err != nil {
			t.Fatal(op, err)
		}
		if !strings.Contains(out.String(), `"operation":"`+op+`"`) {
			t.Fatal(out.String())
		}
	}
	if err := c.Run(context.Background(), "backup-worker", []string{"qualify", "manager", "--non-interactive", "--servers", file}, Options{}); err == nil || !strings.Contains(err.Error(), "operator observations") {
		t.Fatal(err)
	}
	if Matches([]string{"activate", "--local"}) || !Matches([]string{"activate", "manager"}) {
		t.Fatal("activation routing lost specialist or short form")
	}
}
