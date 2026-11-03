package relay

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

func runtimeSourceDirectory(src *safefs.Directory, p interaction.SignalRecoveryCursor) (*safefs.Directory, func(), error) {
	op := p.Handoff.Summary.Manifest.Restore.Operation
	if op == "" {
		return src, func() {}, nil
	}
	d, err := src.Sub("restore/" + op)
	if err != nil {
		return nil, nil, err
	}
	info, err := d.File().Stat()
	if err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
			err = fmt.Errorf("unsafe Signal recovery stage")
		}
	}
	if err != nil {
		d.Close()
		return nil, nil, err
	}
	return d, func() { d.Close() }, nil
}

func verifyRuntimeSource(ctx context.Context, src *safefs.Directory, p interaction.SignalRecoveryCursor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	d, closeDir, err := runtimeSourceDirectory(src, p)
	if err != nil {
		return err
	}
	defer closeDir()
	if err = verifySignalCursorFiles(ctx, d, p); err != nil {
		return err
	}
	var handoff interaction.SignalRecoveryHandoff
	if err = d.ReadJSON(signalHandoffFile, &handoff); err != nil {
		return err
	}
	if protocol.Hash(handoff) != protocol.Hash(p.Handoff) {
		return fmt.Errorf("Signal source handoff changed")
	}
	var pin inboxPin
	if err = src.ReadJSON("identity.json", &pin); err != nil {
		return err
	}
	m := p.Handoff.Summary.Manifest
	if m.Restore.Operation == "" {
		if err = absentRuntimeFile(src, "restore.json"); err != nil {
			return err
		}
		if pin.Identity != m.Relay.Identity || pin.Account != m.Relay.Account || pin.RecoveryOperation != "" || pin.Baseline != m.BaselineHash {
			return fmt.Errorf("Signal source identity changed")
		}
		if err = runtimeDatabaseFiles(src, "inbox.db"); err != nil {
			return err
		}
		db, e := inboxArtifactDB(src, "inbox.db", true)
		if e != nil {
			return e
		}
		defer db.Close()
		var body string
		if e = db.QueryRowContext(ctx, "SELECT body FROM identity WHERE singleton=1").Scan(&body); e != nil {
			return e
		}
		var sqlPin inboxPin
		if e = protocol.Decode([]byte(body), &sqlPin, 4096); e != nil {
			return e
		}
		if sqlPin != pin {
			return fmt.Errorf("Signal source database identity changed")
		}
		if _, e = readInboxBaseline(db, pin); e != nil {
			return e
		}
		tx, e := db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		saved, e := verifyPreparedSignalHandoff(ctx, tx, p.Handoff)
		if e != nil {
			return e
		}
		if saved.Cursor == nil || protocol.Hash(*saved.Cursor) != protocol.Hash(p) {
			return fmt.Errorf("Signal source cursor acknowledgement missing")
		}
		return nil
	}
	var j InboxRestoreState
	if err = src.ReadJSON("restore.json", &j); err != nil {
		return err
	}
	if err = j.validate(m.Relay.Account); err != nil {
		return err
	}
	if j.Phase != "quarantined" || j.Operation != m.Restore.Operation || j.Before != m.Relay.Identity || j.After != m.Restore.After || j.Snapshot.ID != m.Restore.Snapshot || j.Snapshot.DatabaseHash != m.Restore.SnapshotHash || j.GatewayExport != p.Handoff.Summary.Challenge.Operation || j.GatewaySummary != protocol.Hash(p.Handoff.Summary) || j.GatewayHandoff != protocol.Hash(p.Handoff) || j.GatewayCursor != protocol.Hash(p) {
		return fmt.Errorf("restored Signal source acknowledgements changed")
	}
	if pin != (inboxPin{Identity: j.After, Account: j.Account, RecoveryOperation: j.Operation}) {
		return fmt.Errorf("restored Signal identity changed")
	}
	if err = verifyStandaloneInboxFile(d, "source.db", j.Snapshot.DatabaseBytes, j.Snapshot.DatabaseHash); err != nil {
		return err
	}
	if err = verifyStandaloneInboxFile(d, "candidate.db", j.CandidateBytes, j.CandidateHash); err != nil {
		return err
	}
	db, err := inboxArtifactDB(d, "candidate.db", true)
	if err != nil {
		return err
	}
	err = verifyQuarantinedInbox(ctx, db, j)
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	r := RestoredRecoveryInbox{dir: src, stage: d, journal: j}
	if j.Snapshot.Schema == runtimeInboxSchema {
		db, e := inboxArtifactDB(d, "source.db", true)
		if e != nil {
			return e
		}
		baseline, e := readInboxBaselineProof(db, inboxPin{Identity: j.Before, Account: j.Account, Baseline: j.Snapshot.BaselineHash})
		db.Close()
		if e != nil {
			return e
		}
		manifest := r.manifest()
		if e = addBaselineHistory(&manifest, *baseline); e != nil {
			return e
		}
		r.history = manifest.History
	}
	export, err := r.readExport()
	if err != nil {
		return err
	}
	if !r.manifest().Equal(m) || export.Summary == nil || protocol.Hash(*export.Summary) != protocol.Hash(p.Handoff.Summary) {
		return fmt.Errorf("restored Signal source history changed")
	}
	return nil
}

// Validate sidecar inodes before allowing SQLite to inspect a live database.
func runtimeDatabaseFiles(d *safefs.Directory, name string) error {
	f, err := inboxArtifactFile(d, name, 34<<20)
	if err != nil {
		return err
	}
	f.Close()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		f, e := d.Open(name+suffix, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		info, e := f.Stat()
		f.Close()
		if e != nil {
			return e
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 34<<20 {
			return fmt.Errorf("unsafe Signal runtime SQLite sidecar")
		}
	}
	return nil
}
