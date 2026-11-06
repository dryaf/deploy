package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

var errLEDRecoveryPending = errors.New("inherited LED fault requires recovery reconciliation")

type recoveredLEDProof struct {
	*ledAdoptionEvidence
	Remaining []recoveryLEDFault
}

// recoveredLEDAuthority verifies the independent adoption/preservation/inventory
// chain, not merely a row checksum or an identity with the same worker UUID.
// The original identities remain immutable. This supplies ownership only, never
// permission to Set, Clear, replay an old operation or publish healthy output.
func (s *Store) recoveredLEDAuthority(ctx context.Context, q ledJournalReader) (*recoveredLEDProof, error) {
	exists, err := ledAdoptionSchema(ctx, q)
	if err != nil {
		return nil, err
	}
	if s.dir == nil {
		if !exists {
			return nil, nil
		}
		return nil, fmt.Errorf("inherited LED ownership requires independent history")
	}
	var journal snapshotRecovery
	if err = s.dir.ReadJSON("recovery.json", &journal); err != nil {
		if !exists && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	stage, err := s.dir.Sub("recovery/" + journal.Operation)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	if !exists {
		var record recoveryLEDAdoption
		err = stage.ReadJSON(recoveryLEDAdoptionFile, &record)
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inherited LED ownership lost its SQL acknowledgement")
	}
	if err = journal.validate(s.config); err != nil {
		return nil, err
	}
	if journal.After != s.identity || journal.Phase != "quarantined" {
		return nil, fmt.Errorf("inherited LED recovery identity changed")
	}
	var canonical protocol.Identity
	if err = s.dir.ReadJSON("identity.json", &canonical); err != nil {
		return nil, err
	}
	var identityRaw string
	if err = q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&identityRaw); err != nil {
		return nil, err
	}
	var stored protocol.Identity
	if protocol.Decode([]byte(identityRaw), &stored, 4096) != nil || stored != s.identity || canonical != s.identity {
		return nil, fmt.Errorf("inherited LED canonical or database identity changed")
	}
	r := recoveryReconciler{dir: s.dir, stage: stage, journal: journal, config: s.config}
	evidence, err := r.readLEDAdoptionAuthority(ctx, q)
	if err != nil {
		return nil, err
	}
	if evidence == nil || evidence.Phase != "committed" || evidence.After.Layout.UID != os.Getuid() {
		return nil, fmt.Errorf("inherited LED ownership is not committed for this UID")
	}
	epoch, phase, err := r.readLEDEpoch(ctx, q, evidence)
	if err != nil {
		return nil, err
	}
	if phase == "prepared" {
		return nil, fmt.Errorf("LED epoch handoff requires resume")
	}
	imports, err := r.readLEDImports(ctx, q, evidence, epoch)
	if err != nil {
		return nil, err
	}
	for _, record := range imports {
		if !record.Committed {
			return nil, fmt.Errorf("LED cleanup import requires resume")
		}
	}
	wantLayout := evidence.After.Layout
	if phase == "committed" {
		wantLayout = epoch.AfterLayout
	}
	var raw string
	if err = q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil {
		return nil, err
	}
	var layout ledLayout
	if protocol.Decode([]byte(raw), &layout, protocol.MaxRequest) != nil || protocol.Hash(layout) != protocol.Hash(wantLayout) {
		return nil, fmt.Errorf("inherited LED mapping changed")
	}
	remaining, err := s.remainingRuntimeLEDFaults(ctx, q, evidence.After.Faults, remainingInheritedFaults(evidence, imports))
	if err != nil {
		return nil, err
	}
	return &recoveredLEDProof{evidence, remaining}, nil
}

// Old operation/mapping journals must be archived by the explicit epoch
// handoff. An ownership proof does not permit appending new-epoch operations to
// old history or reinterpreting an old mapping transition under a new identity.
func (s *Store) checkInheritedLEDHistory(ctx context.Context, q ledJournalReader) error {
	exists, err := ledAdoptionSchema(ctx, q)
	if err != nil {
		return err
	}
	if !exists {
		// Losing both SQL markers must not hide an independent adoption intent.
		_, err = s.recoveredLEDAuthority(ctx, q)
		return err
	}
	if err = checkLEDOperations(ctx, q, s.identity); err != nil {
		return fmt.Errorf("inherited LED operation history requires epoch handoff: %w", err)
	}
	if err = checkLEDMigrations(ctx, q, s.config, s.identity, false); err != nil {
		return fmt.Errorf("inherited LED mapping history requires epoch handoff: %w", err)
	}
	return nil
}

// recoveredLEDFaults validates every retained inherited row against its exact
// adopted incarnation. Only a validated committed cleanup import can account for
// a removed row. Missing rows fail closed and are not recreated.
func (s *Store) recoveredLEDFaults(ctx context.Context, q ledJournalReader) (map[string]recoveryLEDFault, error) {
	evidence, err := s.recoveredLEDAuthority(ctx, q)
	if err != nil || evidence == nil {
		return nil, err
	}
	proven := make(map[string]recoveryLEDFault, len(evidence.Remaining))
	for _, f := range evidence.Remaining {
		proven[f.ID] = f
	}
	rows, err := q.QueryContext(ctx, "SELECT id,binding,condition,identity,obsolete FROM led_states ORDER BY id LIMIT 4097")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	retained := map[string]recoveryLEDFault{}
	count := 0
	for rows.Next() {
		count++
		if count > 4096 {
			return nil, fmt.Errorf("LED runtime ownership inventory exceeds budget")
		}
		var f recoveryLEDFault
		var obsolete int
		if err = rows.Scan(&f.ID, &f.Binding, &f.Condition, &f.Identity, &obsolete); err != nil {
			return nil, err
		}
		if obsolete != 0 && obsolete != 1 {
			return nil, fmt.Errorf("invalid LED cleanup tombstone")
		}
		if !slices.Contains([]string{"unknown", "paused", "warning", "critical"}, f.Condition) ||
			(f.Binding != workerLEDBinding && !slices.ContainsFunc(s.config.LEDBindings, func(b LEDBinding) bool { return b.ID == f.Binding })) {
			return nil, fmt.Errorf("invalid LED runtime binding or condition")
		}
		if original, ok := proven[f.ID]; ok {
			// Tombstones may change after handoff; they never supply a clear grant.
			if f != original {
				return nil, fmt.Errorf("inherited LED incarnation changed")
			}
			retained[f.ID] = original
		} else if err = validateLEDFaultIdentity(f.ID, f.Binding, f.Identity, s.identity, nil); err != nil {
			return nil, fmt.Errorf("unproven inherited LED fault: %w", err)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(retained) != len(proven) {
		return nil, fmt.Errorf("inherited LED ownership lost retained faults")
	}
	return retained, nil
}

func (s *Store) inheritedLEDState(tx *sql.Tx, id, binding, body string) (bool, error) {
	faults, err := s.recoveredLEDFaults(context.Background(), tx)
	if err != nil {
		return false, err
	}
	f, ok := faults[id]
	if ok && (f.Binding != binding || f.Identity != body) {
		return false, fmt.Errorf("inherited LED state identity mismatch")
	}
	return ok, nil
}
