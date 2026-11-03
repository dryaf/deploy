package worker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func OpenCommandRecoveryCommitSession(ctx context.Context, path string, c Config) (interaction.CommandRecoveryCommitSession, error) {
	s, err := OpenCommandRecoverySession(ctx, path, c)
	if err != nil {
		return nil, err
	}
	return s.(*recoveryReconciler), nil
}

// The caller keeps the offline installation lease throughout the source commit.
// This reads retained evidence without making new receipts, grants or replies.
func (r *recoveryReconciler) PrepareCommandRecoveryCommit(ctx context.Context, p protocol.Producer) (proof interaction.CommandRecoveryCommit, err error) {
	if err = r.commandRecoveryEnrollment(p); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		return r.prepareCommandRecoveryCommit(tx, p, &proof)
	})
	return
}

func (r *recoveryReconciler) prepareCommandRecoveryCommit(tx *sql.Tx, p protocol.Producer, proof *interaction.CommandRecoveryCommit) error {
	s, err := readCommandRecoverySession(tx, p)
	if err != nil {
		return err
	}
	if s.Summary == nil || s.Manifest == nil {
		return protocol.ErrRecoveryIncomplete
	}
	v := *s.Summary
	if protocol.Hash(v.Challenge) != protocol.Hash(s.Challenge) || v.Manifest != *s.Manifest || v.Challenge.Operation != r.journal.Operation || v.Challenge.Identity != r.journal.After || v.Manifest.Identity != r.journal.Before || v.Manifest.Producer != p || v.Challenge.Producer != p {
		return fmt.Errorf("command commit recovery binding changed")
	}
	if err = validateCommandCommitRecords(tx, p, v); err != nil {
		return err
	}
	var enabled int
	if err = tx.QueryRow(`SELECT count(*) FROM commands WHERE producer=? AND
(grant<>'' OR token_hash IS NOT NULL OR confirmed<>0 OR preparation<>'' OR state NOT IN ('outcome_unknown','succeeded','failed','rejected','expired'))`, p.ID).Scan(&enabled); err != nil {
		return err
	}
	if enabled != 0 {
		return fmt.Errorf("command recovery admission fence changed")
	}
	rows, err := tx.Query(`SELECT r.body,c.body,i.command_id,i.input_hash,r.request_key,c.id,c.producer,c.request_key,c.input_hash FROM reconciliation_command_records r
JOIN commands c ON c.id=r.id JOIN command_inbox i ON i.request_key=r.request_key WHERE r.producer=? ORDER BY r.ordinal`, p.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var raw, body, id, input, key, storedID, storedProducer, storedKey, storedInput string
		if err = rows.Scan(&raw, &body, &id, &input, &key, &storedID, &storedProducer, &storedKey, &storedInput); err != nil {
			return err
		}
		var record interaction.CommandRecoveryRecord
		if err = protocol.Decode([]byte(raw), &record, protocol.MaxRequest); err != nil {
			return err
		}
		var command interaction.Command
		if err = protocol.Decode([]byte(body), &command, protocol.MaxRequest); err != nil {
			return err
		}
		if protocol.Hash(command) != protocol.Hash(record.Command) || id != command.ID || input != command.InputHash || key != command.RequestKey || storedID != command.ID || storedProducer != p.ID || storedKey != command.RequestKey || storedInput != command.InputHash {
			return fmt.Errorf("command commit lost its original request mapping")
		}
		hashes = append(hashes, protocol.Hash(interaction.RecoveryResultFloor(record)))
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if uint64(len(hashes)) != v.Manifest.Commands {
		return fmt.Errorf("command commit request mapping incomplete")
	}
	*proof = interaction.CommandRecoveryCommit{Version: 1, Summary: v, ResultFloorsHash: interaction.CommandRecoveryDigest(hashes)}
	return proof.Validate()
}
