//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestCompletedSharedPolicyUpdatePreservesOriginalCutoverAndNeverRestarts(t *testing.T) {
	for _, kind := range []string{"valid", "completion-missing", "scope-changed", "capture-missing", "archive-changed"} {
		t.Run(kind, func(t *testing.T) {
			home, c, desired, manifest, deps, session := freshSharedReportingFixture(t)
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			d, err := root.Sub(".local/state/deploy/backup-worker")
			testutil.Must(t, err)
			defer d.Close()
			var j WorkerSetupJournal
			testutil.Must(t, d.ReadJSON("setup.json", &j))
			j.Declaration.NotificationDesiredHash = notify.Hash("unchanged gateway policy")
			testutil.Must(t, d.WriteJSON("setup.json", j, true))
			testutil.Must(t, connectSharedReporting(context.Background(), home, desired, manifest, deps))
			state, err := openBackupState(backupCutoverPaths(home, c).State, false)
			testutil.Must(t, err)
			var owner backupNotificationOwner
			testutil.Must(t, state.get("notification_owner", &owner))
			originalFile := filepath.Join(state.directory, backupCutoverFile)
			original, err := os.ReadFile(originalFile)
			testutil.Must(t, err)
			// The existing setup transaction captures ownership before quiescing,
			// installs the new policy and preserves the permanent sender fence.
			j.Schema = 2
			j.Shared = &sharedWorkerSetup{Version: 1, Owner: owner, GatewayRoot: session.m.Root, Profile: install.ManagedReportProfile{ManifestHash: manifest, Identity: owner.Binding.Identity, UnitDirectory: session.m.UnitDirectory}}
			c.Policy.Schedule = "hourly"
			c.ConfigHash = notify.Hash("updated backup configuration")
			desired = notify.Hash("updated backup declaration")
			j.Declaration.DesiredHash = desired
			raw, err := json.Marshal(c)
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(filepath.Join(home, ".config/deploy/backup-worker/runtime.json"), raw, 0600))
			j.OwnedFiles[".config/deploy/backup-worker/runtime.json"] = sharedSetupDigest(raw)
			var control ArchiveControl
			testutil.Must(t, state.get("control", &control))
			control.ConfigHash = c.ConfigHash
			testutil.Must(t, state.put("control", control))
			testutil.Must(t, state.put("installation", "staged-awaiting-qualification"))
			switch kind {
			case "completion-missing":
				_, err = state.db.Exec("DELETE FROM meta WHERE key='notification_cutover_complete'")
				testutil.Must(t, err)
			case "scope-changed":
				j.Declaration.NotificationDesiredHash = notify.Hash("changed gateway")
			case "capture-missing":
				j.Shared = nil
				j.Schema = 1
			case "archive-changed":
				control.SharedRequired = false
				testutil.Must(t, state.put("control", control))
			}
			testutil.Must(t, state.Close())
			testutil.Must(t, d.WriteJSON("setup.json", j, true))
			services := deps.Services.(*cutoverFixtureServices)
			services.started = false
			deps.Managed = nil
			err = connectSharedReporting(context.Background(), home, desired, manifest, deps)
			if (err == nil) != (kind == "valid") {
				t.Fatal("unexpected shared policy continuation", kind, err)
			}
			if services.started {
				t.Fatal("inspection restarted delivery")
			}
			after, err := os.ReadFile(originalFile)
			testutil.Must(t, err)
			if string(original) != string(after) {
				t.Fatal("ordinary policy edit rewrote original delivery evidence")
			}
			if kind == "valid" {
				current, view, e := prepareFreshSharedReporting(context.Background(), home, desired, manifest)
				testutil.Must(t, e)
				if view.Version != 6 || view.Predecessor != owner.CutoverHash || view.CompletedPolicyHash != desired {
					t.Fatal("incorrect read-only policy view", view)
				}
				if runBackupCutover(context.Background(), home, current, view, false, backupCutoverDependencies{}) == nil {
					t.Fatal("read-only continuation admitted delivery mutation")
				}
			}
		})
	}
}

func TestSharedScheduleReservationReusesIdentityWithoutChangingSetup(t *testing.T) {
	home, c, _, _, _, _ := freshSharedReportingFixture(t)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	d, err := root.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, err)
	defer d.Close()
	var j WorkerSetupJournal
	testutil.Must(t, d.ReadJSON("setup.json", &j))
	j.Declaration.NotificationDesiredHash = notify.Hash("stable dependency")
	j.Declaration.Sources = map[string]string{}
	j.SourceEnrollments = map[string]source.SourceEnrollmentResult{}
	testutil.Must(t, root.Mkdir(".config/deploy/backup-worker/ssh"))
	keys, err := root.Sub(".config/deploy/backup-worker/ssh")
	testutil.Must(t, err)
	defer keys.Close()
	var sources []SetupSource
	for _, name := range c.Policy.Sources {
		_, err = setupSourceKey(home, keys, d, &j, name)
		testutil.Must(t, err)
		j.Declaration.Sources[name] = notify.Hash("source helper")
		id := testutil.ID(t, "s")
		j.SourceEnrollments[name] = source.SourceEnrollmentResult{Schema: 1, SourceID: id, WorkerID: j.WorkerID, Fingerprint: j.SourceKeys[name]}
		c.Sources = append(c.Sources, source.WorkerSource{Alias: name, Host: "source.invalid", User: "backup", Port: 22, SourceID: id, KeyFile: filepath.Join(home, ".config/deploy/backup-worker/ssh", name+".key"), KnownHosts: filepath.Join(home, ".config/deploy/backup-worker/ssh", name+".known_hosts")})
		sources = append(sources, SetupSource{Alias: name, Host: "source.invalid", User: "backup", Port: 22})
	}
	raw, err := json.Marshal(c)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(filepath.Join(home, ".config/deploy/backup-worker/runtime.json"), raw, 0600))
	j.OwnedFiles[".config/deploy/backup-worker/runtime.json"] = sharedSetupDigest(raw)
	testutil.Must(t, d.WriteJSON("setup.json", j, true))
	before := notify.Hash(j)
	next := *j.Declaration
	next.DesiredHash = notify.Hash("new backup schedule")
	request := WorkerSetupRequest{Schema: 1, Operation: "reserve", Declaration: &next, Policy: c.Policy, Image: c.Image, RelayImage: c.RelayImage, Sources: sources}
	request.Policy.Schedule = "hourly"
	r, err := inspectSetupUpdateReservation(root, d, &j, request)
	testutil.Must(t, err)
	if r != *j.Producer || notify.Hash(j) != before {
		t.Fatal("reservation changed setup or producer")
	}
	var retained WorkerSetupJournal
	testutil.Must(t, d.ReadJSON("setup.json", &retained))
	if notify.Hash(retained) != before {
		t.Fatal("reservation published an uninstalled policy")
	}
	// Simulate a disconnect after the update intent is durable and before the
	// recovery gate is armed. The existing reservation permits backup resume,
	// while ordinary gateway inspection must still refuse this pending setup.
	pending := j
	pending.PreviousDeclaration = j.Declaration
	pending.PreviousRequestHash = j.RequestHash
	pending.Declaration = request.Declaration
	pending.RequestHash = workerSetupRequestHash(request)
	pending.Updating, pending.Stage = true, "prepared"
	testutil.Must(t, d.WriteJSON("setup.json", pending, true))
	for _, damage := range []string{"", "different-request", "missing-predecessor", "helper", "scope", "recovery", "runtime"} {
		t.Run(damage, func(t *testing.T) {
			raw, e := json.Marshal(pending)
			testutil.Must(t, e)
			var candidate WorkerSetupJournal
			testutil.Must(t, json.Unmarshal(raw, &candidate))
			proposed := request
			switch damage {
			case "different-request":
				proposed.Policy.Schedule = "weekly"
			case "missing-predecessor":
				candidate.PreviousDeclaration = nil
			case "helper":
				candidate.PreviousDeclaration.HelperSHA256 = notify.Hash("another helper")
			case "scope":
				candidate.PreviousDeclaration.NotificationDesiredHash = notify.Hash("another scope")
			case "recovery":
				candidate.RecoveryNeeded = true
			case "runtime":
				candidate.OwnedFiles[".config/deploy/backup-worker/runtime.json"] = notify.Hash("changed bytes")
			}
			tree := policyReviewTree(t, home)
			reservation, e := inspectPendingSetupUpdateReservation(root, d, &candidate, proposed)
			if (e == nil) != (damage == "") || e == nil && reservation != *j.Producer || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
				t.Fatal("pending reservation changed ownership or admitted changed pins", reservation, e)
			}
		})
	}
	if _, _, err = InspectReservedProducer(home, j.Declaration.NotificationDesiredHash, j.Declaration.NotificationRoot, j.Declaration.NotificationRoute); err == nil {
		t.Fatal("pending backup update admitted as completed gateway dependency")
	}
	testutil.Must(t, d.WriteJSON("setup.json", j, true))
	next.NotificationDesiredHash = notify.Hash("different gateway scope")
	if _, err = inspectSetupUpdateReservation(root, d, &j, request); err == nil {
		t.Fatal("gateway change accepted as a specialist-only edit")
	}
	next.NotificationDesiredHash = j.Declaration.NotificationDesiredHash
	request.Policy.Storage.Path = filepath.Join(home, "different-archive")
	if _, err = inspectSetupUpdateReservation(root, d, &j, request); err == nil {
		t.Fatal("archive migration accepted as ordinary update")
	}
}
