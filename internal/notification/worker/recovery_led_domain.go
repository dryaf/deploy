package worker

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Domain history is separate from the LED ledger: an obsolete LED row and a
// current ok observation are not proof that an older policy fault was resolved.
type recoveryLEDSubject struct {
	Subject protocol.Subject
	Latch   string
}
type recoveryLEDPolicyFault struct {
	Subject   string
	Revision  uint64
	Condition string
	Active    bool
}
type recoveryLEDDependency struct {
	Subject, Decision string
	Revision          uint64
}
type recoveryLEDDomain struct {
	Status       *protocol.Snapshot
	Subjects     []recoveryLEDSubject
	Faults       []recoveryLEDPolicyFault
	Dependencies []recoveryLEDDependency
	Support      []protocol.AcceptanceSupport
}
type recoveryLEDDomains struct {
	Snapshot, Latest recoveryLEDDomain
	Reconciled       *protocol.Snapshot
}

func (r *recoveryReconciler) collectLEDDomains(ctx context.Context, tx *sql.Tx, p protocol.Producer) (v recoveryLEDDomains, err error) {
	v.Snapshot, err = readLEDDomain(ctx, tx, p)
	if err != nil {
		return
	}
	var challenge, manifest, completion string
	err = tx.QueryRowContext(ctx, "SELECT challenge,manifest,completion FROM reconciliation_producers WHERE producer=?", p.ID).Scan(&challenge, &manifest, &completion)
	if err != nil && err != sql.ErrNoRows {
		return
	}
	if err == nil && completion != "" {
		var summary protocol.RecoverySummary
		var ch protocol.RecoveryChallenge
		var m protocol.RecoveryManifest
		if err = protocol.Decode([]byte(completion), &summary, protocol.MaxRequest+8192); err != nil {
			return
		}
		if err = protocol.Decode([]byte(challenge), &ch, 4096); err != nil {
			return
		}
		if err = protocol.Decode([]byte(manifest), &m, 4096); err != nil {
			return
		}
		if summary.Challenge != ch || summary.Manifest != m || ch.Producer != p || ch.Operation != r.journal.Operation || ch.Identity != r.journal.After || summary.DeliveryEnabled || summary.Snapshot.Producer != p {
			return v, fmt.Errorf("LED domain reconciliation summary mismatch")
		}
		if err = summary.Snapshot.Validate(); err != nil {
			return
		}
		v.Reconciled = &summary.Snapshot
	}
	err = nil
	db, err := r.originalDB(ctx, tx)
	if err != nil {
		return v, err
	}
	defer db.Close()
	v.Latest, err = readLEDDomain(ctx, db, p)
	return
}

func readLEDDomain(ctx context.Context, q ledJournalReader, p protocol.Producer) (v recoveryLEDDomain, err error) {
	var epoch string
	if err = q.QueryRowContext(ctx, "SELECT epoch FROM producers WHERE id=?", p.ID).Scan(&epoch); err != nil {
		return
	}
	if epoch != p.Epoch {
		return v, fmt.Errorf("LED domain producer epoch mismatch")
	}
	var raw, hash string
	var seq, basis uint64
	err = q.QueryRowContext(ctx, "SELECT seq,basis,hash,body FROM status WHERE producer=?", p.ID).Scan(&seq, &basis, &hash, &raw)
	if err != nil && err != sql.ErrNoRows {
		return
	}
	if err == nil {
		var snap protocol.Snapshot
		if err = protocol.Decode([]byte(raw), &snap, protocol.MaxRequest); err != nil {
			return
		}
		if err = snap.Validate(); err != nil {
			return
		}
		if snap.Producer != p || snap.Sequence != seq || snap.BasisRevision != basis || protocol.Hash(snap) != hash {
			return v, fmt.Errorf("LED domain status identity mismatch")
		}
		v.Status = &snap
	}
	err = nil
	// Every query is bounded independently. Overflow preserves the recovery fence.
	queries := []string{
		"SELECT id,body,latched FROM subjects WHERE producer=? ORDER BY id LIMIT 1025",
		"SELECT subject,revision,condition,active FROM policy_faults WHERE producer=? ORDER BY subject,revision LIMIT 1025",
		"SELECT subject,decision,revision FROM policy_dependencies WHERE producer=? ORDER BY subject,decision LIMIT 1025",
		"SELECT id,revision,hash,body FROM policy_support WHERE producer=? ORDER BY id LIMIT 1025",
	}
	for kind, query := range queries {
		rows, e := q.QueryContext(ctx, query, p.ID)
		if e != nil {
			return v, e
		}
		count := 0
		for rows.Next() {
			count++
			if count > 1024 {
				err = fmt.Errorf("LED domain history exceeds budget")
				break
			}
			switch kind {
			case 0:
				var id, body string
				var item recoveryLEDSubject
				if err = rows.Scan(&id, &body, &item.Latch); err == nil {
					err = protocol.Decode([]byte(body), &item.Subject, 8192)
				}
				if err == nil && (id != item.Subject.Key || !protocol.ValidToken(id) || !slices.Contains([]string{"ok", "warning", "critical", "unknown", "paused", "retired"}, item.Subject.Condition) || !slices.Contains([]string{"", "warning", "critical"}, item.Latch) || (item.Subject.FaultRevision != 0 && !protocol.Sequence(item.Subject.FaultRevision))) {
					err = fmt.Errorf("invalid LED domain subject")
				}
				v.Subjects = append(v.Subjects, item)
			case 1:
				var item recoveryLEDPolicyFault
				var active int
				err = rows.Scan(&item.Subject, &item.Revision, &item.Condition, &active)
				if err == nil && (!protocol.ValidToken(item.Subject) || !protocol.Sequence(item.Revision) || (item.Condition != "critical" && item.Condition != "warning") || (active != 0 && active != 1)) {
					err = fmt.Errorf("invalid LED domain fault")
				}
				item.Active = active == 1
				v.Faults = append(v.Faults, item)
			case 2:
				var item recoveryLEDDependency
				err = rows.Scan(&item.Subject, &item.Decision, &item.Revision)
				if err == nil && (!protocol.ValidToken(item.Subject) || !protocol.ValidToken(item.Decision) || !protocol.Sequence(item.Revision)) {
					err = fmt.Errorf("invalid LED policy dependency")
				}
				v.Dependencies = append(v.Dependencies, item)
			case 3:
				var id, h, body string
				var revision uint64
				var item protocol.AcceptanceSupport
				if err = rows.Scan(&id, &revision, &h, &body); err == nil {
					err = protocol.Decode([]byte(body), &item, 8192)
				}
				if err == nil {
					err = item.Validate()
				}
				if err == nil && (id != item.Decision || revision != item.Revision || h != protocol.Hash(item)) {
					err = fmt.Errorf("invalid LED policy support history")
				}
				v.Support = append(v.Support, item)
			}
			if err != nil {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return
		}
	}
	return v, nil
}
