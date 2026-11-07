package install

import (
	"context"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// ReviewDeliveryLEDOutput is read-only and pins the completed production
// installation before a coordinator records an output transition.
func ReviewDeliveryLEDOutput(ctx context.Context, root, expected string, disabled bool) (ManagedManifest, error) {
	var zero ManagedManifest
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if !managedPath(root) || !safefs.DigestPattern.MatchString(expected) {
		return zero, fmt.Errorf("LED output review requires exact delivery pins")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return zero, err
	}
	defer d.Close()
	op, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return zero, err
	}
	defer op.Close()
	lease, err := d.Lock("installation.lock", false, false)
	if err != nil {
		return zero, err
	}
	defer lease.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return zero, err
	}
	m := j.Intent.After
	if j.Phase != "complete" || m.Root != root || protocol.Hash(m) != expected {
		return zero, fmt.Errorf("LED output review requires unchanged completed delivery")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return zero, err
	}
	c, err := verifyManagedArtifacts(d, m)
	if err != nil {
		return zero, err
	}
	if err = reviewDeliveryLEDOutput(d, m, c, disabled, time.Now().UTC()); err != nil {
		return zero, err
	}
	return m, ctx.Err()
}

func reviewDeliveryLEDOutput(d *safefs.Directory, m ManagedManifest, c Runtime, disabled bool, now time.Time) error {
	if m.Mode != "delivery" || m.RecoveryCommitHash != "" || c.LEDSocket == "" {
		return fmt.Errorf("LED output requires an unrecovered delivery installation with its retained daemon")
	}
	proof, original, err := readDeliveryAdmission(d, m, c)
	if err != nil {
		return err
	}
	if m.Commands != nil {
		if _, err = readManagedCommandAdmission(d, m, c); err != nil {
			return err
		}
	}
	if !disabled {
		if err = retainedQualifiedLEDOutput(d, proof, original, c); err != nil {
			return err
		}
	}
	// The retained delivery service must be able to restart after either toggle.
	// Refuse expired evidence before stopping it or retaining a domain operation.
	return checkDeliveryQualification(d, original, c, proof.Qualification, now)
}

// A renewal while output is disabled retains the previously admitted output
// capability. Only exact pause/renewal history can carry it forward; a gateway
// first admitted with output disabled cannot acquire it through repeated renewals.
func retainedQualifiedLEDOutput(d *safefs.Directory, proof deliveryAdmission, verification ManagedManifest, c Runtime) error {
	for depth := 0; depth < 128; depth++ {
		if verification.LEDOutput == nil || !verification.LEDOutput.Disabled {
			if !proof.Qualification.Report.evidenceComplete(c) {
				return fmt.Errorf("retained LED qualification is incomplete")
			}
			return nil
		}
		prior, err := deliveryPausePredecessor(d, proof, verification)
		if err != nil {
			return err
		}
		if prior == nil {
			return fmt.Errorf("LED output was not enabled in the admitted qualification; qualify output before enabling it")
		}
		proof, verification, err = readDeliveryAdmission(d, *prior, c)
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("LED qualification history exceeds review bound")
}
