//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDerivedFreshnessProcessDeath(t *testing.T) {
	if home := os.Getenv("DEPLOY_FRESHNESS_CRASH_HOME"); home != "" {
		d, err := safefs.Open(home)
		testutil.Must(t, err)
		defer d.Close()
		var request VerificationNotificationScopeChange
		testutil.Must(t, d.ReadJSON("freshness-request.json", &request))
		_, err = reconcileNotificationScope(context.Background(), home, request, func(ProducerReservation) (string, error) { return notify.Hash("gateway"), nil }, 14, func(at string) error {
			if at == os.Getenv("DEPLOY_FRESHNESS_CRASH_BOUNDARY") {
				return syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
			return nil
		})
		t.Fatal("process death boundary missed", err)
	}
	for _, at := range []string{"backup-gated", "archive-checkpoint", "runtime-0", "scope-complete"} {
		t.Run(at, func(t *testing.T) {
			home, r, _ := derivedFreshnessFixture(t)
			d, err := safefs.Open(home)
			testutil.Must(t, err)
			defer d.Close()
			testutil.Must(t, d.WriteJSON("freshness-request.json", r, false))
			cmd := exec.Command(os.Args[0], "-test.run=^TestDerivedFreshnessProcessDeath$")
			cmd.Env = append(os.Environ(), "DEPLOY_FRESHNESS_CRASH_HOME="+home, "DEPLOY_FRESHNESS_CRASH_BOUNDARY="+at)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("boundary missed: %v %s", err, out)
			}
			if state, e := openInstalledBackupState(home, false); e == nil {
				state.Close()
				t.Fatal("process death removed runtime fence")
			}
			_, err = ReconcileVerificationDerivedFreshnessScope(context.Background(), home, r, func(ProducerReservation) (string, error) { return notify.Hash("gateway"), nil })
			testutil.Must(t, err)
			state, err := openInstalledBackupState(home, false)
			testutil.Must(t, err)
			state.Close()
			c, err := loadInstalledWorker(home)
			testutil.Must(t, err)
			if c.Policy.Freshness != r.Desired.Backup.Freshness {
				t.Fatal("resume lost policy")
			}
		})
	}
}

func derivedFreshnessFixture(t *testing.T) (string, VerificationNotificationScopeChange, WorkerSetupJournal) {
	t.Helper()
	home, r, j := verificationScopeFixture(t)
	raw, err := json.Marshal(j.Declaration.Desired)
	testutil.Must(t, err)
	testutil.Must(t, json.Unmarshal(raw, &r.Desired))
	r.Desired.Backup.Freshness = "48h"
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	c, err := loadInstalledWorker(home)
	testutil.Must(t, err)
	p, err := declarationPaths(home, j.Declaration)
	testutil.Must(t, err)
	c.Policy = containerNotificationPolicy(c.Policy)
	for i := range c.Sources {
		c.Sources[i].KeyFile = "/keys/" + c.Sources[i].Alias + ".key"
		c.Sources[i].KnownHosts = "/keys/" + c.Sources[i].Alias + ".known_hosts"
	}
	parent, err := openBackupOwnedParent(root, p.OwnedPath(p.ContainerRuntime()))
	testutil.Must(t, err)
	testutil.Must(t, parent.WriteJSON(filepath.Base(p.ContainerRuntime()), c, true))
	parent.Close()
	raw, err = readBackupOwnedFile(root, p.OwnedPath(p.ContainerRuntime()), safefs.MaxRecordBytes)
	testutil.Must(t, err)
	j.OwnedFiles[p.OwnedPath(p.ContainerRuntime())] = sharedSetupDigest(raw)
	testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
	return home, r, j
}

func TestDerivedFreshnessResumesEveryPublication(t *testing.T) {
	for _, at := range []string{"none", "scope-intent", "backup-gated", "gateway-complete", "archive-checkpoint", "runtime-0", "runtime-1", "scope-receipt", "scope-complete"} {
		t.Run(at, func(t *testing.T) {
			home, r, before := derivedFreshnessFixture(t)
			ctx := context.Background()
			original, err := loadInstalledWorker(home)
			testutil.Must(t, err)
			state, err := openInstalledBackupState(home, false)
			testutil.Must(t, err)
			last := WorkerRun{ID: testutil.ID(t, "r"), ConfigHash: original.ConfigHash, FinishedAt: time.Now().UTC(), Complete: true}
			testutil.Must(t, state.put("run", last))
			var originalControl ArchiveControl
			testutil.Must(t, state.get("control", &originalControl))
			state.Close()
			stop := errors.New("disconnect")
			calls := 0
			migrate := func(p ProducerReservation) (string, error) {
				calls++
				if p != *before.Producer {
					t.Fatal("producer replaced")
				}
				if s, e := openInstalledBackupState(home, false); e == nil {
					s.Close()
					t.Fatal("runtime was not gated")
				}
				return notify.Hash("qualified migration receipt"), nil
			}
			_, err = reconcileNotificationScope(ctx, home, r, migrate, 14, func(point string) error {
				if point == at {
					return stop
				}
				return nil
			})
			if at == "none" {
				testutil.Must(t, err)
			} else {
				if !errors.Is(err, stop) {
					t.Fatal("boundary not reached", at, err)
				}
				scope, e := r.Desired.NotificationFingerprint()
				testutil.Must(t, e)
				_, complete, e := InspectVerificationNotificationScope(home, r.Operation, r.BeforeGateway, scope)
				if e != nil || complete {
					t.Fatal("partial migration accepted", complete, e)
				}
				_, err = ReconcileVerificationDerivedFreshnessScope(ctx, home, r, migrate)
				testutil.Must(t, err)
			}
			after, err := loadInstalledWorker(home)
			testutil.Must(t, err)
			if after.Policy.Freshness != "48h" || after.ConfigHash == original.ConfigHash || !reflect.DeepEqual(after.Sources, original.Sources) || after.WorkerID != original.WorkerID || after.ArchiveID != original.ArchiveID {
				t.Fatal("runtime migration lost identity or policy")
			}
			state, err = openInstalledBackupState(home, false)
			testutil.Must(t, err)
			var run WorkerRun
			testutil.Must(t, state.get("run", &run))
			if !reflect.DeepEqual(run, last) {
				t.Fatal("backup history replaced")
			}
			var control ArchiveControl
			testutil.Must(t, state.get("control", &control))
			state.Close()
			if control.ConfigHash != after.ConfigHash || control.Revision != originalControl.Revision+1 {
				t.Fatal("archive checkpoint advanced more than once", control.Revision)
			}
			control.ConfigHash, control.Revision = originalControl.ConfigHash, originalControl.Revision
			if !reflect.DeepEqual(control, originalControl) {
				t.Fatal("archive ownership or contents changed")
			}
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			var j WorkerSetupJournal
			testutil.Must(t, root.ReadJSON(backupSetupFile, &j))
			if j.NotificationScope != nil || j.RequestHash == before.RequestHash || j.Producer == nil || *j.Producer != *before.Producer || !reflect.DeepEqual(j.SourceKeys, before.SourceKeys) {
				t.Fatal("setup identity changed")
			}
			tree := policyReviewTree(t, home)
			previousCalls := calls
			_, err = ReconcileVerificationDerivedFreshnessScope(ctx, home, r, migrate)
			testutil.Must(t, err)
			if calls != previousCalls || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
				t.Fatal("completed retry mutated installation")
			}
		})
	}
}

func TestDerivedFreshnessRejectsUnrelatedOrActiveChanges(t *testing.T) {
	for _, damage := range []string{"schedule", "explicit-producer", "active", "pending-runtime", "container-drift", "deadline", "missing-backup"} {
		t.Run(damage, func(t *testing.T) {
			home, r, j := derivedFreshnessFixture(t)
			switch damage {
			case "missing-backup":
				r.Desired.Backup = nil
			case "schedule":
				r.Desired.Backup.Schedule = "weekly"
			case "explicit-producer":
				r.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {MaxEvidenceSeconds: 3600}}
			case "deadline":
				r.Desired.Backup.Freshness = "1s"
			case "active":
				s, e := openInstalledBackupState(home, false)
				testutil.Must(t, e)
				testutil.Must(t, s.put("installation", "active"))
				s.Close()
			case "pending-runtime":
				p, e := declarationPaths(home, j.Declaration)
				testutil.Must(t, e)
				d, e := safefs.Open(p.State)
				testutil.Must(t, e)
				testutil.Must(t, d.WriteJSON("installation.pending", installationGate{testutil.ID(t, "n")}, false))
				d.Close()
			case "container-drift":
				p, e := declarationPaths(home, j.Declaration)
				testutil.Must(t, e)
				testutil.Must(t, os.WriteFile(p.ContainerRuntime(), []byte("{}"), 0600))
			}
			called := false
			before := policyReviewTree(t, home)
			_, err := ReconcileVerificationDerivedFreshnessScope(context.Background(), home, r, func(ProducerReservation) (string, error) { called = true; return notify.Hash("bad"), nil })
			if err == nil || called {
				t.Fatal("invalid change reached gateway", err)
			}
			if !reflect.DeepEqual(before, policyReviewTree(t, home)) {
				t.Fatal("rejected policy retained side effects")
			}
		})
	}
}

func TestDerivedFreshnessWaitsForReadersAndRoutesExactBackupResume(t *testing.T) {
	home, r, before := derivedFreshnessFixture(t)
	reader, err := openInstalledBackupState(home, false)
	testutil.Must(t, err)
	called := false
	migrate := func(ProducerReservation) (string, error) { called = true; return notify.Hash("gateway"), nil }
	_, err = ReconcileVerificationDerivedFreshnessScope(context.Background(), home, r, migrate)
	if err == nil || called {
		t.Fatal("freshness crossed a live backup state reader", err)
	}
	reader.Close()
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	d, err := root.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, err)
	defer d.Close()
	var pending WorkerSetupJournal
	testutil.Must(t, d.ReadJSON("setup.json", &pending))
	if pending.NotificationScope == nil {
		t.Fatal("missing resumable intent")
	}
	request := pending.NotificationScope.After
	request.Operation = "reserve"
	for _, j := range []WorkerSetupJournal{before, pending} {
		handled, err := inspectCoordinatedNotificationReservation(root, d, j, request)
		if err != nil || !handled {
			t.Fatal("backup command cannot route freshness", err)
		}
		wrong := request
		wrong.Policy.MaxSnapshotBytes++
		if _, err = inspectCoordinatedNotificationReservation(root, d, j, wrong); err == nil {
			t.Fatal("unrelated backup edit routed as freshness")
		}
	}
	_, err = ReconcileVerificationDerivedFreshnessScope(context.Background(), home, r, migrate)
	testutil.Must(t, err)
}
