package interaction

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// CommandRecoveryInstallation records projection of historical evidence. It is
// neither a new result receipt nor permission to execute or send a reply.
type CommandRecoveryInstallation struct {
	Version          int
	Checkpoint       CommandRecoveryCommit
	OutcomesHash     string
	Commands         uint64
	ExecutionEnabled bool
}

func (p CommandRecoveryInstallation) Validate() error {
	if err := p.Checkpoint.Validate(); err != nil {
		return err
	}
	if p.Version != 1 || p.ExecutionEnabled || p.Commands != p.Checkpoint.Summary.Manifest.Commands || !safefs.DigestPattern.MatchString(p.OutcomesHash) {
		return fmt.Errorf("invalid command recovery installation")
	}
	return nil
}

type CommandRecoveryInstallSession interface {
	CommandRecoveryCommitSession
	InstallCommandRecoveryOutcomes(context.Context, CommandRecoveryCommit) (CommandRecoveryInstallation, error)
}

// InstallCommandRecovery verifies a completed source commit on both sides of the
// gateway call. The gateway journals its own installation and replays lost replies.
// The caller retains the offline gateway session; no transaction spans the call.
func InstallCommandRecovery(ctx context.Context, l *Ledger, s CommandRecoveryInstallSession) (result CommandRecoveryInstallation, err error) {
	if l == nil || s == nil {
		return result, fmt.Errorf("command installation dependencies missing")
	}
	read := func() (CommandRecoveryCommit, error) {
		tx, err := l.db.BeginTx(ctx, nil)
		if err != nil {
			return CommandRecoveryCommit{}, err
		}
		defer tx.Rollback()
		summary, floors, err := l.commandCommitEvidence(tx)
		if err != nil {
			return CommandRecoveryCommit{}, err
		}
		_, proof, err := readLedgerCommandCommit(tx)
		if err != nil {
			return CommandRecoveryCommit{}, err
		}
		if proof == nil {
			return CommandRecoveryCommit{}, protocol.ErrRecoveryIncomplete
		}
		if protocol.Hash(proof.Summary) != protocol.Hash(summary) || proof.ResultFloorsHash != floors {
			return CommandRecoveryCommit{}, fmt.Errorf("source command checkpoint changed")
		}
		return *proof, nil
	}
	proof, err := read()
	if err != nil {
		return result, err
	}
	result, err = s.InstallCommandRecoveryOutcomes(ctx, proof)
	if err != nil {
		return
	}
	if err = result.Validate(); err != nil {
		return
	}
	if protocol.Hash(result.Checkpoint) != protocol.Hash(proof) {
		return result, fmt.Errorf("gateway installed a different command checkpoint")
	}
	current, err := read()
	if err != nil {
		return result, err
	}
	if protocol.Hash(current) != protocol.Hash(proof) {
		return result, fmt.Errorf("source command checkpoint changed during installation")
	}
	return result, nil
}
