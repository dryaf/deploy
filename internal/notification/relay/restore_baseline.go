package relay

import (
	"context"
	"database/sql"
	"fmt"
)

const baselineQuarantineSchema = 6

func inboxQuarantineSchema(sourceSchema int) int {
	if sourceSchema == runtimeInboxSchema {
		return baselineQuarantineSchema
	}
	return 3
}
func verifyBaselineQuarantine(ctx context.Context, db *sql.DB, j InboxRestoreState) error {
	m := j.Snapshot
	for _, table := range []string{"identity", "restore_identity", "restore_binding", "signal_cursor", "signal_runtime", "sqlite_sequence"} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("baseline quarantine singleton changed")
		}
	}
	proof, err := readInboxBaselineProof(db, inboxPin{Identity: j.Before, Account: j.Account, Baseline: m.BaselineHash})
	if err != nil {
		return err
	}
	if proof.Handoff.Target.Position != m.BaselinePosition || proof.Handoff.Summary.Manifest.Highest != m.HistoricalHighest || proof.Handoff.Summary.Manifest.EventCount() != m.historicalCount() {
		return fmt.Errorf("baseline quarantine boundary changed")
	}
	digest, count, err := baselineEventDigest(ctx, db, *proof, m.Highest)
	if err != nil {
		return err
	}
	if count != m.RetainedEvents || digest != m.EventsDigest {
		return fmt.Errorf("baseline quarantine history changed")
	}
	return nil
}
