package backup

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// The immutable receiver authority survives an executable upgrade. Its existing
// version still controls rights even when the new helper supports more commands.
type sharedHelperCommands struct {
	Receiver   commandReceiverRecord `json:"receiver"`
	Capability string                `json:"capability"`
}

func validateSharedHelperCommands(j WorkerSetupJournal, c *sharedHelperCommands) error {
	if c == nil || !c.Receiver.Complete || c.Receiver.Request.validate() != nil || j.Shared == nil || j.Producer == nil || j.Declaration == nil {
		return fmt.Errorf("helper upgrade requires a completed retained command receiver")
	}
	r := c.Receiver.Request
	p, err := declarationPaths(j.Declaration.Home, j.Declaration)
	if err != nil {
		return err
	}
	o := j.Shared.Owner
	if r.State != p.State || r.Archive != j.ArchiveID || r.Producer != j.Producer.Producer || r.Producer.ID != j.WorkerID || r.OwnerHash != notify.Hash(o) || r.Identity != o.Binding.Identity || r.Socket != o.Socket {
		return fmt.Errorf("helper upgrade changed command receiver ownership")
	}
	if c.Capability != "backup-shared-commands-v2" && (r.mutations() || c.Capability != "backup-shared-commands-v1") {
		return fmt.Errorf("replacement helper does not support retained command rights")
	}
	return nil
}

func reviewSharedHelperCommands(p InstallationPaths, j WorkerSetupJournal, candidate string) (*sharedHelperCommands, error) {
	d, err := safefs.Open(p.State)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	var record commandReceiverRecord
	err = d.ReadJSON(commandReceiverFile, &record)
	if os.IsNotExist(err) {
		if n := j.Declaration.Desired.Notification; n != nil && (n.Commands.Enabled || len(n.Commands.Workers) != 0 || len(n.Commands.Conversations) != 0 || len(n.Commands.Permissions) != 0) {
			return nil, fmt.Errorf("finish command receiver admission before upgrading its helper")
		}
		if err = requireSharedHelperCommandFree(p); err != nil {
			return nil, err
		}
		return nil, inspectSharedHelperCommands(p, j, nil, false)
	}
	if err != nil {
		return nil, err
	}
	if !record.Complete || record.Request.validate() != nil {
		return nil, fmt.Errorf("finish retained command receiver setup before helper upgrade")
	}
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		return nil, err
	}
	capability, err := inspectCommandReceiverArtifact(context.Background(), f, candidate, record.Request.mutations())
	f.Close()
	if err != nil {
		return nil, err
	}
	review := &sharedHelperCommands{Receiver: record, Capability: capability}
	if err = inspectSharedHelperCommands(p, j, review, false); err != nil {
		return nil, err
	}
	return review, nil
}

// Preflight takes an exclusive schema lease for an immutable read. Completion
// already holds the installer's
// exclusive lease, so it must not acquire another lease on the same database.
// Both paths open SQLite read-only and never initialize missing receiver state.
func inspectSharedHelperCommands(p InstallationPaths, j WorkerSetupJournal, c *sharedHelperCommands, installer bool) error {
	d, err := safefs.Open(p.State)
	if err != nil {
		return err
	}
	defer d.Close()
	if !installer {
		lease, err := d.Lock("schema.lock", true, false)
		if err != nil {
			return err
		}
		defer lease.Close()
	}
	file, err := d.Open("worker.db", os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	file.Close()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0600 {
		return fmt.Errorf("unsafe retained command database")
	}
	uri := url.URL{Scheme: "file", Path: filepath.Join(p.State, "worker.db"), RawQuery: url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)", "query_only(1)", "trusted_schema(OFF)"}}.Encode()}
	if !installer {
		// Managed writers are fenced. A retained WAL requires recovery; never
		// omit it from an immutable read or create SQLite sidecars during review.
		wal, e := d.Open("worker.db-wal", os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if e == nil {
			info, statErr := wal.Stat()
			wal.Close()
			if statErr != nil || info.Size() != 0 {
				return fmt.Errorf("settle retained backup WAL before helper upgrade review")
			}
		} else if !os.IsNotExist(e) {
			return e
		}
		q := uri.Query()
		q.Set("immutable", "1")
		uri.RawQuery = q.Encode()
	}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if c == nil {
		var count int
		if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('backup_command_receiver','backup_command_binding') OR name LIKE 'interactive_%'").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("command receiver records are missing; recover retained command authority before helper upgrade")
		}
		return nil
	}
	if err = validateSharedHelperCommands(j, c); err != nil {
		return err
	}
	var current commandReceiverRecord
	if err = d.ReadJSON(commandReceiverFile, &current); err != nil {
		return err
	}
	if notify.Hash(current) != notify.Hash(c.Receiver) {
		return fmt.Errorf("helper upgrade changed the retained command receiver")
	}
	lock, err := d.Open("commands.lock", os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	lock.Close()
	state := &backupState{db: db, directory: p.State}
	if err = requireCommandReceiverOwner(state, current.Request); err != nil {
		return err
	}
	complete, err := commandReceiverSQL(state, current.Request)
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("retained command ledger is missing")
	}
	_, err = interaction.OpenDomainLedger(context.Background(), db, current.Request.Producer, current.Request.Identity, false, nil)
	return err
}
