package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const signalCursorDatabase = "signal-cursor.db"
const signalCursorIntent = "signal-cursor-intent.json"
const signalCursorManifest = "signal-cursor.json"

// The candidate has its own deliberately unsupported runtime schema. Activation
// must install baseline-aware runtime state; merely renaming this file cannot
// enable intake. The original stream and historical ACK/export evidence remain
// unchanged; the source separately records this candidate's completion.
func stageSignalCursorCandidate(ctx context.Context, source *sql.DB, d *safefs.Directory, p interaction.SignalRecoveryHandoff, completed *interaction.SignalRecoveryCursor, check func() error, fault func(string) error) (proof interaction.SignalRecoveryCursor, err error) {
	if err = p.Validate(); err != nil {
		return
	}
	if p.Target.Position >= math.MaxInt64 {
		return proof, fmt.Errorf("Signal cursor sequence exhausted")
	}
	if err = check(); err != nil {
		return
	}
	var intent interaction.SignalRecoveryHandoff
	e := d.ReadJSON(signalCursorIntent, &intent)
	if os.IsNotExist(e) && completed == nil {
		for _, name := range []string{signalCursorDatabase, signalCursorManifest, signalCursorDatabase + "-journal", signalCursorDatabase + "-wal", signalCursorDatabase + "-shm"} {
			f, e := d.Open(name, os.O_RDONLY, 0)
			if e == nil {
				f.Close()
				return proof, fmt.Errorf("unowned Signal cursor artifact")
			}
			if !os.IsNotExist(e) {
				return proof, e
			}
		}
		if err = d.Capacity(41 << 20); err != nil {
			return
		}
		if err = d.WriteJSON(signalCursorIntent, p, false); err != nil {
			return
		}
	} else if e != nil {
		return proof, e
	} else if protocol.Hash(intent) != protocol.Hash(p) {
		return proof, fmt.Errorf("Signal cursor intent changed")
	}
	if err = inboxRestoreBoundary(fault, "signal_cursor_source_intent"); err != nil {
		return
	}
	e = d.ReadJSON(signalCursorManifest, &proof)
	if e == nil {
		if err = proof.Validate(); err != nil {
			return
		}
		if protocol.Hash(proof.Handoff) != protocol.Hash(p) || (completed != nil && protocol.Hash(*completed) != protocol.Hash(proof)) {
			return proof, fmt.Errorf("Signal cursor completion changed")
		}
		if err = verifySignalCursorFiles(ctx, d, proof); err != nil {
			return
		}
		return proof, check()
	}
	if !os.IsNotExist(e) || completed != nil {
		return proof, fmt.Errorf("Signal cursor completion missing: %w", e)
	}
	f, e := d.Open(signalCursorDatabase, os.O_RDWR|os.O_CREATE, 0600)
	if e != nil {
		return proof, e
	}
	info, e := f.Stat()
	if e == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0600 || info.Size() > 33<<20 {
			e = fmt.Errorf("unsafe Signal cursor database")
		}
	}
	if e == nil {
		e = f.Sync()
	}
	f.Close()
	if e != nil {
		return proof, e
	}
	if err = d.File().Sync(); err != nil {
		return
	}
	db, err := inboxArtifactDB(d, signalCursorDatabase, false)
	if err != nil {
		return
	}
	defer db.Close()
	var mode string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		return
	}
	if mode != "delete" {
		return proof, fmt.Errorf("Signal cursor candidate is not standalone")
	}
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return
	}
	if version == 0 {
		var count int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&count); err != nil {
			return
		}
		if count != 0 {
			return proof, fmt.Errorf("unbound Signal cursor database")
		}
		if err = buildSignalCursorCandidate(ctx, source, db, p, check, fault); err != nil {
			return
		}
	} else if version != 4 {
		return proof, fmt.Errorf("unknown Signal cursor candidate schema")
	}
	if err = verifySignalCursorCandidate(ctx, db, p); err != nil {
		return
	}
	if err = db.Close(); err != nil {
		return
	}
	f, err = inboxArtifactFile(d, signalCursorDatabase, 33<<20)
	if err != nil {
		return
	}
	info, err = f.Stat()
	if err == nil {
		err = f.Sync()
	}
	proof = interaction.SignalRecoveryCursor{Version: 1, Handoff: p}
	if err == nil {
		proof.DatabaseBytes = info.Size()
		proof.DatabaseHash, err = safefs.DigestFile(f)
	}
	f.Close()
	if err != nil {
		return
	}
	if err = proof.Validate(); err != nil {
		return
	}
	if err = check(); err != nil {
		return
	}
	if err = d.File().Sync(); err != nil {
		return
	}
	if err = d.WriteJSON(signalCursorManifest, proof, false); err != nil {
		return
	}
	err = inboxRestoreBoundary(fault, "signal_cursor_source_manifest")
	return
}
func buildSignalCursorCandidate(ctx context.Context, source, db *sql.DB, p interaction.SignalRecoveryHandoff, check func() error, fault func(string) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TABLE identity(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL,acked INTEGER NOT NULL,listed INTEGER NOT NULL);
CREATE TABLE events(position INTEGER PRIMARY KEY AUTOINCREMENT,key TEXT NOT NULL UNIQUE,hash TEXT NOT NULL,body TEXT NOT NULL);
CREATE TABLE signal_cursor(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL);
PRAGMA user_version=4;`); err != nil {
		return err
	}
	pin, _ := json.Marshal(inboxPin{Identity: p.Target.Identity, Account: p.Target.Account, RecoveryOperation: p.Summary.Challenge.Operation})
	body, _ := json.Marshal(p)
	if _, err = tx.Exec("INSERT INTO identity VALUES(1,?,0,0)", string(pin)); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO signal_cursor VALUES(1,?)", string(body)); err != nil {
		return err
	}
	rows, err := source.QueryContext(ctx, "SELECT position,key,hash,body FROM events ORDER BY position")
	if err != nil {
		return err
	}
	var count uint64
	for rows.Next() {
		var pos uint64
		var key, hash, raw string
		if err = rows.Scan(&pos, &key, &hash, &raw); err != nil {
			break
		}
		count++
		if pos != p.Summary.Manifest.PositionAt(count-1) || count > p.Summary.Manifest.EventCount() {
			err = fmt.Errorf("Signal cursor source history changed")
			break
		}
		if _, err = tx.Exec("INSERT INTO events VALUES(?,?,?,?)", pos, key, hash, raw); err != nil {
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if count != p.Summary.Manifest.EventCount() {
		return fmt.Errorf("Signal cursor source history incomplete")
	}
	if _, err = tx.Exec("UPDATE sqlite_sequence SET seq=? WHERE name='events'", p.Target.Position); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO sqlite_sequence(name,seq) SELECT 'events',? WHERE NOT EXISTS(SELECT 1 FROM sqlite_sequence WHERE name='events')", p.Target.Position); err != nil {
		return err
	}
	if err = inboxRestoreBoundary(fault, "signal_cursor_source_transaction"); err != nil {
		return err
	}
	if err = check(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return inboxRestoreBoundary(fault, "signal_cursor_source_commit")
}
func verifySignalCursorCandidate(ctx context.Context, db *sql.DB, p interaction.SignalRecoveryHandoff) error {
	var check, raw string
	var version, count int
	var ack, listed, seq uint64
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return fmt.Errorf("Signal cursor integrity failure")
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 4 {
		return fmt.Errorf("Signal cursor schema changed")
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'").Scan(&count); err != nil || count != 3 {
		return fmt.Errorf("Signal cursor tables changed")
	}
	for _, table := range []string{"identity", "signal_cursor", "sqlite_sequence"} {
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 1 {
			return fmt.Errorf("Signal cursor singleton changed")
		}
	}
	if err := db.QueryRowContext(ctx, "SELECT body FROM signal_cursor WHERE singleton=1").Scan(&raw); err != nil {
		return err
	}
	var saved interaction.SignalRecoveryHandoff
	if err := protocol.Decode([]byte(raw), &saved, 16384); err != nil {
		return err
	}
	if protocol.Hash(saved) != protocol.Hash(p) {
		return fmt.Errorf("Signal cursor binding changed")
	}
	if err := db.QueryRowContext(ctx, "SELECT body,acked,listed,(SELECT seq FROM sqlite_sequence WHERE name='events') FROM identity WHERE singleton=1").Scan(&raw, &ack, &listed, &seq); err != nil {
		return err
	}
	var pin inboxPin
	if err := protocol.Decode([]byte(raw), &pin, 4096); err != nil {
		return err
	}
	if pin != (inboxPin{Identity: p.Target.Identity, Account: p.Target.Account, RecoveryOperation: p.Summary.Challenge.Operation}) || ack != 0 || listed != 0 || seq != p.Target.Position {
		return fmt.Errorf("Signal cursor pin or baseline changed")
	}
	digest, err := manifestEventDigest(ctx, db, p.Summary.Manifest)
	if err != nil {
		return err
	}
	if digest != p.Summary.Manifest.Digest {
		return fmt.Errorf("Signal cursor history changed")
	}
	return nil
}

func verifySignalCursorFiles(ctx context.Context, d *safefs.Directory, p interaction.SignalRecoveryCursor) error {
	if err := p.Validate(); err != nil {
		return err
	}
	var intent interaction.SignalRecoveryHandoff
	if err := d.ReadJSON(signalCursorIntent, &intent); err != nil {
		return err
	}
	var manifest interaction.SignalRecoveryCursor
	if err := d.ReadJSON(signalCursorManifest, &manifest); err != nil {
		return err
	}
	if protocol.Hash(intent) != protocol.Hash(p.Handoff) || protocol.Hash(manifest) != protocol.Hash(p) {
		return fmt.Errorf("Signal cursor artifact binding changed")
	}
	if err := verifyStandaloneInboxFile(d, signalCursorDatabase, p.DatabaseBytes, p.DatabaseHash); err != nil {
		return err
	}
	db, err := inboxArtifactDB(d, signalCursorDatabase, true)
	if err != nil {
		return err
	}
	defer db.Close()
	return verifySignalCursorCandidate(ctx, db, p.Handoff)
}
