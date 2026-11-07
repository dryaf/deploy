package worker

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Each policy transition retains the exact completed output disposition. Later
// output changes may advance the head, but cannot erase its historical record.
func checkEnrolledLimitsLEDHistory(ctx context.Context, d *safefs.Directory, q ledRowReader, v *enrolledLimits) error {
	for count := 0; v != nil && count < 64; count++ {
		if v.Receipt.LEDOutputHash != "" {
			r, err := readLEDOutputRecord(ctx, q, v.Receipt.LEDOutputOperation)
			if err != nil || !r.Complete || r.Intent.Request.Operation != v.Receipt.LEDOutputOperation || r.Intent.Request.Identity != v.Receipt.Identity || protocol.Hash(r) != v.Receipt.LEDOutputHash {
				return fmt.Errorf("enrolled policy lost pinned LED output history")
			}
		}
		if v.Receipt.Previous == "" {
			return nil
		}
		var prior enrolledLimits
		if err := d.ReadJSON("enrolled-limits-history/"+v.Receipt.Previous+".json", &prior); err != nil {
			return err
		}
		if !prior.Complete || prior.digest() != v.Receipt.Previous || prior.validate() != nil || prior.Receipt.Identity != v.Receipt.Identity {
			return fmt.Errorf("enrolled policy LED predecessor changed")
		}
		v = &prior
	}
	if v != nil {
		return fmt.Errorf("enrolled policy LED history exceeds bound")
	}
	return nil
}
