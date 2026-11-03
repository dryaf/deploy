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

// A publication session owns the worker lease but never a transaction across
// provider I/O. The original cursor and live database remain read-only.
type publishedTelegramSession struct {
	root, stage   *safefs.Directory
	lease         *safefs.Lock
	borrowedLease bool
	db            *sql.DB
	liveFile      *os.File
	config        Config
	publication   RecoveryPublicationCommit
	proof         RecoveryCheckpointProof
	cursor        TelegramRecoveryCursor
}

func openPublishedTelegramSession(ctx context.Context, path string, c Config, commit, account string) (s *publishedTelegramSession, err error) {
	return openPublishedTelegramSessionWithLease(ctx, path, c, commit, account, nil)
}

func openPublishedTelegramSessionWithLease(ctx context.Context, path string, c Config, commit, account string, lease *safefs.Lock) (s *publishedTelegramSession, err error) {
	if err = c.Validate(); err != nil {
		return
	}
	enrolled := false
	if c.Interaction != nil {
		for _, d := range c.Interaction.Destinations {
			if d.Provider == "telegram" && d.Account == account {
				enrolled = true
			}
		}
	}
	if !enrolled {
		return nil, fmt.Errorf("Telegram handoff account unavailable")
	}
	s = &publishedTelegramSession{config: c}
	defer func() {
		if err != nil {
			s.close()
			s = nil
		}
	}()
	if s.root, err = privateDirectory(path); err != nil {
		return
	}
	if lease == nil {
		if s.lease, err = s.root.Lock("worker.lock", true, false); err != nil {
			return
		}
	} else {
		s.lease, s.borrowedLease = lease, true
	}
	if s.publication, s.proof, err = verifyCompletedPublication(ctx, s.root, s.lease, c, commit); err != nil {
		return
	}
	if s.stage, err = retainedDirectory(s.root, "recovery", s.publication.Image.Operation); err != nil {
		return
	}
	checkpoint, err := retainedDirectory(s.stage, "checkpoints", s.publication.Image.ReviewHash)
	if err != nil {
		return
	}
	defer checkpoint.Close()
	db, err := immutableOriginalDB(checkpoint, publicationDatabase)
	if err != nil {
		return
	}
	s.cursor, err = readPublishedTelegramCursor(ctx, db, account)
	closeErr := db.Close()
	if err != nil {
		return
	}
	if closeErr != nil {
		return s, closeErr
	}
	if s.cursor.Drain.Identity != s.proof.After || s.cursor.Drain.Operation != s.proof.Operation {
		return s, fmt.Errorf("published Telegram cursor authority changed")
	}
	if err = verifyRecoveryRuntimeFiles(s.root, c); err != nil {
		return
	}
	if s.liveFile, err = snapshotFile(s.root, "state.db", c.MaxDatabaseBytes); err != nil {
		return
	}
	if s.db, err = snapshotDB(s.root, "state.db", true); err != nil {
		return
	}
	err = s.check(ctx)
	return
}

func (s *publishedTelegramSession) close() {
	if s.db != nil {
		s.db.Close()
	}
	if s.liveFile != nil {
		s.liveFile.Close()
	}
	if s.stage != nil {
		s.stage.Close()
	}
	if s.lease != nil && !s.borrowedLease {
		s.lease.Close()
	}
	if s.root != nil {
		s.root.Close()
	}
}

func (s *publishedTelegramSession) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Detect replacement of both the path and the lease inode before another ACK.
	current, err := privateDirectory(s.root.File().Name())
	if err != nil {
		return err
	}
	err = sameRetainedDirectory(s.root, current)
	current.Close()
	if err != nil {
		return err
	}
	f, err := s.root.Open("worker.lock", os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	i, err := f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	held, err := s.lease.File().Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(i, held) {
		return fmt.Errorf("Telegram publication lease changed")
	}
	r := recoveryReconciler{dir: s.root, config: s.config, publication: &s.publication}
	if err = r.checkPublicationGate(); err != nil {
		return err
	}
	for _, name := range []string{publicationCommitSeal, publicationCommitComplete} {
		ok, e := publicationRecord(s.root, name, s.publication)
		if e != nil {
			return e
		}
		if !ok {
			return fmt.Errorf("Telegram publication lost completion")
		}
	}
	var identity protocol.Identity
	if err = readCheckpointJSON(s.root, "identity.json", &identity); err != nil {
		return err
	}
	if identity != s.proof.After {
		return fmt.Errorf("Telegram publication identity changed")
	}
	var journal snapshotRecovery
	if err = readCheckpointJSON(s.root, "recovery.json", &journal); err != nil {
		return err
	}
	if err = journal.validate(s.config); err != nil {
		return err
	}
	proof := s.proof
	if journal.Phase != "quarantined" || journal.CandidateHash != proof.CandidateDatabaseHash || journal.Snapshot.DatabaseHash != proof.SourceDatabaseHash || journal.Before != proof.Before || journal.After != proof.After || journal.Operation != proof.Operation || journal.Snapshot.ID != proof.Snapshot {
		return fmt.Errorf("Telegram recovery gate changed")
	}
	if err = verifyRecoveryRuntimeFiles(s.root, s.config); err != nil {
		return err
	}
	f, err = snapshotFile(s.root, "state.db", s.config.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	i, err = f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	held, err = s.liveFile.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(i, held) {
		return fmt.Errorf("Telegram canonical database replaced")
	}
	if err = verifyRecoveryRuntimeSQL(ctx, s.db, s.proof); err != nil {
		return err
	}
	cursor, err := readPublishedTelegramCursor(ctx, s.db, s.cursor.Drain.Account)
	if err != nil {
		return err
	}
	if cursor != s.cursor {
		return fmt.Errorf("live Telegram cursor differs from published evidence")
	}
	return nil
}

func (s *publishedTelegramSession) verify(ctx context.Context) error {
	p, proof, err := verifyCompletedPublication(ctx, s.root, s.lease, s.config, s.publication.CommitHash)
	if err != nil {
		return err
	}
	if p != s.publication || proof != s.proof {
		return fmt.Errorf("gateway publication changed during Telegram check")
	}
	return s.check(ctx)
}
