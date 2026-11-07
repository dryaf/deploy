package worker

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Enroll intake policy only before delivery and producer activation. The
// specialist receiver and actual command admission remain separate operations.
func ValidateEnrolledCommandsChange(before, after Config) error {
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if before.Interaction != nil || after.Interaction == nil {
		return fmt.Errorf("initial command enrollment requires a command-free gateway")
	}
	next := before
	next.Interaction = after.Interaction
	if protocol.Hash(next) != protocol.Hash(after) {
		return fmt.Errorf("enroll commands separately from other runtime policy")
	}
	return nil
}
func PreflightEnrolledCommands(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightEnrolledPolicy(ctx, state, id, before, after, 11)
}
func MigrateEnrolledCommands(ctx context.Context, state string, id protocol.Identity, before, after Config, op string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, op, 11, nil)
}
func checkInitialEnrolledCommandState(ctx context.Context, s *Store, after Config) error {
	if err := requireEmptyCommandHistory(ctx, s); err != nil {
		return err
	}
	// On a lost SQL reply the exact desired aliases may already be committed.
	retained, err := readEnrolledLimits(s.dir)
	if err != nil {
		return err
	}
	committed := false
	if retained != nil && (retained.Receipt.Version == 11 || retained.Receipt.Version == 16) && protocol.Hash(retained.Receipt.After) == protocol.Hash(after) {
		var anchor string
		if e := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", enrolledLimitsKey).Scan(&anchor); e == nil {
			committed = anchor == retained.digest()
		}
	}
	rows, err := s.db.QueryContext(ctx, "SELECT alias,producer,epoch FROM command_aliases")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if !committed {
			return fmt.Errorf("initial command enrollment found aliases without its SQL commit")
		}
		var alias, id, epoch string
		if err = rows.Scan(&alias, &id, &epoch); err != nil {
			return err
		}
		found := false
		for _, w := range after.Interaction.Workers {
			if w.Alias == alias && w.Producer.ID == id && w.Producer.Epoch == epoch {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("initial command enrollment found foreign alias history")
		}
	}
	return rows.Err()
}
func checkEnrolledCommandAliases(ctx context.Context, d *safefs.Directory, q ledRowReader, v *enrolledLimits, anchor string) error {
	for depth := 0; v != nil && depth < 64; depth++ {
		if (v.Receipt.Version == 11 || v.Receipt.Version == 16 || v.Receipt.Version == 12 || v.Receipt.Version == 17 || v.Receipt.Version == 13 || v.Receipt.Version == 18) && (v.Complete || anchor == v.digest()) && v.Receipt.After.Interaction != nil {
			for _, w := range v.Receipt.After.Interaction.Workers {
				var id, epoch string
				if err := q.QueryRowContext(ctx, "SELECT producer,epoch FROM command_aliases WHERE alias=?", w.Alias).Scan(&id, &epoch); err != nil || id != w.Producer.ID || epoch != w.Producer.Epoch {
					return fmt.Errorf("enrolled command alias authority missing or changed")
				}
			}
		}
		if v.Receipt.Previous == "" {
			return nil
		}
		var prior enrolledLimits
		if err := d.ReadJSON("enrolled-limits-history/"+v.Receipt.Previous+".json", &prior); err != nil {
			return err
		}
		if prior.validate() != nil || !prior.Complete || prior.digest() != v.Receipt.Previous {
			return fmt.Errorf("enrolled command predecessor changed")
		}
		v = &prior
	}
	return fmt.Errorf("enrolled command history exceeds bound")
}

func requireEmptyCommandHistory(ctx context.Context, s *Store) error {
	for _, table := range []string{"commands", "command_inbox", "command_results", "command_replies", "command_reply_attempts", "telegram_cursors", "provider_inbox", "command_rate", "signal_imports", "command_admissions", "interaction_contexts", "notification_references", "notification_deliveries", "gateway_queries", "gateway_pages"} {
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("initial command enrollment found retained interaction history")
		}
	}
	var heads int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE key='managed-command-head'").Scan(&heads); err != nil {
		return err
	}
	if heads != 0 {
		return fmt.Errorf("initial command enrollment found existing command admission")
	}
	if err := absentOriginalFile(s.dir, "managed-commands"); err != nil {
		return err
	}
	return nil
}
