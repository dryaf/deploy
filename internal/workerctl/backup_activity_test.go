package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil"
)

type activityBackupInspector struct {
	disabled bool
	calls    int
}

func (i *activityBackupInspector) Inspect(_ context.Context, s config.ServerConfig) (TargetFacts, error) {
	i.calls++
	if s.Host != "raspberrypi.local" {
		panic("activity attempted source connection")
	}
	f := testFacts()
	if i.disabled {
		f.Linger = false
		f.Tools = map[string]bool{"systemctl": true}
	}
	return f, nil
}
func (*activityBackupInspector) Status(context.Context, config.ServerConfig, string, ResolvedPaths) (TargetStatus, error) {
	return TargetStatus{LoadState: "loaded", Paths: []PathObservation{{Name: "manifest", Exists: true, Safe: true}, {Name: "state", Exists: true, Safe: true}, {Name: "setup", Exists: true, Safe: true}}}, nil
}

func TestShortBackupActivityNeedsNoSourceOrImageWhenDisabling(t *testing.T) {
	dir := t.TempDir()
	inventory := filepath.Join(dir, "server.yaml")
	helper := elfFixture(t, "arm64")
	yaml := strings.Replace(backupOnlyInventory, "sources: [source]", "sources: [source]\n      enabled: false\n      artifacts:\n        helper: "+helper.File+"\n        helper_sha256: "+helper.SHA256, 1)
	testutil.Must(t, os.WriteFile(inventory, []byte(yaml), 0600))
	var out bytes.Buffer
	inspector := &activityBackupInspector{disabled: true}
	controller := qualificationController(func(r ControlRequest) (ControlResult, error) {
		if r.Worker != "backup-worker" || (r.Action != "apply" && r.Action != "resume") || len(r.Inputs) != 0 || r.Desired.Notification != nil {
			t.Fatal("activity changed control scope")
		}
		return activityBackupControl(context.Background(), r, func(_ context.Context, home string, d config.DesiredWorkers, _ string) (backup.ActivityResult, error) {
			if home != testFacts().Home {
				t.Fatal("activity resolved operator home")
			}
			state := "disabled"
			if config.WorkerEnabled(d.Backup.Enabled) {
				state = "staged-awaiting-qualification"
			}
			return backup.ActivityResult{Handled: true, State: state}, nil
		})
	})
	cli := CLI{Inspector: inspector, Controller: controller, Out: &out, SourceTrust: func(context.Context, config.ServerConfig) (string, error) {
		t.Fatal("activity connected source")
		return "", nil
	}}
	for _, action := range []string{"apply", "resume"} {
		out.Reset()
		testutil.Must(t, cli.Run(context.Background(), "backup-worker", []string{action, "manager"}, Options{Inventory: inventory, NonInteractive: true, JSON: true}))
		var r ControlResult
		testutil.Must(t, json.Unmarshal(out.Bytes(), &r))
		if r.State != "disabled" {
			t.Fatal(out.String())
		}
	}
	// Enable still observes the retained target before ordinary source setup.
	inspector.disabled = false
	yaml = strings.Replace(yaml, "enabled: false", "enabled: true", 1)
	testutil.Must(t, os.WriteFile(inventory, []byte(yaml), 0600))
	out.Reset()
	testutil.Must(t, cli.Run(context.Background(), "backup-worker", []string{"manager"}, Options{Inventory: inventory, NonInteractive: true, JSON: true}))
	if !strings.Contains(out.String(), "qualification_required") || !strings.Contains(out.String(), "timers remain disabled") {
		t.Fatal(out.String())
	}
}

func TestBackupActivityControlRejectsPathsAndSecretsBeforeDomain(t *testing.T) {
	inv, err := config.ParseInventory("server.yaml", []byte(backupOnlyInventory))
	testutil.Must(t, err)
	for _, kind := range []string{"path", "secrets"} {
		t.Run(kind, func(t *testing.T) {
			r := ControlRequest{Desired: *inv.Servers["manager"].Desired, Facts: testFacts()}
			b := *r.Desired.Backup
			r.Desired.Backup = &b
			if kind == "path" {
				b.Paths.State = "~/.local/state/deploy/backup-worker"
			} else {
				r.Inputs = map[string][]byte{"token": []byte("private")}
			}
			_, err := activityBackupControl(context.Background(), r, func(context.Context, string, config.DesiredWorkers, string) (backup.ActivityResult, error) {
				t.Fatal("invalid activity reached domain")
				return backup.ActivityResult{}, nil
			})
			if err == nil {
				t.Fatal("invalid activity accepted")
			}
		})
	}
}
