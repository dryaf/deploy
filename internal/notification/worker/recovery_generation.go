package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type RecoveryGeneration = protocol.RecoveryGeneration

type recoveryGenerationIntent struct {
	Version    int
	Ownership  RecoveryOwnership
	Generation string
}

type RecoveryGenerationSession interface {
	RecoveryApplicationSession
	PrepareRecoveryGeneration(context.Context, protocol.Producer) (RecoveryGeneration, error)
	BindRecoveryRetry(context.Context, protocol.Producer, string) (RecoveryRetryBinding, error)
}

func OpenRecoveryGenerationSession(ctx context.Context, path string, c Config) (RecoveryGenerationSession, error) {
	s, err := OpenRecoveryApplicationSession(ctx, path, c)
	if err != nil {
		return nil, err
	}
	r := s.(*recoveryReconciler)
	if err = r.initializeRetryBindings(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func generationFile(p protocol.Producer) string { return "generation-" + protocol.Hash(p) + ".json" }

func (r *recoveryReconciler) lockRecoveryProducer(ctx context.Context, p protocol.Producer) (*safefs.Lock, func() error, error) {
	lease, err := r.dir.Lock(ownershipName(p)+".lock", true, false)
	if err != nil {
		return nil, nil, err
	}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.checkAuthority(); err != nil {
			return err
		}
		held, err := lease.File().Stat()
		if err != nil {
			return err
		}
		f, err := r.dir.Open(ownershipName(p)+".lock", os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		current, err := f.Stat()
		f.Close()
		if err != nil {
			return err
		}
		if !os.SameFile(held, current) {
			return fmt.Errorf("recovery producer lease changed")
		}
		return nil
	}
	return lease, check, nil
}

func recoverySummaryHash(tx *sql.Tx, p protocol.Producer) (string, error) {
	var raw string
	if err := tx.QueryRow("SELECT completion FROM reconciliation_producers WHERE producer=?", p.ID).Scan(&raw); err != nil {
		return "", err
	}
	if raw == "" {
		return "", protocol.ErrRecoveryIncomplete
	}
	var summary protocol.RecoverySummary
	if err := protocol.Decode([]byte(raw), &summary, protocol.MaxRequest+8192); err != nil {
		return "", err
	}
	if summary.Challenge.Producer != p || summary.DeliveryEnabled {
		return "", fmt.Errorf("recovery summary binding changed")
	}
	return protocol.Hash(summary), nil
}

func (r *recoveryReconciler) validateGenerationIntent(v recoveryGenerationIntent, p protocol.Producer, hash string) error {
	if err := v.Ownership.Validate(); err != nil {
		return err
	}
	if v.Version != 1 || !safefs.ValidID(v.Generation, "n") || v.Ownership.Producer != p || v.Ownership.Operation != r.journal.Operation ||
		v.Ownership.Before != r.journal.Before || v.Ownership.After != r.journal.After || v.Ownership.SummaryHash != hash {
		return fmt.Errorf("recovery generation intent changed")
	}
	return nil
}

// Called under both installation and producer leases. Staging has its own file
// intent so a lost response cannot allocate a second generation.
func (r *recoveryReconciler) PrepareRecoveryGeneration(ctx context.Context, p protocol.Producer) (result RecoveryGeneration, err error) {
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		hash, err := recoverySummaryHash(tx, p)
		if err != nil {
			return err
		}
		lease, check, err := r.lockRecoveryProducer(ctx, p)
		if err != nil {
			return err
		}
		defer lease.Close()
		if err = check(); err != nil {
			return err
		}
		var plan recoveryGenerationIntent
		err = r.stage.ReadJSON(generationFile(p), &plan)
		if os.IsNotExist(err) {
			ownership, e := r.readRecoveryOwnership(p, hash)
			if e != nil {
				return e
			}
			g, e := safefs.NewID("n")
			if e != nil {
				return e
			}
			plan = recoveryGenerationIntent{Version: 1, Ownership: ownership, Generation: g}
			if err = r.stage.Capacity(r.config.MinFreeBytes + 3*safefs.MaxRecordBytes); err != nil {
				return err
			}
			if err = r.stage.WriteJSON(generationFile(p), plan, false); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err = r.validateGenerationIntent(plan, p, hash); err != nil {
			return err
		}
		if err = recoveryBoundary(r.fault, "generation_intent"); err != nil {
			return err
		}
		if err = check(); err != nil {
			return err
		}
		s := &Store{dir: r.dir, identity: r.journal.After, config: r.config}
		s.ownershipFault = func(at string) error {
			if err := check(); err != nil {
				return err
			}
			return recoveryBoundary(r.fault, "generation_ownership_"+at)
		}
		var intent ownershipIntent
		if err = r.dir.ReadJSON(ownershipName(p)+".journal.json", &intent); err != nil {
			return err
		}
		if intent.Transition.Operation == "stage" && intent.Transition.Generation == plan.Generation &&
			intent.Before.Revision == plan.Ownership.Revision && intent.Before.History == plan.Ownership.History {
			if err = s.prepareProducerOwnership(p, false, false); err != nil {
				return err
			}
		} else {
			prior, e := r.readRecoveryOwnership(p, hash)
			if e != nil {
				return e
			}
			if prior != plan.Ownership {
				return fmt.Errorf("generation ownership changed")
			}
			current, e := s.readOwnership(p)
			if e != nil {
				return e
			}
			if err = s.commitOwnership(&current, "stage", plan.Generation); err != nil {
				return err
			}
		}
		if err = check(); err != nil {
			return err
		}
		result, err = r.readRecoveryGeneration(p, hash)
		return err
	})
	return
}

func (r *recoveryReconciler) readRecoveryGeneration(p protocol.Producer, hash string) (result RecoveryGeneration, err error) {
	var plan recoveryGenerationIntent
	if err = r.stage.ReadJSON(generationFile(p), &plan); err != nil {
		return
	}
	if err = r.validateGenerationIntent(plan, p, hash); err != nil {
		return
	}
	s := &Store{dir: r.dir, identity: r.journal.After, config: r.config}
	current, err := s.readOwnership(p)
	if err != nil {
		return
	}
	if err = s.checkOwnershipHistoryTail(p, current.Revision); err != nil {
		return
	}
	if err = s.verifyOwnershipPrefix(current); err != nil {
		return
	}
	var intent ownershipIntent
	if err = r.dir.ReadJSON(ownershipName(p)+".journal.json", &intent); err != nil {
		return
	}
	if current.Active != "" || current.Generations[plan.Generation] != "staged" || current.Revision != plan.Ownership.Revision+1 ||
		intent.Before.Revision != plan.Ownership.Revision || intent.Before.History != plan.Ownership.History ||
		intent.Transition.Operation != "stage" || intent.Transition.Generation != plan.Generation {
		return result, fmt.Errorf("recovery generation is not the exact staged successor")
	}
	for g, state := range current.Generations {
		if g != plan.Generation && state != "revoked" {
			return result, fmt.Errorf("old generation regained authority")
		}
	}
	// The preceding immutable event must really be this recovery, not merely a
	// caller-supplied history digest that happens to precede the stage operation.
	var migration ownershipTransition
	if err = r.dir.ReadJSON(ownershipHistoryPath(p, plan.Ownership.Revision), &migration); err != nil {
		return
	}
	want := ownershipRecovery{Binding: r.journal.binding(), SummaryHash: hash}
	if migration.Recovery == nil || *migration.Recovery != want || protocol.Hash(migration) != plan.Ownership.History ||
		migration.Previous != plan.Ownership.PreviousHistory || migration.Revision != plan.Ownership.PreviousRevision+1 {
		return result, fmt.Errorf("generation lost its recovery provenance")
	}
	result = RecoveryGeneration{Version: 1, Ownership: plan.Ownership, Generation: plan.Generation, Revision: current.Revision, History: current.History}
	err = result.Validate()
	return
}
