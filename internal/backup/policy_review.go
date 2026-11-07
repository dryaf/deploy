package backup

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

// SetupPolicyReview verifies existing journals, owned inputs, source keys, helper
// provenance, database identity and archive writer authority under the setup
// lease. Legacy policy can be inspected without creating a declaration or an
// adoption record. This result grants no setup/update authority.
type SetupPolicyReview struct {
	Installation WorkerInstallation
	Paths        InstallationPaths
	Desired      *config.DesiredWorkers
}

func InspectSetupPolicyReview(ctx context.Context, home string) (SetupPolicyReview, error) {
	var result SetupPolicyReview
	root, err := safefs.Open(home)
	if err != nil {
		return result, err
	}
	defer root.Close()
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", false, false)
	if err != nil {
		return result, fmt.Errorf("backup setup ownership unavailable or busy")
	}
	defer lock.Close()
	observed, err := inspectSetupForReviewLocked(ctx, root, home, true)
	if err != nil {
		return result, err
	}
	if observed.Stage != "complete" || observed.Paths == nil || observed.ActivationPending || observed.SharedReportingPending {
		return result, fmt.Errorf("policy review requires complete retained backup ownership; finish pending transitions first")
	}
	var j WorkerSetupJournal
	if err = root.ReadJSON(backupSetupFile, &j); err != nil {
		return result, err
	}
	if err = readInstalledWorkerJSON(root, &result.Installation); err != nil {
		return result, err
	}
	result.Paths = *observed.Paths
	if _, _, err = inspectSetupSourcePins(root, j, result.Installation, result.Paths); err != nil {
		return SetupPolicyReview{}, err
	}
	if j.Declaration != nil {
		result.Desired = activityPolicy(j)
	}
	if result.Desired == nil && result.Installation.Policy.Notifications.Mode == "shared" {
		return SetupPolicyReview{}, fmt.Errorf("shared backup import requires retained gateway policy and adoption review")
	}
	return result, nil
}
