package worker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type ownership struct {
	Identity    protocol.Identity `json:"identity"`
	Producer    protocol.Producer `json:"producer"`
	Active      string            `json:"active"`
	Generations map[string]string `json:"generations"`
	Revision    uint64            `json:"revision"`
	History     string            `json:"history"`
	// A fence is not reconciliation. Ordinary staging must not bypass this marker.
	RecoveryPending *recoveryBinding    `json:"recovery_pending,omitempty"`
	Retirement      *ProducerRetirement `json:"retirement,omitempty"`
}

// RequireInactiveProducers is used while the exclusive worker lease is held by
// lifecycle tools. Neither active nor draining generations may be uninstalled
// or silently adopted by a verification-only installation.
func (s *Store) RequireInactiveProducers() error {
	for _, e := range s.config.Producers {
		if err := s.ownership(e.Producer, func(o *ownership) error {
			if o.Active != "" {
				return fmt.Errorf("producer delivery must be explicitly revoked before lifecycle changes")
			}
			for _, state := range o.Generations {
				if state == "active" || state == "draining" {
					return fmt.Errorf("producer generation is still active or draining")
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) OwnershipStatus(p protocol.Producer, g string) (status protocol.OwnershipStatus, err error) {
	if !protocol.ValidToken(g) {
		return status, fmt.Errorf("invalid generation")
	}
	status = protocol.OwnershipStatus{Producer: p, Generation: g, State: "unknown"}
	err = s.ownership(p, func(o *ownership) error {
		if state := o.Generations[g]; state != "" {
			status.State = state
		}
		status.Active = o.Active == g && status.State == "active"
		return nil
	})
	return
}

func (s *Store) ownership(p protocol.Producer, fn func(*ownership) error) error {
	if _, err := s.config.enrollment(p); err != nil {
		return err
	}
	name := "owner-" + safefs.LockName(p.ID)
	lock, err := s.dir.Lock(name+".lock", true, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	o, err := s.readOwnership(p)
	if err != nil {
		return err
	}
	return fn(&o)
}

// Stage/Activate/Revoke are lifecycle operations, never producer API methods.
// Activation callers must first complete installation qualification and handoff.
func (s *Store) Stage(p protocol.Producer, g string) error {
	if !protocol.ValidToken(g) {
		return fmt.Errorf("invalid generation")
	}
	return s.ownership(p, func(o *ownership) error {
		if state := o.Generations[g]; state != "" {
			if state == "staged" {
				return nil
			}
			return fmt.Errorf("generation already used")
		}
		return s.commitOwnership(o, "stage", g)
	})
}
func (s *Store) Activate(p protocol.Producer, g string) error {
	if s.verificationOnly {
		return fmt.Errorf("producer activation unavailable in verification mode")
	}
	return s.ownership(p, func(o *ownership) error {
		if o.Active == g && o.Generations[g] == "active" {
			return nil
		}
		if o.Active != "" || o.Generations[g] != "staged" {
			return fmt.Errorf("generation cannot activate")
		}
		return s.commitOwnership(o, "activate", g)
	})
}
func (s *Store) Revoke(ctx context.Context, p protocol.Producer, g string) error {
	return s.ownership(p, func(o *ownership) error {
		if o.Generations[g] == "revoked" {
			return nil
		}
		if o.Generations[g] == "" {
			return fmt.Errorf("unknown generation")
		}
		if o.Generations[g] != "draining" {
			if err := s.commitOwnership(o, "drain", g); err != nil {
				return err
			}
		}
		err := s.transaction(ctx, func(tx *sql.Tx) error {
			var count int
			if err := tx.QueryRow("SELECT count(*) FROM messages WHERE producer=? AND generation=? AND attempt<>''", p.ID, g).Scan(&count); err != nil {
				return err
			}
			if count > 0 {
				return fmt.Errorf("generation draining: attempts remain")
			}
			return nil
		})
		if err != nil {
			return err
		}
		return s.commitOwnership(o, "revoke", g)
	})
}
