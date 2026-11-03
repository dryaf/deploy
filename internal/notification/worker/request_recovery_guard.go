package worker

import (
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func requestRecoveryAdmissionReady(tx *sql.Tx) (bool, error) {
	exists, err := requestRecoverySchema(tx)
	if err != nil {
		return false, err
	}
	var bindingJSON string
	err = tx.QueryRow("SELECT value FROM metadata WHERE key='recovery'").Scan(&bindingJSON)
	if err == sql.ErrNoRows && !exists {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !exists {
		return false, fmt.Errorf("recovery request inventory not sealed")
	}
	var binding recoveryBinding
	if err = protocol.Decode([]byte(bindingJSON), &binding, 4096); err != nil {
		return false, err
	}
	var raw string
	if err = tx.QueryRow("SELECT body FROM reconciliation_request_seal WHERE singleton=1").Scan(&raw); err != nil {
		return false, err
	}
	var seal RecoveryRequestSummary
	if err = protocol.Decode([]byte(raw), &seal, 8192); err != nil {
		return false, err
	}
	if err = seal.Validate(); err != nil {
		return false, err
	}
	if seal.Operation != binding.Operation || seal.Identity != binding.After {
		return false, fmt.Errorf("request seal belongs to another recovery")
	}
	var count uint64
	if err = tx.QueryRow("SELECT count(*) FROM reconciliation_request_fences").Scan(&count); err != nil {
		return false, err
	}
	if count != seal.Requests {
		return false, fmt.Errorf("request recovery fences missing")
	}
	return true, nil
}

func recoveredRequestFor(tx *sql.Tx, in interaction.Inbound) (*recoveredRequest, error) {
	ready, err := requestRecoveryAdmissionReady(tx)
	if err != nil || !ready {
		return nil, err
	}
	var raw string
	var hash, target string
	err = tx.QueryRow("SELECT hash,body,target_id FROM reconciliation_request_fences WHERE key=?", in.RequestKey()).Scan(&hash, &raw, &target)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v recoveredRequest
	if err = protocol.Decode([]byte(raw), &v, 4096); err != nil {
		return nil, err
	}
	if v.Key != in.RequestKey() || protocol.Hash(v) != hash || v.TargetID != target {
		return nil, fmt.Errorf("request recovery fence changed")
	}
	return &v, nil
}
func guardRequestAdmission(tx *sql.Tx, in interaction.Inbound) error {
	v, err := recoveredRequestFor(tx, in)
	if err != nil {
		return err
	}
	if v == nil {
		if err := guardSignalRecoveryInput(tx, in); err != nil {
			return err
		}
		return guardTelegramRecoveryInput(tx, in)
	}
	if v.TargetID == "" {
		return interaction.RejectQuietly("historical request quarantined; send a new request")
	}
	var hash, id string
	if err = tx.QueryRow("SELECT input_hash,command_id FROM command_inbox WHERE request_key=?", v.Key).Scan(&hash, &id); err != nil {
		return err
	}
	if hash != v.InputHash || id != v.TargetID || protocol.Hash(in) != hash {
		return fmt.Errorf("historical request mapping changed")
	}
	m, err := readRetainedRequest(tx, in)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("historical request mapping missing")
	}
	targetHash := protocol.Hash(m.Command)
	if m.Query != nil {
		targetHash = protocol.Hash(m.Query)
	}
	if targetHash != v.TargetHash {
		return fmt.Errorf("historical request target changed")
	}
	// An existing mapping is read-only replay through the ordinary admission path.
	return nil
}
