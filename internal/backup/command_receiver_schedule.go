package backup

import (
	"context"
	"fmt"
	"os"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// The existing owned report timer runs every 30 seconds, independently of backup
// captures. Reserve at most eight seconds for commands, leaving the report pass
// its own 55-second budget within the unit's 70-second startup limit. A receiver
// error is reported separately and must not prevent current backup evaluation.
func runScheduledSharedCommands(ctx context.Context, state *backupState, step func(context.Context, string, string) error) error {
	d, err := safefs.Open(state.directory)
	if err != nil {
		return err
	}
	defer d.Close()
	var record commandReceiverRecord
	readErr := d.ReadJSON(commandReceiverFile, &record)
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	var installed int
	if err = state.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='backup_command_receiver'").Scan(&installed); err != nil {
		return err
	}
	if os.IsNotExist(readErr) {
		if installed != 0 {
			return fmt.Errorf("scheduled command receiver lost its retained record")
		}
		return nil
	}
	if !record.Complete || record.Request.State != state.directory || record.Request.validate() != nil || installed != 1 {
		return fmt.Errorf("finish the retained command receiver setup before scheduled execution")
	}
	if step == nil {
		return fmt.Errorf("command receiver runner unavailable")
	}
	budget, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return step(budget, state.directory, notify.Hash(record))
}
