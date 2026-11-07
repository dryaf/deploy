package backup

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// StepSharedCommandReceiver performs one bounded pass using an already staged
// receiver. Its caller must retain the exact receipt and schedule passes only
// through the owned worker lifecycle. This never initializes missing records.
func StepSharedCommandReceiver(ctx context.Context, statePath, receipt string) error {
	if !backupRecoveryPath(statePath) || !safefs.DigestPattern.MatchString(receipt) {
		return fmt.Errorf("command receiver requires exact state and receipt")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d, err := safefs.Open(statePath)
	if err != nil {
		return err
	}
	defer d.Close()
	lock, err := d.Lock("commands.lock", true, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	var record commandReceiverRecord
	if err = d.ReadJSON(commandReceiverFile, &record); err != nil {
		return err
	}
	r := record.Request
	if !record.Complete || r.State != statePath || r.validate() != nil || notify.Hash(record) != receipt {
		return fmt.Errorf("command receiver receipt or retained policy changed")
	}
	state, err := openBackupState(statePath, false)
	if err != nil {
		return err
	}
	defer state.Close()
	if err = requireCommandReceiverOwner(state, r); err != nil {
		return err
	}
	if present, err := commandReceiverSQL(state, r); err != nil {
		return err
	} else if !present {
		return fmt.Errorf("command receiver SQL receipt missing")
	}
	ledger, err := interaction.OpenDomainLedger(ctx, state.db, r.Producer, r.Identity, false, nil)
	if err != nil {
		return err
	}
	handlers, err := backupCommandHandlers(r.Policy, r.Producer, r.mutations(), nil)
	if err != nil {
		return err
	}
	for name, handler := range handlers {
		handlers[name] = commandReceiverHandler{CommandHandler: handler, request: r}
	}
	return (client.Runner{Client: client.Client{Socket: r.Socket, Identity: r.Identity}, Producer: r.Producer, Ledger: ledger, Handlers: handlers}).Step(ctx)
}

type commandReceiverHandler struct {
	client.CommandHandler
	request CommandReceiverRequest
}

func (h commandReceiverHandler) Apply(tx *sql.Tx, command interaction.Command) (interaction.Result, error) {
	r := h.request
	var control ArchiveControl
	if err := backupEvaluationMeta(tx, "control", &control); err != nil {
		return interaction.Result{}, err
	}
	owner, err := readBackupNotificationOwner(tx)
	if err != nil {
		return interaction.Result{}, err
	}
	var pin string
	if err = tx.QueryRow("SELECT request_hash FROM backup_command_receiver WHERE singleton=1").Scan(&pin); err != nil {
		return interaction.Result{}, err
	}
	var complete string
	if err = backupEvaluationMeta(tx, "notification_cutover_complete", &complete); err != nil {
		return interaction.Result{}, err
	}
	if control.ArchiveID != r.Archive || control.WriterID != r.Producer.ID || control.Epoch != r.WriterEpoch || control.ReportingDisabled || complete != owner.CutoverHash || owner.Mode != "shared" || notify.Hash(owner) != r.OwnerHash || pin != notify.Hash(r) {
		return interaction.Result{}, fmt.Errorf("command receiver authority changed before execution")
	}
	if err = requireNoPendingMigrationCycle(tx); err != nil {
		return interaction.Result{}, err
	}
	return h.CommandHandler.Apply(tx, command)
}
