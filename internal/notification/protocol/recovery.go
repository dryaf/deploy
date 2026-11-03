package protocol

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/dryaf/deploy/internal/safefs"
)

// Only this explicit result permits a coordinator to start/resume an exchange.
// Transport, corruption and enrollment failures must not be treated as absence.
var ErrRecoveryIncomplete = errors.New("producer reconciliation is incomplete")

// RecoverySession is an internal offline exchange, not a dispatcher or an API
// endpoint. Completion records evidence; it grants no delivery or command rights.
type RecoverySession interface {
	Challenge(context.Context, Producer) (RecoveryChallenge, error)
	Begin(context.Context, RecoveryChallenge, RecoveryManifest) error
	Import(context.Context, RecoveryChallenge, []RecoveryRecord) error
	Finish(context.Context, RecoveryChallenge, Snapshot) (RecoverySummary, error)
	Summary(context.Context, Producer) (RecoverySummary, error)
	Close() error
}

// Recovery types are shared by producer ledgers and the internal reconciler.
// Their presence does not expose a restore or activation operation on the API.
type RecoveryChallenge struct {
	Operation           string
	Identity            Identity
	Producer            Producer
	Nonce               string
	IssuedAt, ExpiresAt time.Time
	BootID              string
	Uptime              float64
}

type RecoveryManifest struct {
	HighestIssued       uint64
	HighestAcknowledged uint64
	CompactedThrough    uint64
	OutcomeCheckpoint   string
	ImportedReceipts    Cursor
}

type RetainedReceipt struct {
	Identity Identity
	Producer Producer
	Receipt  Receipt
}

type RecoveryRecord struct {
	Message Message
	Receipt *RetainedReceipt
}

type RecoverySummary struct {
	Challenge                 RecoveryChallenge
	Manifest                  RecoveryManifest
	Snapshot                  Snapshot
	Ambiguous                 int
	UnknownFirst, UnknownLast uint64
	DeliveryEnabled           bool
}

// RecoveryOwnership is evidence that canonical ownership moved to the recovery
// epoch with every old generation revoked. It is never activation or drain proof.
type RecoveryOwnership struct {
	Version                    int
	Operation                  string
	Before, After              Identity
	Producer                   Producer
	SummaryHash                string
	PreviousRevision, Revision uint64
	PreviousHistory, History   string
	DeliveryEnabled            bool
}

func (p RecoveryOwnership) Validate() error {
	if p.Version != 1 || !safefs.ValidID(p.Operation, "n") || !p.Before.Valid() || !p.After.Valid() ||
		p.Before.Worker != p.After.Worker || p.Before.Recovery == p.After.Recovery || !p.Producer.Valid() ||
		!safefs.DigestPattern.MatchString(p.SummaryHash) || p.PreviousRevision == 0 || p.PreviousRevision == math.MaxUint64 ||
		p.Revision != p.PreviousRevision+1 || !safefs.DigestPattern.MatchString(p.PreviousHistory) ||
		!safefs.DigestPattern.MatchString(p.History) || p.History == p.PreviousHistory || p.DeliveryEnabled {
		return fmt.Errorf("invalid recovery ownership preparation")
	}
	return nil
}

// Callers retain the offline session for the whole handoff. Summary and ownership
// must describe the same completed exchange; transport errors are never absence.
type RecoveryOwnershipSession interface {
	Summary(context.Context, Producer) (RecoverySummary, error)
	PrepareRecoveryOwnership(context.Context, Producer) (RecoveryOwnership, error)
	Close() error
}

// RecoveryGeneration records a fresh staged generation after ownership recovery.
// It grants no attempts and never reuses a revoked generation.
type RecoveryGeneration struct {
	Version         int
	Ownership       RecoveryOwnership
	Generation      string
	Revision        uint64
	History         string
	DeliveryEnabled bool
}

func (g RecoveryGeneration) Validate() error {
	if err := g.Ownership.Validate(); err != nil {
		return err
	}
	if g.Version != 1 || !safefs.ValidID(g.Generation, "n") || g.Ownership.Revision == math.MaxUint64 ||
		g.Revision != g.Ownership.Revision+1 || !safefs.DigestPattern.MatchString(g.History) || g.History == g.Ownership.History || g.DeliveryEnabled {
		return fmt.Errorf("invalid recovery delivery generation")
	}
	return nil
}

// RecoveryProducerCommit prepares the delivery ledger's epoch transition. The
// new stream starts at zero: none of its outcome receipts have been imported.
// CommandSummaryHash is required when this producer has an interactive ledger.
// This proof neither activates delivery nor changes the command ledger identity.
type RecoveryProducerCommit struct {
	Version            int
	Generation         RecoveryGeneration
	Cursor             Cursor
	HighestIssued      uint64
	CommandSummaryHash string
	DeliveryEnabled    bool
}

func (p RecoveryProducerCommit) Validate() error {
	if err := p.Generation.Validate(); err != nil {
		return err
	}
	if p.Version != 1 || p.Cursor.Identity != p.Generation.Ownership.After || p.Cursor.Producer != p.Generation.Ownership.Producer || p.Cursor.Position != 0 ||
		(p.HighestIssued != 0 && !Sequence(p.HighestIssued)) || (p.CommandSummaryHash != "" && !safefs.DigestPattern.MatchString(p.CommandSummaryHash)) || p.DeliveryEnabled {
		return fmt.Errorf("invalid recovery producer commit proof")
	}
	return nil
}

type RecoveryProducerSession interface {
	RecoveryOwnershipSession
	PrepareRecoveryGeneration(context.Context, Producer) (RecoveryGeneration, error)
	PrepareRecoveryProducerCommit(context.Context, Producer) (RecoveryProducerCommit, error)
}
