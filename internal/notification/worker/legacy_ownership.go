package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// RestoreLegacy reserves a canonical generation that the shared dispatcher can
// never grant. Its caller retains the stopped legacy sender/attempt leases and
// reconciles all shared outcomes before publishing local legacy authority.
func (s *Store) RestoreLegacy(p protocol.Producer, previous, generation string) error {
	if s.verificationOnly {
		return fmt.Errorf("legacy activation unavailable in verification mode")
	}
	if !protocol.ValidToken(previous) || !protocol.ValidToken(generation) || generation == previous {
		return fmt.Errorf("legacy restoration requires distinct exact generations")
	}
	return s.ownership(p, func(o *ownership) error {
		if o.Generations[previous] != "revoked" {
			return fmt.Errorf("shared generation has not been revoked")
		}
		if o.Active == generation && o.Generations[generation] == "legacy-active" {
			return nil
		}
		return s.commitOwnership(o, "legacy-activate", generation)
	})
}

// This is a lifecycle method, not a producer RPC. The caller must stop/drain its
// legacy service first. Active legacy I/O also retains the same canonical lock.
func (s *Store) RevokeLegacy(p protocol.Producer, generation string) error {
	return s.ownership(p, func(o *ownership) error {
		if o.Generations[generation] == "revoked" {
			return nil
		}
		if o.Generations[generation] == "" {
			if err := s.commitOwnership(o, "stage", generation); err != nil {
				return err
			}
		}
		if o.Generations[generation] == "staged" {
			if err := s.commitOwnership(o, "drain", generation); err != nil {
				return err
			}
		}
		if o.Generations[generation] == "draining" {
			return s.commitOwnership(o, "revoke", generation)
		}
		return s.commitOwnership(o, "legacy-revoke", generation)
	})
}

func (s *Store) RequireLegacyAccepted(ctx context.Context, m protocol.Message) error {
	status, err := s.OwnershipStatus(m.Producer, m.Generation)
	if err != nil || status.State != "revoked" {
		return fmt.Errorf("shared generation not revoked")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		j, err := readJob(tx, m.Producer.ID, m.ID)
		if err != nil {
			return err
		}
		if protocol.Hash(j.Message) != protocol.Hash(m) || j.State != "provider_accepted" || j.Attempt != "" {
			return fmt.Errorf("canonical acceptance unavailable")
		}
		var raw string
		if err = tx.QueryRow("SELECT body FROM receipts WHERE producer=? AND json_extract(body,'$.event_id')=? AND json_extract(body,'$.state')='provider_accepted' ORDER BY position DESC LIMIT 1", m.Producer.ID, m.ID).Scan(&raw); err != nil {
			return fmt.Errorf("retained canonical acceptance required before rollback: %w", err)
		}
		var receipt protocol.Receipt
		if err = protocol.Decode([]byte(raw), &receipt, 16384); err != nil {
			return err
		}
		if receipt.EventID != m.ID || receipt.Sequence != m.Sequence || receipt.Acceptance == nil || !receipt.Acceptance.Eligible {
			return fmt.Errorf("canonical acceptance was not eligible")
		}
		return nil
	})
}

// Missing queue rows are never proof of no send below the submission watermark.
// This check is used under the stopped managed worker lease after revocation.
func (s *Store) RequireLegacyUnsent(ctx context.Context, m protocol.Message) error {
	status, err := s.OwnershipStatus(m.Producer, m.Generation)
	if err != nil || status.State != "revoked" {
		return fmt.Errorf("legacy reconciliation requires a revoked shared generation")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		j, err := readJob(tx, m.Producer.ID, m.ID)
		if err == sql.ErrNoRows {
			var submitted uint64
			if err = tx.QueryRow("SELECT submitted FROM producers WHERE id=?", m.Producer.ID).Scan(&submitted); err != nil {
				return err
			}
			if m.Sequence > submitted {
				return nil
			}
			return fmt.Errorf("missing shared history cannot establish unsent delivery")
		}
		if err != nil {
			return err
		}
		if protocol.Hash(j.Message) != protocol.Hash(m) || j.Tries != 0 || j.Attempt != "" || j.State == "provider_accepted" || j.State == "ambiguous" {
			return fmt.Errorf("shared message requires outcome reconciliation before legacy restoration")
		}
		return nil
	})
}

// AcquireLegacyAuthority holds the canonical producer lock across bounded legacy
// provider I/O. It opens that stable inode read-only, so a native service may
// mount canonical worker state read-only. No queue database is opened or changed.
func AcquireLegacyAuthority(root string, identity protocol.Identity, p protocol.Producer, generation string) (check func() error, release func(), err error) {
	if !identity.Valid() || !p.Valid() || !protocol.ValidToken(generation) {
		return nil, nil, fmt.Errorf("invalid canonical legacy binding")
	}
	dir, err := safefs.Open(root)
	if err != nil {
		return nil, nil, err
	}
	name := ownershipName(p) + ".lock"
	file, err := dir.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		dir.Close()
		return nil, nil, err
	}
	release = func() { file.Close(); dir.Close() }
	if err = unix.Flock(int(file.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		release()
		return nil, nil, fmt.Errorf("canonical producer is busy: %w", err)
	}
	s := &Store{dir: dir, identity: identity}
	check = func() error {
		held, err := dir.File().Stat()
		if err != nil {
			return err
		}
		current, err := os.Lstat(root)
		if err != nil || !os.SameFile(held, current) {
			return fmt.Errorf("canonical worker directory changed")
		}
		held, err = file.Stat()
		if err != nil {
			return err
		}
		probe, err := dir.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		current, statErr := probe.Stat()
		probe.Close()
		if statErr != nil || !os.SameFile(held, current) {
			return fmt.Errorf("canonical producer lease changed")
		}
		var currentIdentity protocol.Identity
		if err := dir.ReadJSON("identity.json", &currentIdentity); err != nil {
			return err
		}
		if currentIdentity != identity {
			return fmt.Errorf("canonical worker identity changed")
		}
		// The native sender is independent of the gateway process. A recovery
		// may publish its quarantine before migrating this producer's owner;
		// that old owner must not authorize another native attempt meanwhile.
		for _, gate := range []string{"recovery.json", publicationCommitIntent, "schema-migration.json"} {
			f, err := dir.Open(gate, os.O_RDONLY|unix.O_NONBLOCK, 0)
			if err == nil {
				f.Close()
				return fmt.Errorf("canonical legacy delivery is fenced by %s", gate)
			}
			if !os.IsNotExist(err) {
				return err
			}
		}
		o, err := s.readOwnership(p)
		if err != nil {
			return err
		}
		if o.Identity != identity || o.Producer != p || o.Active != generation || o.Generations[generation] != "legacy-active" {
			return fmt.Errorf("canonical legacy generation is not active")
		}
		return nil
	}
	if err = check(); err != nil {
		release()
		return nil, nil, err
	}
	// A native pass does not open Store, so it must perform the startup audit
	// itself. Checking only the adjacent transition cannot detect a missing
	// successor that hides later retained revocation history.
	o, err := s.readOwnership(p)
	if err == nil {
		err = s.checkOwnershipHistoryTail(p, o.Revision)
	}
	if err == nil {
		err = s.verifyOwnershipPrefix(o)
	}
	if err != nil {
		release()
		return nil, nil, err
	}
	return check, release, nil
}
