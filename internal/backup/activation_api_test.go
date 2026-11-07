package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDeclaredActivationReconcilesLostReplyAndNeverRestartsCompleted(t *testing.T) {
	home, s, j, c := declaredQualificationFixture(t)
	ctx := context.Background()
	d := backupQualificationDraft{CreatedAt: time.Now().UTC(), Checks: map[string]backupQualificationObservation{}}
	for _, name := range workerQualificationChecks(c) {
		d.Checks[name] = backupQualificationObservation{Result: "passed", EvidenceHash: strings.Repeat("a", 64)}
	}
	proof := backupDraftProof(d, c)
	testutil.Must(t, s.put("qualification", proof))
	binding, e := backupQualificationBinding(j, c)
	testutil.Must(t, e)
	testutil.Must(t, s.put("qualification_binding", backupQualificationBindingRecord{1, binding, notify.Hash(proof)}))
	testutil.Must(t, s.put("run", WorkerRun{Complete: true, ConfigHash: c.ConfigHash, FinishedAt: time.Now().UTC()}))
	active, starts, validations := false, 0, 0
	deps := activationDependencies{
		validate: func(c WorkerInstallation, s *backupState) ([]string, error) {
			validations++
			return validateWorkerActivation(c, s, func(string, WorkerInstallation, *backupState, backupNotificationOwner) error { return nil }, func(source.WorkerSource, config.WorkerTimeouts) error { return nil })
		},
		unitsActive: func(context.Context, []string) (bool, error) { return active, nil },
		enable: func(context.Context, []string) error {
			starts++
			active = true
			return errors.New("lost manager response")
		},
	}
	call := func() (ActivationResult, error) {
		return activateDeclaredWorker(ctx, home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256, deps)
	}
	if _, err := call(); err == nil || starts != 1 {
		t.Fatal(starts, err)
	}
	if err := requireSettledDeclaredActivation(home); err == nil {
		t.Fatal("pending activation did not fence update")
	}
	if _, err := QualifyDeclaredWorker(ctx, home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256, "begin", install.QualificationObservation{}); err == nil {
		t.Fatal("qualification changed during pending activation")
	}
	result, err := call()
	if err != nil || result.State != "active" || starts != 1 {
		t.Fatal(result, starts, err)
	}
	testutil.Must(t, requireSettledDeclaredActivation(home))
	before := validations
	result, err = call()
	if err != nil || !result.Unchanged || starts != 1 || validations != before {
		t.Fatal("completed retry restarted or reran restrictions", result, err)
	}
	active = false
	if _, err = call(); err == nil || starts != 1 {
		t.Fatal("stopped unit silently restarted", starts, err)
	}
}

func TestDeclaredActivationUnitOwnershipInspection(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	unit := "deploy-backup.timer"
	script := "#!/bin/sh\nprintf 'LoadState=loaded\\nActiveState=%s\\nUnitFileState=enabled\\nFragmentPath=%s\\nDropInPaths=%s\\nNames=%s\\n' \"$TEST_ACTIVATION_ACTIVE\" \"$TEST_ACTIVATION_FRAGMENT\" \"$TEST_ACTIVATION_DROPINS\" \"$TEST_ACTIVATION_NAMES\"\n"
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("TEST_ACTIVATION_ACTIVE", "active")
	t.Setenv("TEST_ACTIVATION_FRAGMENT", filepath.Join(home, ".config/systemd/user", unit))
	t.Setenv("TEST_ACTIVATION_DROPINS", "")
	t.Setenv("TEST_ACTIVATION_NAMES", unit)
	if active, err := declaredUnitsActive(context.Background(), home, []string{unit}); err != nil || !active {
		t.Fatal(active, err)
	}
	t.Setenv("TEST_ACTIVATION_ACTIVE", "inactive")
	if active, err := declaredUnitsActive(context.Background(), home, []string{unit}); err != nil || active {
		t.Fatal(active, err)
	}
	for _, change := range [][2]string{{"TEST_ACTIVATION_FRAGMENT", "/etc/systemd/user/foreign.timer"}, {"TEST_ACTIVATION_DROPINS", "/etc/systemd/user/deploy-backup.timer.d/override.conf"}, {"TEST_ACTIVATION_NAMES", unit + " other.timer"}} {
		t.Run(change[0], func(t *testing.T) {
			t.Setenv(change[0], change[1])
			if _, err := declaredUnitsActive(context.Background(), home, []string{unit}); err == nil {
				t.Fatal("foreign loaded unit accepted")
			}
		})
	}
}

func TestDeclaredActivationRejectsExpiredProofAndFailedOrFutureRun(t *testing.T) {
	home, s, j, c := declaredQualificationFixture(t)
	ctx := context.Background()
	d := backupQualificationDraft{CreatedAt: time.Now().UTC().Add(-8 * 24 * time.Hour), Checks: map[string]backupQualificationObservation{}}
	for _, name := range workerQualificationChecks(c) {
		d.Checks[name] = backupQualificationObservation{Result: "passed", EvidenceHash: strings.Repeat("a", 64)}
	}
	testutil.Must(t, s.put("qualification", backupDraftProof(d, c)))
	deps := activationDependencies{validate: func(WorkerInstallation, *backupState) ([]string, error) {
		t.Fatal("expired proof reached activation")
		return nil, nil
	}}
	if _, err := activateDeclaredWorker(ctx, home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256, deps); err == nil {
		t.Fatal("expired proof accepted")
	}
	d.CreatedAt = time.Now().UTC()
	testutil.Must(t, s.put("qualification", backupDraftProof(d, c)))
	for _, run := range []WorkerRun{
		{Complete: true, ConfigHash: c.ConfigHash, FinishedAt: time.Now().Add(time.Hour)},
		{Complete: true, ConfigHash: c.ConfigHash, FinishedAt: time.Now(), Failures: map[string]string{"source": "failed"}},
		{Complete: true, ConfigHash: strings.Repeat("b", 64), FinishedAt: time.Now()},
	} {
		testutil.Must(t, s.put("run", run))
		if _, err := validateWorkerActivation(c, s, func(string, WorkerInstallation, *backupState, backupNotificationOwner) error { return nil }, func(source.WorkerSource, config.WorkerTimeouts) error {
			t.Fatal("invalid run reached source test")
			return nil
		}); err == nil {
			t.Fatal("invalid manual run accepted")
		}
	}
}
