package interaction

import (
	"context"
	"fmt"
	"math"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// SignalRecoveryCursor binds an offline relay candidate to the agreed handoff.
// Installation into candidates is not permission to start either receiver.
type SignalRecoveryCursor struct {
	Version       int
	Handoff       SignalRecoveryHandoff
	DatabaseHash  string
	DatabaseBytes int64
	IntakeEnabled bool
}

func (p SignalRecoveryCursor) Validate() error {
	if err := p.Handoff.Validate(); err != nil {
		return err
	}
	if p.Version != 1 || p.IntakeEnabled || !safefs.DigestPattern.MatchString(p.DatabaseHash) || p.DatabaseBytes <= 0 || p.DatabaseBytes > 33<<20 || p.Handoff.Target.Position >= math.MaxInt64 {
		return fmt.Errorf("invalid Signal cursor candidate")
	}
	return nil
}

type SignalCursorSource interface {
	SignalHandoffSource
	StageSignalCursor(context.Context, SignalRecoveryHandoff) (SignalRecoveryCursor, error)
}
type SignalCursorSession interface {
	SignalHandoffSession
	SignalCursorHandoff(context.Context, string) (SignalRecoveryHandoff, error)
	InstallSignalCursor(context.Context, SignalRecoveryCursor) error
}

func InstallSignalRecoveryCursor(ctx context.Context, source SignalCursorSource, gateway SignalCursorSession) (proof SignalRecoveryCursor, err error) {
	if source == nil || gateway == nil {
		return proof, fmt.Errorf("Signal cursor dependencies missing")
	}
	handoff, err := gateway.SignalCursorHandoff(ctx, source.Cursor().Account)
	if err != nil {
		return proof, err
	}
	if err = handoff.Validate(); err != nil {
		return
	}
	if handoff.Summary.Manifest.Relay != source.Cursor() {
		return proof, fmt.Errorf("Signal cursor source identity changed")
	}
	proof, err = source.StageSignalCursor(ctx, handoff)
	if err != nil {
		return
	}
	if err = proof.Validate(); err != nil {
		return
	}
	if protocol.Hash(proof.Handoff) != protocol.Hash(handoff) {
		return proof, fmt.Errorf("Signal cursor candidate handoff changed")
	}
	if err = gateway.InstallSignalCursor(ctx, proof); err != nil {
		return
	}
	again, err := source.StageSignalCursor(ctx, handoff)
	if err != nil {
		return proof, err
	}
	if protocol.Hash(again) != protocol.Hash(proof) {
		return proof, fmt.Errorf("Signal cursor candidate changed during installation")
	}
	return
}
