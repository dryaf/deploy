package worker

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const managedDeliveryOwnerFile = "managed-delivery-owner.json"
const managedDeliveryOwnerKey = "managed-delivery-owner"

// ManagedDeliveryOwner permanently enrolls state in one installation. It is
// independent of a service activation and remains after deactivation/uninstall.
type ManagedDeliveryOwner struct {
	Version      int               `json:"version"`
	Root         string            `json:"root"`
	Installation string            `json:"installation"`
	Identity     protocol.Identity `json:"identity"`
	ConfigHash   string            `json:"config_hash"`
}

func (o ManagedDeliveryOwner) validate() error {
	if o.Version != 1 || !filepath.IsAbs(o.Root) || filepath.Clean(o.Root) != o.Root || o.Root == "/" || !safefs.ValidID(o.Installation, "n") || !o.Identity.Valid() || !safefs.DigestPattern.MatchString(o.ConfigHash) {
		return fmt.Errorf("invalid managed delivery ownership")
	}
	return nil
}

func InspectManagedDeliveryOwner(path string) (ManagedDeliveryOwner, error) {
	var owner ManagedDeliveryOwner
	d, err := privateDirectory(path)
	if err != nil {
		return owner, err
	}
	defer d.Close()
	if err = d.ReadJSON(managedDeliveryOwnerFile, &owner); err != nil {
		return owner, err
	}
	return owner, owner.validate()
}

func deliveryConfig(c Config) error {
	return c.Validate()
}

func (s *Store) EnrollManagedDelivery(o ManagedDeliveryOwner) error {
	if !s.verificationOnly || s.installationRevision == "" || s.recoveryCommitHash != "" || o.validate() != nil || o.Identity != s.identity || o.ConfigHash != protocol.Hash(s.config) {
		return fmt.Errorf("delivery enrollment requires the exact initialized managed verification state")
	}
	if err := deliveryConfig(s.config); err != nil {
		return err
	}
	if err := s.RequireInactiveProducers(); err != nil {
		return err
	}
	var current ManagedDeliveryOwner
	var stored string
	dbErr := s.db.QueryRow("SELECT value FROM metadata WHERE key=?", managedDeliveryOwnerKey).Scan(&stored)
	if dbErr != nil && dbErr != sql.ErrNoRows {
		return dbErr
	}
	if dbErr == nil {
		var anchored ManagedDeliveryOwner
		if protocol.Decode([]byte(stored), &anchored, 8192) != nil || anchored != o {
			return fmt.Errorf("managed delivery SQL owner changed")
		}
	}
	err := s.dir.ReadJSON(managedDeliveryOwnerFile, &current)
	if os.IsNotExist(err) {
		if dbErr == nil {
			return fmt.Errorf("managed delivery owner file was lost")
		}
		if err = s.dir.WriteJSON(managedDeliveryOwnerFile, o, false); err != nil {
			return err
		}
		current = o
	} else if err != nil {
		return err
	}
	if current != o {
		return fmt.Errorf("managed delivery owner changed")
	}
	if dbErr == sql.ErrNoRows {
		raw, err := json.Marshal(o)
		if err != nil {
			return err
		}
		_, err = s.db.Exec("INSERT INTO metadata(key,value) VALUES(?,?)", managedDeliveryOwnerKey, string(raw))
		return err
	}
	return nil
}

func (s *Store) RequireManagedDeliveryOwner(o ManagedDeliveryOwner, hash string) error {
	return s.requireManagedDeliveryOwner(o, hash, s.identity)
}

func (s *Store) requireManagedDeliveryOwner(o ManagedDeliveryOwner, hash string, identity protocol.Identity) error {
	if err := s.checkRetirementLease(); err != nil {
		return err
	}
	held, err := s.dir.File().Stat()
	if err != nil {
		return err
	}
	currentDir, err := os.Lstat(s.dir.File().Name())
	if err != nil {
		return err
	}
	if !os.SameFile(held, currentDir) {
		return fmt.Errorf("managed delivery state directory changed")
	}

	if o.validate() != nil || protocol.Hash(o) != hash || o.Identity != identity || o.ConfigHash != protocol.Hash(s.config) {
		return fmt.Errorf("managed delivery owner does not bind this instance")
	}
	var current ManagedDeliveryOwner
	if err := s.dir.ReadJSON(managedDeliveryOwnerFile, &current); err != nil {
		return err
	}
	if current != o {
		return fmt.Errorf("retained delivery ownership changed")
	}
	var raw string
	if err := s.db.QueryRow("SELECT value FROM metadata WHERE key=?", managedDeliveryOwnerKey).Scan(&raw); err != nil {
		return err
	}
	var anchored ManagedDeliveryOwner
	if protocol.Decode([]byte(raw), &anchored, 8192) != nil || anchored != o {
		return fmt.Errorf("managed delivery SQL owner changed")
	}
	return nil
}

// OpenManagedDelivery never initializes/migrates state or activates a producer.
// The installer must retain its shared lease and verify current admission before
// startup and before each external send. Commands require separate admission;
// the installer guards every LED request with the qualified activation.
func OpenManagedDelivery(path string, c Config, expected protocol.Identity, revision string, owner ManagedDeliveryOwner, ownerHash, activationHash string) (*Store, error) {
	if !expected.Valid() || !safefs.ValidID(revision, "n") || !safefs.DigestPattern.MatchString(activationHash) {
		return nil, fmt.Errorf("managed delivery requires exact instance and activation pins")
	}
	if err := deliveryConfig(c); err != nil {
		return nil, err
	}
	s, err := openStore(path, c, false, nil, true, expected, "")
	if err != nil {
		return nil, err
	}
	if err = s.RequireManagedDeliveryOwner(owner, ownerHash); err != nil {
		s.Close()
		return nil, err
	}
	s.installationRevision, s.deliveryActivationHash = revision, activationHash
	return s, nil
}
