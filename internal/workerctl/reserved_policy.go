package workerctl

import (
	"errors"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
)

var errReservedNotificationConsumer = errors.New("reserved backup producer requires coordinated notification policy migration")

// An unbound gateway can already be a dependency of a reserved backup. Changing
// its desired scope or helper at that point would strand the backup's immutable
// setup request even though no generation has been enrolled in the gateway yet.
// Check the predecessor declaration, never a proposed removal of that dependency.
func requireUnreservedNotificationPolicy(r ControlRequest, paths ResolvedPaths, before config.DesiredWorkers, prior OperationJournal) error {
	if before.Backup == nil || before.Backup.Notifications.Worker != r.Server {
		return nil
	}
	_, present, err := backup.InspectReservedProducer(r.Facts.Home, prior.Pins.DesiredHash, paths.Installation, before.Backup.Notifications.Route)
	if err != nil {
		return err
	}
	if present {
		return errReservedNotificationConsumer
	}
	return nil
}
