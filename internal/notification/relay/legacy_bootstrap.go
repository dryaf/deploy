package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const legacyBootstrapIntent = "legacy-bootstrap-intent.json"
const legacyBootstrapComplete = "legacy-bootstrap-complete.json"

// LegacyInboxBootstrap records the absence of a legacy command inbox, never
// completeness of Signal provider history. It authorizes no account consumer.
type LegacyInboxBootstrap struct {
	Version                         int
	Operation, Account              string
	Identity                        protocol.Identity
	DirectoryDevice, DirectoryInode uint64
	HistoryComplete                 bool
}

func (v LegacyInboxBootstrap) validate(d *safefs.Directory) error {
	info, err := d.File().Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if v.Version != 1 || !safefs.ValidID(v.Operation, "n") || !protocol.ValidToken(v.Account) || !v.Identity.Valid() || v.HistoryComplete || !ok || v.DirectoryDevice != uint64(st.Dev) || v.DirectoryInode != st.Ino {
		return fmt.Errorf("legacy Signal inbox bootstrap binding changed")
	}
	return nil
}

// BootstrapLegacyInbox is called only inside the common account coordinator's
// stopped-service/exclusive-account section. The caller supplies a new private
// directory. Partial creation resumes only the exact retained operation; existing
// inboxes, events and unidentified files are never adopted or erased.
func BootstrapLegacyInbox(ctx context.Context, path, account, operation string) (LegacyInboxBootstrap, error) {
	return bootstrapLegacyInbox(ctx, path, account, operation, nil)
}
func bootstrapLegacyInbox(ctx context.Context, path, account, operation string, fault func(string) error) (v LegacyInboxBootstrap, err error) {
	if !protocol.ValidToken(account) || !safefs.ValidID(operation, "n") {
		return v, fmt.Errorf("invalid legacy Signal bootstrap request")
	}
	d, err := privateInboxDirectory(path)
	if err != nil {
		return v, err
	}
	defer d.Close()
	lease, err := d.Lock("inbox.lock", true, true)
	if err != nil {
		return v, err
	}
	defer lease.Close()
	boundary := func(at string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fault != nil {
			return fault(at)
		}
		return nil
	}
	err = d.ReadJSON(legacyBootstrapIntent, &v)
	if os.IsNotExist(err) {
		entries, e := d.File().ReadDir(-1)
		if e != nil {
			return v, e
		}
		for _, entry := range entries {
			if entry.Name() != "inbox.lock" && !safefs.IsJSONTemporary(entry.Name()) {
				return v, fmt.Errorf("legacy Signal bootstrap requires a new empty inbox")
			}
		}
		v = LegacyInboxBootstrap{Version: 1, Operation: operation, Account: account}
		v.Identity.Worker, err = safefs.NewID("inbox")
		if err != nil {
			return v, err
		}
		v.Identity.Recovery, err = safefs.NewID("epoch")
		if err != nil {
			return v, err
		}
		info, e := d.File().Stat()
		if e != nil {
			return v, e
		}
		st := info.Sys().(*syscall.Stat_t)
		v.DirectoryDevice, v.DirectoryInode = uint64(st.Dev), st.Ino
		if err = d.WriteJSON(legacyBootstrapIntent, v, false); err != nil {
			return v, err
		}
	} else if err != nil {
		return v, err
	}
	if v.Operation != operation || v.Account != account {
		return v, fmt.Errorf("another legacy Signal bootstrap is pending")
	}
	if err = v.validate(d); err != nil {
		return v, err
	}
	if err = boundary("legacy_inbox_intent"); err != nil {
		return v, err
	}
	var complete LegacyInboxBootstrap
	err = d.ReadJSON(legacyBootstrapComplete, &complete)
	if err == nil {
		if complete != v {
			return v, fmt.Errorf("legacy Signal bootstrap completion changed")
		}
		return v, verifyLegacyBootstrap(ctx, d, v)
	} else if !os.IsNotExist(err) {
		return v, err
	}
	pin := inboxPin{Identity: v.Identity, Account: account}
	if err = runtimeJSONOnce(d, "identity.json", pin); err != nil {
		return v, err
	}
	if err = boundary("legacy_inbox_identity"); err != nil {
		return v, err
	}
	f, err := d.Open("inbox.db", os.O_RDWR|os.O_CREATE|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return v, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return v, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0600 || info.Size() > inboxSnapshotLimit {
		f.Close()
		return v, fmt.Errorf("unsafe legacy bootstrap database")
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return v, err
	}
	if err = d.File().Sync(); err != nil {
		return v, err
	}
	db, err := inboxArtifactDB(d, "inbox.db", false)
	if err != nil {
		return v, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return v, err
	}
	defer tx.Rollback()
	var version, n int
	if err = tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return v, err
	}
	if version == 0 {
		if err = tx.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
			return v, err
		}
		if n != 0 {
			return v, fmt.Errorf("unidentified legacy inbox schema")
		}
		if _, err = tx.Exec("CREATE TABLE identity(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL,acked INTEGER NOT NULL DEFAULT 0,listed INTEGER NOT NULL DEFAULT 0);CREATE TABLE events(position INTEGER PRIMARY KEY AUTOINCREMENT,key TEXT NOT NULL UNIQUE,hash TEXT NOT NULL,body TEXT NOT NULL);PRAGMA user_version=1;"); err != nil {
			return v, err
		}
		raw, _ := json.Marshal(pin)
		if _, err = tx.Exec("INSERT INTO identity(singleton,body) VALUES(1,?)", string(raw)); err != nil {
			return v, err
		}
	} else if version != 1 {
		return v, fmt.Errorf("legacy bootstrap database already used")
	}
	if err = verifyEmptyLegacyInbox(tx, v); err != nil {
		return v, err
	}
	if err = boundary("legacy_inbox_transaction"); err != nil {
		return v, err
	}
	if err = tx.Commit(); err != nil {
		return v, err
	}
	if err = db.Close(); err != nil {
		return v, err
	}
	if err = boundary("legacy_inbox_commit"); err != nil {
		return v, err
	}
	if err = d.WriteJSON(legacyBootstrapComplete, v, false); err != nil {
		return v, err
	}
	if err = boundary("legacy_inbox_complete"); err != nil {
		return v, err
	}
	return v, verifyLegacyBootstrap(ctx, d, v)
}
func verifyEmptyLegacyInbox(tx *sql.Tx, v LegacyInboxBootstrap) error {
	var raw string
	var ack, listed, count int
	if err := tx.QueryRow("SELECT body,acked,listed,(SELECT count(*) FROM events) FROM identity WHERE singleton=1").Scan(&raw, &ack, &listed, &count); err != nil {
		return err
	}
	var pin inboxPin
	if protocol.Decode([]byte(raw), &pin, 4096) != nil || pin != (inboxPin{Identity: v.Identity, Account: v.Account}) || ack != 0 || listed != 0 || count != 0 {
		return fmt.Errorf("legacy bootstrap inbox contains changed or unreviewed history")
	}
	return nil
}
func verifyLegacyBootstrap(ctx context.Context, d *safefs.Directory, v LegacyInboxBootstrap) error {
	if err := v.validate(d); err != nil {
		return err
	}
	for _, name := range []string{legacyBootstrapIntent, legacyBootstrapComplete} {
		var actual LegacyInboxBootstrap
		if err := d.ReadJSON(name, &actual); err != nil {
			return err
		}
		if actual != v {
			return fmt.Errorf("legacy bootstrap proof changed")
		}
	}
	var pin inboxPin
	if err := d.ReadJSON("identity.json", &pin); err != nil {
		return err
	}
	if pin != (inboxPin{Identity: v.Identity, Account: v.Account}) {
		return fmt.Errorf("legacy bootstrap identity changed")
	}
	if err := runtimeDatabaseFiles(d, "inbox.db"); err != nil {
		return err
	}
	db, err := inboxArtifactDB(d, "inbox.db", true)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return verifyEmptyLegacyInbox(tx, v)
}
func VerifyLegacyInboxBootstrap(ctx context.Context, path string, v LegacyInboxBootstrap) error {
	d, err := privateInboxDirectory(path)
	if err != nil {
		return err
	}
	defer d.Close()
	lease, err := d.Lock("inbox.lock", true, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	return verifyLegacyBootstrap(ctx, d, v)
}

func ReadLegacyInboxBootstrap(ctx context.Context, path, hash string) (v LegacyInboxBootstrap, err error) {
	if !safefs.DigestPattern.MatchString(hash) {
		return v, fmt.Errorf("exact legacy bootstrap hash required")
	}
	d, err := privateInboxDirectory(path)
	if err != nil {
		return v, err
	}
	err = d.ReadJSON(legacyBootstrapComplete, &v)
	d.Close()
	if err != nil {
		return v, err
	}
	if protocol.Hash(v) != hash {
		return v, fmt.Errorf("legacy bootstrap result changed")
	}
	return v, VerifyLegacyInboxBootstrap(ctx, path, v)
}
