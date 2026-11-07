package worker

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Layout transitions may occur between policy receipts. Only an exact chain of
// completed domain mappings can bridge the configurations; matching fields alone
// never authorize the gap.
func verifyEnrolledPolicyMappingGap(ctx context.Context, q ledRowReader, id protocol.Identity, before, after Config) error {
	if protocol.Hash(before) == protocol.Hash(after) {
		return nil
	}
	if err := ValidateLEDBindingChange(before, after); err != nil {
		return err
	}
	var count int
	if err := q.QueryRowContext(ctx, "SELECT COALESCE(max(position),0) FROM led_migrations").Scan(&count); err != nil {
		return err
	}
	if count > maxLEDMigrations {
		return fmt.Errorf("LED mapping history exceeds bound")
	}
	wanted := protocol.Hash(before)
	for position := 1; position <= count; position++ {
		var op string
		if err := q.QueryRowContext(ctx, "SELECT id FROM led_migrations WHERE position=?", position).Scan(&op); err != nil {
			return err
		}
		m, err := readLEDMapping(ctx, q, op)
		if err != nil {
			return err
		}
		if m.Intent.Position != position || m.Intent.ID != op {
			return fmt.Errorf("LED mapping position changed")
		}
		if m.Intent.BeforeConfigHash != wanted {
			continue
		}
		if !m.Completed {
			return fmt.Errorf("finish pending LED mapping before policy update")
		}
		if err = validateLEDMapping(m, before, id); err != nil {
			return err
		}
		if err = checkLEDMappingOutputPin(ctx, q, m.Intent); err != nil {
			return err
		}
		if m.Intent.BaseConfigHash != ledBaseConfigHash(before) {
			return fmt.Errorf("LED mapping changed unrelated policy")
		}
		wanted = m.Intent.AfterConfigHash
		if wanted == protocol.Hash(after) {
			return nil
		}
	}
	return fmt.Errorf("enrolled policy lost the exact intervening LED mapping history")
}

func checkEnrolledPolicyMappingHistory(ctx context.Context, d *safefs.Directory, q ledRowReader, v *enrolledLimits) error {
	for count := 0; v != nil && count < 64; count++ {
		if v.Receipt.Previous == "" {
			return nil
		}
		var prior enrolledLimits
		if err := d.ReadJSON("enrolled-limits-history/"+v.Receipt.Previous+".json", &prior); err != nil {
			return err
		}
		if !prior.Complete || prior.validate() != nil || prior.digest() != v.Receipt.Previous || prior.Receipt.Identity != v.Receipt.Identity {
			return fmt.Errorf("enrolled mapping predecessor changed")
		}
		if err := verifyEnrolledPolicyMappingGap(ctx, q, v.Receipt.Identity, prior.Receipt.After, v.Receipt.Before); err != nil {
			return err
		}
		v = &prior
	}
	if v != nil {
		return fmt.Errorf("enrolled mapping history exceeds bound")
	}
	return nil
}
