package relay

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func addBaselineHistory(m *interaction.SignalRecoveryManifest, proof interaction.SignalRecoveryCursor) error {
	if err := proof.Validate(); err != nil {
		return err
	}
	if m.Relay.Identity != proof.Handoff.Target.Identity || m.Relay.Account != proof.Handoff.Target.Account {
		return fmt.Errorf("Signal history identity changed")
	}
	m.BaselineHash = protocol.Hash(proof)
	m.BaselinePosition = proof.Handoff.Target.Position
	history, err := interaction.RecoveredHistory(proof.Handoff.Summary.Manifest, m.BaselinePosition, m.Highest)
	if err != nil {
		return err
	}
	m.History = history
	m.RetainedEvents = proof.Handoff.Summary.Manifest.EventCount() + m.Highest - m.BaselinePosition
	return nil
}

func manifestEventDigest(ctx context.Context, db *sql.DB, m interaction.SignalRecoveryManifest) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	rows, err := db.QueryContext(ctx, "SELECT position,key,hash,body FROM events ORDER BY position")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var pos uint64
		var key, hash, raw string
		var event interaction.SpoolEvent
		if err = rows.Scan(&pos, &key, &hash, &raw); err != nil {
			return "", err
		}
		if err = protocol.Decode([]byte(raw), &event, 16384); err != nil {
			return "", err
		}
		if pos != m.PositionAt(uint64(len(hashes))) || event.Position != 0 || event.Key != key || event.Hash != hash {
			return "", fmt.Errorf("Signal retained history changed")
		}
		event.Position = pos
		if err = interaction.ValidateSignalRecoveryEvent(event, m.Relay); err != nil {
			return "", err
		}
		hashes = append(hashes, protocol.Hash(event))
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if uint64(len(hashes)) != m.EventCount() {
		return "", fmt.Errorf("Signal retained history incomplete")
	}
	return interaction.RecoveryEvidenceDigest(hashes), nil
}
