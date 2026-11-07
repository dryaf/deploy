//go:build linux

package backup

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/testutil"
)

func makeCombinedSourceRequest(t *testing.T, before WorkerSetupRequest, r *WorkerSetupRequest) {
	t.Helper()
	extra := r.Sources[len(r.Sources)-1]
	helper := r.Declaration.Sources[extra.Alias]
	makeSourceRetirementRequest(t, before, r)
	r.Sources = append(r.Sources, extra)
	r.Policy.Sources = append(r.Policy.Sources, extra.Alias)
	r.Declaration.Sources[extra.Alias] = helper
	r.Declaration.Desired.Backup.Sources = append([]string(nil), r.Policy.Sources...)
	var err error
	r.Declaration.DesiredHash, err = r.Declaration.Desired.Fingerprint()
	testutil.Must(t, err)
}

func TestBackupCombinedSourceReplacementPinsBothSides(t *testing.T) {
	for _, damage := range []string{"", "new-endpoint", "new-trust", "old-disposition", "old-key", "extra-key", "missing-retirement", "changed-predecessor"} {
		t.Run(damage, func(t *testing.T) {
			home, before, r := sourceExpansionFixtureWithHelper(t, false, "source", "retire")
			makeCombinedSourceRequest(t, before, &r)
			root, state, keys, close := expansionRoots(t, home)
			defer close()
			var j WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &j))
			testutil.Must(t, beginWorkerSetupUpdate(root, state, &j, r))
			if j.SourceExpansion == nil || j.SourceRetirement == nil || j.SourceExpansion.Version != 2 || j.SourceRetirement.Version != 2 {
				t.Fatal("combined edit lost one intent")
			}
			testutil.Must(t, checkSetupSourceRetirement(root, j, &r))
			_, err := setupSourceKey(home, keys, state, &j, "extra")
			testutil.Must(t, err)
			// A reserved key is retained before the external enrollment reply arrives.
			testutil.Must(t, checkSetupSourceRetirement(root, j, &r))
			testutil.Must(t, checkSetupSourceExpansion(root, j, &r))
			e := source.SourceEnrollmentResult{Schema: 1, WorkerID: j.WorkerID, SourceID: testutil.ID(t, "s"), Fingerprint: j.SourceKeys["extra"]}
			testutil.Must(t, recordSetupSourceEnrollment(state, &j, "extra", e))
			raw, err := json.Marshal(j)
			testutil.Must(t, err)
			var original WorkerSetupJournal
			testutil.Must(t, json.Unmarshal(raw, &original))
			switch damage {
			case "new-endpoint":
				r.Sources[1].Host = "redirect.invalid"
			case "new-trust":
				r.Sources[1].KnownHosts += "changed"
			case "old-disposition":
				r.Declaration.Desired.Backup.RetireSources["retire"] = "other reason"
			case "old-key":
				j.SourceKeys["retire"] = "changed"
			case "extra-key":
				j.SourceKeys["unrequested"] = "changed"
			case "missing-retirement":
				j.SourceRetirement = nil
			case "changed-predecessor":
				j.SourceRetirement.Before.RequestHash = "changed"
			}
			tree := policyReviewTree(t, home)
			err = checkSetupSourceExpansion(root, j, &r)
			if damage != "" && err == nil {
				t.Fatal("changed combined authority accepted")
			}
			if damage == "" {
				testutil.Must(t, err)
				testutil.Must(t, checkSetupSourceRetirement(root, j, &r))
				if !reflect.DeepEqual(original.SourceExpansion.Before.SourceKeys, original.SourceRetirement.Before.SourceKeys) || original.SourceRetirement.Before.SourceKeys["extra"] != "" {
					t.Fatal("predecessor absorbed new key")
				}
			}
			if !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
				t.Fatal("review changed retained state")
			}
		})
	}
}

func TestBackupCombinedSourceReplacementUsesAliasesRatherThanCounts(t *testing.T) {
	for _, shape := range []struct {
		name      string
		old, next []string
	}{
		{"same", []string{"source", "retire"}, []string{"source", "extra"}},
		{"growing", []string{"source", "retire"}, []string{"source", "extra", "another"}},
		{"shrinking", []string{"source", "retire", "older"}, []string{"source", "extra"}},
		{"all", []string{"retire"}, []string{"extra"}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			home, _, r := sourceExpansionFixtureWithHelper(t, false, shape.old...)
			active := map[string]bool{}
			for _, alias := range shape.next {
				active[alias] = true
			}
			r.Sources = nil
			for _, alias := range shape.next {
				r.Sources = append(r.Sources, SetupSource{Alias: alias, Host: alias + ".invalid", User: "app", Port: 22})
			}
			root, state, keys, close := expansionRoots(t, home)
			defer close()
			var j WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &j))
			installed, err := loadInstalledWorker(home)
			testutil.Must(t, err)
			// Retained endpoint/trust bytes remain exact; additions use independent aliases.
			for i := range r.Sources {
				s := &r.Sources[i]
				r.Declaration.Sources[s.Alias] = r.Declaration.HelperSHA256
				for _, old := range installed.Sources {
					if s.Alias == old.Alias {
						s.Host, s.User, s.Port = old.Host, old.User, old.Port
						r.Declaration.Sources[s.Alias] = j.Declaration.Sources[s.Alias]
					}
				}
				raw, e := readBackupOwnedFile(root, workerTrustDirectory+"/"+shape.old[0]+".known_hosts", 16384)
				testutil.Must(t, e)
				s.KnownHosts = string(raw)
			}
			r.Declaration.Desired.Backup.RetireSources = map[string]string{}
			for _, alias := range shape.old {
				if !active[alias] {
					delete(r.Declaration.Sources, alias)
					r.Declaration.Desired.Backup.RetireSources[alias] = "explicit replacement"
				}
			}
			r.Policy.Sources = shape.next
			r.Declaration.Desired.Backup.Sources = shape.next
			r.Declaration.DesiredHash, err = r.Declaration.Desired.Fingerprint()
			testutil.Must(t, err)
			testutil.Must(t, beginWorkerSetupUpdate(root, state, &j, r))
			for _, s := range r.Sources {
				if j.SourceKeys[s.Alias] != "" {
					continue
				}
				_, err = setupSourceKey(home, keys, state, &j, s.Alias)
				testutil.Must(t, err)
				testutil.Must(t, recordSetupSourceEnrollment(state, &j, s.Alias, source.SourceEnrollmentResult{Schema: 1, WorkerID: j.WorkerID, SourceID: testutil.ID(t, "s"), Fingerprint: j.SourceKeys[s.Alias]}))
			}
			testutil.Must(t, checkSetupSourceExpansion(root, j, &r))
			testutil.Must(t, checkSetupSourceRetirement(root, j, &r))
		})
	}
}

func TestBackupCombinedSourceReplacementReservationReviewIsReadOnly(t *testing.T) {
	home, before, r, _ := sharedSourceExpansionFixture(t, "source", "retire")
	makeCombinedSourceRequest(t, before, &r)
	root, state, _, close := expansionRoots(t, home)
	defer close()
	var j WorkerSetupJournal
	testutil.Must(t, state.ReadJSON("setup.json", &j))
	tree := policyReviewTree(t, home)
	got, err := inspectSetupUpdateReservation(root, state, &j, r)
	testutil.Must(t, err)
	if j.Producer == nil || got != *j.Producer || j.SourceExpansion != nil || j.SourceRetirement != nil || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("combined replacement review changed reserved ownership")
	}
}
