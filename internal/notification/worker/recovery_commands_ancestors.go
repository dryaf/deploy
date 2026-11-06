package worker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// A specialist may retain receipts older than the immediately previous epoch.
// Admit them only as fenced evidence, and only when the independently retained
// latest gateway contains the same command record and completed reconciliation.
func (r *recoveryReconciler) verifyHistoricalCommand(tx *sql.Tx, v interaction.CommandRecoveryRecord) error {
	if v.HistoricalIdentity == nil {
		return nil
	}
	if r.journal.Version != 2 {
		return fmt.Errorf("historical command requires retained ancestor recovery")
	}
	db, err := r.originalDB(context.Background(), tx)
	if err != nil {
		return err
	}
	defer db.Close()
	var raw, hash string
	if err = db.QueryRow("SELECT hash,body FROM reconciliation_command_records WHERE producer=? AND id=?", v.Command.Producer.ID, v.Command.ID).Scan(&hash, &raw); err != nil {
		return err
	}
	var old interaction.CommandRecoveryRecord
	if err = protocol.Decode([]byte(raw), &old, protocol.MaxRequest); err != nil {
		return err
	}
	if protocol.Hash(old) != hash {
		return fmt.Errorf("ancestor command record changed")
	}
	var sessionRaw string
	if err = db.QueryRow("SELECT body FROM reconciliation_command_sessions WHERE producer=?", v.Command.Producer.ID).Scan(&sessionRaw); err != nil {
		return err
	}
	var session commandRecoverySession
	if err = protocol.Decode([]byte(sessionRaw), &session, protocol.MaxRequest); err != nil {
		return err
	}
	if session.Summary == nil || session.Manifest == nil || session.Summary.Challenge.Identity != r.journal.Before {
		return fmt.Errorf("ancestor command reconciliation incomplete")
	}
	origin := session.Manifest.Identity
	if old.HistoricalIdentity != nil {
		origin = *old.HistoricalIdentity
	}
	old.HistoricalIdentity = &origin
	old.Fenced = true
	if protocol.Hash(old) != protocol.Hash(v) {
		return fmt.Errorf("historical command differs from retained ancestor")
	}
	return nil
}
