package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// ManagedDeliveryRecovery preserves the original installation owner. Adoption
// only records continuity; a separate, freshly qualified activation admits sends.
type ManagedDeliveryRecovery struct {
	Version    int                  `json:"version"`
	Owner      ManagedDeliveryOwner `json:"owner"`
	Before     protocol.Identity    `json:"before"`
	After      protocol.Identity    `json:"after"`
	CommitHash string               `json:"commit_hash"`
}

func (r ManagedDeliveryRecovery) Validate() error {
	validOwnerEpoch := r.Version == 1 && (r.Owner.Identity == r.Before || r.Owner.Identity == r.After) || r.Version == 2 && r.Owner.Identity.Worker == r.Before.Worker
	if !validOwnerEpoch || r.Owner.validate() != nil || !r.Before.Valid() || !r.After.Valid() || r.Before.Worker != r.After.Worker || r.Before.Recovery == r.After.Recovery || !safefs.DigestPattern.MatchString(r.CommitHash) {
		return fmt.Errorf("invalid managed delivery recovery binding")
	}
	return nil
}

func (r ManagedDeliveryRecovery) name() string { return "managed-delivery-recovery-" + r.CommitHash }

func (s *Store) checkDeliveryRecovery(r ManagedDeliveryRecovery) error {
	if err := s.checkRetirementLease(); err != nil {
		return err
	}
	held, err := s.dir.File().Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(s.dir.File().Name())
	if err != nil {
		return err
	}
	if !os.SameFile(held, current) {
		return fmt.Errorf("recovered delivery state directory changed")
	}
	if r.Validate() != nil || r.After != s.identity || r.CommitHash != s.recoveryCommitHash || r.Owner.ConfigHash != protocol.Hash(s.config) || r.Before != s.recoveryProof.Before || r.After != s.recoveryProof.After {
		return fmt.Errorf("delivery recovery does not bind the verified publication")
	}
	// Startup audits all immutable source evidence. During bounded operations,
	// retain that worker lease and check the current publication/adoption anchors.
	for _, name := range []string{publicationCommitIntent, publicationCommitSeal, publicationCommitComplete} {
		ok, err := publicationRecord(s.dir, name, s.recoveryPublication)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("delivery recovery publication anchor missing")
		}
	}
	want := RecoveryVerification{1, r.CommitHash, protocol.Hash(s.config), r.Before, r.After, true}
	for _, name := range []string{recoveryVerificationIntent, recoveryVerificationComplete} {
		ok, err := verificationRecord(s.dir, name, want)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("delivery recovery adoption anchor missing")
		}
	}
	var id protocol.Identity
	if err := readCheckpointJSON(s.dir, "identity.json", &id); err != nil {
		return err
	}
	var gate snapshotRecovery
	if err := readCheckpointJSON(s.dir, "recovery.json", &gate); err != nil {
		return err
	}
	p := s.recoveryProof
	if gate.RuntimeHash != p.OriginalRuntimeHash || gate.Snapshot.RecoveryHash != p.AncestorHash || id != r.After || gate.validate(s.config) != nil || gate.Phase != "quarantined" || gate.Before != p.Before || gate.After != p.After || gate.Operation != p.Operation || gate.Snapshot.ID != p.Snapshot || gate.CandidateHash != p.CandidateDatabaseHash || gate.Snapshot.DatabaseHash != p.SourceDatabaseHash {
		return fmt.Errorf("delivery recovery identity or quarantine changed")
	}
	return verifyRecoveryRuntimeSQL(context.Background(), s.db, p)
}

func deliveryRecoveryRecord(d *safefs.Directory, name string, want ManagedDeliveryRecovery) (bool, error) {
	var got ManagedDeliveryRecovery
	err := readCheckpointJSON(d, name, &got)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if got != want {
		return false, fmt.Errorf("managed delivery recovery record changed")
	}
	return true, nil
}

// EnrollManagedDeliveryRecovery imports only an independently retained owner
// proven in the original database of this publication. An older snapshot may
// lack the SQL owner; the original source and owner file must both retain it.
func (s *Store) EnrollManagedDeliveryRecovery(r ManagedDeliveryRecovery) error {
	return s.enrollManagedDeliveryRecovery(r, nil)
}

func (s *Store) enrollManagedDeliveryRecovery(r ManagedDeliveryRecovery, fault func(string) error) error {
	if !s.verificationOnly || s.installationRevision == "" {
		return fmt.Errorf("delivery recovery enrollment requires managed verification")
	}
	if err := deliveryConfig(s.config); err != nil {
		return err
	}
	if err := s.checkDeliveryRecovery(r); err != nil {
		return err
	}
	if err := s.RequireInactiveProducers(); err != nil {
		return err
	}
	if r.Version == 2 {
		if err := s.requireAncestorDeliveryOwner(r); err != nil {
			return err
		}
	}
	fresh := r.Owner.Identity == r.After
	var owner ManagedDeliveryOwner
	err := readCheckpointJSON(s.dir, managedDeliveryOwnerFile, &owner)
	missingOwner := os.IsNotExist(err)
	if err != nil && (!missingOwner || !fresh) {
		return err
	}
	if !missingOwner && owner != r.Owner {
		return fmt.Errorf("original delivery owner file changed")
	}
	if err := verifyRetainedDatabase(context.Background(), s.dir, publicationExchange, s.recoveryPublication.OriginalDatabaseHash, s.recoveryPublication.OriginalDatabaseBytes, s.config.MaxDatabaseBytes); err != nil {
		return err
	}
	archive, err := immutableOriginalDB(s.dir, publicationExchange)
	if err != nil {
		return err
	}
	var raw string
	err = archive.QueryRow("SELECT value FROM metadata WHERE key=?", managedDeliveryOwnerKey).Scan(&raw)
	closeErr := archive.Close()
	if err != nil && !(fresh && err == sql.ErrNoRows) {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if fresh && err != sql.ErrNoRows {
		return fmt.Errorf("original source was already enrolled; preserve its delivery owner")
	}
	if !fresh && (protocol.Decode([]byte(raw), &owner, 8192) != nil || owner != r.Owner) {
		return fmt.Errorf("original source does not prove delivery ownership")
	}
	intent, err := deliveryRecoveryRecord(s.dir, r.name()+".json", r)
	if err != nil {
		return err
	}
	complete, err := deliveryRecoveryRecord(s.dir, r.name()+"-complete.json", r)
	if err != nil {
		return err
	}
	if missingOwner {
		var existing int
		e := s.db.QueryRow("SELECT count(*) FROM metadata WHERE key IN (?,?)", managedDeliveryOwnerKey, r.name()).Scan(&existing)
		if intent || complete || e != nil || existing != 0 {
			return fmt.Errorf("recovered owner file missing while independent authority remains")
		}
		if err = s.dir.WriteJSON(managedDeliveryOwnerFile, r.Owner, false); err != nil {
			return err
		}
	}
	err = s.db.QueryRow("SELECT value FROM metadata WHERE key=?", r.name()).Scan(&raw)
	anchored := err == nil
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if anchored {
		var got ManagedDeliveryRecovery
		if !intent || protocol.Decode([]byte(raw), &got, 16384) != nil || got != r {
			return fmt.Errorf("delivery recovery lost or changed its intent")
		}
		if err = s.requireManagedDeliveryOwner(r.Owner, protocol.Hash(r.Owner), r.Owner.Identity); err != nil {
			return err
		}
	}
	if complete {
		if !anchored || !intent {
			return fmt.Errorf("completed delivery recovery lost its anchors")
		}
		return nil
	}
	if !intent {
		if err = s.dir.WriteJSON(r.name()+".json", r, false); err != nil {
			return err
		}
	}
	if err = recoveryBoundary(fault, "delivery-recovery-intent"); err != nil {
		return err
	}
	if !anchored {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		err = tx.QueryRow("SELECT value FROM metadata WHERE key=?", managedDeliveryOwnerKey).Scan(&raw)
		if err == sql.ErrNoRows {
			b, _ := json.Marshal(r.Owner)
			_, err = tx.Exec("INSERT INTO metadata(key,value) VALUES(?,?)", managedDeliveryOwnerKey, string(b))
		} else if err == nil && (protocol.Decode([]byte(raw), &owner, 8192) != nil || owner != r.Owner) {
			err = fmt.Errorf("recovered SQL delivery owner conflicts with original source")
		}
		if err != nil {
			return err
		}
		b, _ := json.Marshal(r)
		if _, err = tx.Exec("INSERT INTO metadata(key,value) VALUES(?,?)", r.name(), string(b)); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	if err = recoveryBoundary(fault, "delivery-recovery-sql"); err != nil {
		return err
	}
	if err = s.dir.WriteJSON(r.name()+"-complete.json", r, false); err != nil {
		return err
	}
	return recoveryBoundary(fault, "delivery-recovery-complete")
}

func (s *Store) RequireManagedDeliveryRecovery(r ManagedDeliveryRecovery, ownerHash string) error {
	if err := s.checkDeliveryRecovery(r); err != nil {
		return err
	}
	if r.Version == 2 {
		if err := s.requireAncestorDeliveryOwner(r); err != nil {
			return err
		}
	}
	if err := s.requireManagedDeliveryOwner(r.Owner, ownerHash, r.Owner.Identity); err != nil {
		return err
	}
	for _, name := range []string{r.name() + ".json", r.name() + "-complete.json"} {
		ok, err := deliveryRecoveryRecord(s.dir, name, r)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("delivery recovery enrollment incomplete")
		}
	}
	var raw string
	if err := s.db.QueryRow("SELECT value FROM metadata WHERE key=?", r.name()).Scan(&raw); err != nil {
		return err
	}
	var got ManagedDeliveryRecovery
	if protocol.Decode([]byte(raw), &got, 16384) != nil || got != r {
		return fmt.Errorf("delivery recovery SQL anchor changed")
	}
	return nil
}

// OpenManagedRecoveredDelivery audits recovery under the worker lease before
// enabling outbound execution. It never activates a producer generation.
func OpenManagedRecoveredDelivery(path string, c Config, revision string, r ManagedDeliveryRecovery, ownerHash, activationHash string) (*Store, error) {
	if r.Validate() != nil || !safefs.DigestPattern.MatchString(activationHash) {
		return nil, fmt.Errorf("recovered delivery requires exact ownership and activation")
	}
	if err := deliveryConfig(c); err != nil {
		return nil, err
	}
	s, err := OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
	if err != nil {
		return nil, err
	}
	if err = s.RequireManagedDeliveryRecovery(r, ownerHash); err != nil {
		s.Close()
		return nil, err
	}
	s.verificationOnly = false
	s.deliveryActivationHash = activationHash
	return s, nil
}

// A later epoch must prove the same owner was enrolled in its immediate parent,
// not merely share a worker identifier with an arbitrary older installation.
func (s *Store) requireAncestorDeliveryOwner(r ManagedDeliveryRecovery) error {
	stage, err := s.dir.Sub("recovery/" + s.recoveryProof.Operation)
	if err != nil {
		return err
	}
	defer stage.Close()
	var j snapshotRecovery
	if err = readCheckpointJSON(s.dir, "recovery.json", &j); err != nil {
		return err
	}
	e, err := readSnapshotRecovery(stage, j.Snapshot, s.config)
	if err != nil {
		return err
	}
	if e == nil || e.Proof.After != r.Before {
		return fmt.Errorf("later delivery recovery lost its parent")
	}
	db, err := immutableOriginalDB(s.dir, publicationExchange)
	if err != nil {
		return err
	}
	defer db.Close()
	var raw string
	name := "managed-delivery-recovery-" + e.Publication.CommitHash
	if err = db.QueryRow("SELECT value FROM metadata WHERE key=?", name).Scan(&raw); err != nil {
		return err
	}
	var parent ManagedDeliveryRecovery
	if protocol.Decode([]byte(raw), &parent, 16384) != nil || parent.Validate() != nil || parent.Owner != r.Owner || parent.Before != e.Proof.Before || parent.After != r.Before || parent.CommitHash != e.Publication.CommitHash {
		return fmt.Errorf("later recovery changed its independently retained owner")
	}
	return nil
}
