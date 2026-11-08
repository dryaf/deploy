package backup

import (
	"fmt"
	"reflect"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Each completed update supplies its exact predecessor. Notification scopes and
// ordinary backup updates may alternate; neither can stand in for the other.
func reportingOrigin(root *safefs.Directory, j WorkerSetupJournal, c WorkerInstallation, desired, scope, runtimeHash, helperHash string) (WorkerSetupJournal, WorkerInstallation, error) {
	for depth := 0; depth < 128; depth++ {
		if j.Declaration == nil || j.SharedPolicyUpdate != nil || j.NotificationScope != nil || j.Updating || j.RecoveryNeeded {
			return j, c, fmt.Errorf("finish pending backup changes before reporting")
		}
		if j.SharedPolicyHistory != nil {
			if _, err := readSharedPolicyHistory(root, j.SharedPolicyHistory); err != nil {
				return j, c, fmt.Errorf("completed backup history unavailable: %w", err)
			}
		}
		if j.Declaration.DesiredHash == desired && notify.Hash(c) == runtimeHash && j.Declaration.HelperSHA256 == helperHash {
			if j.Declaration.NotificationDesiredHash != scope {
				return j, c, fmt.Errorf("original reporting scope changed")
			}
			return j, c, nil
		}
		if update, handled, err := sharedPolicyPredecessor(root, j); err != nil {
			return j, c, fmt.Errorf("completed backup predecessor differs: %w", err)
		} else if handled {
			var after, before WorkerInstallation
			if err = safefs.DecodeJSON(update.Runtime, &after, safefs.MaxRecordBytes); err != nil {
				return j, c, err
			}
			if err = safefs.DecodeJSON(update.Intent.Runtime, &before, safefs.MaxRecordBytes); err != nil {
				return j, c, err
			}
			if !reflect.DeepEqual(c, after) {
				return j, c, fmt.Errorf("backup runtime differs from its completed policy update")
			}
			j, c = update.Intent.Before, before
			continue
		}
		if err := verifyNotificationScopeHistory(root, j); err != nil {
			return j, c, fmt.Errorf("notification policy history unavailable: %w", err)
		}
		ref := j.NotificationScopeHistory
		if ref == nil {
			return j, c, fmt.Errorf("reporting policy lacks a completed continuation")
		}
		var receipt setupNotificationScopeReceipt
		if err := root.ReadJSON(".local/state/deploy/backup-worker/notification-scope-"+ref.Operation+".json", &receipt); err != nil {
			return j, c, err
		}
		v := receipt.Intent
		if v.validate() != nil || receipt.Version != 1 || notify.Hash(receipt) != ref.Receipt || ((v.Version < 2 || v.Version > 5) && !reportPolicyScope(v.Version) && v.Version != 22) {
			return j, c, fmt.Errorf("unsupported reporting continuation")
		}
		want := v.Before
		want.Declaration, want.RequestHash, want.NotificationScopeHistory = v.After.Declaration, workerSetupRequestHash(v.After), ref
		if productionFreshnessScope(v.Version) {
			want.OwnedFiles = v.freshnessOwnedFiles()
			paths, err := declarationPaths(v.Before.Declaration.Home, v.Before.Declaration)
			if err != nil {
				return j, c, err
			}
			var before, after WorkerInstallation
			if err = safefs.DecodeJSON(v.Freshness.After[paths.OwnedPath(paths.Runtime)], &after, safefs.MaxRecordBytes); err != nil {
				return j, c, err
			}
			if err = safefs.DecodeJSON(v.Freshness.Before[paths.OwnedPath(paths.Runtime)], &before, safefs.MaxRecordBytes); err != nil {
				return j, c, err
			}
			if !reflect.DeepEqual(c, after) {
				return j, c, fmt.Errorf("runtime differs from completed freshness change")
			}
			c = before
		}
		if reportPolicyScope(v.Version) {
			want = v.providerReportsAfter(want, receipt.Gateway)
		}
		if !reflect.DeepEqual(want, j) {
			if err := verifyCommandScopeCapture(root, j, want, receipt.Gateway); err != nil {
				return j, c, fmt.Errorf("backup journal differs from notification predecessor: %w", err)
			}
		}
		j = v.Before
	}
	return j, c, fmt.Errorf("reporting continuation exceeds review bound")
}

func reportingTriggersActive(root *safefs.Directory, j WorkerSetupJournal) (bool, error) {
	for depth := 0; depth < 128; depth++ {
		if _, handled, err := sharedPolicyPredecessor(root, j); err != nil {
			return false, err
		} else if handled {
			return false, nil
		}
		if j.NotificationScopeHistory == nil {
			return true, nil
		}
		if err := verifyNotificationScopeHistory(root, j); err != nil {
			return false, err
		}
		var receipt setupNotificationScopeReceipt
		if err := root.ReadJSON(".local/state/deploy/backup-worker/notification-scope-"+j.NotificationScopeHistory.Operation+".json", &receipt); err != nil {
			return false, err
		}
		if receipt.Intent.Reports != nil {
			return receipt.Intent.Reports.Active, nil
		}
		j = receipt.Intent.Before
	}
	return false, fmt.Errorf("reporting activity history exceeds review bound")
}
