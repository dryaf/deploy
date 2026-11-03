package relay

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func verifyPreparedSignalHandoff(ctx context.Context, tx *sql.Tx, p interaction.SignalRecoveryHandoff) (saved recoveryExport, err error) {
	saved, err = readRecoveryExport(tx, p.Summary.Challenge.Operation)
	if err != nil {
		return
	}
	if saved.Handoff == nil || protocol.Hash(*saved.Handoff) != protocol.Hash(p) || saved.Summary == nil || protocol.Hash(*saved.Summary) != protocol.Hash(p.Summary) || !saved.Manifest.Equal(p.Summary.Manifest) || protocol.Hash(saved.Challenge) != protocol.Hash(p.Summary.Challenge) {
		return saved, fmt.Errorf("Signal cursor requires matching prepared source")
	}
	err = verifySignalHandoffExport(ctx, tx, saved)
	return
}
func (r *RecoveryInbox) StageSignalCursor(ctx context.Context, p interaction.SignalRecoveryHandoff) (proof interaction.SignalRecoveryCursor, err error) {
	if _, err = r.PrepareSignalHandoff(ctx, p); err != nil {
		return
	}
	s := r.inbox
	s.mu.Lock()
	defer s.mu.Unlock()
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return s.checkLease()
	}
	if err = check(); err != nil {
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	saved, err := verifyPreparedSignalHandoff(ctx, tx, p)
	tx.Rollback()
	if err != nil {
		return
	}
	if err = writeSignalHandoffIntent(s.dir, p, true); err != nil {
		return
	}
	proof, err = stageSignalCursorCandidate(ctx, s.db, s.dir, p, saved.Cursor, check, r.fault)
	if err != nil {
		return
	}
	if err = verifySignalCursorFiles(ctx, s.dir, proof); err != nil {
		return
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	saved, err = verifyPreparedSignalHandoff(ctx, tx, p)
	if err != nil {
		return
	}
	if saved.Cursor != nil && protocol.Hash(*saved.Cursor) != protocol.Hash(proof) {
		return proof, fmt.Errorf("Signal cursor completion changed")
	}
	saved.Cursor = &proof
	if err = saveRecoveryExport(tx, saved); err != nil {
		return
	}
	if err = check(); err != nil {
		return
	}
	if err = tx.Commit(); err != nil {
		return
	}
	err = r.boundary("signal_cursor_source_published")
	return
}
func (r *RestoredRecoveryInbox) StageSignalCursor(ctx context.Context, p interaction.SignalRecoveryHandoff) (proof interaction.SignalRecoveryCursor, err error) {
	if _, err = r.PrepareSignalHandoff(ctx, p); err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err = r.check(ctx); err != nil {
		return
	}
	if r.journal.GatewayHandoff != protocol.Hash(p) {
		return proof, fmt.Errorf("Signal cursor handoff changed")
	}
	var completed *interaction.SignalRecoveryCursor
	if r.journal.GatewayCursor != "" {
		completed = &interaction.SignalRecoveryCursor{}
		if err = r.stage.ReadJSON(signalCursorManifest, completed); err != nil {
			return
		}
		if protocol.Hash(*completed) != r.journal.GatewayCursor {
			return proof, fmt.Errorf("Signal cursor completion changed")
		}
	}
	source, err := inboxArtifactDB(r.stage, "source.db", true)
	if err != nil {
		return
	}
	defer source.Close()
	proof, err = stageSignalCursorCandidate(ctx, source, r.stage, p, completed, func() error { return r.check(ctx) }, r.fault)
	if err != nil {
		return
	}
	if err = r.check(ctx); err != nil {
		return
	}
	if err = verifySignalCursorFiles(ctx, r.stage, proof); err != nil {
		return
	}
	next := r.journal
	next.GatewayCursor = protocol.Hash(proof)
	if err = r.dir.WriteJSON("restore.json", next, true); err != nil {
		return
	}
	r.journal = next
	err = inboxRestoreBoundary(r.fault, "signal_cursor_source_published")
	return
}
