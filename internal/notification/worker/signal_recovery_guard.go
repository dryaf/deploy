package worker

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func readSignalRecoveryCursor(tx *sql.Tx, account string) (*interaction.SignalRecoveryCursor, error) {
	exists, err := signalCursorSchema(tx)
	if err != nil {
		return nil, err
	}
	var raw string
	err = tx.QueryRow("SELECT value FROM metadata WHERE key='recovery'").Scan(&raw)
	if err == sql.ErrNoRows && !exists {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, protocol.ErrRecoveryIncomplete
	}
	var binding recoveryBinding
	if err = protocol.Decode([]byte(raw), &binding, 4096); err != nil {
		return nil, err
	}
	var hash string
	if err = tx.QueryRow("SELECT hash,body FROM reconciliation_signal_cursors WHERE account=?", account).Scan(&hash, &raw); err != nil {
		return nil, err
	}
	var proof interaction.SignalRecoveryCursor
	if err = protocol.Decode([]byte(raw), &proof, 16384); err != nil {
		return nil, err
	}
	if err = proof.Validate(); err != nil {
		return nil, err
	}
	p := proof.Handoff
	if p.Target.Account != account || p.Summary.Challenge.Operation != binding.Operation || p.Summary.Challenge.Gateway != binding.After || protocol.Hash(proof) != hash {
		return nil, fmt.Errorf("Signal runtime recovery binding changed")
	}
	ready, err := requestRecoveryAdmissionReady(tx)
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, protocol.ErrRecoveryIncomplete
	}
	if err = tx.QueryRow("SELECT body FROM reconciliation_request_seal WHERE singleton=1").Scan(&raw); err != nil {
		return nil, err
	}
	var seal RecoveryRequestSummary
	if err = protocol.Decode([]byte(raw), &seal, 8192); err != nil {
		return nil, err
	}
	if protocol.Hash(seal) != p.RequestsHash {
		return nil, fmt.Errorf("Signal runtime request seal changed")
	}
	handoffReady, err := signalHandoffSchema(tx)
	if err != nil {
		return nil, err
	}
	if !handoffReady {
		return nil, protocol.ErrRecoveryIncomplete
	}
	if err = tx.QueryRow("SELECT hash,body FROM reconciliation_signal_handoffs WHERE account=?", account).Scan(&hash, &raw); err != nil {
		return nil, err
	}
	var handoff interaction.SignalRecoveryHandoff
	if err = protocol.Decode([]byte(raw), &handoff, 16384); err != nil {
		return nil, err
	}
	if hash != protocol.Hash(p) || protocol.Hash(handoff) != hash {
		return nil, fmt.Errorf("Signal runtime handoff changed")
	}
	if err = tx.QueryRow("SELECT cursor FROM signal_imports WHERE account=?", account).Scan(&raw); err != nil {
		return nil, err
	}
	var cursor interaction.SpoolCursor
	if err = protocol.Decode([]byte(raw), &cursor, 4096); err != nil {
		return nil, err
	}
	if cursor.Account != account || cursor.Identity != p.Target.Identity || cursor.Position < p.Target.Position || cursor.Position > math.MaxInt64 {
		return nil, fmt.Errorf("Signal runtime cursor regressed")
	}
	return &proof, nil
}
func guardSignalRecoveryInput(tx *sql.Tx, in interaction.Inbound) error {
	if in.Provider != "signal" {
		return nil
	}
	proof, err := readSignalRecoveryCursor(tx, in.Account)
	if err != nil || proof == nil {
		return err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var found bool
	err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM provider_inbox WHERE account=? AND id>? AND id<=json_extract((SELECT cursor FROM signal_imports WHERE account=?),'$.position') AND body=? AND state IN ('pending','converted'))", in.Account, proof.Handoff.Target.Position, in.Account, string(raw)).Scan(&found)
	if err != nil {
		return err
	}
	if !found {
		return interaction.RejectQuietly("Signal input has no post-recovery inbox evidence")
	}
	return nil
}
