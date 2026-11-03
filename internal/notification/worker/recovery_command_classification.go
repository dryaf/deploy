package worker

import (
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Derive the outcome from retained evidence and the immutable pre-recovery
// command, never from the projection which installation is about to replace.
func recoveryCommandOutcome(tx *sql.Tx, v interaction.CommandRecoveryRecord) (state string, conflict bool, err error) {
	state = "outcome_unknown"
	if len(v.Results) > 0 {
		last := v.Results[len(v.Results)-1]
		if interaction.Terminal(last.Result.State) {
			state = last.Result.State
		}
		if last.Receipt != nil && last.Receipt.Disposition == "superseded_no_effect" {
			state = last.Receipt.TerminalState
		}
	}
	if v.NonAdmission != nil {
		state = "expired"
	}
	for _, result := range v.Results {
		if result.Receipt != nil && result.Receipt.Disposition == "execution_conflict" {
			conflict = true
		}
	}
	var raw, oldState string
	var oldConflict int
	var revision uint64
	err = tx.QueryRow("SELECT body,state,conflict,revision FROM recovery_commands WHERE id=?", v.Command.ID).Scan(&raw, &oldState, &oldConflict, &revision)
	if err != nil && err != sql.ErrNoRows {
		return
	}
	if err == nil {
		var old interaction.Command
		if err = protocol.Decode([]byte(raw), &old, protocol.MaxRequest); err != nil {
			return
		}
		if protocol.Hash(old) != protocol.Hash(v.Command) {
			return "", false, fmt.Errorf("restored command differs from retained identity")
		}
		if oldConflict != 0 || revision > v.Revision || (interaction.Terminal(oldState) && state != oldState) {
			conflict = true
		}
	}
	err = nil
	if conflict {
		state = "outcome_unknown"
	}
	return
}
