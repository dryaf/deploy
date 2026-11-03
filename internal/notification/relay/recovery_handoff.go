package relay

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const signalHandoffFile = "signal-handoff.json"

// Preparation retains the exclusive offline lease and all original identity,
// sequence, ACK and listed evidence. It is not authority to restart a receiver.
func (r *RecoveryInbox) PrepareSignalHandoff(ctx context.Context, p interaction.SignalRecoveryHandoff) (interaction.SignalRecoveryHandoff, error) {
	if err := p.Validate(); err != nil {
		return p, err
	}
	if p.Summary.Manifest.Relay != r.Cursor() || p.Summary.Manifest.Restore.Operation != "" {
		return p, fmt.Errorf("Signal handoff source mismatch")
	}
	err := r.inbox.transaction(ctx, func(tx *sql.Tx) error {
		saved, err := readRecoveryExport(tx, p.Summary.Challenge.Operation)
		if err != nil {
			return err
		}
		if saved.Summary == nil || protocol.Hash(*saved.Summary) != protocol.Hash(p.Summary) {
			return fmt.Errorf("Signal handoff completion mismatch")
		}
		if !saved.Manifest.Equal(p.Summary.Manifest) || protocol.Hash(saved.Challenge) != protocol.Hash(p.Summary.Challenge) {
			return fmt.Errorf("Signal handoff export changed")
		}
		if err = verifySignalHandoffExport(ctx, tx, saved); err != nil {
			return err
		}
		if saved.Handoff != nil && protocol.Hash(*saved.Handoff) != protocol.Hash(p) {
			return fmt.Errorf("Signal handoff changed")
		}
		if err = writeSignalHandoffIntent(r.inbox.dir, p, saved.Handoff != nil); err != nil {
			return err
		}
		if err = r.boundary("signal_handoff_source_intent"); err != nil {
			return err
		}
		saved.Handoff = &p
		if err = saveRecoveryExport(tx, saved); err != nil {
			return err
		}
		if err = r.boundary("signal_handoff_source_transaction"); err != nil {
			return err
		}
		return writeSignalHandoffIntent(r.inbox.dir, p, true)
	})
	if err == nil {
		err = r.boundary("signal_handoff_source_commit")
	}
	return p, err
}
func verifySignalHandoffExport(ctx context.Context, tx *sql.Tx, s recoveryExport) error {
	if err := verifyRecoveryExport(tx, s); err != nil {
		return err
	}
	var highest, acked, listed, count uint64
	if err := tx.QueryRowContext(ctx, "SELECT acked,listed,COALESCE((SELECT seq FROM sqlite_sequence WHERE name='events'),0),(SELECT count(*) FROM events) FROM identity WHERE singleton=1").Scan(&acked, &listed, &highest, &count); err != nil {
		return err
	}
	m := s.Manifest
	if highest != m.Highest || count != m.EventCount() || acked != m.Acknowledged || listed != m.Listed {
		return fmt.Errorf("Signal stream advanced since reconciliation")
	}
	rows, err := tx.QueryContext(ctx, "SELECT r.position,r.hash,e.key,e.hash,e.body FROM recovery_records r LEFT JOIN events e ON e.position=r.position WHERE r.operation=? ORDER BY r.position", s.Challenge.Operation)
	if err != nil {
		return err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var position uint64
		var evidence, key, hash, raw string
		if err = rows.Scan(&position, &evidence, &key, &hash, &raw); err != nil {
			return err
		}
		var e interaction.SpoolEvent
		if err = protocol.Decode([]byte(raw), &e, 16384); err != nil {
			return err
		}
		if position != m.PositionAt(uint64(len(hashes))) || position > m.Highest || e.Position != 0 || e.Key != key || e.Hash != hash {
			return fmt.Errorf("Signal handoff relay event changed")
		}
		e.Position = position
		if err = interaction.ValidateSignalRecoveryEvent(e, m.Relay); err != nil {
			return err
		}
		if evidence != protocol.Hash(e) {
			return fmt.Errorf("Signal handoff relay evidence changed")
		}
		hashes = append(hashes, evidence)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if uint64(len(hashes)) != m.EventCount() || interaction.RecoveryEvidenceDigest(hashes) != m.Digest {
		return fmt.Errorf("Signal handoff relay digest changed")
	}
	return nil
}
func writeSignalHandoffIntent(d *safefs.Directory, p interaction.SignalRecoveryHandoff, completed bool) error {
	var saved interaction.SignalRecoveryHandoff
	err := d.ReadJSON(signalHandoffFile, &saved)
	if os.IsNotExist(err) && !completed {
		return d.WriteJSON(signalHandoffFile, p, false)
	}
	if err != nil {
		return err
	}
	if protocol.Hash(saved) != protocol.Hash(p) {
		return fmt.Errorf("Signal handoff intent changed")
	}
	return nil
}
func (r *RestoredRecoveryInbox) PrepareSignalHandoff(ctx context.Context, p interaction.SignalRecoveryHandoff) (interaction.SignalRecoveryHandoff, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(ctx); err != nil {
		return p, err
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	if !p.Summary.Manifest.Equal(r.manifest()) {
		return p, fmt.Errorf("restored Signal handoff manifest changed")
	}
	saved, err := r.readExport()
	if err != nil {
		return p, err
	}
	if saved.Summary == nil || protocol.Hash(*saved.Summary) != protocol.Hash(p.Summary) || r.journal.GatewaySummary != protocol.Hash(p.Summary) {
		return p, fmt.Errorf("restored Signal handoff completion missing")
	}
	if r.journal.GatewayHandoff != "" && r.journal.GatewayHandoff != protocol.Hash(p) {
		return p, fmt.Errorf("restored Signal handoff changed")
	}
	if err = writeSignalHandoffIntent(r.stage, p, r.journal.GatewayHandoff != ""); err != nil {
		return p, err
	}
	if err = inboxRestoreBoundary(r.fault, "signal_handoff_source_intent"); err != nil {
		return p, err
	}
	if err = r.check(ctx); err != nil {
		return p, err
	}
	if err = writeSignalHandoffIntent(r.stage, p, true); err != nil {
		return p, err
	}
	next := r.journal
	next.GatewayHandoff = protocol.Hash(p)
	if err = r.dir.WriteJSON("restore.json", next, true); err != nil {
		return p, err
	}
	r.journal = next
	return p, inboxRestoreBoundary(r.fault, "signal_handoff_source_commit")
}
