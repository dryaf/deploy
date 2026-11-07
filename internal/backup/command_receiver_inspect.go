package backup

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// InspectSharedCommandReceiver distinguishes pending setup from lost authority
// without initializing a ledger, creating a lock or completing a pending record.
func InspectSharedCommandReceiver(ctx context.Context, r CommandReceiverRequest) (string, bool, error) {
	if err := r.validate(); err != nil {
		return "", false, err
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	state, err := openBackupState(r.State, false)
	if err != nil {
		return "", false, err
	}
	defer state.Close()
	if err = requireCommandReceiverOwner(state, r); err != nil {
		return "", false, err
	}
	d, err := safefs.Open(r.State)
	if err != nil {
		return "", false, err
	}
	defer d.Close()
	var record commandReceiverRecord
	readErr := d.ReadJSON(commandReceiverFile, &record)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", false, readErr
	}
	if readErr == nil && (record.Request.validate() != nil || notify.Hash(record.Request) != notify.Hash(r)) {
		return "", false, fmt.Errorf("command receiver inspection changed its retained request")
	}
	installed, err := commandReceiverSQL(state, r)
	if err != nil {
		return "", false, err
	}
	if os.IsNotExist(readErr) {
		if installed {
			return "", false, fmt.Errorf("command receiver receipt missing; recovery required")
		}
		return "", false, nil
	}
	lock, err := d.Lock("commands.lock", false, false)
	if err != nil {
		return "", false, err
	}
	defer lock.Close()
	if !record.Complete {
		return notify.Hash(record), false, nil
	}
	if !installed {
		return "", false, fmt.Errorf("completed command receiver lost SQL authority")
	}
	if _, err = interaction.OpenDomainLedger(ctx, state.db, r.Producer, r.Identity, false, nil); err != nil {
		return "", false, err
	}
	return notify.Hash(record), true, ctx.Err()
}
