package worker

import (
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func readTelegramRecoveryCursor(tx *sql.Tx, account string) (*TelegramRecoveryCursor, error) {
	exists, err := telegramCursorSchema(tx)
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
	if err = tx.QueryRow("SELECT hash,body FROM reconciliation_telegram_cursors WHERE account=?", account).Scan(&hash, &raw); err != nil {
		return nil, err
	}
	var proof TelegramRecoveryCursor
	if err = protocol.Decode([]byte(raw), &proof, 16384); err != nil {
		return nil, err
	}
	if err = proof.Validate(); err != nil {
		return nil, err
	}
	if proof.Drain.Operation != binding.Operation || proof.Drain.Identity != binding.After || proof.Drain.Account != account || protocol.Hash(proof) != hash {
		return nil, fmt.Errorf("Telegram recovery cursor binding changed")
	}
	if ready, e := requestRecoveryAdmissionReady(tx); e != nil {
		return nil, e
	} else if !ready {
		return nil, protocol.ErrRecoveryIncomplete
	}
	if err = tx.QueryRow("SELECT body FROM reconciliation_request_seal WHERE singleton=1").Scan(&raw); err != nil {
		return nil, err
	}
	var requests RecoveryRequestSummary
	if err = protocol.Decode([]byte(raw), &requests, 8192); err != nil {
		return nil, err
	}
	if protocol.Hash(requests) != proof.RequestsHash {
		return nil, fmt.Errorf("Telegram recovery request inventory changed")
	}
	return &proof, nil
}
func guardTelegramRecoveryInput(tx *sql.Tx, in interaction.Inbound) error {
	if in.Provider != "telegram" {
		return nil
	}
	proof, err := telegramAdmissionCursor(tx, in.Account)
	if err != nil {
		return err
	}
	if proof != nil && !in.SentAt.After(proof.NotBefore) {
		return interaction.RejectQuietly("request predates recovery cutoff; send a new request")
	}
	return nil
}
