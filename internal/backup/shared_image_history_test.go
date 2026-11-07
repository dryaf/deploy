//go:build linux

package backup

import (
	"encoding/json"
	"reflect"
	"testing"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestSharedBackupImageReservationReviewsOnlyTheImage(t *testing.T) {
	for _, change := range []string{"image", "policy", "source-helper", "endpoint", "unpinned", "disabled"} {
		t.Run(change, func(t *testing.T) {
			home, before, _, _ := sharedSourceExpansionFixture(t)
			raw, err := json.Marshal(before)
			testutil.Must(t, err)
			var request WorkerSetupRequest
			testutil.Must(t, json.Unmarshal(raw, &request))
			request.Image = "fixture.invalid/backup@sha256:" + notify.Hash("replacement image")
			request.Declaration.Desired.Backup.Artifacts.Image = request.Image
			switch change {
			case "policy":
				request.Policy.MaxSnapshotBytes++
				request.Declaration.Desired.Backup.MaxSnapshotBytes = request.Policy.MaxSnapshotBytes
			case "source-helper":
				request.Declaration.Sources[request.Sources[0].Alias] = notify.Hash("changed source helper")
			case "endpoint":
				request.Sources[0].Host = "elsewhere.invalid"
			case "disabled":
				disabled := false
				request.Declaration.Desired.Backup.Enabled = &disabled
			case "unpinned":
				request.Image = "fixture.invalid/backup:latest"
			}
			request.Declaration.DesiredHash, err = request.Declaration.Desired.Fingerprint()
			testutil.Must(t, err)
			root, state, _, _ := expansionRoots(t, home)
			var j WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &j))
			tree := policyReviewTree(t, home)
			original := notify.Hash(j)
			request.Operation = "reserve"
			producer, err := inspectSetupUpdateReservation(root, state, &j, request)
			if (err == nil) != (change == "image") {
				t.Fatal(change, err)
			}
			if notify.Hash(j) != original || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
				t.Fatal("image review changed retained authority")
			}
			if err == nil {
				if producer != *j.Producer {
					t.Fatal("image review replaced producer")
				}
				request.Operation = "apply"
				testutil.Must(t, beginWorkerSetupUpdate(root, state, &j, request))
				if j.SharedPolicyUpdate == nil || j.SharedPolicyUpdate.Version != 6 || j.SharedPolicyUpdate.Before.NotificationScopeHistory != nil {
					t.Fatal("first image update lost its original reporting predecessor")
				}
				request.Operation = "reserve"
				tree = policyReviewTree(t, home)
				retained, e := inspectPendingSetupUpdateReservation(root, state, &j, request)
				testutil.Must(t, e)
				if retained != producer || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
					t.Fatal("pending image review changed producer or retained files")
				}
			}
		})
	}
}
