package worker

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func ValidateEnrolledCommandActivityChange(before, after Config) error {
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if (before.Interaction == nil) == (after.Interaction == nil) {
		return fmt.Errorf("verification command activity requires a changed enabled state")
	}
	next := before
	next.Interaction = after.Interaction
	if protocol.Hash(next) != protocol.Hash(after) {
		return fmt.Errorf("change command activity separately from other policy")
	}
	return nil
}
func PreflightEnrolledCommandActivity(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightEnrolledPolicy(ctx, state, id, before, after, 13)
}
func MigrateEnrolledCommandActivity(ctx context.Context, state string, id protocol.Identity, before, after Config, op string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, op, 13, nil)
}

// Only a completed earlier policy may be restored. A pending enable intent
// cannot supply its own authority, including after a lost SQL commit reply.
func retainedVerificationCommands(d *safefs.Directory) (*interaction.Config, error) {
	v, err := readEnrolledLimits(d)
	if err != nil {
		return nil, err
	}
	for depth := 0; v != nil; depth++ {
		if depth >= 64 {
			return nil, fmt.Errorf("verification command history exceeds bound")
		}
		if v.Complete && v.Receipt.After.Interaction != nil {
			return v.Receipt.After.Interaction, nil
		}
		if v.Receipt.Previous == "" {
			break
		}
		var prior enrolledLimits
		if err = d.ReadJSON("enrolled-limits-history/"+v.Receipt.Previous+".json", &prior); err != nil {
			return nil, err
		}
		v = &prior
	}
	e, err := retainedBootstrapEnrollment(d.File().Name())
	if err != nil {
		return nil, err
	}
	if e != nil && e.Complete {
		return e.Config.Interaction, nil
	}
	return nil, nil
}

func VerificationCommandsRetained(state string) (bool, error) {
	if _, err := EnrolledLimitsRequired(state); err != nil {
		return false, err
	}
	d, err := safefs.Open(state)
	if err != nil {
		return false, err
	}
	defer d.Close()
	c, err := retainedVerificationCommands(d)
	return c != nil, err
}

func checkVerificationCommandActivity(ctx context.Context, s *Store, after Config) error {
	if err := checkVerificationCommandState(ctx, s, after); err != nil {
		return err
	}
	c, err := retainedVerificationCommands(s.dir)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("no retained verification command policy to toggle")
	}
	if after.Interaction != nil && protocol.Hash(c) != protocol.Hash(after.Interaction) {
		return fmt.Errorf("re-enable the exact retained command policy before changing it")
	}
	return nil
}
