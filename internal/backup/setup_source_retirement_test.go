//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func sourceRetirementFixture(t *testing.T, actualHelper bool) (string, WorkerSetupRequest, WorkerSetupRequest) {
	t.Helper()
	home, before, _ := sourceExpansionFixtureWithHelper(t, actualHelper, "source", "retire")
	raw, err := json.Marshal(before)
	testutil.Must(t, err)
	var r WorkerSetupRequest
	testutil.Must(t, json.Unmarshal(raw, &r))
	makeSourceRetirementRequest(t, before, &r)
	return home, before, r
}

func makeSourceRetirementRequest(t *testing.T, before WorkerSetupRequest, r *WorkerSetupRequest) {
	t.Helper()
	r.Policy = before.Policy
	r.Policy.Sources = []string{"source"}
	r.Sources = append([]SetupSource(nil), before.Sources[:1]...)
	delete(r.Declaration.Sources, "retire")
	delete(r.Declaration.Sources, "extra")
	r.Declaration.Desired.Backup.Sources = []string{"source"}
	r.Declaration.Desired.Backup.RetireSources = map[string]string{"retire": "service decommissioned"}
	var err error
	r.Declaration.DesiredHash, err = r.Declaration.Desired.Fingerprint()
	testutil.Must(t, err)
}

func TestBackupSourceRetirementInstallsAndRecoversPublication(t *testing.T) {
	testBackupPolicyPublication(t, "retirement")
}

func verifyRetirementPublication(t *testing.T, home string, root, stateRoot *safefs.Directory, j *WorkerSetupJournal, r WorkerSetupRequest) {
	t.Helper()
	if j.SourceRetirement == nil {
		t.Fatal("publication forgot pending source revocation")
	}
	before := j.SourceRetirement.Before
	s := j.SourceRetirement.Sources["retire"]
	observed, err := inspectSetupLocked(context.Background(), root, home)
	wantStage := "source_retirement_pending"
	if j.SourceRetirement.Version == 2 {
		wantStage = "source_replacement_pending"
	}
	if err != nil || observed.Stage != wantStage {
		t.Fatal(observed, err)
	}
	state, err := openInstalledBackupState(home, false)
	testutil.Must(t, err)
	policy := r.Policy.Notifications
	policy.Failure = "immediate"
	testutil.Must(t, state.observeIncident("discovery", s.SourceID, "offline before retirement", time.Now().UTC(), policy))
	testutil.Must(t, state.observeIncident("discovery", before.SourceEnrollments["source"].SourceID, "unrelated failure", time.Now().UTC(), policy))
	state.Close()
	for range 2 {
		testutil.Must(t, prepareSourceRetirementDisposition(home, root, *j))
	}
	state, err = openInstalledBackupState(home, false)
	testutil.Must(t, err)
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	if control.RetiredSources[s.SourceID] != s.Reason {
		t.Fatal("retirement not archive backed")
	}
	var active, dispositions, recoveries int
	testutil.Must(t, state.db.QueryRow("SELECT count(*) FROM incidents WHERE resolved IS NULL").Scan(&active))
	testutil.Must(t, state.db.QueryRow("SELECT count(*) FROM meta WHERE key LIKE 'incident_resolution/%' AND json_extract(value,'$.disposition')='retired-by-operator'").Scan(&dispositions))
	testutil.Must(t, state.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='recovery'").Scan(&recoveries))
	state.Close()
	if active != 1 || dispositions != 1 || recoveries != 0 {
		t.Fatal("retirement lost unrelated failure or fabricated recovery", active, dispositions, recoveries)
	}
	if err = commitSourceRetirement(stateRoot, j, nil); err == nil {
		t.Fatal("unconfirmed retirement completed")
	}
	acks := map[string]source.SourceEnrollmentResult{"retire": {Schema: 1, WorkerID: j.WorkerID, SourceID: s.SourceID}}
	var pending []byte
	if j.SharedPolicyUpdate != nil {
		if j.SharedPolicyUpdate.Version == 5 {
			if j.SharedPolicyExpansion == nil {
				t.Fatal("published replacement lost enrollment history")
			}
			changed := *j
			changed.SharedPolicyExpansion = nil
			if checkSharedPolicyUpdate(changed) == nil {
				t.Fatal("published replacement accepted missing enrollment history")
			}
		}
		if !reflect.DeepEqual(j.SharedPolicyHistory, j.SharedPolicyUpdate.Before.SharedPolicyHistory) {
			t.Fatal("unconfirmed retirement already advanced history")
		}
		pending, err = json.Marshal(j)
		testutil.Must(t, err)
	}
	testutil.Must(t, commitSourceRetirement(stateRoot, j, acks))
	if pending != nil {
		completed := notify.Hash(j)
		*j = WorkerSetupJournal{}
		testutil.Must(t, json.Unmarshal(pending, j))
		testutil.Must(t, stateRoot.WriteJSON("setup.json", j, true))
		testutil.Must(t, commitSourceRetirement(stateRoot, j, acks))
		if notify.Hash(j) != completed {
			t.Fatal("lost completion reply replaced retirement history")
		}
	}
	if j.SourceRetirement != nil || j.RetiredSources["retire"] != s || len(j.SourceKeys) != len(r.Sources) || len(j.SourceEnrollments) != len(r.Sources) {
		t.Fatal("retirement did not preserve completed disposition")
	}
	testutil.Must(t, verifySetupSourceKeys(root, *j))
	testutil.Must(t, validateSourceRetirementHistory(*j))
	changed := *j
	changed.RetiredSources = nil
	if err = validateSourceRetirementHistory(changed); err == nil {
		t.Fatal("lost retirement history accepted")
	}
	for _, suffix := range []string{".key", ".known_hosts"} {
		if _, err = os.Stat(filepath.Join(home, workerTrustDirectory, "retire"+suffix)); err != nil {
			t.Fatal("retirement deleted retained key/trust history", err)
		}
	}
	if j.WorkerID != before.WorkerID || j.ArchiveID != before.ArchiveID {
		t.Fatal("retirement changed worker/archive identity")
	}
}

func TestSourceRetirementCannotReserveFreshInstallation(t *testing.T) {
	home := t.TempDir()
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	d := &SetupDeclaration{Desired: &config.DesiredWorkers{Backup: &config.BackupDeclaration{RetireSources: map[string]string{"old": "decommissioned"}}}}
	before := policyReviewTree(t, home)
	if err = checkDeclarativeSetupReservation(root, d); err == nil || !reflect.DeepEqual(before, policyReviewTree(t, home)) {
		t.Fatal("fresh retirement created orphaned installation authority", err)
	}
}

func TestBackupSourceRetirementPreflightAndPendingPins(t *testing.T) {
	for _, damage := range []string{"", "missing-reason", "unknown-alias", "still-active", "source-replacement", "policy", "artifact", "endpoint", "trust", "missing-key", "shared"} {
		t.Run(damage, func(t *testing.T) {
			home, _, r := sourceRetirementFixture(t, false)
			root, state, _, _ := expansionRoots(t, home)
			var j WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &j))
			switch damage {
			case "missing-reason":
				r.Declaration.Desired.Backup.RetireSources = nil
			case "unknown-alias":
				r.Declaration.Desired.Backup.RetireSources["other"] = "not enrolled"
			case "still-active":
				r.Declaration.Desired.Backup.RetireSources["source"] = "still active"
			case "source-replacement":
				r.Sources[0].Alias = "replacement"
			case "policy":
				r.Policy.Schedule = "weekly"
			case "artifact":
				r.Declaration.HelperSHA256 = r.Declaration.Sources["source"]
			case "endpoint":
				r.Sources[0].Host = "other.invalid"
			case "trust":
				r.Sources[0].KnownHosts += "\n"
			case "missing-key":
				testutil.Must(t, os.Remove(filepath.Join(home, workerTrustDirectory, "retire.key")))
			case "shared":
				j.Producer = &ProducerReservation{}
			}
			var err error
			r.Declaration.DesiredHash, err = r.Declaration.Desired.Fingerprint()
			testutil.Must(t, err)
			before := policyReviewTree(t, home)
			err = beginWorkerSetupUpdate(root, state, &j, r)
			if damage == "" {
				testutil.Must(t, err)
				testutil.Must(t, checkSetupSourceRetirement(root, j, &r))
				changed := r
				changed.Sources = append([]SetupSource(nil), r.Sources...)
				changed.Sources[0].Host = "redirect.invalid"
				if err = checkSetupSourceRetirement(root, j, &changed); err == nil {
					t.Fatal("retargeted pending retirement")
				}
				j.SourceKeys["retire"] = "different-key"
				if err = checkSetupSourceRetirement(root, j, &r); err == nil {
					t.Fatal("changed retained revoked key")
				}
			} else if err == nil || !reflect.DeepEqual(before, policyReviewTree(t, home)) {
				t.Fatal("rejected retirement changed installation", err)
			}
		})
	}
}

func TestSharedSourceRetirementReservationIsReadOnly(t *testing.T) {
	home, before, r, _ := sharedSourceExpansionFixture(t, "source", "retire")
	makeSourceRetirementRequest(t, before, &r)
	root, state, _, _ := expansionRoots(t, home)
	var j WorkerSetupJournal
	testutil.Must(t, state.ReadJSON("setup.json", &j))
	producer := *j.Producer
	tree := policyReviewTree(t, home)
	got, err := inspectSetupUpdateReservation(root, state, &j, r)
	if err != nil || got != producer || j.SourceRetirement != nil || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("retirement reservation changed authority", err)
	}
	testutil.Must(t, beginWorkerSetupUpdate(root, state, &j, r))
	tree = policyReviewTree(t, home)
	got, err = inspectPendingSetupUpdateReservation(root, state, &j, r)
	if err != nil || got != producer || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("pending retirement cannot reuse retained producer", err)
	}
	for _, damage := range []string{"scope", "generation", "owner", "request"} {
		raw, err := json.Marshal(j)
		testutil.Must(t, err)
		var changed WorkerSetupJournal
		testutil.Must(t, json.Unmarshal(raw, &changed))
		switch damage {
		case "scope":
			changed.Declaration.NotificationDesiredHash = strings.Repeat("a", 64)
		case "generation":
			changed.Producer.Generation = testutil.ID(t, "g")
		case "owner":
			changed.Shared.Owner.CutoverHash = strings.Repeat("b", 64)
		case "request":
			changed.SourceRetirement.Request.Sources[0].Host = "other.invalid"
		}
		if _, err = inspectPendingSetupUpdateReservation(root, state, &changed, r); err == nil || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
			t.Fatal("changed retirement reservation admitted", damage, err)
		}
	}
}

func TestBackupSourceRetirementInterruptedChild(t *testing.T) {
	home := os.Getenv("DEPLOY_SOURCE_RETIREMENT_FIXTURE")
	if home == "" {
		t.Skip("subprocess helper")
	}
	root, state, _, _ := expansionRoots(t, home)
	var j WorkerSetupJournal
	var r WorkerSetupRequest
	testutil.Must(t, state.ReadJSON("setup.json", &j))
	testutil.Must(t, root.ReadJSON("retirement-request.json", &r))
	testutil.Must(t, beginWorkerSetupUpdate(root, state, &j, r))
	p, err := os.FindProcess(os.Getpid())
	testutil.Must(t, err)
	testutil.Must(t, p.Kill())
}

func TestBackupSourceRetirementProcessDeathRetainsIntent(t *testing.T) {
	for _, shared := range []bool{false, true} {
		var home string
		var before, r WorkerSetupRequest
		if shared {
			home, before, r, _ = sharedSourceExpansionFixture(t, "source", "retire")
			makeSourceRetirementRequest(t, before, &r)
		} else {
			home, before, r = sourceRetirementFixture(t, false)
		}
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		testutil.Must(t, root.WriteJSON("retirement-request.json", r, false))
		root.Close()
		cmd := exec.Command(os.Args[0], "-test.run=^TestBackupSourceRetirementInterruptedChild$")
		cmd.Env = append(os.Environ(), "DEPLOY_SOURCE_RETIREMENT_FIXTURE="+home)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(err.Error(), "killed") {
			t.Fatalf("child did not reach SIGKILL boundary: %v %s", err, out)
		}
		root, state, _, _ := expansionRoots(t, home)
		var j WorkerSetupJournal
		testutil.Must(t, state.ReadJSON("setup.json", &j))
		testutil.Must(t, checkSetupSourceRetirement(root, j, &r))
		if j.SourceRetirement == nil || !j.Updating || j.SourceKeys["retire"] == "" {
			t.Fatal("process death lost exact retirement authority")
		}
		if err = checkSetupSourceRetirement(root, j, &before); err == nil {
			t.Fatal("old YAML silently cancelled retirement")
		}
		if err = prepareSourceRetirementDisposition(home, root, j); err == nil {
			t.Fatal("source retired before runtime publication")
		}
	}
}
