package worker

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Delivery limits keep the original immutable owner. Only the independently
// anchored policy history permits its configuration to evolve. The installer
// must pause and stop the managed service before calling this migration.
func MigrateDeliveryLimits(ctx context.Context, state string, id protocol.Identity, before, after Config, operation string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, operation, 21, nil)
}
func PreflightDeliveryLimits(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightEnrolledPolicy(ctx, state, id, before, after, 21)
}

// Never accept a changed config merely because its immutable owner exists.
// Every step back to the admission config must be a typed, same-owner limits
// receipt with matching file and SQL authority. Pending changes only authorize
// their before config until publication is completed.
func validateDeliveryOwnerConfig(d *safefs.Directory, db ledRowReader, c Config, owner ManagedDeliveryOwner) error {
	if owner.ConfigHash == protocol.Hash(c) {
		return nil
	}
	v, err := readEnrolledLimits(d)
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("delivery configuration lacks policy history")
	}
	var pending *enrolledLimits
	if !v.Complete {
		pending = v
	}
	if err = checkEnrolledLimitsSQL(context.Background(), d, db, v, pending); err != nil {
		return err
	}
	wanted := protocol.Hash(c)
	if pending != nil {
		if wanted != protocol.Hash(v.Receipt.Before) {
			return fmt.Errorf("pending limits only authorize their previous config")
		}
		v, err = priorDeliveryLimits(d, v)
		if err != nil {
			return err
		}
	}
	for count := 0; v != nil && count < 64; count++ {
		r := v.Receipt
		if !v.Complete || r.DeliveryOwner == nil || *r.DeliveryOwner != owner || wanted != protocol.Hash(r.After) {
			return fmt.Errorf("delivery configuration lost same-owner policy ancestry")
		}
		wanted = protocol.Hash(r.Before)
		if wanted == owner.ConfigHash {
			return nil
		}
		v, err = priorDeliveryLimits(d, v)
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("delivery configuration does not reach original admission")
}
func priorDeliveryLimits(d *safefs.Directory, v *enrolledLimits) (*enrolledLimits, error) {
	if v.Receipt.Previous == "" {
		return nil, nil
	}
	var prior enrolledLimits
	if err := d.ReadJSON("enrolled-limits-history/"+v.Receipt.Previous+".json", &prior); err != nil {
		return nil, err
	}
	if prior.validate() != nil || !prior.Complete || prior.digest() != v.Receipt.Previous {
		return nil, fmt.Errorf("delivery limits predecessor changed")
	}
	return &prior, nil
}

func MigrateDeliveryProducerPolicy(ctx context.Context, state string, id protocol.Identity, before, after Config, op string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, op, 24, nil)
}
func PreflightDeliveryProducerPolicy(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightEnrolledPolicy(ctx, state, id, before, after, 24)
}

func MigrateDeliveryRoutes(ctx context.Context, state string, id protocol.Identity, before, after Config, op string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, op, 23, nil)
}
func PreflightDeliveryRoutes(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightEnrolledPolicy(ctx, state, id, before, after, 23)
}

func MigrateDeliveryProviders(ctx context.Context, state string, id protocol.Identity, before, after Config, op string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, op, 25, nil)
}
func PreflightDeliveryProviders(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightEnrolledPolicy(ctx, state, id, before, after, 25)
}

// A declaration-only edit reads ownership and policy proofs without acquiring a
// runtime lease, writing a receipt, changing capacity or requiring an empty queue.
func PreflightDeliveryDeclaration(ctx context.Context, state string, id protocol.Identity, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Interaction != nil {
		return fmt.Errorf("declaration continuation excludes command policy")
	}
	return inspectEnrolledPolicyState(ctx, state, id, c, func(s *Store) error {
		var owner ManagedDeliveryOwner
		if err := s.dir.ReadJSON(managedDeliveryOwnerFile, &owner); err != nil {
			return err
		}
		return checkManagedDeliveryOwnerRecord(s.dir, s.db, c, owner, protocol.Hash(owner), id)
	})
}
