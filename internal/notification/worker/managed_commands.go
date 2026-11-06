package worker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

// ConfigureManagedCommands is called once before a managed delivery store is
// exposed. The installer must independently validate the retained command
// admission and qualification while holding its installation lease. Delivery
// alone never admits intake, execution or command replies.
func (s *Store) ConfigureManagedCommands(mode, hash string) error {
	if s.verificationOnly || s.installationRevision == "" || s.deliveryActivationHash == "" || s.config.Interaction == nil || !safefs.DigestPattern.MatchString(hash) || (mode != "read" && mode != "all" && mode != "drain") {
		return fmt.Errorf("managed commands require exact admitted delivery and command policy")
	}
	if err := s.checkRetirementLease(); err != nil {
		return err
	}
	if s.recoveryCommitHash != "" {
		for _, destination := range s.config.Interaction.Destinations {
			if destination.Provider == "signal" {
				if err := s.transaction(context.Background(), func(tx *sql.Tx) error {
					proof, err := readSignalRecoveryCursor(tx, destination.Account)
					if err != nil {
						return err
					}
					if proof == nil {
						return fmt.Errorf("managed recovered Signal commands require retained handoff")
					}
					return nil
				}); err != nil {
					return err
				}
				continue
			}
			if destination.Provider != "telegram" {
				return fmt.Errorf("managed recovered commands require Telegram handoff")
			}
			handoff, err := readManagedTelegramHandoff(context.Background(), s.dir, s.db, destination.Account)
			if err != nil {
				return err
			}
			if handoff == nil {
				return fmt.Errorf("managed recovered commands have no consumed Telegram handoff")
			}
			if err := s.RequireManagedTelegramHandoff(context.Background(), *handoff, protocol.Hash(*handoff)); err != nil {
				return err
			}
		}
	}
	if err := s.RequireManagedCommandHistory(hash); err != nil {
		return err
	}
	head, err := s.commandSQLHead()
	if err != nil {
		return err
	}
	if head == nil || head.Mode != mode || head.ActivationHash != s.deliveryActivationHash {
		return fmt.Errorf("command admission does not match durable authority")
	}
	if s.managedCommandMode != "" || len(s.runtimeLoops) != 0 {
		return fmt.Errorf("command admission is immutable for an open runtime")
	}
	if head.Policy != "" {
		policy, err := narrowedCommandConfig(s.config.Interaction, "", head.Policy)
		if err != nil {
			return err
		}
		s.managedCommandPolicy = policy
	}
	if err := s.transaction(context.Background(), s.reserveAliases); err != nil {
		return err
	}
	s.managedCommandMode, s.managedCommandHash = mode, hash
	return nil
}

// RequireManagedSignalHandoff checks the existing immutable recovery handoff
// against the admitted endpoint. Runtime cursor advancement cannot replace it.
func (s *Store) RequireManagedSignalHandoff(ctx context.Context, account provider.SignalAccount) error {
	if s.installationRevision == "" || s.recoveryCommitHash == "" || account.Validate() != nil {
		return fmt.Errorf("managed recovered Signal endpoint required")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		proof, err := readSignalRecoveryCursor(tx, account.Account)
		if err != nil {
			return err
		}
		if proof == nil || proof.Handoff.Target.Identity != account.InboxIdentity || proof.Handoff.Summary.Challenge.Gateway != s.identity {
			return fmt.Errorf("Signal command handoff differs from admitted endpoint")
		}
		return nil
	})
}

func (s *Store) commandIntakeEnabled() bool {
	return !s.verificationOnly && (s.installationRevision == "" || s.managedCommandMode == "read" || s.managedCommandMode == "all")
}

func (s *Store) commandEffectEnabled(effect string) bool {
	return s.commandIntakeEnabled() && (s.installationRevision == "" || s.managedCommandMode == "all" || effect == "read")
}

func (s *Store) commandRepliesEnabled() bool {
	return !s.verificationOnly && (s.installationRevision == "" || s.managedCommandMode == "read" || s.managedCommandMode == "all" || s.managedCommandMode == "drain")
}

func (s *Store) BindManagedCommandAuthority(check func(context.Context) error) error {
	if s.managedCommandMode == "" || s.managedCommandCheck != nil || check == nil || len(s.runtimeLoops) != 0 {
		return fmt.Errorf("command authority must be bound once before runtime startup")
	}
	if err := s.checkRetirementLease(); err != nil {
		return err
	}
	s.managedCommandCheck = check
	return nil
}

func (s *Store) checkManagedCommandAuthority(ctx context.Context) error {
	if s.installationRevision == "" {
		return nil
	}
	if s.managedCommandCheck == nil {
		return fmt.Errorf("managed command authority is not bound")
	}
	return s.managedCommandCheck(ctx)
}

// WaitManagedCommandGrants closes the bounded admission window of grants issued
// before disable. Already admitted jobs retain their identity/results; this does
// not assert that those jobs were cancelled or that an uncertain effect failed.
func (s *Store) WaitManagedCommandGrants(ctx context.Context) error {
	if (!s.verificationOnly && s.managedCommandMode != "drain") || s.installationRevision == "" || len(s.runtimeLoops) != 0 {
		return fmt.Errorf("command grant drain requires offline managed drain mode")
	}
	if err := s.checkRetirementLease(); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT grant FROM commands WHERE grant<>'' AND state NOT IN ('succeeded','failed','rejected','expired')")
	if err != nil {
		return err
	}
	var latest time.Time
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var grant interaction.Grant
		if err = protocol.Decode([]byte(raw), &grant, 8192); err != nil {
			break
		}
		if grant.Identity == s.identity && grant.ExpiresAt.After(latest) {
			latest = grant.ExpiresAt
		}
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	delay := latest.Sub(s.now())
	if delay > 11*time.Second {
		return fmt.Errorf("command grant deadline exceeds bounded drain; clock or evidence changed")
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if latest.After(s.now()) {
		return fmt.Errorf("command drain clock moved backwards")
	}
	return s.checkRetirementLease()
}
