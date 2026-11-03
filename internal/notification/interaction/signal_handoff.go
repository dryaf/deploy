package interaction

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// SignalRecoveryHandoff agrees on a future stream baseline. Target is not an
// installed cursor or an acknowledgement; historical ACK/listed state is retained.
type SignalRecoveryHandoff struct {
	Version        int
	Summary        SignalRecoverySummary
	RequestsHash   string
	Before, Target SpoolCursor
	IntakeEnabled  bool
}

func (p SignalRecoveryHandoff) Validate() error {
	if err := p.Summary.Validate(); err != nil {
		return err
	}
	s := p.Summary
	target := s.Manifest.Relay
	if s.Manifest.Restore.Operation != "" {
		target.Identity = s.Manifest.Restore.After
	}
	target.Position = max(s.Manifest.Highest, s.GatewayHighest)
	if p.Version != 1 || p.IntakeEnabled || !safefs.DigestPattern.MatchString(p.RequestsHash) || p.Target != target || p.Before.Account != target.Account || p.Before.Identity != s.Manifest.Relay.Identity || p.Before.Position > target.Position {
		return fmt.Errorf("invalid Signal recovery handoff")
	}
	return nil
}

type SignalHandoffSource interface {
	SignalRecoverySource
	PrepareSignalHandoff(context.Context, SignalRecoveryHandoff) (SignalRecoveryHandoff, error)
}
type SignalHandoffSession interface {
	SignalRecoverySession
	PrepareSignalHandoff(context.Context, string) (SignalRecoveryHandoff, error)
	RecordSignalHandoff(context.Context, SignalRecoveryHandoff) error
}

// The caller retains both offline leases. No transaction spans a remote call,
// and agreement changes neither stream's operational identity nor cursor.
func PrepareSignalRecoveryHandoff(ctx context.Context, source SignalHandoffSource, gateway SignalHandoffSession) (proof SignalRecoveryHandoff, err error) {
	if source == nil || gateway == nil {
		return proof, fmt.Errorf("Signal handoff dependencies missing")
	}
	proof, err = gateway.PrepareSignalHandoff(ctx, source.Cursor().Account)
	if err != nil {
		return
	}
	if err = proof.Validate(); err != nil {
		return
	}
	if proof.Summary.Manifest.Relay != source.Cursor() {
		return proof, fmt.Errorf("Signal handoff source identity changed")
	}
	confirmed, err := source.PrepareSignalHandoff(ctx, proof)
	if err != nil {
		return proof, err
	}
	if err = confirmed.Validate(); err != nil {
		return proof, err
	}
	if protocol.Hash(confirmed) != protocol.Hash(proof) {
		return proof, fmt.Errorf("relay prepared another Signal handoff")
	}
	if err = gateway.RecordSignalHandoff(ctx, confirmed); err != nil {
		return
	}
	// Recheck source evidence after the gateway call, including lost-response replay.
	confirmed, err = source.PrepareSignalHandoff(ctx, proof)
	if err != nil {
		return proof, err
	}
	if protocol.Hash(confirmed) != protocol.Hash(proof) {
		return proof, fmt.Errorf("Signal source changed during handoff")
	}
	return
}
