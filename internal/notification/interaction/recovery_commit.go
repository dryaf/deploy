package interaction

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// CommandRecoveryCommit is a checkpoint of already reconciled history, never a
// new result acknowledgement or execution grant. Floors cover every retained
// command, including commands with no results and uncertain running jobs.
type CommandRecoveryCommit struct {
	Version          int
	Summary          CommandRecoverySummary
	ResultFloorsHash string
	ExecutionEnabled bool
}

type CommandResultFloor struct {
	CommandID, CommandHash, RecordHash string
	Revision                           uint64
}

func RecoveryResultFloor(v CommandRecoveryRecord) CommandResultFloor {
	return CommandResultFloor{CommandID: v.Command.ID, CommandHash: protocol.Hash(v.Command), RecordHash: protocol.Hash(v), Revision: v.Revision}
}

func (p CommandRecoveryCommit) Validate() error {
	s := p.Summary
	ch := s.Challenge
	if err := ValidateCommandRecoveryChallenge(ch, s.Manifest.Identity, s.Manifest.Producer, ch.IssuedAt, ch.BootID, ch.Uptime); err != nil {
		return err
	}
	if p.Version != 1 || p.ExecutionEnabled || s.ExecutionEnabled || s.Manifest.Commands > 65536 || s.Manifest.Results > 8*s.Manifest.Commands ||
		s.Terminal > s.Manifest.Commands || s.Uncertain != s.Manifest.Commands-s.Terminal || s.MissingRestored > 65536 ||
		!safefs.DigestPattern.MatchString(s.Manifest.Digest) || !safefs.DigestPattern.MatchString(p.ResultFloorsHash) {
		return fmt.Errorf("invalid command recovery commit")
	}
	return nil
}

type CommandRecoveryCommitSession interface {
	CommandRecoverySession
	PrepareCommandRecoveryCommit(context.Context, protocol.Producer) (CommandRecoveryCommit, error)
}
