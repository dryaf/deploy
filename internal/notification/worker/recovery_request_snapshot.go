package worker

import (
	"database/sql"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// Lazily open the immutable quarantine once per inventory pass. Only rows
// absent from declared relay ranges may use this independent gateway evidence.
type requestSnapshotEvidence struct {
	r  *recoveryReconciler
	db *sql.DB
}

func (s *requestSnapshotEvidence) close() {
	if s.db != nil {
		s.db.Close()
	}
}

func (s *requestSnapshotEvidence) missingSignal(tx *sql.Tx, account string, id int64, hash, body string) (string, error) {
	r := s.r
	session, err := readSignalRecovery(tx, account)
	if err != nil {
		return "", err
	}
	if err = r.validateSignalSummary(session); err != nil {
		return "", err
	}
	m := session.Summary.Manifest
	position := uint64(id)
	missing := id > 0 && ((m.BaselineHash != "" && position <= m.Highest && !m.RetainsPosition(position)) || (session.Summary.MissingRelayFrom > 0 && position >= session.Summary.MissingRelayFrom && position <= session.Summary.MissingRelayThrough))
	if !missing {
		return "", fmt.Errorf("Signal evidence missing outside declared recovery gap")
	}
	if s.db == nil {
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			f, e := r.stage.Open("candidate.db"+suffix, os.O_RDONLY|unix.O_NONBLOCK, 0)
			if e == nil {
				f.Close()
				return "", fmt.Errorf("immutable request evidence has a sidecar")
			}
			if !os.IsNotExist(e) {
				return "", e
			}
		}
		f, e := snapshotFile(r.stage, "candidate.db", r.config.MaxDatabaseBytes)
		if e != nil {
			return "", e
		}
		digest, e := safefs.DigestFile(f)
		f.Close()
		if e != nil {
			return "", e
		}
		if digest != r.journal.CandidateHash {
			return "", fmt.Errorf("immutable request evidence changed")
		}
		s.db, e = snapshotDB(r.stage, "candidate.db", true)
		if e != nil {
			return "", e
		}
	}
	var oldHash, oldBody, state string
	if err = s.db.QueryRow("SELECT hash,body,state FROM provider_inbox WHERE account=? AND id=?", account, id).Scan(&oldHash, &oldBody, &state); err != nil {
		return "", err
	}
	if oldHash != hash || oldBody != body || state != "recovery_quarantined" {
		return "", fmt.Errorf("missing Signal row differs from immutable gateway snapshot")
	}
	return protocol.Hash([]any{"missing-signal", r.journal.CandidateHash, protocol.Hash(session.Summary), account, id, hash, body}), nil
}
