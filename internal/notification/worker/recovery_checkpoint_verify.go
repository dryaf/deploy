package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// RecoveryCheckpointProof binds retained artifacts independently of the current
// canonical epoch. It is historical evidence, not completed publication, current
// provider freshness, permission to execute, or permission to clear a fault.
type RecoveryCheckpointProof struct {
	Version                int               `json:"version"`
	Operation              string            `json:"operation"`
	Snapshot               string            `json:"snapshot"`
	Before                 protocol.Identity `json:"before"`
	After                  protocol.Identity `json:"after"`
	ConfigHash             string            `json:"config_hash"`
	ReviewHash             string            `json:"review_hash"`
	CheckpointHash         string            `json:"checkpoint_hash"`
	EvidenceArchiveHash    string            `json:"evidence_archive_hash"`
	OriginalSourceHash     string            `json:"original_source_hash"`
	SourceDatabaseHash     string            `json:"source_database_hash"`
	CandidateDatabaseHash  string            `json:"candidate_database_hash"`
	OriginalDatabaseHash   string            `json:"original_database_hash"`
	ProjectionDatabaseHash string            `json:"projection_database_hash"`
	PendingStages          int               `json:"pending_stages"`
	ProofHash              string            `json:"proof_hash"`
	PublicationAllowed     bool              `json:"publication_allowed"`
	DeliveryEnabled        bool              `json:"delivery_enabled"`
	ExecutionEnabled       bool              `json:"execution_enabled"`
}

// VerifyRecoveryCheckpoint verifies an exact historical checkpoint while the
// worker is offline. Canonical state.db, identity/ownership heads, mutable stage
// JSON and reconciled.db are intentionally not inputs. All reads are immutable
// or descriptor-relative; missing evidence is never initialized or repaired.
func VerifyRecoveryCheckpoint(ctx context.Context, path string, c Config, operation, reviewHash, checkpointHash string) (out RecoveryCheckpointProof, err error) {
	if err = c.Validate(); err != nil {
		return
	}
	if !safefs.ValidID(operation, "n") || !safefs.DigestPattern.MatchString(reviewHash) || !safefs.DigestPattern.MatchString(checkpointHash) {
		return out, fmt.Errorf("exact recovery operation, review and checkpoint digests required")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err = ctx.Err(); err != nil {
		return
	}
	dir, err := privateDirectory(path)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	lease, err := dir.Lock("worker.lock", true, false)
	if err != nil {
		return out, err
	}
	defer lease.Close()
	return verifyRetainedCheckpoint(ctx, dir, lease, c, operation, reviewHash, checkpointHash, nil)
}

// The caller retains the existing worker lease throughout both observations.
// Producer leases are unnecessary: only their frozen evidence copies are read.
func verifyRetainedCheckpoint(ctx context.Context, root *safefs.Directory, lease *safefs.Lock, c Config, operation, reviewHash, checkpointHash string, fault func(string) error) (out RecoveryCheckpointProof, err error) {
	if !safefs.ValidID(operation, "n") || !safefs.DigestPattern.MatchString(reviewHash) || !safefs.DigestPattern.MatchString(checkpointHash) {
		return out, fmt.Errorf("invalid retained checkpoint reference")
	}
	err = withRetainedCheckpoint(ctx, root, lease, operation, reviewHash, func(stage, d *safefs.Directory, check func() error) error {
		var e error
		if out, e = readRetainedCheckpoint(ctx, stage, d, c, operation, reviewHash, checkpointHash); e != nil {
			return e
		}
		if e = recoveryBoundary(fault, "recovery_checkpoint_verified"); e != nil {
			return e
		}
		if e = check(); e != nil {
			return e
		}
		again, e := readRetainedCheckpoint(ctx, stage, d, c, operation, reviewHash, checkpointHash)
		if e != nil {
			return e
		}
		if again != out {
			return fmt.Errorf("retained recovery proof changed during verification")
		}
		return nil
	})
	return
}

// Keep the worker lease and directory identities pinned across all retained
// artifact observations. This does not read live canonical recovery authority.
func withRetainedCheckpoint(ctx context.Context, root *safefs.Directory, lease *safefs.Lock, operation, reviewHash string, action func(stage, checkpoint *safefs.Directory, check func() error) error) error {
	stage, err := retainedDirectory(root, "recovery", operation)
	if err != nil {
		return err
	}
	defer stage.Close()
	d, err := retainedDirectory(stage, "checkpoints", reviewHash)
	if err != nil {
		return err
	}
	defer d.Close()
	check := func() error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if lease == nil || lease.File() == nil {
			return fmt.Errorf("checkpoint verification lease unavailable")
		}
		f, e := root.Open("worker.lock", os.O_RDONLY|unix.O_NONBLOCK, 0)
		if e != nil {
			return e
		}
		defer f.Close()
		a, e := lease.File().Stat()
		if e != nil {
			return e
		}
		b, e := f.Stat()
		if e != nil {
			return e
		}
		if !os.SameFile(a, b) {
			return fmt.Errorf("checkpoint verification lease changed")
		}
		current, e := privateDirectory(root.File().Name())
		if e != nil {
			return e
		}
		defer current.Close()
		if e = sameRetainedDirectory(root, current); e != nil {
			return e
		}
		s, e := retainedDirectory(root, "recovery", operation)
		if e != nil {
			return e
		}
		defer s.Close()
		if e = sameRetainedDirectory(stage, s); e != nil {
			return e
		}
		cp, e := retainedDirectory(s, "checkpoints", reviewHash)
		if e != nil {
			return e
		}
		defer cp.Close()
		return sameRetainedDirectory(d, cp)
	}
	if err = check(); err != nil {
		return err
	}
	if err = action(stage, d, check); err != nil {
		return err
	}
	return check()
}

func retainedDirectory(root *safefs.Directory, parent, child string) (*safefs.Directory, error) {
	p, err := root.Sub(parent)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	if err = privateCheckpointDirectory(p); err != nil {
		return nil, err
	}
	d, err := p.Sub(child)
	if err != nil {
		return nil, err
	}
	if err = privateCheckpointDirectory(d); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func sameRetainedDirectory(a, b *safefs.Directory) error {
	ai, err := a.File().Stat()
	if err != nil {
		return err
	}
	bi, err := b.File().Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(ai, bi) {
		return fmt.Errorf("retained recovery directory replaced")
	}
	return nil
}
