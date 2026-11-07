//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestSharedBackupPolicyHistoryRetainsInterruptedCompletion(t *testing.T) {
	home, r, original, deps, _, profile := providerScopeFixture(t)
	r.Desired.Notification.Providers = original.Declaration.Desired.Notification.Providers
	r.Desired.Notification.Limits.MaxMessages = 9000
	deps.credentials = slices.Clone(profile.Credentials)
	_, err := reconcileNotificationScopeReports(context.Background(), home, r, func(ProducerReservation) (string, error) {
		profile.ManifestHash = notify.Hash("new limits")
		return profile.ManifestHash, nil
	}, 17, deps)
	testutil.Must(t, err)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	dir, err := root.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, err)
	defer dir.Close()
	var j WorkerSetupJournal
	testutil.Must(t, dir.ReadJSON("setup.json", &j))
	c, err := loadInstalledWorker(home)
	testutil.Must(t, err)
	originalRuntime := c
	paths, err := declarationPaths(home, j.Declaration)
	testutil.Must(t, err)
	cutover, err := os.ReadFile(filepath.Join(paths.State, backupCutoverFile))
	testutil.Must(t, err)
	for cycle := range 3 {
		oldHash := notify.Hash(j)
		var desired config.DesiredWorkers
		raw, err := json.Marshal(j.Declaration.Desired)
		testutil.Must(t, err)
		testutil.Must(t, json.Unmarshal(raw, &desired))
		policy := c.Policy
		if cycle == 0 {
			desired.Backup.Storage.MinFreeBytes += 1024
			policy.Storage.MinFreeBytes = desired.Backup.Storage.MinFreeBytes
		} else if cycle == 1 {
			desired.Backup.MaxSnapshotBytes += 1048576
			policy.MaxSnapshotBytes = desired.Backup.MaxSnapshotBytes
		}
		d := *j.Declaration
		d.Desired = &desired
		d.DesiredHash, err = desired.Fingerprint()
		testutil.Must(t, err)
		request := WorkerSetupRequest{Schema: 1, Operation: "apply", Declaration: &d, Policy: policy, Image: c.Image, RelayImage: c.RelayImage}
		for _, source := range c.Sources {
			known, err := os.ReadFile(source.KnownHosts)
			testutil.Must(t, err)
			request.Sources = append(request.Sources, SetupSource{source.Alias, source.Host, source.User, source.Port, string(known)})
		}
		if cycle == 2 {
			request.Sources[0].Host = "moved.invalid"
			request.Sources[0].Port = 2222
			// Endpoint changes do not change the worker policy fingerprint.
			// Projection must still walk this receipt to the original runtime.
			if d.DesiredHash != j.Declaration.DesiredHash {
				t.Fatal("endpoint test changed policy")
			}
		}
		changedArtifact := request
		changedArtifact.Image = "registry.invalid/replacement@sha256:" + notify.Hash("different artifact")
		probe := j
		if e := prepareSharedPolicyHistory(root, &probe, changedArtifact); e == nil || probe.SharedPolicyUpdate != nil {
			t.Fatal("artifact replacement accepted as policy continuation")
		}
		testutil.Must(t, beginWorkerSetupUpdate(root, dir, &j, request))
		if j.SharedPolicyUpdate == nil {
			t.Fatal("missing update predecessor")
		}
		changedPending := j
		changedPending.RequestHash = notify.Hash("different pending request")
		if e := checkSharedPolicyUpdate(changedPending); e == nil {
			t.Fatal("pending policy was retargeted")
		}

		pending, err := json.Marshal(j)
		testutil.Must(t, err)
		beforeRuntime, err := readBackupOwnedFile(root, paths.OwnedPath(paths.Runtime), safefs.MaxRecordBytes)
		testutil.Must(t, err)
		if cycle == 2 {
			c.Sources = slices.Clone(c.Sources)
			c.Sources[0].Host, c.Sources[0].Port = request.Sources[0].Host, request.Sources[0].Port
		}
		c.Policy, c.ConfigHash = policy, ""
		qualified := c.Qualification
		c.Qualification = map[string]bool{}
		c.ConfigHash = notify.Hash(c)
		c.Qualification = qualified
		raw, err = json.Marshal(c)
		testutil.Must(t, err)
		testutil.Must(t, os.WriteFile(paths.Runtime, raw, 0600))
		j.Files[paths.OwnedPath(paths.Runtime)] = setupFileChange{Before: beforeRuntime, Existed: true, AfterHash: sharedSetupDigest(raw)}
		// This isolates journal publication; the CLI fixture exercises archive,
		// container runtime and service publication through the real transaction.
		pending, err = json.Marshal(j)
		testutil.Must(t, err)
		testutil.Must(t, commitWorkerSetupJournal(dir, &j))
		completed := j
		if j.SharedPolicyHistory == nil || j.SharedPolicyUpdate != nil {
			t.Fatal("completion history missing")
		}
		// The immutable receipt survived, but the outer completion reply did not.
		j = WorkerSetupJournal{}
		testutil.Must(t, json.Unmarshal(pending, &j))
		testutil.Must(t, dir.WriteJSON("setup.json", j, true))
		testutil.Must(t, commitWorkerSetupJournal(dir, &j))
		if notify.Hash(j) != notify.Hash(completed) {
			t.Fatal("lost reply replaced completion")
		}
		if cycle == 2 {
			receipt, e := readSharedPolicyHistory(root, j.SharedPolicyHistory)
			testutil.Must(t, e)
			if receipt.Intent.Version != 2 {
				t.Fatal("endpoint history lost its type")
			}
			var oldRuntime WorkerInstallation
			testutil.Must(t, json.Unmarshal(receipt.Intent.Runtime, &oldRuntime))
			_, projected, e := reportingOrigin(root, j, c, receipt.Intent.Before.Declaration.DesiredHash, receipt.Intent.Before.Declaration.NotificationDesiredHash, notify.Hash(oldRuntime), receipt.Intent.Before.Declaration.HelperSHA256)
			testutil.Must(t, e)
			if !reflect.DeepEqual(projected, oldRuntime) {
				t.Fatal("endpoint-only projection stopped at matching policy")
			}
			receipt.Intent.Version = 1
			if receipt.validate() == nil {
				t.Fatal("endpoint continuation accepted as ordinary policy")
			}
		}
		predecessor, handled, err := sharedPolicyPredecessor(root, j)
		testutil.Must(t, err)
		if !handled || notify.Hash(predecessor.Intent.Before) != oldHash {
			t.Fatal("wrong backup predecessor")
		}
		origin, projected, err := reportingOrigin(root, j, c, original.Declaration.DesiredHash, original.Declaration.NotificationDesiredHash, notify.Hash(originalRuntime), original.Declaration.HelperSHA256)
		testutil.Must(t, err)
		if origin.RequestHash != original.RequestHash || !reflect.DeepEqual(projected, originalRuntime) {
			t.Fatal("reporting origin was replaced")
		}
		changed := c
		changed.Policy.MaxSnapshotBytes++
		if _, _, e := reportingOrigin(root, j, changed, original.Declaration.DesiredHash, original.Declaration.NotificationDesiredHash, notify.Hash(originalRuntime), original.Declaration.HelperSHA256); e == nil {
			t.Fatal("unrecorded policy accepted")
		}
		name := "shared-policy-" + j.SharedPolicyHistory.Operation + ".json"
		receipt, err := safefs.ReadFile(dir, name, safefs.MaxRecordBytes)
		testutil.Must(t, err)
		testutil.Must(t, os.Remove(filepath.Join(home, ".local/state/deploy/backup-worker", name)))
		if _, _, e := reportingOrigin(root, j, c, original.Declaration.DesiredHash, original.Declaration.NotificationDesiredHash, notify.Hash(originalRuntime), original.Declaration.HelperSHA256); e == nil {
			t.Fatal("lost receipt reconstructed")
		}
		testutil.Must(t, os.WriteFile(filepath.Join(home, ".local/state/deploy/backup-worker", name), receipt, 0600))
	}
	retained, err := os.ReadFile(filepath.Join(paths.State, backupCutoverFile))
	testutil.Must(t, err)
	if string(retained) != string(cutover) {
		t.Fatal("original handoff was rewritten")
	}
}
