package protocol

import (
	"context"
	"time"
)

// RecoveryLEDClearResult is a historical exact-ID attempt, never a replay grant.
type RecoveryLEDClearResult struct {
	InventoryID  string    `json:"inventory_id"`
	Binding      string    `json:"binding"`
	FaultID      string    `json:"fault_id"`
	EvidenceHash string    `json:"evidence_hash"`
	Outcome      string    `json:"outcome"`
	IssuedAt     time.Time `json:"issued_at"`
	FinishedAt   time.Time `json:"finished_at"`
}

// RecoveryLEDDomainGuard revalidates source-owned domain evidence while the
// producer coordinator retains its stable ownership and notifier leases.
type RecoveryLEDDomainGuard func(context.Context, RecoveryLEDEvidence) error

type RecoveryLEDClearRequest struct {
	InventoryID  string `json:"inventory_id"`
	Binding      string `json:"binding"`
	FaultID      string `json:"fault_id"`
	EvidenceHash string `json:"evidence_hash"`
}

type RecoveryLEDCleanupSession interface {
	ClearLED(context.Context, RecoveryLEDClearRequest, RecoveryLEDDomainGuard) (RecoveryLEDClearResult, error)
	Close() error
}

// RecoveryLEDChallenge is a request for new domain evaluation, never permission
// to act on the daemon. Its inventory and both source ledgers are immutable pins.
type RecoveryLEDChallenge struct {
	Version                                                         int
	Operation                                                       string
	Before, After                                                   Identity
	Producer                                                        Producer
	InventoryID, InventoryHash, SourcesHash, DomainHash, ConfigHash string
	PreviousHash                                                    string
	Nonce                                                           string
	IssuedAt, ExpiresAt                                             time.Time
	BootID                                                          string
	Uptime                                                          float64
}
type RecoveryLEDReport struct {
	Snapshot    Snapshot
	Transitions []FaultTransition
}
type RecoveryLEDEvidence struct {
	Challenge  RecoveryLEDChallenge
	Report     RecoveryLEDReport
	ReceivedAt time.Time
	Hash       string
}
type RecoveryLEDEvidenceSession interface {
	ChallengeLED(context.Context, string, Producer) (RecoveryLEDChallenge, error)
	SubmitLED(context.Context, RecoveryLEDChallenge, RecoveryLEDReport) (RecoveryLEDEvidence, error)
	LEDEvidence(context.Context, string, Producer) (RecoveryLEDEvidence, error)
	Close() error
}
