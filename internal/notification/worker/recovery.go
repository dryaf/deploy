package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// This is the quarantine stage, not an activation API. Production restore will
// also need producer/command reconciliation and managed service/relay draining.
// The original live database and canonical ownership histories remain untouched.
type snapshotRecovery struct {
	Version       int               `json:"version"`
	Operation     string            `json:"operation"`
	Before        protocol.Identity `json:"before"`
	After         protocol.Identity `json:"after"`
	Snapshot      SnapshotManifest  `json:"snapshot"`
	Phase         string            `json:"phase"`
	CandidateHash string            `json:"candidate_hash,omitempty"`
}

type recoveryBinding struct {
	Operation string            `json:"operation"`
	Snapshot  string            `json:"snapshot"`
	Before    protocol.Identity `json:"before"`
	After     protocol.Identity `json:"after"`
}

func (j snapshotRecovery) binding() recoveryBinding {
	return recoveryBinding{j.Operation, j.Snapshot.ID, j.Before, j.After}
}

func (j snapshotRecovery) validate(c Config) error {
	if j.Snapshot.Schema == 6 && len(c.LEDBindings) != 0 {
		return fmt.Errorf("legacy recovery cannot contain LED bindings")
	}
	if j.Version != 1 || !safefs.ValidID(j.Operation, "n") || !j.Before.Valid() || !j.After.Valid() ||
		j.Before.Worker != j.After.Worker || j.Before.Recovery == j.After.Recovery || j.Snapshot.Identity != j.Before ||
		j.Snapshot.Version != 1 || !supportedSnapshotSchema(j.Snapshot.Schema) || !safefs.ValidID(j.Snapshot.ID, "n") ||
		j.Snapshot.ConfigHash != protocol.Hash(c) || !safefs.DigestPattern.MatchString(j.Snapshot.DatabaseHash) ||
		j.Snapshot.DatabaseBytes <= 0 || j.Snapshot.DatabaseBytes > c.MaxDatabaseBytes || j.Snapshot.CreatedAt.IsZero() {
		return fmt.Errorf("invalid recovery authority or snapshot")
	}
	switch j.Phase {
	case "prepared", "epoch_published":
		if j.CandidateHash != "" {
			return fmt.Errorf("premature recovery completion evidence")
		}
	case "quarantined":
		if !safefs.DigestPattern.MatchString(j.CandidateHash) {
			return fmt.Errorf("missing recovery candidate digest")
		}
	default:
		return fmt.Errorf("unknown recovery phase")
	}
	return nil
}

func recoveryBoundary(fault func(string) error, phase string) error {
	if fault != nil {
		return fault(phase)
	}
	return nil
}

func prepareSnapshotRecovery(ctx context.Context, statePath, snapshotPath string, c Config, fault func(string) error) (j snapshotRecovery, err error) {
	return preparePinnedSnapshotRecovery(ctx, statePath, snapshotPath, c, nil, fault)
}

func preparePinnedSnapshotRecovery(ctx context.Context, statePath, snapshotPath string, c Config, expected *SnapshotManifest, fault func(string) error) (j snapshotRecovery, err error) {
	m, source, err := readSnapshot(ctx, snapshotPath, c)
	if err != nil {
		return j, err
	}
	defer source.Close()
	if expected != nil && m != *expected {
		return j, fmt.Errorf("reviewed recovery snapshot changed")
	}
	// Open takes the stable instance lease and upgrades the readable schema.
	// It refuses a running worker; this primitive never stops a live service.
	s, err := Open(statePath, c, false)
	if err != nil {
		return j, err
	}
	defer s.Close()
	if s.Identity() != m.Identity {
		return j, fmt.Errorf("snapshot belongs to another worker or recovery epoch")
	}
	if err = s.dir.Capacity(2*c.MaxDatabaseBytes + c.MinFreeBytes); err != nil {
		return j, err
	}
	j = snapshotRecovery{Version: 1, Before: s.Identity(), After: s.Identity(), Snapshot: m, Phase: "prepared"}
	if j.Operation, err = safefs.NewID("n"); err != nil {
		return j, err
	}
	if j.After.Recovery, err = safefs.NewID("n"); err != nil {
		return j, err
	}
	if err = s.dir.Mkdir("recovery/" + j.Operation); err != nil {
		return j, err
	}
	stage, err := s.dir.Sub("recovery/" + j.Operation)
	if err != nil {
		return j, err
	}
	defer stage.Close()
	if err = copySnapshotFile(source, "state.db", stage, "source.db", m); err != nil {
		return j, err
	}
	if err = copySnapshotFile(stage, "source.db", stage, "candidate.db", m); err != nil {
		return j, err
	}
	if err = s.dir.WriteJSON("recovery.json", j, false); err != nil {
		return j, err
	}
	if err = recoveryBoundary(fault, "prepared"); err != nil {
		return j, err
	}
	err = finishSnapshotRecovery(ctx, s.dir, c, &j, fault)
	return
}

func resumeSnapshotRecovery(ctx context.Context, path string, c Config, fault func(string) error) (j snapshotRecovery, err error) {
	if err = c.Validate(); err != nil {
		return
	}
	dir, err := privateDirectory(path)
	if err != nil {
		return j, err
	}
	defer dir.Close()
	lease, err := dir.Lock("worker.lock", true, false)
	if err != nil {
		return j, err
	}
	defer lease.Close()
	if err = dir.ReadJSON("recovery.json", &j); err != nil {
		return j, err
	}
	err = finishSnapshotRecovery(ctx, dir, c, &j, fault)
	return
}

func finishSnapshotRecovery(ctx context.Context, dir *safefs.Directory, c Config, j *snapshotRecovery, fault func(string) error) error {
	if err := j.validate(c); err != nil {
		return err
	}
	var identity protocol.Identity
	if err := dir.ReadJSON("identity.json", &identity); err != nil {
		return err
	}
	if identity != j.Before && identity != j.After {
		return fmt.Errorf("installation identity changed outside recovery")
	}
	if j.Phase != "prepared" && identity != j.After {
		return fmt.Errorf("recovery epoch publication was rolled back")
	}
	stage, err := dir.Sub("recovery/" + j.Operation)
	if err != nil {
		return err
	}
	defer stage.Close()
	source, err := snapshotFile(stage, "source.db", c.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	hash, err := safefs.DigestFile(source)
	source.Close()
	if err != nil {
		return err
	}
	if hash != j.Snapshot.DatabaseHash {
		return fmt.Errorf("retained recovery source changed")
	}
	if j.Phase == "quarantined" {
		candidate, e := snapshotFile(stage, "candidate.db", c.MaxDatabaseBytes)
		if e != nil {
			return e
		}
		hash, e = safefs.DigestFile(candidate)
		candidate.Close()
		if e != nil {
			return e
		}
		if hash != j.CandidateHash {
			return fmt.Errorf("quarantined candidate changed")
		}
		return nil
	}
	if err = dir.WriteJSON("identity.json", j.After, true); err != nil {
		return err
	}
	if err = recoveryBoundary(fault, "identity"); err != nil {
		return err
	}
	j.Phase = "epoch_published"
	if err = dir.WriteJSON("recovery.json", j, true); err != nil {
		return err
	}
	if err = recoveryBoundary(fault, "epoch_published"); err != nil {
		return err
	}
	if err = quarantineSnapshot(ctx, stage, *j, c, fault); err != nil {
		return err
	}
	candidate, err := snapshotFile(stage, "candidate.db", c.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	j.CandidateHash, err = safefs.DigestFile(candidate)
	candidate.Close()
	if err != nil {
		return err
	}
	j.Phase = "quarantined"
	if err = dir.WriteJSON("recovery.json", j, true); err != nil {
		return err
	}
	return recoveryBoundary(fault, "quarantined")
}

func quarantineSnapshot(ctx context.Context, dir *safefs.Directory, j snapshotRecovery, c Config, fault func(string) error) (err error) {
	file, err := snapshotFile(dir, "candidate.db", c.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	file.Close()
	db, err := snapshotDB(dir, "candidate.db", false)
	if err != nil {
		return err
	}
	defer db.Close()
	// Opening the writable candidate also recovers an interrupted SQLite
	// transaction. The canonical live DB is never opened on this resume path.
	var check, body string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return fmt.Errorf("recovery candidate integrity unavailable")
	}
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != j.Snapshot.Schema {
		return fmt.Errorf("recovery candidate schema mismatch")
	}
	err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='recovery'").Scan(&body)
	if err == nil {
		var binding recoveryBinding
		if err = protocol.Decode([]byte(body), &binding, 4096); err != nil || binding != j.binding() {
			return fmt.Errorf("candidate recovery binding mismatch")
		}
		if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&body); err != nil {
			return err
		}
		var id protocol.Identity
		if err = protocol.Decode([]byte(body), &id, 4096); err != nil || id != j.After {
			return fmt.Errorf("candidate recovery identity mismatch")
		}
		return nil // Transaction was committed before a lost journal acknowledgement.
	}
	if err != sql.ErrNoRows {
		return err
	}
	file, err = snapshotFile(dir, "candidate.db", c.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	hash, err := safefs.DigestFile(file)
	file.Close()
	if err != nil {
		return err
	}
	if hash != j.Snapshot.DatabaseHash {
		return fmt.Errorf("unbound recovery candidate changed")
	}
	if err = verifySnapshotDB(ctx, db, j.Snapshot, c); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	identity, _ := json.Marshal(j.After)
	binding, _ := json.Marshal(j.binding())
	if _, err = tx.ExecContext(ctx, "UPDATE metadata SET value=? WHERE key='identity'", string(identity)); err != nil {
		return err
	}
	if err = recoveryBoundary(fault, "binding"); err != nil {
		return err
	}
	if err = archiveAdministrativeDecisions(ctx, tx, j.Before); err != nil {
		return err
	}
	// Retain old outcomes and cursors as explicitly old-epoch audit data. No
	// old receipt is relabeled as a new-epoch delivery acknowledgement.
	_, err = tx.ExecContext(ctx, `
CREATE TABLE recovery_messages AS SELECT * FROM messages;
CREATE TABLE recovery_receipts AS SELECT * FROM receipts;
CREATE TABLE recovery_producers AS SELECT * FROM producers;
CREATE TABLE recovery_commands AS SELECT * FROM commands;
UPDATE messages SET state='ambiguous',attempt='' WHERE state NOT IN ('provider_accepted','cancelled','superseded','expired');
DELETE FROM receipts;
UPDATE producers SET acked=0,listed=0;
UPDATE incidents SET attempt='';
UPDATE status SET session='';
UPDATE commands SET state=CASE WHEN state IN ('succeeded','failed','rejected','expired') THEN state ELSE 'outcome_unknown' END,grant='',token_hash=NULL,confirmed=0,preparation='';
UPDATE command_replies SET state='recovery_quarantined',attempt='' WHERE state<>'provider_accepted';
UPDATE command_reply_attempts SET state='ambiguous' WHERE state='sending';
UPDATE provider_inbox SET state='recovery_quarantined';
`)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES('recovery',?)", string(binding)); err != nil {
		return err
	}
	if err = recoveryBoundary(fault, "classified"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = recoveryBoundary(fault, "committed"); err != nil {
		return err
	}
	if err = db.Close(); err != nil {
		return err
	}
	return dir.File().Sync()
}
