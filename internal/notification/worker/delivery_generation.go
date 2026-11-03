package worker

import (
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// The generation column normally equals the immutable envelope. A recovery
// override requires the independently journaled retry and its canonical history;
// editing just that column never authorizes a send. All ordinary validity and
// status guards still run after this check.
func (s *Store) deliveryGeneration(tx *sql.Tx, o ownership, j Job) (string, error) {
	m := j.Message
	var generation, hash string
	if err := tx.QueryRow("SELECT generation,hash FROM messages WHERE producer=? AND id=?", m.Producer.ID, m.ID).Scan(&generation, &hash); err != nil {
		return "", err
	}
	if hash != protocol.Hash(m) {
		return "", fmt.Errorf("delivery payload changed")
	}
	if generation == m.Generation {
		return generation, nil
	}
	if exists, err := retryBindingSchema(tx); err != nil {
		return "", err
	} else if !exists {
		return "", fmt.Errorf("delivery generation has no retry authority")
	}
	var raw string
	if err := tx.QueryRow("SELECT body FROM reconciliation_retry_bindings WHERE producer=? AND message=?", m.Producer.ID, m.ID).Scan(&raw); err != nil {
		return "", err
	}
	var b RecoveryRetryBinding
	if err := protocol.Decode([]byte(raw), &b, 32768); err != nil {
		return "", err
	}
	if err := b.Validate(); err != nil {
		return "", err
	}
	d := b.Applied.Application.Decision.Decision
	if d.Identity != s.identity || d.Producer != m.Producer || d.MessageID != m.ID || d.MessageHash != hash ||
		b.OriginalGeneration != m.Generation || b.Generation.Generation != generation || o.Generations[m.Generation] != "revoked" {
		return "", fmt.Errorf("delivery retry binding changed")
	}
	stage, err := s.dir.Sub("recovery/" + d.Operation)
	if err != nil {
		return "", err
	}
	defer stage.Close()
	r := &recoveryReconciler{stage: stage, journal: snapshotRecovery{Operation: d.Operation, Before: b.Generation.Ownership.Before, After: s.identity}}
	verified, phase, err := r.retryBindingEvidence(tx, m.Producer, m.ID)
	if err != nil {
		return "", err
	}
	if phase != "bound" || protocol.Hash(verified) != protocol.Hash(b) {
		return "", fmt.Errorf("delivery retry is not committed")
	}
	var plan recoveryGenerationIntent
	if err = stage.ReadJSON(generationFile(m.Producer), &plan); err != nil {
		return "", err
	}
	if err = r.validateGenerationIntent(plan, m.Producer, d.SummaryHash); err != nil {
		return "", err
	}
	if plan.Ownership != b.Generation.Ownership || plan.Generation != generation {
		return "", fmt.Errorf("delivery generation intent changed")
	}
	if err = s.checkOwnershipHistoryTail(m.Producer, o.Revision); err != nil {
		return "", err
	}
	if err = s.verifyOwnershipPrefix(o); err != nil {
		return "", err
	}
	var migration, event ownershipTransition
	if err = s.dir.ReadJSON(ownershipHistoryPath(m.Producer, b.Generation.Ownership.Revision), &migration); err != nil {
		return "", err
	}
	if err = s.dir.ReadJSON(ownershipHistoryPath(m.Producer, b.Generation.Revision), &event); err != nil {
		return "", err
	}
	if protocol.Hash(event) != b.Generation.History || event.Operation != "stage" || event.Generation != generation ||
		event.Previous != b.Generation.Ownership.History || protocol.Hash(migration) != b.Generation.Ownership.History ||
		migration.Recovery == nil || migration.Recovery.Binding.Operation != d.Operation || migration.Recovery.Binding.Before != b.Generation.Ownership.Before ||
		migration.Recovery.Binding.After != s.identity || migration.Recovery.SummaryHash != d.SummaryHash {
		return "", fmt.Errorf("delivery generation history changed")
	}
	return generation, nil
}
