package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// Reconciliation edits a separate projection, never the immutable quarantine
// candidate. It cannot activate generations, publish health or execute commands.
type recoveryReconciler struct {
	dir, stage  *safefs.Directory
	lease       *safefs.Lock
	db          *sql.DB
	journal     snapshotRecovery
	config      Config
	mu          sync.Mutex
	now         func() time.Time
	clock       func() (string, float64, error)
	fault       func(string) error
	publication *RecoveryPublicationCommit // Only the offline publisher may reopen a frozen reconciliation.
}

type reconciliationManifest struct {
	Version       int
	Binding       recoveryBinding
	ConfigHash    string
	CandidateHash string
	State         string
}

func openRecoveryReconciler(ctx context.Context, path string, c Config, fault func(string) error) (r *recoveryReconciler, err error) {
	if err = c.Validate(); err != nil {
		return
	}
	r = &recoveryReconciler{config: c, now: time.Now, clock: process.HostClock, fault: fault}
	defer func() {
		if err != nil {
			r.Close()
		}
	}()
	if r.dir, err = privateDirectory(path); err != nil {
		return
	}
	if r.lease, err = r.dir.Lock("worker.lock", true, false); err != nil {
		return
	}
	if err = r.checkPublicationGate(); err != nil {
		return
	}
	if err = r.dir.ReadJSON("recovery.json", &r.journal); err != nil {
		return
	}
	if r.journal.Phase != "quarantined" {
		err = fmt.Errorf("complete quarantine before reconciliation")
		return
	}
	if err = finishSnapshotRecovery(ctx, r.dir, c, &r.journal, nil); err != nil {
		return
	}
	if r.stage, err = r.dir.Sub("recovery/" + r.journal.Operation); err != nil {
		return
	}
	want := reconciliationManifest{1, r.journal.binding(), protocol.Hash(c), r.journal.CandidateHash, "initializing"}
	var manifest reconciliationManifest
	err = r.stage.ReadJSON("reconciliation.json", &manifest)
	if os.IsNotExist(err) {
		f, e := r.stage.Open("reconciled.db", os.O_RDONLY, 0)
		if e == nil {
			f.Close()
			err = fmt.Errorf("unowned reconciliation database")
			return
		}
		if !os.IsNotExist(e) {
			err = e
			return
		}
		manifest = want
		if err = r.stage.WriteJSON("reconciliation.json", manifest, false); err != nil {
			return
		}
	} else if err != nil {
		return
	}
	state := manifest.State
	manifest.State = "initializing"
	if manifest != want || (state != "initializing" && state != "ready") {
		err = fmt.Errorf("reconciliation ownership mismatch")
		return
	}
	manifest.State = state
	if err = recoveryBoundary(fault, "reconciliation_intent"); err != nil {
		return
	}
	f, e := snapshotFile(r.stage, "reconciled.db", c.MaxDatabaseBytes)
	if os.IsNotExist(e) && state == "initializing" {
		if err = r.stage.Capacity(c.MaxDatabaseBytes + c.MinFreeBytes); err != nil {
			return
		}
		candidate, e := snapshotFile(r.stage, "candidate.db", c.MaxDatabaseBytes)
		if e != nil {
			err = e
			return
		}
		info, e := candidate.Stat()
		candidate.Close()
		if e != nil {
			err = e
			return
		}
		id, e := safefs.NewID("n")
		if e != nil {
			err = e
			return
		}
		temp := "reconcile-" + id + ".db"
		m := SnapshotManifest{DatabaseBytes: info.Size(), DatabaseHash: r.journal.CandidateHash}
		if err = copySnapshotFile(r.stage, "candidate.db", r.stage, temp, m); err != nil {
			return
		}
		if err = r.stage.Rename(temp, r.stage, "reconciled.db"); err != nil {
			return
		}
	} else if e != nil {
		err = e
		return
	} else {
		f.Close()
	}
	if err = recoveryBoundary(fault, "reconciliation_copy"); err != nil {
		return
	}
	if r.db, err = snapshotDB(r.stage, "reconciled.db", false); err != nil {
		return
	}
	if err = r.initialize(ctx, want, state); err != nil {
		return
	}
	if err = r.reconcileAdministrativeAudit(ctx); err != nil {
		return
	}
	manifest.State = "ready"
	if err = r.stage.WriteJSON("reconciliation.json", manifest, true); err != nil {
		return
	}
	err = recoveryBoundary(fault, "reconciliation_ready")
	return
}

func (r *recoveryReconciler) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.db != nil {
		err = r.db.Close()
	}
	if r.lease != nil {
		r.lease.Close()
	}
	if r.stage != nil {
		r.stage.Close()
	}
	if r.dir != nil {
		r.dir.Close()
	}
	return err
}

func (r *recoveryReconciler) initialize(ctx context.Context, want reconciliationManifest, state string) error {
	var integrity, body string
	var version int
	if err := r.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("reconciliation integrity unavailable")
	}
	if err := r.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != r.journal.Snapshot.Schema {
		return fmt.Errorf("reconciliation schema mismatch")
	}
	var ledHistory ledJournalReader = r.db
	epochExists, err := ledEpochSchema(ctx, r.db)
	if err != nil {
		return err
	}
	var adoption *ledAdoptionEvidence
	if epochExists {
		adoption, err = r.readLEDAdoptionAuthority(ctx, r.db)
		if err != nil {
			return err
		}
	}
	_, epochPhase, err := r.readLEDEpoch(ctx, r.db, adoption)
	if err != nil {
		return err
	}
	if epochPhase == "committed" {
		ledHistory = ledEpochReader{r.db}
	}
	if version >= 7 && version < 9 {
		if err := checkLEDLegacyMapping(ctx, ledHistory); err != nil {
			return err
		}
	}
	if version >= 7 {
		if err := checkLEDLayout(ctx, ledHistory, r.config, r.journal.Before, false); err != nil {
			return err
		}
	}
	if version >= 8 {
		if err := checkLEDOperations(ctx, ledHistory, r.journal.Before); err != nil {
			return err
		}
	}
	if version >= 9 {
		if err := checkLEDMigrations(ctx, ledHistory, r.config, r.journal.Before, false); err != nil {
			return err
		}
	}
	if err := r.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&body); err != nil {
		return err
	}
	var identity protocol.Identity
	if err := protocol.Decode([]byte(body), &identity, 4096); err != nil || identity != r.journal.After {
		return fmt.Errorf("reconciliation epoch mismatch")
	}
	if err := r.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='recovery'").Scan(&body); err != nil {
		return err
	}
	var binding recoveryBinding
	if err := protocol.Decode([]byte(body), &binding, 4096); err != nil || binding != want.Binding {
		return fmt.Errorf("reconciliation recovery binding mismatch")
	}
	err = r.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='reconciliation'").Scan(&body)
	if err == nil {
		if body != protocol.Hash(want) {
			return fmt.Errorf("reconciliation manifest mismatch")
		}
		if err := r.checkLEDEvidenceHistory(ctx, r.db); err != nil {
			return err
		}
		if _, err := r.readLEDClears(ctx, r.db); err != nil {
			return err
		}
		if _, _, err := r.readLEDPreservation(ctx, r.db); err != nil {
			return err
		}
		_, err := r.readLEDAdoption(ctx, r.db)
		return err
	}
	if err != sql.ErrNoRows {
		return err
	}
	if state == "ready" {
		return fmt.Errorf("initialized reconciliation database lost its ledger")
	}
	f, err := snapshotFile(r.stage, "reconciled.db", r.config.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	hash, err := safefs.DigestFile(f)
	f.Close()
	if err != nil {
		return err
	}
	if hash != r.journal.CandidateHash {
		return fmt.Errorf("uninitialized reconciliation copy changed")
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE reconciliation_producers(producer TEXT PRIMARY KEY,challenge TEXT NOT NULL,manifest TEXT NOT NULL DEFAULT '',completion TEXT NOT NULL DEFAULT '');
CREATE TABLE reconciliation_records(producer TEXT NOT NULL,seq INTEGER NOT NULL,id TEXT NOT NULL,hash TEXT NOT NULL,evidence_hash TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(producer,seq),UNIQUE(producer,id));
CREATE TABLE reconciliation_receipts(position INTEGER PRIMARY KEY,producer TEXT NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL);
CREATE TABLE reconciliation_unknown(producer TEXT PRIMARY KEY,first INTEGER NOT NULL,last INTEGER NOT NULL,checkpoint TEXT NOT NULL);
`)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO metadata VALUES('reconciliation',?)", protocol.Hash(want))
		return err
	})
}

func (r *recoveryReconciler) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	return r.transactionWithValidation(ctx, fn, nil)
}

// Time-sensitive handoffs recheck their lease after the potentially large
// inventory validation and immediately before publishing the transaction.
func (r *recoveryReconciler) transactionWithValidation(ctx context.Context, fn func(*sql.Tx) error, validate func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.checkAuthority(); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = r.checkRequestSeal(tx); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	if err = recoveryBoundary(r.fault, "reconciliation_transaction"); err != nil {
		return err
	}
	if err = r.checkAuthority(); err != nil {
		return err
	}
	if err = r.checkRequestSeal(tx); err != nil {
		return err
	}
	if validate != nil {
		if err = validate(); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return recoveryBoundary(r.fault, "reconciliation_commit")
}

// Existing recovery handles must stop if the installation lease or epoch is
// replaced. Opening offline once does not grant authority for the handle's life.
func (r *recoveryReconciler) checkAuthority() error {
	if r.lease == nil || r.lease.File() == nil {
		return fmt.Errorf("reconciliation lease closed")
	}
	held, err := r.lease.File().Stat()
	if err != nil {
		return err
	}
	f, err := r.dir.Open("worker.lock", os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	current, err := f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	if !os.SameFile(held, current) {
		return fmt.Errorf("reconciliation lock changed")
	}
	if err := r.checkPublicationGate(); err != nil {
		return err
	}
	var j snapshotRecovery
	if err = r.dir.ReadJSON("recovery.json", &j); err != nil {
		return err
	}
	if protocol.Hash(j) != protocol.Hash(r.journal) {
		return fmt.Errorf("reconciliation recovery journal changed")
	}
	var identity protocol.Identity
	if err = r.dir.ReadJSON("identity.json", &identity); err != nil {
		return err
	}
	if identity != r.journal.After {
		return fmt.Errorf("reconciliation identity changed")
	}
	var manifest reconciliationManifest
	if err = r.stage.ReadJSON("reconciliation.json", &manifest); err != nil {
		return err
	}
	if manifest.Version != 1 || manifest.Binding != r.journal.binding() || manifest.ConfigHash != protocol.Hash(r.config) ||
		manifest.CandidateHash != r.journal.CandidateHash || (manifest.State != "ready" && manifest.State != "initializing") {
		return fmt.Errorf("reconciliation manifest changed")
	}
	return nil
}

func (r *recoveryReconciler) capacity(tx *sql.Tx, bytes int) error {
	file, err := snapshotFile(r.stage, "reconciled.db", r.config.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	file.Close()
	// Count logical pages, including pages still in the WAL. File size alone
	// undercounts committed history until a checkpoint.
	var pages, size int64
	if err = tx.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		return err
	}
	if err = tx.QueryRow("PRAGMA page_size").Scan(&size); err != nil {
		return err
	}
	if pages*size+int64(bytes)*3+(256<<10) > r.config.MaxDatabaseBytes {
		return fmt.Errorf("reconciliation database budget exhausted")
	}
	return r.stage.Capacity(r.config.MinFreeBytes + int64(bytes)*3 + (1 << 20))
}
