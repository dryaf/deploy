package workerctl

import (
	"errors"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

func declarationFingerprint(d config.DesiredWorkers, scope int) (string, error) {
	if scope == 1 {
		return d.NotificationFingerprint()
	}
	if scope != 0 {
		return "", fmt.Errorf("unsupported declaration scope")
	}
	return d.Fingerprint()
}
func snapshotScope(scope []int) int {
	if len(scope) == 0 {
		return 0
	}
	if len(scope) != 1 {
		return -1
	}
	return scope[0]
}

func retainDeclaration(d *safefs.Directory, desired config.DesiredWorkers, expected string, scope ...int) error {
	hash, err := declarationFingerprint(desired, snapshotScope(scope))
	if err != nil || hash != expected || !validDigest(expected) {
		return fmt.Errorf("declaration fingerprint changed")
	}
	if err = d.Mkdir("declarations"); err != nil {
		return err
	}
	name := "declarations/" + expected + ".json"
	var saved config.DesiredWorkers
	err = d.ReadJSON(name, &saved)
	if errors.Is(err, os.ErrNotExist) {
		return d.WriteJSON(name, desired, false)
	}
	if err != nil {
		return err
	}
	actual, err := declarationFingerprint(saved, snapshotScope(scope))
	if err != nil || actual != expected {
		return fmt.Errorf("retained declaration changed")
	}
	return nil
}

func readDeclaration(d *safefs.Directory, expected string, scope ...int) (config.DesiredWorkers, error) {
	var result config.DesiredWorkers
	if !validDigest(expected) {
		return result, fmt.Errorf("invalid retained declaration pin")
	}
	if err := d.ReadJSON("declarations/"+expected+".json", &result); err != nil {
		return result, fmt.Errorf("retained declaration unavailable; explicit import required: %w", err)
	}
	actual, err := declarationFingerprint(result, snapshotScope(scope))
	if err != nil || actual != expected {
		return result, fmt.Errorf("retained declaration changed")
	}
	return result, nil
}

// Command operations pin the complete declaration as well as notification scope.
// A later backup-only change can revisit the same notification scope, so its
// original scope snapshot cannot identify the new operation's backup policy.
func readCommandDeclaration(d *safefs.Directory, j OperationJournal) (config.DesiredWorkers, error) {
	if !commandJournal(j) && !commandActivityJournal(j) && !commandPauseJournal(j) && !commandDeliveryJournal(j) && !commandLimitsJournal(j) {
		return readDeclaration(d, j.Pins.DesiredHash, j.Pins.DesiredScope)
	}
	full := j.Pins.Artifacts["command-backup-policy"]
	desired, err := readDeclaration(d, full)
	if errors.Is(err, os.ErrNotExist) {
		// Older operations only retained the notification snapshot. Accept it only
		// when it also proves the operation's complete pinned declaration.
		desired, err = readDeclaration(d, j.Pins.DesiredHash, 1)
		if err == nil {
			actual, e := desired.Fingerprint()
			if e != nil || actual != full {
				return desired, fmt.Errorf("retained command backup declaration changed")
			}
		}
	}
	if err != nil {
		return desired, err
	}
	scope, err := desired.NotificationFingerprint()
	if err != nil || j.Pins.DesiredScope != 1 || scope != j.Pins.DesiredHash {
		return desired, fmt.Errorf("retained command notification declaration changed")
	}
	return desired, nil
}
