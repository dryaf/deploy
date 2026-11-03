package worker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type RecoveryOwnership = protocol.RecoveryOwnership
type RecoveryOwnershipSession = protocol.RecoveryOwnershipSession

func OpenRecoveryOwnershipSession(ctx context.Context, path string, c Config) (RecoveryOwnershipSession, error) {
	return openRecoveryReconciler(ctx, path, c, nil)
}

func (r *recoveryReconciler) PrepareRecoveryOwnership(ctx context.Context, p protocol.Producer) (result RecoveryOwnership, err error) {
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		var raw, challenge, manifest string
		if err := tx.QueryRow("SELECT completion,challenge,manifest FROM reconciliation_producers WHERE producer=?", p.ID).Scan(&raw, &challenge, &manifest); err != nil {
			if err == sql.ErrNoRows {
				return protocol.ErrRecoveryIncomplete
			}
			return err
		}
		if raw == "" {
			return protocol.ErrRecoveryIncomplete
		}
		var summary protocol.RecoverySummary
		var ch protocol.RecoveryChallenge
		var m protocol.RecoveryManifest
		if err := protocol.Decode([]byte(raw), &summary, protocol.MaxRequest+8192); err != nil {
			return err
		}
		if err := protocol.Decode([]byte(challenge), &ch, 4096); err != nil {
			return err
		}
		if err := protocol.Decode([]byte(manifest), &m, 4096); err != nil {
			return err
		}
		if ch.Operation != r.journal.Operation || ch.Identity != r.journal.After || ch.Producer != p ||
			protocol.Hash(ch) != protocol.Hash(summary.Challenge) || m != summary.Manifest || summary.DeliveryEnabled ||
			summary.Snapshot.Producer != p || summary.Snapshot.Validate() != nil || summary.Ambiguous < 0 {
			return fmt.Errorf("ownership recovery summary changed")
		}
		want := ownershipRecovery{Binding: r.journal.binding(), SummaryHash: protocol.Hash(summary)}
		lease, check, err := r.lockRecoveryProducer(ctx, p)
		if err != nil {
			return err
		}
		defer lease.Close()
		if err = check(); err != nil {
			return err
		}
		var intent ownershipIntent
		if err = r.dir.ReadJSON(ownershipName(p)+".journal.json", &intent); err != nil {
			return err
		}
		// These scoped stores only operate on ownership files. They have no DB,
		// provider, dispatcher or independent installation lease.
		s := &Store{dir: r.dir, identity: r.journal.After, config: r.config}
		s.ownershipFault = func(stage string) error {
			if err := check(); err != nil {
				return err
			}
			return recoveryBoundary(r.fault, "recovery_ownership_"+stage)
		}
		if intent.Transition.Operation == "stage" && intent.Before.Identity == r.journal.After {
			result, err = r.readRecoveryOwnership(p, want.SummaryHash)
			return err
		} else if intent.Transition.Operation == "recovery-fence" {
			if intent.Transition.Recovery == nil || intent.Transition.Recovery.Binding != want.Binding || intent.Transition.Recovery.SummaryHash != "" {
				return fmt.Errorf("pending ownership recovery binding changed")
			}
			if err = s.prepareProducerOwnership(p, false, false); err != nil {
				return err
			}
			current, e := s.readOwnership(p)
			if e != nil {
				return e
			}
			event := ownershipTransition{Schema: 3, Identity: current.Identity, Producer: p,
				Revision: current.Revision + 1, Previous: current.History, BeforeHash: protocol.Hash(current),
				Operation: "recover", Recovery: &want}
			if err = check(); err != nil {
				return err
			}
			if err = s.commitOwnershipEvent(&current, event); err != nil {
				return err
			}
		} else if intent.Transition.Operation == "recover" && intent.Transition.Recovery != nil &&
			intent.Transition.Recovery.Binding.After == r.journal.After {
			if *intent.Transition.Recovery != want {
				return fmt.Errorf("ownership recovery binding changed")
			}
			// Complete precisely the durable intent, including a lost history or
			// owner publication. Never rotate the epoch again on retry.
			if err = s.prepareProducerOwnership(p, false, false); err != nil {
				return err
			}
		} else {
			old := &Store{dir: r.dir, identity: r.journal.Before, config: r.config}
			old.ownershipFault = func(string) error { return check() }
			if err = old.prepareProducerOwnership(p, false, false); err != nil {
				return err
			}
			current, err := old.readOwnership(p)
			if err != nil {
				return err
			}
			event := ownershipTransition{Schema: 2, Identity: current.Identity, Producer: p,
				Revision: current.Revision + 1, Previous: current.History, BeforeHash: protocol.Hash(current),
				Operation: "recover", Recovery: &want}
			if err = check(); err != nil {
				return err
			}
			if err = s.commitOwnershipEvent(&current, event); err != nil {
				return err
			}
		}
		if err = check(); err != nil {
			return err
		}
		result, err = r.readRecoveryOwnership(p, want.SummaryHash)
		return err
	})
	return
}

// The caller holds the worker and producer leases. This observation does not
// complete pending ownership transitions or create new authority.
func (r *recoveryReconciler) readRecoveryOwnership(p protocol.Producer, summaryHash string) (result RecoveryOwnership, err error) {
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
	if current.Active != "" {
		return result, fmt.Errorf("recovered ownership unexpectedly active")
	}
	if current.RecoveryPending != nil {
		return result, protocol.ErrRecoveryIncomplete
	}
	for _, state := range current.Generations {
		if state != "revoked" {
			generation, err := r.readRecoveryGeneration(p, summaryHash)
			return generation.Ownership, err
		}
	}
	var intent ownershipIntent
	if err = r.dir.ReadJSON(ownershipName(p)+".journal.json", &intent); err != nil {
		return
	}
	want := ownershipRecovery{Binding: r.journal.binding(), SummaryHash: summaryHash}
	if intent.Transition.Recovery == nil || *intent.Transition.Recovery != want {
		return result, fmt.Errorf("ownership recovery binding changed")
	}
	result = RecoveryOwnership{Version: 1, Operation: r.journal.Operation, Before: r.journal.Before, After: r.journal.After,
		Producer: p, SummaryHash: summaryHash, PreviousRevision: intent.Before.Revision,
		Revision: current.Revision, PreviousHistory: intent.Before.History, History: current.History}
	err = result.Validate()
	return
}

var _ RecoveryOwnershipSession = (*recoveryReconciler)(nil)
