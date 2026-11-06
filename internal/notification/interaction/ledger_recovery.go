package interaction

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
)

type ledgerRecovery struct {
	Version   int
	Challenge protocol.RecoveryChallenge
	Manifest  CommandRecoveryManifest
	Bytes     int
}
type ledgerIdentityPin struct {
	Producer          protocol.Producer  `json:"producer"`
	Identity          protocol.Identity  `json:"identity"`
	RecoveryOperation string             `json:"recovery_operation,omitempty"`
	RecoveryCommit    string             `json:"recovery_commit,omitempty"`
	Ancestor          string             `json:"ancestor,omitempty"`
	NextOperation     string             `json:"next_operation,omitempty"`
	NextIdentity      *protocol.Identity `json:"next_identity,omitempty"`
}
type CommandRecoveryPage struct {
	Records []CommandRecoveryRecord
	Next    uint64
	Done    bool
}

// Presence, even of a damaged/empty recovery table, fences every already-open
// ledger handle. Ordinary opens do not delete it or replace the identity pin.
func (l *Ledger) requireCommandLedgerActive(tx *sql.Tx) error {
	var raw string
	if err := tx.QueryRow("SELECT body FROM interactive_identity WHERE singleton=1").Scan(&raw); err != nil {
		return err
	}
	var pin ledgerIdentityPin
	if err := protocol.Decode([]byte(raw), &pin, 4096); err != nil {
		return err
	}
	if pin.Producer != l.producer || pin.Identity != l.identity {
		return fmt.Errorf("command ledger identity changed")
	}
	if pin.Ancestor != "" || pin.NextOperation != "" || pin.RecoveryCommit != "" {
		return fmt.Errorf("command ledger recovery is fenced")
	}
	if pin.RecoveryOperation != "" {
		return fmt.Errorf("command ledger recovery is fenced")
	}
	var count int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE name LIKE 'interactive_recovery%'").Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("command ledger recovery is fenced")
	}
	return nil
}

func (l *Ledger) CaptureRecovery(ctx context.Context, ch protocol.RecoveryChallenge) (CommandRecoveryManifest, error) {
	boot, uptime, err := process.HostClock()
	if err != nil {
		return CommandRecoveryManifest{}, err
	}
	return l.captureRecovery(ctx, ch, time.Now().UTC(), boot, uptime, nil)
}

func (l *Ledger) captureRecovery(ctx context.Context, ch protocol.RecoveryChallenge, now time.Time, boot string, uptime float64, fault func(string) error) (manifest CommandRecoveryManifest, err error) {
	if err = ValidateCommandRecoveryChallenge(ch, l.identity, l.producer, now, boot, uptime); err != nil {
		return
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if err = l.checkRecoveryPin(tx); err != nil {
		return
	}
	pinBefore, e := readLedgerPin(tx)
	if e != nil {
		return manifest, e
	}
	if pinBefore.NextOperation != "" && (pinBefore.NextOperation != ch.Operation || pinBefore.NextIdentity == nil || *pinBefore.NextIdentity != ch.Identity) {
		return manifest, fmt.Errorf("next command recovery pins changed")
	}
	if intent, _, e := readLedgerCommandCommit(tx); e != nil {
		return manifest, e
	} else if intent != nil {
		return manifest, fmt.Errorf("command recovery capture is sealed for commit")
	}
	var exists int
	if err = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='interactive_recovery'").Scan(&exists); err != nil {
		return
	}
	if exists != 0 {
		var saved ledgerRecovery
		if saved, err = l.readRecovery(tx); err != nil {
			return
		}
		old := saved.Challenge
		if old.Operation != ch.Operation || old.Identity != ch.Identity || old.Producer != ch.Producer || ch.IssuedAt.Before(old.IssuedAt) ||
			(ch.Nonce == old.Nonce && protocol.Hash(ch) != protocol.Hash(old)) {
			return manifest, fmt.Errorf("command recovery session changed")
		}
		saved.Challenge = ch
		body, _ := json.Marshal(saved)
		if _, err = tx.Exec("UPDATE interactive_recovery SET body=? WHERE singleton=1", string(body)); err != nil {
			return
		}
		manifest = saved.Manifest
		err = tx.Commit()
		return
	}
	var unowned int
	if err = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='interactive_recovery_records'").Scan(&unowned); err != nil {
		return
	}
	if unowned != 0 {
		return manifest, fmt.Errorf("unowned command recovery evidence")
	}
	if _, err = tx.Exec(`CREATE TABLE interactive_recovery(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL);
CREATE TABLE interactive_recovery_records(ordinal INTEGER PRIMARY KEY,id TEXT NOT NULL UNIQUE,hash TEXT NOT NULL,body TEXT NOT NULL);`); err != nil {
		return
	}
	saved := ledgerRecovery{Version: 1, Challenge: ch, Manifest: CommandRecoveryManifest{Identity: l.identity, Producer: l.producer}}
	rows, err := tx.Query("SELECT id,request_key,hash,body,grant_id,fenced,revision,phase,state FROM interactive_inbox ORDER BY id")
	if err != nil {
		return
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var id, key, hash, body string
		var v CommandRecoveryRecord
		if err = rows.Scan(&id, &key, &hash, &body, &v.GrantID, &v.Fenced, &v.Revision, &v.Phase, &v.State); err != nil {
			return
		}
		if err = protocol.Decode([]byte(body), &v.Command, protocol.MaxRequest); err != nil {
			return
		}
		if v.Command.ID != id || v.Command.RequestKey != key || hash != protocol.Hash(v.Command) {
			return manifest, fmt.Errorf("retained command identity damaged")
		}
		results, e := tx.Query("SELECT revision,body,receipt,acked FROM interactive_results WHERE id=? ORDER BY revision", id)
		if e != nil {
			return manifest, e
		}
		for results.Next() {
			var rev uint64
			var raw, receipt string
			var r RecoveryResult
			if err = results.Scan(&rev, &raw, &receipt, &r.Acknowledged); err != nil {
				break
			}
			if err = protocol.Decode([]byte(raw), &r.Result, protocol.MaxRequest); err != nil {
				break
			}
			if rev != r.Result.Revision {
				err = fmt.Errorf("retained result revision damaged")
				break
			}
			if receipt != "" {
				r.Receipt = &ResultReceipt{}
				if err = protocol.Decode([]byte(receipt), r.Receipt, 8192); err != nil {
					break
				}
			}
			v.Results = append(v.Results, r)
			if len(v.Results) > 8 {
				err = fmt.Errorf("retained result history exceeds bound")
				break
			}
		}
		if err == nil {
			err = results.Err()
		}
		results.Close()
		if err != nil {
			return
		}
		var proof string
		err = tx.QueryRow("SELECT body FROM interactive_non_admission WHERE id=?", id).Scan(&proof)
		if err == nil {
			v.NonAdmission = &NonAdmissionProof{}
			err = protocol.Decode([]byte(proof), v.NonAdmission, 8192)
		}
		if err != nil && err != sql.ErrNoRows {
			return
		}
		err = nil
		if err = attachCommandAncestry(tx, pinBefore, &v); err != nil {
			return manifest, err
		}
		if err = v.Validate(saved.Manifest); err != nil {
			return
		}
		raw, _ := json.Marshal(v)
		saved.Bytes += len(raw)
		saved.Manifest.Commands++
		saved.Manifest.Results += uint64(len(v.Results))
		if saved.Bytes > 32<<20 || saved.Manifest.Commands > 65536 {
			return manifest, fmt.Errorf("command recovery export exceeds bounded capacity")
		}
		hash = protocol.Hash(v)
		hashes = append(hashes, hash)
		if _, err = tx.Exec("INSERT INTO interactive_recovery_records VALUES(?,?,?,?)", saved.Manifest.Commands, id, hash, string(raw)); err != nil {
			return
		}
	}
	if err = rows.Err(); err != nil {
		return
	}
	var orphans int
	if err = tx.QueryRow(`SELECT
(SELECT count(*) FROM interactive_results r WHERE NOT EXISTS(SELECT 1 FROM interactive_inbox i WHERE i.id=r.id))+
(SELECT count(*) FROM interactive_non_admission n WHERE NOT EXISTS(SELECT 1 FROM interactive_inbox i WHERE i.id=n.id))`).Scan(&orphans); err != nil {
		return
	}
	if orphans != 0 {
		return manifest, fmt.Errorf("orphaned command history requires reconciliation")
	}
	saved.Manifest.Digest = CommandRecoveryDigest(hashes)
	body, _ := json.Marshal(saved)
	if _, err = tx.Exec("INSERT INTO interactive_recovery VALUES(1,?)", string(body)); err != nil {
		return
	}
	// Older helpers compare the exact original pin JSON and therefore cannot
	// reopen a fenced ledger. The active identity itself is not changed.
	pin, _ := json.Marshal(ledgerIdentityPin{Producer: l.producer, Identity: l.identity, RecoveryOperation: ch.Operation, Ancestor: pinBefore.Ancestor})
	if _, err = tx.Exec("UPDATE interactive_identity SET body=? WHERE singleton=1", string(pin)); err != nil {
		return
	}
	if fault != nil {
		if err = fault("command_capture_transaction"); err != nil {
			return
		}
	}
	if err = tx.Commit(); err != nil {
		return
	}
	manifest = saved.Manifest
	if fault != nil {
		err = fault("command_capture_commit")
	}
	return
}

func (l *Ledger) checkRecoveryPin(tx *sql.Tx) error {
	var raw string
	if err := tx.QueryRow("SELECT body FROM interactive_identity WHERE singleton=1").Scan(&raw); err != nil {
		return err
	}
	var pin ledgerIdentityPin
	if err := protocol.Decode([]byte(raw), &pin, 4096); err != nil {
		return err
	}
	if _, err := readCommandAncestors(tx, pin); err != nil {
		return err
	}
	intent, proof, err := readLedgerCommandCommit(tx)
	if err != nil {
		return err
	}
	if proof != nil {
		original, target := proof.Summary.Manifest.Identity, proof.Summary.Challenge.Identity
		if pin.Producer != l.producer || pin.Identity != target || pin.RecoveryOperation != proof.Summary.Challenge.Operation || pin.RecoveryCommit != protocol.Hash(proof) || (l.identity != original && l.identity != target) || proof.Summary.Manifest.Producer != l.producer {
			return fmt.Errorf("command recovery committed pin changed")
		}
	} else {
		if pin.Producer != l.producer || pin.Identity != l.identity || pin.RecoveryCommit != "" {
			return fmt.Errorf("command ledger identity changed")
		}
		if intent != nil && (pin.Identity != intent.Manifest.Identity || pin.RecoveryOperation != intent.Challenge.Operation) {
			return fmt.Errorf("command recovery pending pin changed")
		}
	}
	if pin.RecoveryOperation != "" {
		var raw string
		var saved ledgerRecovery
		if err := tx.QueryRow("SELECT body FROM interactive_recovery WHERE singleton=1").Scan(&raw); err != nil {
			return err
		}
		if err := protocol.Decode([]byte(raw), &saved, 8192); err != nil {
			return err
		}
		if saved.Challenge.Operation != pin.RecoveryOperation {
			return fmt.Errorf("command recovery pin disagrees with session")
		}
	}
	return nil
}

func (l *Ledger) readRecovery(tx *sql.Tx) (saved ledgerRecovery, err error) {
	if err = l.checkRecoveryPin(tx); err != nil {
		return
	}
	var raw string
	if err = tx.QueryRow("SELECT body FROM interactive_recovery WHERE singleton=1").Scan(&raw); err != nil {
		return
	}
	if err = protocol.Decode([]byte(raw), &saved, 8192); err != nil {
		return
	}
	var pinBody string
	if err = tx.QueryRow("SELECT body FROM interactive_identity WHERE singleton=1").Scan(&pinBody); err != nil {
		return
	}
	var pin ledgerIdentityPin
	if err = protocol.Decode([]byte(pinBody), &pin, 4096); err != nil {
		return
	}
	if pin.RecoveryOperation != saved.Challenge.Operation {
		return saved, fmt.Errorf("command recovery pin is missing or changed")
	}
	var count uint64
	var bytes int
	if err = tx.QueryRow("SELECT count(*),COALESCE(sum(length(CAST(body AS BLOB))),0) FROM interactive_recovery_records").Scan(&count, &bytes); err != nil {
		return
	}
	origin := l.identity
	if _, proof, e := readLedgerCommandCommit(tx); e != nil {
		return saved, e
	} else if proof != nil {
		origin = proof.Summary.Manifest.Identity
		if protocol.Hash(saved.Challenge) != protocol.Hash(proof.Summary.Challenge) || saved.Manifest != proof.Summary.Manifest {
			return saved, fmt.Errorf("command recovery committed capture changed")
		}
	}
	if saved.Version != 1 || saved.Manifest.Identity != origin || saved.Manifest.Producer != l.producer ||
		count != saved.Manifest.Commands || bytes != saved.Bytes {
		err = fmt.Errorf("command recovery export incomplete")
	}
	return
}

func (l *Ledger) RecoveryPage(ctx context.Context, ch protocol.RecoveryChallenge, after uint64) (page CommandRecoveryPage, err error) {
	boot, uptime, err := process.HostClock()
	if err != nil {
		return
	}
	if err = ValidateCommandRecoveryChallenge(ch, l.identity, l.producer, time.Now().UTC(), boot, uptime); err != nil {
		return
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	saved, err := l.readRecovery(tx)
	if err != nil {
		return
	}
	if protocol.Hash(ch) != protocol.Hash(saved.Challenge) || after > saved.Manifest.Commands {
		return page, fmt.Errorf("command recovery page session mismatch")
	}
	rows, err := tx.Query("SELECT ordinal,hash,body FROM interactive_recovery_records WHERE ordinal>? ORDER BY ordinal LIMIT 128", after)
	if err != nil {
		return
	}
	defer rows.Close()
	page.Next = after
	size := 2
	for rows.Next() {
		var n uint64
		var hash, body string
		var record CommandRecoveryRecord
		if err = rows.Scan(&n, &hash, &body); err != nil {
			return
		}
		if err = protocol.Decode([]byte(body), &record, protocol.MaxRequest); err != nil {
			return
		}
		if n != page.Next+1 || hash != protocol.Hash(record) {
			return page, fmt.Errorf("command recovery page damaged")
		}
		if err = record.Validate(saved.Manifest); err != nil {
			return
		}
		extra := len(body)
		if len(page.Records) > 0 {
			extra++
		}
		if size+extra+4096 > protocol.MaxRequest {
			if len(page.Records) == 0 {
				return page, fmt.Errorf("command recovery record too large")
			}
			break
		}
		size += extra
		page.Records = append(page.Records, record)
		page.Next = n
	}
	if err = rows.Err(); err != nil {
		return
	}
	page.Done = page.Next == saved.Manifest.Commands
	if !page.Done && len(page.Records) == 0 {
		err = fmt.Errorf("command recovery page missing")
	}
	return
}
