package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// WithPublishedSignalCursor binds offline relay installation to the exact
// published gateway cursor. The worker lease spans the external installation;
// no SQLite transaction is held while action runs. It grants no intake authority.
func WithPublishedSignalCursor(ctx context.Context, path string, c Config, commit, account string, action func(interaction.SignalRecoveryCursor) error) error {
	if action == nil {
		return fmt.Errorf("Signal publication action missing")
	}
	return WithPublishedSignalCursorChecked(ctx, path, c, commit, account, func(p interaction.SignalRecoveryCursor, _ func() error) error { return action(p) })
}

// WithPublishedSignalCursorChecked additionally lets a service coordinator
// recheck publication authority between its durable steps without reacquiring
// the worker lease. The callback must not retain check after returning.
func WithPublishedSignalCursorChecked(ctx context.Context, path string, c Config, commit, account string, action func(interaction.SignalRecoveryCursor, func() error) error) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if action == nil || c.Interaction == nil {
		return fmt.Errorf("Signal publication installation dependencies missing")
	}
	enrolled := false
	for _, d := range c.Interaction.Destinations {
		if d.Provider == "signal" && d.Account == account {
			enrolled = true
		}
	}
	if !enrolled {
		return fmt.Errorf("Signal publication account not enrolled")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	root, err := privateDirectory(path)
	if err != nil {
		return err
	}
	defer root.Close()
	lease, err := root.Lock("worker.lock", true, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	publication, proof, err := verifyCompletedPublication(ctx, root, lease, c, commit)
	if err != nil {
		return err
	}
	stage, err := retainedDirectory(root, "recovery", publication.Image.Operation)
	if err != nil {
		return err
	}
	defer stage.Close()
	checkpoint, err := retainedDirectory(stage, "checkpoints", publication.Image.ReviewHash)
	if err != nil {
		return err
	}
	defer checkpoint.Close()
	db, err := immutableOriginalDB(checkpoint, publicationDatabase)
	if err != nil {
		return err
	}
	cursor, err := readPublishedSignalCursor(ctx, db, account, proof.After)
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	liveFile, err := snapshotFile(root, "state.db", c.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	defer liveFile.Close()
	check := func() error {
		currentRoot, e := privateDirectory(path)
		if e != nil {
			return e
		}
		e = sameRetainedDirectory(root, currentRoot)
		currentRoot.Close()
		if e != nil {
			return e
		}
		// Keep path and lock identity pinned across external service work, even
		// if replacement files contain byte-for-byte copies of valid evidence.
		for name, held := range map[string]*os.File{"worker.lock": lease.File(), "state.db": liveFile} {
			f, e := root.Open(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if e != nil {
				return e
			}
			actual, e := f.Stat()
			f.Close()
			if e != nil {
				return e
			}
			original, e := held.Stat()
			if e != nil {
				return e
			}
			if !os.SameFile(actual, original) {
				return fmt.Errorf("Signal gateway %s replaced", name)
			}
		}
		current, verified, e := verifyCompletedPublication(ctx, root, lease, c, commit)
		if e != nil {
			return e
		}
		if current != publication || verified != proof {
			return fmt.Errorf("gateway publication changed during Signal installation")
		}
		if e = verifyRecoveryRuntimeFiles(root, c); e != nil {
			return e
		}
		live, e := snapshotDB(root, "state.db", true)
		if e != nil {
			return e
		}
		defer live.Close()
		if e = verifyRecoveryRuntimeSQL(ctx, live, proof); e != nil {
			return e
		}
		actual, e := readPublishedSignalCursor(ctx, live, account, proof.After)
		if e != nil {
			return e
		}
		if protocol.Hash(actual) != protocol.Hash(cursor) {
			return fmt.Errorf("live Signal cursor differs from published evidence")
		}
		return ctx.Err()
	}
	if err = check(); err != nil {
		return err
	}
	if err = action(cursor, check); err != nil {
		return err
	}
	return check()
}
func readPublishedSignalCursor(ctx context.Context, db *sql.DB, account string, identity protocol.Identity) (p interaction.SignalRecoveryCursor, err error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	var hash, raw string
	if err = tx.QueryRowContext(ctx, "SELECT hash,body FROM reconciliation_signal_cursors WHERE account=?", account).Scan(&hash, &raw); err != nil {
		return
	}
	if err = protocol.Decode([]byte(raw), &p, 16384); err != nil {
		return
	}
	if err = p.Validate(); err != nil {
		return
	}
	if p.Handoff.Summary.Challenge.Gateway != identity || p.Handoff.Target.Account != account || protocol.Hash(p) != hash {
		return p, fmt.Errorf("published Signal cursor binding changed")
	}
	var operational interaction.SpoolCursor
	if err = tx.QueryRowContext(ctx, "SELECT cursor FROM signal_imports WHERE account=?", account).Scan(&raw); err != nil {
		return p, err
	}
	if err = protocol.Decode([]byte(raw), &operational, 4096); err != nil {
		return p, err
	}
	if operational != p.Handoff.Target {
		return p, fmt.Errorf("published Signal operational cursor changed")
	}
	if err = tx.QueryRowContext(ctx, "SELECT body FROM reconciliation_request_seal WHERE singleton=1").Scan(&raw); err != nil {
		return
	}
	var requests RecoveryRequestSummary
	if err = protocol.Decode([]byte(raw), &requests, 8192); err != nil {
		return
	}
	if protocol.Hash(requests) != p.Handoff.RequestsHash {
		return p, fmt.Errorf("published Signal request seal changed")
	}
	ready, err := requestRecoveryAdmissionReady(tx)
	if err != nil {
		return p, err
	}
	if !ready {
		return p, protocol.ErrRecoveryIncomplete
	}
	return
}
