package interaction

import (
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func readLedgerCommandCommit(tx *sql.Tx) (intent *CommandRecoverySummary, proof *CommandRecoveryCommit, err error) {
	var tables int
	if err = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('interactive_recovery_commit','interactive_recovery_result_floors')").Scan(&tables); err != nil {
		return
	}
	if tables == 0 {
		return
	}
	if tables != 2 {
		return nil, nil, fmt.Errorf("command recovery commit schema incomplete")
	}
	var raw, body string
	var version int
	if err = tx.QueryRow("SELECT version,intent,proof FROM interactive_recovery_commit WHERE singleton=1").Scan(&version, &raw, &body); err != nil {
		return
	}
	if version != 1 {
		return nil, nil, fmt.Errorf("unknown command recovery commit schema")
	}
	intent = &CommandRecoverySummary{}
	if err = protocol.Decode([]byte(raw), intent, 16384); err != nil {
		return
	}
	candidate := CommandRecoveryCommit{Version: 1, Summary: *intent, ResultFloorsHash: CommandRecoveryDigest(nil)}
	if err = candidate.Validate(); err != nil {
		return
	}
	if body == "" {
		var count int
		if err = tx.QueryRow("SELECT count(*) FROM interactive_recovery_result_floors").Scan(&count); err != nil {
			return
		}
		if count != 0 {
			err = fmt.Errorf("uncommitted command result floors")
		}
		return
	}
	proof = &CommandRecoveryCommit{}
	if err = protocol.Decode([]byte(body), proof, 16384); err != nil {
		return
	}
	if err = proof.Validate(); err != nil {
		return
	}
	if protocol.Hash(proof.Summary) != protocol.Hash(intent) {
		return intent, proof, fmt.Errorf("command recovery commit lost its intent")
	}
	err = validateCommittedResultFloors(tx, *proof)
	return
}

func validateCommittedResultFloors(tx *sql.Tx, proof CommandRecoveryCommit) error {
	var count, records uint64
	if err := tx.QueryRow("SELECT (SELECT count(*) FROM interactive_recovery_result_floors),(SELECT count(*) FROM interactive_recovery_records)").Scan(&count, &records); err != nil {
		return err
	}
	if count != proof.Summary.Manifest.Commands || records != count {
		return fmt.Errorf("command result floor membership changed")
	}
	rows, err := tx.Query(`SELECT r.ordinal,r.id,r.hash,r.body,f.revision,f.command_hash,f.record_hash FROM interactive_recovery_records r
 JOIN interactive_recovery_result_floors f ON f.id=r.id ORDER BY r.ordinal`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var n uint64
		var id, hash, raw string
		var floor CommandResultFloor
		if err = rows.Scan(&n, &id, &hash, &raw, &floor.Revision, &floor.CommandHash, &floor.RecordHash); err != nil {
			return err
		}
		var record CommandRecoveryRecord
		if err = protocol.Decode([]byte(raw), &record, protocol.MaxRequest); err != nil {
			return err
		}
		if err = record.Validate(proof.Summary.Manifest); err != nil {
			return err
		}
		floor.CommandID = id
		if n != uint64(len(hashes))+1 || hash != protocol.Hash(record) || floor != RecoveryResultFloor(record) {
			return fmt.Errorf("command result floor changed")
		}
		hashes = append(hashes, protocol.Hash(floor))
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if uint64(len(hashes)) != count || CommandRecoveryDigest(hashes) != proof.ResultFloorsHash {
		return fmt.Errorf("command result floor checkpoint changed")
	}
	return nil
}

// Check the operational ledger against its captured evidence before changing
// identity. After commit only the additional admission fences may differ.
func validateCommandProjection(tx *sql.Tx, v CommandRecoveryRecord, committed bool) error {
	var raw, key, hash, grant, phase, state string
	var revision uint64
	var fenced bool
	if err := tx.QueryRow("SELECT body,request_key,hash,grant_id,phase,state,revision,fenced FROM interactive_inbox WHERE id=?", v.Command.ID).Scan(&raw, &key, &hash, &grant, &phase, &state, &revision, &fenced); err != nil {
		return err
	}
	var c Command
	if err := protocol.Decode([]byte(raw), &c, protocol.MaxRequest); err != nil {
		return err
	}
	wantFence := v.Fenced || committed
	if protocol.Hash(c) != protocol.Hash(v.Command) || key != c.RequestKey || hash != protocol.Hash(c) || grant != v.GrantID || phase != v.Phase || state != v.State || revision != v.Revision || fenced != wantFence {
		return fmt.Errorf("command ledger differs from retained recovery evidence")
	}
	rows, err := tx.Query("SELECT revision,body,receipt,acked FROM interactive_results WHERE id=? ORDER BY revision", c.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var rev uint64
		var body, receipt string
		var result RecoveryResult
		if err = rows.Scan(&rev, &body, &receipt, &result.Acknowledged); err != nil {
			return err
		}
		if err = protocol.Decode([]byte(body), &result.Result, protocol.MaxRequest); err != nil {
			return err
		}
		if receipt != "" {
			result.Receipt = &ResultReceipt{}
			if err = protocol.Decode([]byte(receipt), result.Receipt, 8192); err != nil {
				return err
			}
		}
		if n >= len(v.Results) || rev != uint64(n+1) || protocol.Hash(result) != protocol.Hash(v.Results[n]) {
			return fmt.Errorf("command result provenance changed before commit")
		}
		n++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if n != len(v.Results) {
		return fmt.Errorf("command result history missing")
	}
	var nonAdmission string
	err = tx.QueryRow("SELECT body FROM interactive_non_admission WHERE id=?", c.ID).Scan(&nonAdmission)
	if err == sql.ErrNoRows && v.NonAdmission == nil {
		return nil
	}
	if err != nil {
		return err
	}
	var proof NonAdmissionProof
	if err = protocol.Decode([]byte(nonAdmission), &proof, 8192); err != nil {
		return err
	}
	if v.NonAdmission == nil || proof != *v.NonAdmission {
		return fmt.Errorf("command non-admission proof changed")
	}
	return nil
}
