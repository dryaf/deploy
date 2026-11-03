package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func (r *recoveryReconciler) buildPublicationImage(ctx context.Context, d *safefs.Directory, anchor recoveryPublicationAnchor) error {
	if err := originalSidecars(d, publicationDatabase, true); err != nil {
		return err
	}
	if err := resetRecoveryCopy(d, publicationCopy, r.config.MaxDatabaseBytes); err != nil {
		return err
	}
	if err := d.Capacity(r.config.MaxDatabaseBytes + r.config.MinFreeBytes); err != nil {
		return err
	}
	var cp RecoveryCheckpoint
	if err := readCheckpointJSON(d, "checkpoint.json", &cp); err != nil {
		return err
	}
	if cp.CheckpointHash != anchor.Proof.CheckpointHash || cp.DatabaseHash != anchor.Proof.ProjectionDatabaseHash || cp.DatabaseBytes <= 0 || cp.DatabaseBytes > r.config.MaxDatabaseBytes {
		return fmt.Errorf("publication source checkpoint changed")
	}
	if err := copySnapshotFile(d, checkpointDatabase, d, publicationCopy, SnapshotManifest{DatabaseHash: cp.DatabaseHash, DatabaseBytes: cp.DatabaseBytes}); err != nil {
		return err
	}
	if err := recoveryBoundary(r.fault, "publication_copied"); err != nil {
		return err
	}
	// Revalidate the owned copy before SQLite opens it for writing, including
	// private-file/link checks and the exact source bytes. A replaced temporary
	// must not redirect the anchor write into retained or canonical evidence.
	if err := verifyRetainedDatabase(ctx, d, publicationCopy, cp.DatabaseHash, cp.DatabaseBytes, r.config.MaxDatabaseBytes); err != nil {
		return err
	}
	if err := addPublicationAnchor(ctx, d, anchor, r.config.MaxDatabaseBytes, r.fault); err != nil {
		return err
	}
	f, err := snapshotFile(d, publicationCopy, r.config.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return err
	}
	return d.File().Sync()
}

// Journaling is disabled only for this intent-owned disposable copy. A killed
// unsealed copy is discarded; neither the source nor canonical DB is opened rw.
func addPublicationAnchor(ctx context.Context, d *safefs.Directory, anchor recoveryPublicationAnchor, limit int64, fault func(string) error) (err error) {
	u := url.URL{Scheme: "file", Path: fmt.Sprintf("/proc/self/fd/%d/%s", d.File().Fd(), publicationCopy)}
	u.RawQuery = url.Values{"mode": {"rw"}, "_pragma": {"journal_mode(OFF)", "synchronous(OFF)"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { err = errors.Join(err, db.Close()) }()
	var pageSize, pages int64
	if err = db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return err
	}
	if pageSize < 512 || pageSize > 65536 || limit < pageSize {
		return fmt.Errorf("invalid publication image page budget")
	}
	if err = db.QueryRowContext(ctx, fmt.Sprintf("PRAGMA max_page_count=%d", limit/pageSize)).Scan(&pages); err != nil {
		return err
	}
	if pages > limit/pageSize {
		return fmt.Errorf("publication image exceeds database budget")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE key=?", publicationAnchorKey).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("existing publication anchor requires ancestor reconciliation")
	}
	raw, err := json.Marshal(anchor)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?)", publicationAnchorKey, string(raw)); err != nil {
		return err
	}
	if err = recoveryBoundary(fault, "publication_anchor_transaction"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return recoveryBoundary(fault, "publication_anchor_commit")
}

// Every schema/header/row and implicit rowid must match the reviewed projection
// after excluding only the exact anchor row. In particular NULL metadata keys
// are not accidentally omitted from this comparison.
func inspectPublicationImage(ctx context.Context, d *safefs.Directory, name string, anchor recoveryPublicationAnchor, review RecoveryStatus, limit int64) (out RecoveryPublicationImage, err error) {
	if name == publicationDatabase {
		if err = absentOriginalFile(d, publicationCopy); err != nil {
			return
		}
	}
	if err = originalSidecars(d, publicationCopy, true); err != nil {
		return
	}
	if err = originalSidecars(d, name, true); err != nil {
		return
	}
	f, err := snapshotFile(d, name, limit)
	if err != nil {
		return out, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return out, err
	}
	before, err := boundedEvidenceDigest(f, info.Size())
	f.Close()
	if err != nil {
		return out, err
	}
	db, err := immutableOriginalDB(d, name)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var integrity, raw string
	if err = tx.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return out, err
	}
	if integrity != "ok" {
		return out, fmt.Errorf("publication image integrity unavailable")
	}
	if err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", publicationAnchorKey).Scan(&raw); err != nil {
		return out, err
	}
	var stored recoveryPublicationAnchor
	if err = protocol.Decode([]byte(raw), &stored, safefs.MaxRecordBytes); err != nil {
		return out, err
	}
	if stored != anchor || anchor.Version != 1 || anchor.Mode != "fenced" || anchor.Proof.ReviewHash != review.ReviewHash {
		return out, fmt.Errorf("publication image anchor changed")
	}
	projection, err := recoveryProjectionHashWithoutMetadata(ctx, tx, limit, publicationAnchorKey)
	if err != nil {
		return out, err
	}
	if projection != review.ProjectionHash {
		return out, fmt.Errorf("publication image changed reviewed data")
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	if err = verifyRetainedDatabase(ctx, d, name, before, info.Size(), limit); err != nil {
		return out, err
	}
	out = RecoveryPublicationImage{Version: 1, Operation: anchor.Proof.Operation, ReviewHash: review.ReviewHash,
		CheckpointHash: anchor.Proof.CheckpointHash, ProofHash: anchor.Proof.ProofHash, AnchorHash: protocol.Hash(anchor), DatabaseHash: before, DatabaseBytes: info.Size()}
	out.ImageHash = protocol.Hash(out)
	return out, nil
}
