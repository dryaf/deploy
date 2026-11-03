package worker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// RecoveryOwnershipFence records revocation without claiming that the producer
// has supplied fresh recovery evidence. It cannot be used as RecoveryOwnership.
type RecoveryOwnershipFence struct {
	Version                    int
	Operation                  string
	Before, After              protocol.Identity
	Producer                   protocol.Producer
	PreviousRevision, Revision uint64
	PreviousHistory, History   string
	ReconciliationPending      bool
	DeliveryEnabled            bool
}

// FenceRecoveryOwnership moves one enrolled producer into the new epoch while
// keeping it unable to stage or activate a generation. This offline operation
// requires an already quarantined recovery and never contacts the producer.
func FenceRecoveryOwnership(ctx context.Context, path string, c Config, p protocol.Producer) (RecoveryOwnershipFence, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return RecoveryOwnershipFence{}, err
	}
	defer r.Close()
	return r.fenceRecoveryOwnership(ctx, p)
}

func (r *recoveryReconciler) fenceRecoveryOwnership(ctx context.Context, p protocol.Producer) (result RecoveryOwnershipFence, err error) {
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	err = r.transaction(ctx, func(_ *sql.Tx) error {
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
		want := ownershipRecovery{Binding: r.journal.binding()}
		s := &Store{dir: r.dir, identity: r.journal.After, config: r.config}
		s.ownershipFault = func(stage string) error {
			if err := check(); err != nil {
				return err
			}
			return recoveryBoundary(r.fault, "recovery_fence_"+stage)
		}
		if intent.Transition.Operation == "recovery-fence" && intent.Transition.Recovery != nil &&
			intent.Transition.Recovery.Binding.After == r.journal.After {
			if *intent.Transition.Recovery != want {
				return fmt.Errorf("ownership recovery fence binding changed")
			}
			if err = s.prepareProducerOwnership(p, false, false); err != nil {
				return err
			}
		} else {
			// Replaying an older epoch is not permission to fence an already
			// reconciled producer or replace any later generation intent.
			old := &Store{dir: r.dir, identity: r.journal.Before, config: r.config}
			old.ownershipFault = func(string) error { return check() }
			if err = old.prepareProducerOwnership(p, false, false); err != nil {
				return err
			}
			current, err := old.readOwnership(p)
			if err != nil {
				return err
			}
			event := ownershipTransition{Schema: 3, Identity: current.Identity, Producer: p,
				Revision: current.Revision + 1, Previous: current.History, BeforeHash: protocol.Hash(current),
				Operation: "recovery-fence", Recovery: &want}
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
		current, err := s.readOwnership(p)
		if err != nil {
			return err
		}
		if err = s.verifyOwnershipPrefix(current); err != nil {
			return err
		}
		if current.RecoveryPending == nil || *current.RecoveryPending != want.Binding {
			return fmt.Errorf("ownership recovery fence missing")
		}
		if err = r.dir.ReadJSON(ownershipName(p)+".journal.json", &intent); err != nil {
			return err
		}
		result = RecoveryOwnershipFence{Version: 1, Operation: r.journal.Operation,
			Before: r.journal.Before, After: r.journal.After, Producer: p,
			PreviousRevision: intent.Before.Revision, Revision: current.Revision,
			PreviousHistory: intent.Before.History, History: current.History, ReconciliationPending: true}
		return nil
	})
	return
}
