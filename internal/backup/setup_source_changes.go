package backup

import (
	"fmt"
	"maps"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/safefs"
)

// Classify aliases, not counts: one edit can add and retire sources even when
// the total stays the same. Reusing an already retired alias remains separate.
func setupSourceChanges(before WorkerInstallation, request WorkerSetupRequest) (added, removed map[string]bool) {
	old, next := map[string]bool{}, map[string]bool{}
	for _, s := range before.Sources {
		old[s.Alias] = true
	}
	for _, s := range request.Sources {
		next[s.Alias] = true
	}
	added, removed = map[string]bool{}, map[string]bool{}
	for alias := range next {
		if !old[alias] {
			added[alias] = true
		}
	}
	for alias := range old {
		if !next[alias] {
			removed[alias] = true
		}
	}
	return
}

// During a combined edit, retired keys stay reserved until remote revocation,
// and added keys survive rollback. Verify this union without treating a missing
// new enrollment reply as an acknowledged source. Completion requires all new
// enrollments; no unrequested key may enter the union.
func verifyCombinedSourceKeys(root *safefs.Directory, j, b WorkerSetupJournal, request WorkerSetupRequest) error {
	view := j
	declaration := *request.Declaration
	declaration.Sources = map[string]string{}
	view.Declaration = &declaration
	view.SourceRetirement = nil
	view.SourceEnrollments = maps.Clone(j.SourceEnrollments)
	for alias, pin := range b.SourceKeys {
		if j.SourceKeys[alias] != pin || j.SourceEnrollments[alias] != b.SourceEnrollments[alias] {
			return fmt.Errorf("combined source edit changed an existing enrollment")
		}
	}
	complete := j.RequestHash == workerSetupRequestHash(request) && !j.Updating && j.Stage == "complete"
	for alias, pin := range j.SourceKeys {
		helper := request.Declaration.Sources[alias]
		if b.SourceKeys[alias] != "" {
			helper = b.Declaration.Sources[alias]
		}
		if helper == "" || pin == "" {
			return fmt.Errorf("combined source edit has an unreserved key")
		}
		declaration.Sources[alias] = helper
		if e, ok := j.SourceEnrollments[alias]; ok {
			if e.Schema != 1 || e.WorkerID != j.WorkerID || !safefs.ValidID(e.SourceID, "s") || e.Fingerprint != pin {
				return fmt.Errorf("combined source enrollment identity changed")
			}
		} else {
			if complete {
				return fmt.Errorf("combined source edit lacks acknowledged enrollment")
			}
			view.SourceEnrollments[alias] = source.SourceEnrollmentResult{Fingerprint: pin}
		}
	}
	for alias := range j.SourceEnrollments {
		if j.SourceKeys[alias] == "" {
			return fmt.Errorf("combined source enrollment has no reserved key")
		}
	}
	if complete {
		for alias := range request.Declaration.Sources {
			if j.SourceKeys[alias] == "" {
				return fmt.Errorf("combined source publication lost an active key")
			}
		}
	}
	return verifySetupSourceKeys(root, view)
}
