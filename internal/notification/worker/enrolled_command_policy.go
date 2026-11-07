package worker

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Verification command policy has no admitted receiver, accepted input or
// delivery owner. Keep the producer contract while reviewing its initial policy.
func ValidateEnrolledCommandPolicyChange(before, after Config) error {
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if before.Interaction == nil || after.Interaction == nil {
		return fmt.Errorf("verification command changes require an existing command declaration")
	}
	if len(before.Interaction.Workers) != len(after.Interaction.Workers) {
		return fmt.Errorf("command policy cannot add or retire producers")
	}
	for i, w := range before.Interaction.Workers {
		n := after.Interaction.Workers[i]
		if w.Producer != n.Producer || w.Generation != n.Generation {
			return fmt.Errorf("command policy must retain producer identity and generation")
		}
	}
	next := before
	next.Interaction = after.Interaction
	if protocol.Hash(next) != protocol.Hash(after) {
		return fmt.Errorf("change command policy separately from other runtime settings")
	}
	return nil
}

func PreflightEnrolledCommandPolicy(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightEnrolledPolicy(ctx, state, id, before, after, 12)
}
func MigrateEnrolledCommandPolicy(ctx context.Context, state string, id protocol.Identity, before, after Config, op string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, op, 12, nil)
}

// Every alias must have already been reserved by a retained initial or policy
// transaction. Include an interrupted change only after its exact SQL commit.
func checkVerificationCommandState(ctx context.Context, s *Store, after Config) error {
	if err := requireEmptyCommandHistory(ctx, s); err != nil {
		return err
	}
	expected := map[string]protocol.Producer{}
	add := func(c Config) error {
		if c.Interaction == nil {
			return nil
		}
		for _, w := range c.Interaction.Workers {
			if old, ok := expected[w.Alias]; ok && old != w.Producer {
				return fmt.Errorf("command alias history changes producer identity")
			}
			expected[w.Alias] = w.Producer
		}
		return nil
	}
	e, err := retainedBootstrapEnrollment(s.dir.File().Name())
	if err != nil {
		return err
	}
	if e == nil || !e.Complete {
		return fmt.Errorf("completed producer enrollment required")
	}
	if err = add(e.Config); err != nil {
		return err
	}
	v, err := readEnrolledLimits(s.dir)
	if err != nil {
		return err
	}
	var anchor string
	if v != nil {
		if err = s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", enrolledLimitsKey).Scan(&anchor); err != nil && v.Complete {
			return err
		}
	}
	for depth := 0; v != nil; depth++ {
		if depth >= 64 {
			return fmt.Errorf("command policy history exceeds bound")
		}
		if v.Complete || anchor == v.digest() {
			if err = add(v.Receipt.After); err != nil {
				return err
			}
		}
		if v.Receipt.Previous == "" {
			break
		}
		var prior enrolledLimits
		if err = s.dir.ReadJSON("enrolled-limits-history/"+v.Receipt.Previous+".json", &prior); err != nil {
			return err
		}
		v = &prior // readEnrolledLimits validated this bounded chain.
	}
	rows, err := s.db.QueryContext(ctx, "SELECT alias,producer,epoch FROM command_aliases")
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var alias, id, epoch string
		if err = rows.Scan(&alias, &id, &epoch); err != nil {
			return err
		}
		p, ok := expected[alias]
		if !ok || p.ID != id || p.Epoch != epoch {
			return fmt.Errorf("unproved command alias authority")
		}
		seen++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if seen != len(expected) {
		return fmt.Errorf("retained command alias authority missing")
	}
	if after.Interaction == nil {
		return nil
	}
	for _, w := range after.Interaction.Workers {
		if p, ok := expected[w.Alias]; ok && p != w.Producer {
			return fmt.Errorf("command alias reassignment requires migration")
		}
	}
	return nil
}
