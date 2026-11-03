package relay

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

const runtimeInboxSchema = 5
const signalRuntimeFile = "signal-runtime.json"

type inboxRuntimeReader interface{ QueryRow(string, ...any) *sql.Row }

// Schema 4 remains an inert candidate. Schema 5 requires a separately published
// runtime pin and matching file/SQL proof. This reader does not publish activation.
func readInboxBaseline(db inboxRuntimeReader, pin inboxPin) (proof *interaction.SignalRecoveryCursor, err error) {
	var version, n int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return
	}
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('signal_cursor','signal_runtime')").Scan(&n); err != nil {
		return
	}
	if version == 1 || version == 2 {
		if pin.Baseline != "" || n != 0 {
			return nil, fmt.Errorf("relay baseline schema downgraded")
		}
		return nil, nil
	}
	if version != runtimeInboxSchema || n != 2 || pin.Baseline == "" || pin.RecoveryOperation != "" {
		return nil, fmt.Errorf("relay baseline schema missing or fenced")
	}
	proof, err = readInboxBaselineProof(db, pin)
	if err != nil {
		return
	}
	var raw string
	if err = db.QueryRow("SELECT body FROM identity WHERE singleton=1").Scan(&raw); err != nil {
		return
	}
	var actual inboxPin
	if err = protocol.Decode([]byte(raw), &actual, 4096); err != nil {
		return
	}
	if actual != pin {
		return nil, fmt.Errorf("relay runtime pin changed")
	}
	for _, table := range []string{"identity", "signal_cursor", "signal_runtime", "sqlite_sequence"} {
		if err = db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			return
		}
		if n != 1 {
			return nil, fmt.Errorf("relay runtime singleton changed")
		}
	}
	return
}

// Quarantine uses this to validate preserved old-epoch proof independently of
// the new fenced identity and quarantine schema.
func readInboxBaselineProof(db inboxRuntimeReader, pin inboxPin) (proof *interaction.SignalRecoveryCursor, err error) {
	var raw string
	if err = db.QueryRow("SELECT body FROM signal_runtime WHERE singleton=1").Scan(&raw); err != nil {
		return
	}
	proof = &interaction.SignalRecoveryCursor{}
	if err = protocol.Decode([]byte(raw), proof, 16384); err != nil {
		return
	}
	if err = proof.Validate(); err != nil {
		return
	}
	if protocol.Hash(*proof) != pin.Baseline || proof.Handoff.Target.Identity != pin.Identity || proof.Handoff.Target.Account != pin.Account {
		return nil, fmt.Errorf("relay runtime baseline identity changed")
	}
	if err = db.QueryRow("SELECT body FROM signal_cursor WHERE singleton=1").Scan(&raw); err != nil {
		return
	}
	var handoff interaction.SignalRecoveryHandoff
	if err = protocol.Decode([]byte(raw), &handoff, 16384); err != nil {
		return
	}
	if protocol.Hash(handoff) != protocol.Hash(proof.Handoff) {
		return nil, fmt.Errorf("relay runtime handoff changed")
	}
	return
}

func (s *Inbox) baselinePosition() uint64 {
	if s.baseline == nil {
		return 0
	}
	return s.baseline.Handoff.Target.Position
}
func (s *Inbox) checkBaselineFile() error {
	var proof interaction.SignalRecoveryCursor
	err := s.dir.ReadJSON(signalRuntimeFile, &proof)
	if s.baseline == nil {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("relay runtime proof has no matching pin")
	}
	if err != nil {
		return err
	}
	if protocol.Hash(proof) != protocol.Hash(*s.baseline) {
		return fmt.Errorf("relay runtime proof changed")
	}
	return nil
}
func (s *Inbox) checkBaseline(db inboxRuntimeReader) error {
	pin := inboxPin{Identity: s.identity, Account: s.account}
	if s.baseline != nil {
		pin.Baseline = protocol.Hash(*s.baseline)
	}
	current, err := readInboxBaseline(db, pin)
	if err != nil {
		return err
	}
	if (current == nil) != (s.baseline == nil) {
		return fmt.Errorf("relay runtime baseline changed")
	}
	if current == nil {
		return nil
	}
	if protocol.Hash(*current) != protocol.Hash(*s.baseline) {
		return fmt.Errorf("relay runtime proof regressed")
	}
	floor := s.baselinePosition()
	var acked, listed, seq, highest uint64
	if err = db.QueryRow("SELECT acked,listed,(SELECT seq FROM sqlite_sequence WHERE name='events'),COALESCE((SELECT max(position) FROM events),0) FROM identity WHERE singleton=1").Scan(&acked, &listed, &seq, &highest); err != nil {
		return err
	}
	if seq < floor || max(highest, floor) != seq || listed > seq || acked > listed || (acked != 0 && acked <= floor) || (listed != 0 && listed <= floor) {
		return fmt.Errorf("relay runtime watermarks invalid")
	}
	return nil
}
func (s *Inbox) verifyBaselineHistory(ctx context.Context) error {
	if s.baseline == nil {
		return nil
	}
	var highest uint64
	if err := s.db.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name='events'").Scan(&highest); err != nil {
		return err
	}
	_, _, err := baselineEventDigest(ctx, s.db, *s.baseline, highest)
	return err
}
