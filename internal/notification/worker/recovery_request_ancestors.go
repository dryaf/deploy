package worker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// A provider may have compacted its inbox after the previous recovery. Its
// independently sealed request fences still prevent those inputs becoming new
// commands. Carry the union from both the restored snapshot and latest original.
func (r *recoveryReconciler) ancestorRequestFences(tx *sql.Tx, byKey map[string]recoveredRequest) ([]string, error) {
	if r.journal.Version != 2 {
		return nil, nil
	}
	ctx := context.Background()
	var evidence []string
	for _, name := range []string{"source.db", originalArchiveFile} {
		var db *sql.DB
		var err error
		if name == originalArchiveFile {
			db, err = r.originalDB(ctx, tx)
		} else {
			err = verifyRetainedDatabase(ctx, r.stage, name, r.journal.Snapshot.DatabaseHash, r.journal.Snapshot.DatabaseBytes, r.config.MaxDatabaseBytes)
			if err == nil {
				db, err = immutableOriginalDB(r.stage, name)
			}
		}
		if err != nil {
			return nil, err
		}
		hashes, err := readAncestorRequestFences(db, byKey)
		closeErr := db.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		evidence = append(evidence, protocol.Hash([]any{name, hashes}))
	}
	return evidence, nil
}

func readAncestorRequestFences(db *sql.DB, byKey map[string]recoveredRequest) ([]string, error) {
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	exists, err := requestRecoverySchema(tx)
	if err != nil || !exists {
		return nil, err
	}
	if ready, err := requestRecoveryAdmissionReady(tx); err != nil {
		return nil, err
	} else if !ready {
		return nil, fmt.Errorf("ancestor request seal is incomplete")
	}
	rows, err := tx.Query("SELECT key,hash,body,target_id FROM reconciliation_request_fences ORDER BY key LIMIT 65537")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var key, hash, raw, target string
		if err = rows.Scan(&key, &hash, &raw, &target); err != nil {
			return nil, err
		}
		var v recoveredRequest
		if protocol.Decode([]byte(raw), &v, 4096) != nil || !safefs.DigestPattern.MatchString(key) || v.Key != key || protocol.Hash(v) != hash || v.TargetID != target {
			return nil, fmt.Errorf("ancestor request fence changed")
		}
		if current, ok := byKey[key]; ok {
			if v.InputHash != "" && current.InputHash != "" && v.InputHash != current.InputHash || v.TargetID != "" && current.TargetID != "" && (v.TargetID != current.TargetID || v.TargetHash != current.TargetHash) {
				return nil, fmt.Errorf("ancestor request mapping conflicts with recovered history")
			}
			if current.InputHash != "" {
				v.InputHash = current.InputHash
			}
			if current.TargetID != "" {
				v.TargetID, v.TargetHash = current.TargetID, current.TargetHash
			}
		}
		byKey[key] = v
		hashes = append(hashes, hash)
		if len(byKey) > 65536 || len(hashes) > 65536 {
			return nil, fmt.Errorf("ancestor request inventory exceeds budget")
		}
	}
	return hashes, rows.Err()
}
