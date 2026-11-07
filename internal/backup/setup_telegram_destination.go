package backup

import "fmt"

// Never reinterpret queued or ambiguous alerts for a newly selected audience.
// Preflight uses a normal state lease. Publication rechecks after the installer
// has stopped senders and holds the exclusive schema/archive writer leases.
func requireSettledBackupDestination(state *backupState) error {
	if err := requireNoNotificationHandoff(state); err != nil {
		return err
	}
	tx, err := state.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	journal, err := readLegacyDeliveryJournal(tx)
	if err != nil {
		return err
	}
	if journal.pending != nil {
		return fmt.Errorf("finish the reserved Telegram delivery before changing its destination")
	}
	var pending bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM outbox WHERE accepted IS NULL)").Scan(&pending); err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("finish pending backup alerts before changing the Telegram destination")
	}
	return nil
}
