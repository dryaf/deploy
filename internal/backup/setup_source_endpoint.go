package backup

import (
	"fmt"
	"reflect"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/safefs"
)

func validateSourceEndpointMove(before WorkerInstallation, previous *SetupDeclaration, request WorkerSetupRequest) error {
	d := request.Declaration
	if d == nil || previous == nil || len(before.Sources) != len(request.Sources) || !reflect.DeepEqual(before.Policy, request.Policy) || before.Image != request.Image || before.RelayImage != request.RelayImage || previous.HelperSHA256 != d.HelperSHA256 || !safefs.DigestPattern.MatchString(d.HelperSHA256) || !reflect.DeepEqual(previous.Sources, d.Sources) {
		return fmt.Errorf("move source endpoints separately from source sets, policy and artifact changes")
	}
	for _, old := range before.Sources {
		if !safefs.ValidID(old.SourceID, "s") || !safefs.DigestPattern.MatchString(previous.Sources[old.Alias]) {
			return fmt.Errorf("source endpoint move requires retained source identity and helper pins")
		}
	}
	return nil
}

// Existing sources with unchanged artifacts need no enrollment mutation. This
// also applies to retries after a source receipt was recorded. Their source ID,
// key and helper must all still exist at the selected endpoint. An actual helper
// upgrade continues through the original enrollment/install transaction.
func readOnlySetupSources(j WorkerSetupJournal) map[string]source.SourceEnrollmentResult {
	if j.Declaration == nil {
		return nil
	}
	previous := j.Declaration
	if j.Updating && j.PreviousDeclaration != nil {
		previous = j.PreviousDeclaration
	}
	var result map[string]source.SourceEnrollmentResult
	for alias, enrollment := range j.SourceEnrollments {
		if previous.Sources[alias] != "" && previous.Sources[alias] == j.Declaration.Sources[alias] {
			if result == nil {
				result = map[string]source.SourceEnrollmentResult{}
			}
			result[alias] = enrollment
		}
	}
	return result
}

func sourceEndpointsDiffer(before WorkerInstallation, request WorkerSetupRequest) bool {
	for _, old := range before.Sources {
		for _, next := range request.Sources {
			if old.Alias == next.Alias && (old.Host != next.Host || old.User != next.User || old.Port != next.Port) {
				return true
			}
		}
	}
	return false
}
