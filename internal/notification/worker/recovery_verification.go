package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

const recoveryVerificationIntent = "recovery-verification.json"
const recoveryVerificationComplete = "recovery-verification-complete.json"

// RecoveryVerification binds a completed publication to verification-only runtime.
// It does not clear recovery quarantine or grant provider/producer authority.
type RecoveryVerification struct {
	Version          int               `json:"version"`
	CommitHash       string            `json:"commit_hash"`
	ConfigHash       string            `json:"config_hash"`
	Before           protocol.Identity `json:"before"`
	After            protocol.Identity `json:"after"`
	VerificationOnly bool              `json:"verification_only"`
}

// ReviewPublishedRecovery checks the publication before a managed installation
// prepares its identity transition. It creates no adoption records.
func ReviewPublishedRecovery(ctx context.Context, path string, c Config, commit string) (RecoveryVerification, error) {
	return recoveryVerification(ctx, path, c, commit, false, nil)
}

// AdoptPublishedRecoveryVerification durably admits only verification runtime.
// The caller must hold the exclusive installation lease after stopping its unit.
func AdoptPublishedRecoveryVerification(ctx context.Context, path string, c Config, commit string) (RecoveryVerification, error) {
	return recoveryVerification(ctx, path, c, commit, true, nil)
}

func recoveryVerification(ctx context.Context, path string, c Config, commit string, adopt bool, fault func(string) error) (out RecoveryVerification, err error) {
	if err = c.Validate(); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	d, err := privateDirectory(path)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, d.Close()) }()
	lease, err := d.Lock("worker.lock", true, false)
	if err != nil {
		return out, err
	}
	defer lease.Close()
	p, proof, err := verifyCompletedPublication(ctx, d, lease, c, commit)
	if err != nil {
		return out, err
	}
	out = RecoveryVerification{1, commit, protocol.Hash(c), proof.Before, proof.After, true}
	intent, err := verificationRecord(d, recoveryVerificationIntent, out)
	if err != nil {
		return out, err
	}
	complete, err := verificationRecord(d, recoveryVerificationComplete, out)
	if err != nil {
		return out, err
	}
	if complete && !intent {
		return out, fmt.Errorf("recovery verification completion lost its intent")
	}
	if !complete {
		// No canonical writes occur before completion. A lost completion can only be
		// resumed against the exact published bytes, never a subsequently used store.
		if err = verifyRetainedDatabase(ctx, d, "state.db", p.Image.DatabaseHash, p.Image.DatabaseBytes, c.MaxDatabaseBytes); err != nil {
			return out, err
		}
	}
	if err = verifyRecoveryRuntimeFiles(d, c); err != nil {
		return out, err
	}
	db, err := snapshotDB(d, "state.db", true)
	if err != nil {
		return out, err
	}
	err = verifyRecoveryRuntimeSQL(ctx, db, proof)
	err = errors.Join(err, db.Close())
	if err != nil {
		return out, err
	}
	if !adopt || complete {
		return out, nil
	}
	if !intent {
		if err = d.WriteJSON(recoveryVerificationIntent, out, false); err != nil {
			return out, err
		}
	}
	if err = recoveryBoundary(fault, "recovery_verification_intent"); err != nil {
		return out, err
	}
	// Recheck after the durable boundary; records never substitute for source proof.
	if _, _, err = verifyCompletedPublication(ctx, d, lease, c, commit); err != nil {
		return out, err
	}
	if intent, err = verificationRecord(d, recoveryVerificationIntent, out); err != nil {
		return out, err
	} else if !intent {
		return out, fmt.Errorf("recovery verification intent disappeared")
	}
	if err = verifyRetainedDatabase(ctx, d, "state.db", p.Image.DatabaseHash, p.Image.DatabaseBytes, c.MaxDatabaseBytes); err != nil {
		return out, err
	}
	if err = d.WriteJSON(recoveryVerificationComplete, out, false); err != nil {
		return out, err
	}
	err = recoveryBoundary(fault, "recovery_verification_complete")
	return
}

func verificationRecord(d *safefs.Directory, name string, want RecoveryVerification) (bool, error) {
	var got RecoveryVerification
	err := readCheckpointJSON(d, name, &got)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if got != want {
		return false, fmt.Errorf("recovery verification binding changed: %s", name)
	}
	return true, nil
}

func verifyCompletedPublication(ctx context.Context, d *safefs.Directory, lease *safefs.Lock, c Config, pin string) (p RecoveryPublicationCommit, proof RecoveryCheckpointProof, err error) {
	if !safefs.DigestPattern.MatchString(pin) {
		return p, proof, fmt.Errorf("exact recovery publication commit required")
	}
	if err = readCheckpointJSON(d, publicationCommitIntent, &p); err != nil {
		return
	}
	r := recoveryReconciler{dir: d, config: c, publication: &p}
	if err = r.checkPublicationGate(); err != nil {
		return
	}
	if p.CommitHash != pin {
		return p, proof, fmt.Errorf("recovery publication commit mismatch")
	}
	for _, name := range []string{publicationCommitSeal, publicationCommitComplete} {
		ok, e := publicationRecord(d, name, p)
		if e != nil {
			return p, proof, e
		}
		if !ok {
			return p, proof, fmt.Errorf("publication is incomplete: %s", name)
		}
	}
	im := p.Image
	image, e := verifyRetainedPublication(ctx, d, lease, c, im.Operation, im.ReviewHash, im.CheckpointHash, im.ProofHash, im.ImageHash, nil)
	if e != nil {
		return p, proof, e
	}
	if image != im {
		return p, proof, fmt.Errorf("publication image changed")
	}
	proof, err = verifyRetainedCheckpoint(ctx, d, lease, c, im.Operation, im.ReviewHash, im.CheckpointHash, nil)
	if err != nil {
		return
	}
	if proof.OriginalSourceHash != p.OriginalSourceHash || proof.OriginalDatabaseHash != p.OriginalDatabaseHash {
		return p, proof, fmt.Errorf("publication original source mismatch")
	}
	if err = verifyRetainedDatabase(ctx, d, publicationExchange, p.OriginalDatabaseHash, p.OriginalDatabaseBytes, c.MaxDatabaseBytes); err != nil {
		return
	}
	var id protocol.Identity
	if err = readCheckpointJSON(d, "identity.json", &id); err != nil {
		return
	}
	if id != proof.After {
		return p, proof, fmt.Errorf("published worker identity changed")
	}
	var j snapshotRecovery
	if err = readCheckpointJSON(d, "recovery.json", &j); err != nil {
		return
	}
	if err = j.validate(c); err != nil {
		return
	}
	if j.RuntimeHash != proof.OriginalRuntimeHash || j.Snapshot.RecoveryHash != proof.AncestorHash || j.Phase != "quarantined" || j.CandidateHash != proof.CandidateDatabaseHash || j.Snapshot.DatabaseHash != proof.SourceDatabaseHash || j.Before != proof.Before || j.After != proof.After || j.Operation != proof.Operation || j.Snapshot.ID != proof.Snapshot {
		return p, proof, fmt.Errorf("published recovery gate changed")
	}
	return
}

func verifyRecoveryRuntimeFiles(d *safefs.Directory, c Config) error {
	f, err := snapshotFile(d, "state.db", c.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	f.Close()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		f, e := d.Open("state.db"+suffix, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		info, e := f.Stat()
		f.Close()
		if e != nil {
			return e
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0600 || info.Size() > c.MaxDatabaseBytes {
			return fmt.Errorf("unsafe recovery runtime SQLite sidecar")
		}
	}
	return nil
}

func verifyRecoveryRuntimeSQL(ctx context.Context, db *sql.DB, proof RecoveryCheckpointProof) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != currentSchema {
		return fmt.Errorf("published recovery requires current storage schema")
	}
	var raw string
	if err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", publicationAnchorKey).Scan(&raw); err != nil {
		return err
	}
	var anchor recoveryPublicationAnchor
	if err := protocol.Decode([]byte(raw), &anchor, safefs.MaxRecordBytes); err != nil {
		return err
	}
	if anchor != (recoveryPublicationAnchor{1, "fenced", proof}) {
		return fmt.Errorf("live recovery publication anchor changed")
	}
	if err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='recovery'").Scan(&raw); err != nil {
		return err
	}
	var binding recoveryBinding
	if err := protocol.Decode([]byte(raw), &binding, 4096); err != nil {
		return err
	}
	if binding != (recoveryBinding{proof.Operation, proof.Snapshot, proof.Before, proof.After}) {
		return fmt.Errorf("live recovery quarantine changed")
	}
	if err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&raw); err != nil {
		return err
	}
	var id protocol.Identity
	if err := protocol.Decode([]byte(raw), &id, 4096); err != nil {
		return err
	}
	if id != proof.After {
		return fmt.Errorf("live recovery identity changed")
	}
	return nil
}

// OpenManagedRecoveryVerification requires the explicit adoption records. It
// sets verification mode before opening the database and cannot activate work.
func OpenManagedRecoveryVerification(path string, c Config, expected protocol.Identity, revision, commit string) (*Store, error) {
	if !expected.Valid() || !safefs.ValidID(revision, "n") || !safefs.DigestPattern.MatchString(commit) {
		return nil, fmt.Errorf("managed recovery requires identity, revision and commit pins")
	}
	s, err := openStore(path, c, false, nil, true, expected, commit)
	if err == nil {
		s.installationRevision = revision
	}
	return s, err
}
