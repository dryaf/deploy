package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestShortBackupRunPinsInstalledPolicyAndSeparatesCompletion(t *testing.T) {
	inv, err := config.ParseInventory("server.yaml", []byte(backupOnlyInventory))
	testutil.Must(t, err)
	p, err := DeclarationPlan(inv, "backup-worker", "manager", "run")
	testutil.Must(t, err)
	r := ControlRequest{Version: 1, Worker: "backup-worker", Server: "manager", Action: "run", Facts: testFacts(), Desired: *inv.Servers["manager"].Desired, DesiredHash: p.DesiredHash, HelperSHA256: strings.Repeat("a", 64)}
	calls := 0
	for _, state := range []string{"start_requested", "running_or_busy", "start_unconfirmed", "completed"} {
		result, err := requestBackupRunControl(context.Background(), r, func(_ context.Context, home, desired, helper string) (backup.RunRequestResult, error) {
			calls++
			if home != r.Facts.Home || desired != r.DesiredHash || helper != r.HelperSHA256 {
				t.Fatal("manual backup changed installed policy pins")
			}
			if state == "start_unconfirmed" {
				return backup.RunRequestResult{State: state}, errors.New("lost reply")
			}
			return backup.RunRequestResult{State: state}, nil
		})
		if (err == nil) != (state == "start_requested" || state == "running_or_busy") || err == nil && (result.State != state || !strings.Contains(result.Next, "not a successful backup")) {
			t.Fatal("run result implied completed capture", state, result, err)
		}
	}
	if calls != 4 {
		t.Fatal("run retried without a separate user request")
	}
	if !Matches([]string{"run", "manager"}) || Matches([]string{"run", "--local"}) {
		t.Fatal("manual run lost legacy/local or short routing")
	}
}

func TestShortBackupRunReportsUncertainReplyWithoutRetry(t *testing.T) {
	for _, shared := range []bool{false, true} {
		data := backupOnlyInventory
		if shared {
			data = cliInventory
		}
		inv, err := config.ParseInventory("server.yaml", []byte(data))
		testutil.Must(t, err)
		p, err := DeclarationPlan(inv, "backup-worker", "manager", "run")
		testutil.Must(t, err)
		p.Artifacts = &ResolvedArtifacts{Helper: Artifact{File: "/fixture/deploy", SHA256: strings.Repeat("a", 64)}}
		var out bytes.Buffer
		calls := 0
		lost := errors.New("lost reply")
		cli := CLI{Out: &out, Controller: qualificationController(func(r ControlRequest) (ControlResult, error) {
			calls++
			if r.Action != "run" || r.Worker != "backup-worker" || r.DesiredHash != p.DesiredHash || len(r.Inputs) != 0 || r.Observation != nil || (r.Desired.Notification != nil) != shared {
				t.Fatal("manual run changed declaration or performed another operation", r.Action)
			}
			return ControlResult{Version: 1, Code: "run_unconfirmed", State: "start_unconfirmed", Next: "inspect status before requesting another run"}, lost
		})}
		err = cli.requestBackupRun(context.Background(), Invocation{Server: "manager", Options: Options{JSON: true}}, inv, testFacts(), p)
		if !errors.Is(err, lost) || calls != 1 {
			t.Fatal("manual run retried uncertain request", err, calls)
		}
		var result ControlResult
		testutil.Must(t, json.Unmarshal(out.Bytes(), &result))
		if result.Code != "run_unconfirmed" || result.State != "start_unconfirmed" {
			t.Fatal("uncertain request claimed completion", result)
		}
	}
}
