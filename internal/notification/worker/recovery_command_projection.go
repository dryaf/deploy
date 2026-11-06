package worker

import (
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

type recoveredCommandHead struct {
	ID, RecordHash     string
	State              string
	Revision           uint64
	Imported, Conflict int
	NonAdmission       string
}

func recoveryCommandHeads(tx *sql.Tx, v interaction.CommandRecoveryRecord) (before, after recoveredCommandHead, err error) {
	before = recoveredCommandHead{ID: v.Command.ID, RecordHash: protocol.Hash(v), State: "outcome_unknown", Imported: 1}
	err = tx.QueryRow("SELECT state,revision,imported,conflict,non_admission FROM recovery_commands WHERE id=?", v.Command.ID).Scan(&before.State, &before.Revision, &before.Imported, &before.Conflict, &before.NonAdmission)
	if err != nil && err != sql.ErrNoRows {
		return
	}
	if !interaction.Terminal(before.State) {
		before.State = "outcome_unknown"
	}
	state, conflict, err := recoveryCommandOutcome(tx, v)
	if err != nil {
		return before, after, err
	}
	if conflict {
		before.State = "outcome_unknown"
		before.Conflict = 1
	}
	after = before
	after.State = state
	after.Revision = max(before.Revision, v.Revision)
	after.Imported = 1
	after.NonAdmission = ""
	return before, after, nil
}

func commandInstallationPlan(tx *sql.Tx, checkpoint interaction.CommandRecoveryCommit) (plan commandInstallationIntent, projection string, err error) {
	p := checkpoint.Summary.Manifest.Producer
	rows, err := tx.Query("SELECT body,outcome FROM reconciliation_command_records WHERE producer=? ORDER BY ordinal", p.ID)
	if err != nil {
		return
	}
	defer rows.Close()
	var beforeHashes, afterHashes, currentHashes []string
	for rows.Next() {
		var raw, outcome string
		if err = rows.Scan(&raw, &outcome); err != nil {
			return
		}
		var v interaction.CommandRecoveryRecord
		if err = protocol.Decode([]byte(raw), &v, protocol.MaxRequest); err != nil {
			return
		}
		before, after, e := recoveryCommandHeads(tx, v)
		if e != nil {
			return plan, "", e
		}
		if outcome != after.State {
			return plan, "", fmt.Errorf("installed outcome differs from original evidence")
		}
		current := before
		if err = tx.QueryRow("SELECT state,revision,imported,conflict,non_admission FROM commands WHERE id=?", v.Command.ID).Scan(&current.State, &current.Revision, &current.Imported, &current.Conflict, &current.NonAdmission); err != nil {
			return
		}
		if current != before && current != after {
			return plan, "", fmt.Errorf("recovered command %s projection differs: current=%+v before=%+v after=%+v", v.Command.ID, current, before, after)
		}
		beforeHashes = append(beforeHashes, protocol.Hash(before))
		afterHashes = append(afterHashes, protocol.Hash(after))
		currentHashes = append(currentHashes, protocol.Hash(current))
	}
	if err = rows.Err(); err != nil {
		return
	}
	if uint64(len(afterHashes)) != checkpoint.Summary.Manifest.Commands {
		return plan, "", fmt.Errorf("command installation membership changed")
	}
	history, err := commandResultHistoryHash(tx, p, checkpoint.Summary.Manifest.Identity)
	if err != nil {
		return plan, "", err
	}
	plan = commandInstallationIntent{Version: 1, BeforeHash: interaction.CommandRecoveryDigest(beforeHashes), HistoryHash: history,
		Installation: interaction.CommandRecoveryInstallation{Version: 1, Checkpoint: checkpoint, OutcomesHash: interaction.CommandRecoveryDigest(afterHashes), Commands: uint64(len(afterHashes))}}
	err = plan.Installation.Validate()
	return plan, interaction.CommandRecoveryDigest(currentHashes), err
}

// Retain old result/receipt bytes and ACK flags, including commands missing from
// the specialist. They are never relabelled into the new epoch's receipt stream.
func commandResultHistoryHash(tx *sql.Tx, p protocol.Producer, identity protocol.Identity) (string, error) {
	rows, err := tx.Query(`SELECT r.command_id,r.revision,r.hash,r.body,r.receipt,r.acked,c.body FROM command_results r JOIN commands c ON c.id=r.command_id WHERE c.producer=? ORDER BY r.command_id,r.revision`, p.ID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var id, hash, body, receipt, command string
		var revision uint64
		var acked int
		if err = rows.Scan(&id, &revision, &hash, &body, &receipt, &acked, &command); err != nil {
			return "", err
		}
		var result interaction.Result
		if err = protocol.Decode([]byte(body), &result, protocol.MaxRequest); err != nil {
			return "", err
		}
		if err = result.Validate(); err != nil {
			return "", err
		}
		var c interaction.Command
		if err = protocol.Decode([]byte(command), &c, protocol.MaxRequest); err != nil {
			return "", err
		}
		var r interaction.ResultReceipt
		if err = protocol.Decode([]byte(receipt), &r, 8192); err != nil {
			return "", err
		}
		if result.CommandID != id || result.Revision != revision || hash != protocol.Hash(result) || result.CommandHash != protocol.Hash(c) ||
			r.Identity != identity || r.Producer != p || r.CommandID != id || r.Revision != revision || r.Hash != hash || (acked != 0 && acked != 1) {
			return "", fmt.Errorf("historical command result provenance changed")
		}
		hashes = append(hashes, protocol.Hash([]any{id, revision, hash, body, receipt, acked}))
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	return interaction.CommandRecoveryDigest(hashes), nil
}

func applyCommandInstallation(tx *sql.Tx, p protocol.Producer) error {
	rows, err := tx.Query("SELECT body FROM reconciliation_command_records WHERE producer=? ORDER BY ordinal", p.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		var v interaction.CommandRecoveryRecord
		if err = protocol.Decode([]byte(raw), &v, protocol.MaxRequest); err != nil {
			return err
		}
		_, after, err := recoveryCommandHeads(tx, v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE commands SET state=?,revision=?,imported=1,conflict=?,non_admission='' WHERE id=?", after.State, after.Revision, after.Conflict, after.ID); err != nil {
			return err
		}
	}
	return rows.Err()
}
