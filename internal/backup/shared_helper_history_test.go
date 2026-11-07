//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/backup/helper"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func sharedHelperHistoryFixture(t *testing.T) (string, WorkerSetupRequest, WorkerSetupRequest, func()) {
	t.Helper()
	bin := t.TempDir()
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	var candidate string
	home, before, after, _ := sharedSourceExpansionFixtureWithHook(t, nil, func(home string, root, state *safefs.Directory, j *WorkerSetupJournal) {
		candidate = j.Declaration.HelperSHA256
		// Distinct offline artifacts, with the current test binary as the new
		// candidate. This exercises real helper publication and rollback.
		body, err := os.ReadFile("/proc/self/exe")
		testutil.Must(t, err)
		body = append(body, []byte("old offline fixture artifact")...)
		old := sharedSetupDigest(body)
		path := filepath.Join(home, ".local/lib/deploy/backup-worker")
		release := filepath.Join(path, "releases", old)
		testutil.Must(t, os.MkdirAll(release, 0700))
		testutil.Must(t, os.WriteFile(filepath.Join(release, "deploy"), body, 0700))
		testutil.Must(t, os.WriteFile(filepath.Join(path, "deploy"), body, 0700))
		d, err := safefs.Open(path)
		testutil.Must(t, err)
		testutil.Must(t, d.WriteJSON("artifact.json", helper.HelperArtifactManifest{Schema: 1, Role: "backup-worker", Current: old}, true))
		d.Close()
		j.Declaration.HelperSHA256 = old
		j.ArchivePath = j.Declaration.Desired.Backup.Storage.Path
		testutil.Must(t, installRecoverySupervisorFrom(home, root, state, j, filepath.Join(path, "deploy")))
	})
	raw, err := json.Marshal(before)
	testutil.Must(t, err)
	var replacement WorkerSetupRequest
	testutil.Must(t, json.Unmarshal(raw, &replacement))
	// Preserve the declaration pointer retained by the cutover assertion.
	*after.Declaration = *replacement.Declaration
	replacement.Declaration = after.Declaration
	after = replacement
	after.Declaration.HelperSHA256 = candidate
	after.Declaration.Desired.Backup.Artifacts.HelperSHA256 = candidate
	after.Declaration.DesiredHash, err = after.Declaration.Desired.Fingerprint()
	testutil.Must(t, err)
	originalBytes, err := os.ReadFile(filepath.Join(home, ".local/state/deploy/backup-worker/state", backupCutoverFile))
	testutil.Must(t, err)
	return home, before, after, func() {
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		defer root.Close()
		var j WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &j))
		r, err := readSharedPolicyHistory(root, j.SharedPolicyHistory)
		testutil.Must(t, err)
		if r.Intent.Version != 7 || r.Version != 6 || r.Helper == nil || r.Helper.Current != candidate || r.Helper.Previous != before.Declaration.HelperSHA256 {
			t.Fatal("helper completion lost old/new artifacts")
		}
		state, err := root.Sub(".local/state/deploy/backup-worker")
		testutil.Must(t, err)
		defer state.Close()
		// Model a lost outer completion write: immutable receipt and published
		// artifacts are present, but the setup journal still owns the same intent.
		pending := r.After
		pending.SharedPolicyUpdate = &r.Intent
		pending.Stage, pending.Updating = "installing", true
		pending.PreviousRequestHash, pending.PreviousDeclaration = r.Intent.Before.RequestHash, r.Intent.Before.Declaration
		testutil.Must(t, state.WriteJSON("setup.json", pending, true))
		receiptName := "shared-policy-" + r.Intent.Operation + ".json"
		receiptBytes, err := safefs.ReadFile(state, receiptName, safefs.MaxRecordBytes)
		testutil.Must(t, err)
		testutil.Must(t, commitWorkerSetupJournal(state, &pending))
		if notify.Hash(pending) != notify.Hash(j) {
			t.Fatal("completion retry replaced helper history")
		}
		afterBytes, err := safefs.ReadFile(state, receiptName, safefs.MaxRecordBytes)
		testutil.Must(t, err)
		if string(afterBytes) != string(receiptBytes) {
			t.Fatal("completion retry rewrote immutable receipt")
		}
		cycled := j
		cycled.NotificationScopeHistory = &setupNotificationScopeReference{Operation: testutil.ID(t, "n"), Receipt: notify.Hash("later notification scope")}
		if _, handled, err := sharedPolicyPredecessor(root, cycled); err != nil || handled {
			t.Fatal("backup receipt skipped a newer notification scope", handled, err)
		}
		bad := r
		bad.Helper = nil
		if bad.validate() == nil {
			t.Fatal("helper history accepted missing artifact receipt")
		}
		for _, missing := range []string{"before", "after"} {
			raw, err := json.Marshal(r.Intent)
			testutil.Must(t, err)
			var altered sharedPolicyUpdate
			testutil.Must(t, json.Unmarshal(raw, &altered))
			if missing == "before" {
				altered.Before.Declaration.Desired = nil
			} else {
				altered.After.Declaration.Desired = nil
			}
			if altered.validate() == nil {
				t.Fatal("helper upgrade accepted missing declaration", missing)
			}
		}
		c, err := loadInstalledWorker(home)
		testutil.Must(t, err)
		var original backupCutoverIntent
		d, err := safefs.Open(backupCutoverPaths(home, c).State)
		testutil.Must(t, err)
		testutil.Must(t, d.ReadJSON(backupCutoverFile, &original))
		d.Close()
		if original.Spec.Version != 5 || original.Spec.HelperHash != before.Declaration.HelperSHA256 {
			t.Fatal("upgrade rewrote original reporting helper")
		}
		_, _, err = reportingOrigin(root, j, c, original.Spec.Fresh.DesiredHash, original.Spec.Fresh.NotificationDesiredHash, original.Spec.ConfigHash, original.Spec.HelperHash)
		testutil.Must(t, err)
		retained, err := os.ReadFile(filepath.Join(backupCutoverPaths(home, c).State, backupCutoverFile))
		testutil.Must(t, err)
		if string(retained) != string(originalBytes) {
			t.Fatal("helper upgrade rewrote cutover")
		}
		spec := original.Spec
		spec.Version, spec.HelperHash, spec.Predecessor = 8, candidate, notify.Hash(original)
		spec.ConfigHash, spec.CompletedPolicyHash = notify.Hash(c), j.Declaration.DesiredHash
		testutil.Must(t, requireFreshBackupReportingSetup(root, home, j, spec))
		complete, err := inspectFreshReportingCompletion(context.Background(), home, c, spec, backupCutoverDependencies{Completed: func(context.Context, string, WorkerInstallation, backupCutoverSpec, backupNotificationOwner) error {
			return nil
		}})
		testutil.Must(t, err)
		if !complete {
			t.Fatal("helper continuation lost completed reporting")
		}
		if err := runBackupCutover(context.Background(), home, c, spec, false, backupCutoverDependencies{}); err == nil {
			t.Fatal("inspection view admitted a new cutover")
		}
		if notify.Hash(j.Producer) != original.Spec.Fresh.ReservationHash {
			t.Fatal("helper upgrade replaced producer")
		}
	}
}

func TestSharedBackupHelperPublishesAndRollsBack(t *testing.T) {
	testBackupPolicyPublication(t, "shared-history-helper")
}

func TestSharedBackupHelperReservation(t *testing.T) {
	for _, change := range []string{"helper", "helper-image", "policy", "source-helper", "endpoint", "receiver", "receiver-lock", "artifact-drift"} {
		t.Run(change, func(t *testing.T) {
			home, _, request, _ := sharedHelperHistoryFixture(t)
			switch change {
			case "helper-image":
				request.Image = "fixture.invalid/backup@sha256:" + notify.Hash("new image")
				request.Declaration.Desired.Backup.Artifacts.Image = request.Image
			case "policy":
				request.Policy.MaxSnapshotBytes++
				request.Declaration.Desired.Backup.MaxSnapshotBytes = request.Policy.MaxSnapshotBytes
			case "source-helper":
				request.Declaration.Sources[request.Sources[0].Alias] = notify.Hash("new source helper")
			case "endpoint":
				request.Sources[0].Host = "elsewhere.invalid"
			case "receiver", "receiver-lock":
				name := commandReceiverFile
				if change == "receiver-lock" {
					name = "commands.lock"
				}
				testutil.Must(t, os.WriteFile(filepath.Join(home, ".local/state/deploy/backup-worker/state", name), []byte("retained authority"), 0600))
			case "artifact-drift":
				testutil.Must(t, os.WriteFile(filepath.Join(home, ".local/lib/deploy/backup-worker/deploy"), []byte("changed outside setup"), 0700))
			}
			var err error
			request.Declaration.DesiredHash, err = request.Declaration.Desired.Fingerprint()
			testutil.Must(t, err)
			root, state, _, _ := expansionRoots(t, home)
			var j WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &j))
			tree, original := policyReviewTree(t, home), notify.Hash(j)
			request.Operation = "reserve"
			producer, err := inspectSetupUpdateReservation(root, state, &j, request)
			if (err == nil) != (change == "helper" || change == "helper-image") {
				t.Fatal(change, err)
			}
			if notify.Hash(j) != original || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
				t.Fatal("helper review wrote authority")
			}
			if err != nil {
				return
			}
			request.Operation = "apply"
			testutil.Must(t, beginWorkerSetupUpdate(root, state, &j, request))
			request.Operation = "reserve"
			tree = policyReviewTree(t, home)
			retained, err := inspectPendingSetupUpdateReservation(root, state, &j, request)
			testutil.Must(t, err)
			if retained != producer || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
				t.Fatal("pending helper review changed authority")
			}
			request.Image = "fixture.invalid/backup@sha256:" + notify.Hash("different retry")
			if _, err := inspectPendingSetupUpdateReservation(root, state, &j, request); err == nil {
				t.Fatal("helper retry accepted changed request")
			}
		})
	}
}
