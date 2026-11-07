//go:build linux

package workerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

type commandCoordinatorCrashFixture struct {
	Request            ControlRequest
	Paths              ResolvedPaths
	Before             install.ManagedManifest
	Receiver           backup.CommandReceiverRequest
	Staged, Admitted   bool
	Stages, Admissions int
}

func commandCoordinatorCrashLifecycle(t *testing.T, f *commandCoordinatorCrashFixture, file, at string) notificationCommandLifecycle {
	t.Helper()
	mode, version := "read", 1
	if f.Request.Desired.Notification.Commands.Mode == "mutation" {
		mode, version = "all", 2
	}
	request := install.CommandPolicyRequest{Root: f.Paths.Installation, ManifestHash: protocol.Hash(f.Before), Operation: "enable-commands", Mode: mode, QualificationHash: protocol.Hash("qualification")}
	after := f.Before
	after.Commands = &install.ManagedCommandAdmission{Mode: mode}
	persist := func(boundary string) {
		raw, err := json.Marshal(f)
		testutil.Must(t, err)
		testutil.Must(t, os.WriteFile(file, raw, 0600))
		if at == boundary {
			testutil.Must(t, syscall.Kill(os.Getpid(), syscall.SIGKILL))
			t.Fatal("survived SIGKILL")
		}
	}
	return notificationCommandLifecycle{
		review: func(_ context.Context, root, hash, mode string) (install.CommandPolicyRequest, bool, error) {
			if root != request.Root || hash != request.ManifestHash || mode != request.Mode {
				t.Fatal("changed admission review")
			}
			return request, true, nil
		},
		receiverReview: func(_ context.Context, home, desired, manifest, operation string) (backup.CommandReceiverRequest, error) {
			if home != f.Request.Facts.Home || desired != f.Request.DesiredHash || manifest != request.ManifestHash {
				t.Fatal("changed receiver review")
			}
			return backup.CommandReceiverRequest{Version: version, Operation: operation, Identity: f.Before.Identity, Producer: protocol.Producer{ID: "backup", Epoch: "one"}}, nil
		},
		receiverInspect: func(_ context.Context, r backup.CommandReceiverRequest) (string, bool, error) {
			if !f.Staged {
				return "", false, nil
			}
			if protocol.Hash(r) != protocol.Hash(f.Receiver) {
				return "", false, fmt.Errorf("changed receiver request")
			}
			return protocol.Hash(r), true, nil
		},
		receiverStage: func(_ context.Context, r backup.CommandReceiverRequest) (string, error) {
			if f.Admitted {
				t.Fatal("command admission preceded receiver")
			}
			f.Receiver, f.Staged = r, true
			f.Stages++
			persist("receiver")
			return protocol.Hash(r), nil
		},
		inspect: func(_ context.Context, r install.CommandPolicyRequest) (install.ManagedManifest, bool, error) {
			if !f.Staged || r != request {
				t.Fatal("unreviewed admission")
			}
			return after, f.Admitted, nil
		},
		ensure: func(_ context.Context, r install.CommandPolicyRequest) (install.ManagedManifest, error) {
			if !f.Staged || r != request {
				t.Fatal("unreviewed enable")
			}
			f.Admitted = true
			f.Admissions++
			persist("admission")
			return after, nil
		},
		connect: func(_ context.Context, home, desired, hash string) error {
			if !f.Admitted || home != f.Request.Facts.Home || desired != f.Request.DesiredHash || hash != protocol.Hash(after) {
				t.Fatal("changed reporting continuation")
			}
			persist("continuation")
			return nil
		},
	}
}

func TestCommandCoordinatorProcessDeath(t *testing.T) {
	if file := os.Getenv("DEPLOY_COMMAND_COORDINATOR_CRASH_INPUT"); file != "" {
		raw, err := os.ReadFile(file)
		testutil.Must(t, err)
		var f commandCoordinatorCrashFixture
		testutil.Must(t, json.Unmarshal(raw, &f))
		d, err := safefs.Open(f.Paths.Control)
		testutil.Must(t, err)
		defer d.Close()
		var old OperationJournal
		testutil.Must(t, d.ReadJSON("operation.json", &old))
		_, err = advanceNotificationCommands(context.Background(), f.Request, f.Paths, d, old, ControlResult{Version: 1}, commandCoordinatorCrashLifecycle(t, &f, file, os.Getenv("DEPLOY_COMMAND_COORDINATOR_CRASH_AT")))
		t.Fatal("crash boundary not reached", err)
	}
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, declaredMode := range []string{"read", "mutation"} {
		for _, at := range []string{"receiver", "admission", "continuation"} {
			t.Run(declaredMode+"/"+at, func(t *testing.T) {
				r, paths := bootstrapControlFixture(t)
				declareFixtureReadCommands(t, &r)
				r.Desired.Notification.Commands.Mode = declaredMode
				r.DesiredHash, _ = r.Desired.Fingerprint()
				d, err := ensureConfigDirectory(paths.Control)
				testutil.Must(t, err)
				defer d.Close()
				before := install.ManagedManifest{Version: 1, Mode: "delivery", Root: paths.Installation, Identity: protocol.Identity{Worker: testutil.ID(t, "w"), Recovery: testutil.ID(t, "g")}, DeliveryActivation: &install.ManagedDeliveryActivation{Operation: "delivery", Hash: protocol.Hash("delivery")}}
				old := OperationJournal{Version: 1, ID: testutil.ID(t, "n"), Complete: true, Pins: OperationPins{Worker: r.Worker, Action: "apply", DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Artifacts: map[string]string{"helper": r.HelperSHA256}, Installations: map[string]string{"notification-worker": paths.Installation, "state": paths.State, "runtime": paths.Runtime}}, Children: []ChildReference{{Name: "notification-delivery", Operation: testutil.ID(t, "n"), Complete: true, Receipt: protocol.Hash(before)}}}
				lock, err := d.Lock("operation.lock", true, true)
				testutil.Must(t, err)
				lock.Close()
				testutil.Must(t, d.WriteJSON("operation.json", old, false))
				f := commandCoordinatorCrashFixture{Request: r, Paths: paths, Before: before}
				file := filepath.Join(t.TempDir(), "fixture.json")
				raw, err := json.Marshal(f)
				testutil.Must(t, err)
				testutil.Must(t, os.WriteFile(file, raw, 0600))
				cmd := exec.Command(os.Args[0], "-test.run=^TestCommandCoordinatorProcessDeath$", "-test.timeout=30s")
				cmd.Env = append(os.Environ(), "DEPLOY_COMMAND_COORDINATOR_CRASH_INPUT="+file, "DEPLOY_COMMAND_COORDINATOR_CRASH_AT="+at)
				out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
				if err == nil || cmd.ProcessState == nil {
					t.Fatal("missing process death", err, string(out))
				}
				status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatal("wrong child exit", string(out))
				}
				raw, err = os.ReadFile(file)
				testutil.Must(t, err)
				testutil.Must(t, json.Unmarshal(raw, &f))
				var current OperationJournal
				testutil.Must(t, d.ReadJSON("operation.json", &current))
				l := commandCoordinatorCrashLifecycle(t, &f, file, "")
				for range 2 {
					result, err := advanceNotificationCommands(context.Background(), r, paths, d, current, ControlResult{Version: 1}, l)
					testutil.Must(t, err)
					if result.State != "commands_admitted" || f.Stages != 1 || f.Admissions != 1 {
						t.Fatal("process death repeated effects", result.State, f.Stages, f.Admissions)
					}
					current = OperationJournal{}
					testutil.Must(t, d.ReadJSON("operation.json", &current))
				}
				if !current.Complete {
					t.Fatal("resumed coordinator incomplete")
				}
			})
		}
	}
}
