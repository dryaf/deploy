package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const commandReceiverFile = "command-receiver.json"

// CommandReceiverRequest binds initial command storage to a completed
// shared reporting owner. It does not enable gateway intake or start a service.
// Operation and policy must be retained by the coordinator before calling Stage.
type CommandReceiverRequest struct {
	Version     int                `json:"version"`
	Operation   string             `json:"operation"`
	State       string             `json:"state"`
	Archive     string             `json:"archive"`
	WriterEpoch uint64             `json:"writer_epoch"`
	OwnerHash   string             `json:"owner_hash"`
	Producer    notify.Producer    `json:"producer"`
	Identity    notify.Identity    `json:"identity"`
	Socket      string             `json:"socket"`
	Policy      interaction.Config `json:"policy"`
}

type commandReceiverRecord struct {
	Request  CommandReceiverRequest `json:"request"`
	Complete bool                   `json:"complete"`
}

// BackupCommandDescriptors returns the specialist contract, rather than allowing
// executable command descriptors to come from a user's inventory.
func BackupCommandDescriptors() map[string]interaction.Descriptor {
	return backupCommandManifest(false)
}

// BackupIncidentCommandDescriptors adds only the implemented reminder decisions;
// it does not grant job execution, arbitrary commands or incident resolution.
func BackupIncidentCommandDescriptors() map[string]interaction.Descriptor {
	return backupCommandManifest(true)
}

// Version 1 remains read-only, including when loaded by a newer helper. Version 2
// explicitly pins incident management in the immutable file and SQL receipts.
func (r CommandReceiverRequest) mutations() bool { return r.Version == 2 }

func (r CommandReceiverRequest) validate() error {
	if (r.Version != 1 && r.Version != 2) || !safefs.ValidID(r.Operation, "n") || !backupRecoveryPath(r.State) || !safefs.ValidID(r.Archive, "a") || r.WriterEpoch == 0 || !safefs.DigestPattern.MatchString(r.OwnerHash) || !r.Producer.Valid() || !r.Identity.Valid() || !backupRecoveryPath(r.Socket) || len(r.Socket) > 103 {
		return fmt.Errorf("command receiver requires exact shared archive and gateway authority")
	}
	if err := r.Policy.Validate(); err != nil {
		return err
	}
	// Expanded rights still require separate gateway admission; an existing
	// receiver cannot be upgraded by editing its immutable record.
	if _, err := backupCommandHandlers(r.Policy, r.Producer, r.mutations(), nil); err != nil {
		return err
	}
	for _, p := range r.Policy.Permissions {
		if p.Worker == r.Producer.ID {
			for _, permission := range p.Permissions {
				if permission != "read" && !(r.mutations() && permission == "incident.manage") {
					return fmt.Errorf("receiver permission is not supported by its retained version")
				}
			}
		}
	}
	return nil
}

func requireCommandReceiverOwner(state *backupState, r CommandReceiverRequest) error {
	var control ArchiveControl
	if err := state.get("control", &control); err != nil {
		return err
	}
	if control.ArchiveID != r.Archive || control.WriterID != r.Producer.ID || control.Epoch != r.WriterEpoch || control.ReportingDisabled {
		return fmt.Errorf("command receiver archive authority changed")
	}
	owner, err := currentSharedBackupOwner(state)
	if err != nil {
		return err
	}
	if owner == nil || owner.CutoverHash == "" || notify.Hash(*owner) != r.OwnerHash || owner.Binding.Producer != r.Producer || owner.Binding.Identity != r.Identity || owner.Binding.Archive != r.Archive || owner.Socket != r.Socket {
		return fmt.Errorf("command receiver shared reporting owner changed")
	}
	for _, w := range r.Policy.Workers {
		if w.Producer == r.Producer && w.Generation != owner.Binding.Generation {
			return fmt.Errorf("command receiver policy changed its reporting generation")
		}
	}
	var complete string
	if err = state.get("notification_cutover_complete", &complete); err != nil || complete != owner.CutoverHash {
		return fmt.Errorf("finish shared reporting cutover before command receiver setup or execution")
	}
	return nil
}

func commandReceiverSQL(state *backupState, r CommandReceiverRequest) (bool, error) {
	var present int
	if err := state.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='backup_command_receiver'").Scan(&present); err != nil {
		return false, err
	}
	if present == 0 {
		return false, nil
	}
	var pin string
	if err := state.db.QueryRow("SELECT request_hash FROM backup_command_receiver WHERE singleton=1").Scan(&pin); err != nil || pin != notify.Hash(r) {
		return false, fmt.Errorf("command receiver SQL authority changed")
	}
	var archive string
	var epoch uint64
	if err := state.db.QueryRow("SELECT archive_id,epoch FROM backup_command_binding WHERE singleton=1").Scan(&archive, &epoch); err != nil || archive != r.Archive || epoch != r.WriterEpoch {
		return false, fmt.Errorf("command ledger archive binding changed")
	}
	for _, table := range []string{"interactive_identity", "interactive_inbox", "interactive_results", "interactive_non_admission", "backup_command_suppressions", "backup_command_audit", "backup_command_evidence", "backup_command_event_context", "backup_command_delivery_guard"} {
		var present int
		if err := state.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&present); err != nil || present != 1 {
			return false, fmt.Errorf("command receiver lost retained ledger or domain tables")
		}
	}
	return true, nil
}

func StageSharedCommandReceiver(ctx context.Context, r CommandReceiverRequest) (string, error) {
	return stageSharedCommandReceiver(ctx, r, nil)
}

func stageSharedCommandReceiver(ctx context.Context, r CommandReceiverRequest, fault func(string) error) (string, error) {
	if err := r.validate(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	d, err := safefs.Open(r.State)
	if err != nil {
		return "", err
	}
	defer d.Close()
	// All receiver initialization is serialized by the existing schema lease.
	// Check SQL before creating a command lock so missing receipts cannot cause
	// reconstruction of a previously installed receiver's lost lock.
	schema, err := d.Lock("schema.lock", true, false)
	if err != nil {
		return "", err
	}
	defer schema.Close()
	var retained commandReceiverRecord
	readErr := d.ReadJSON(commandReceiverFile, &retained)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", readErr
	}
	if readErr == nil && (retained.Request.validate() != nil || notify.Hash(retained.Request) != notify.Hash(r)) {
		return "", fmt.Errorf("command receiver retry changed its retained request")
	}
	if f, e := d.Open("installation.pending", os.O_RDONLY, 0); e == nil {
		f.Close()
		return "", fmt.Errorf("finish backup installation before command enrollment")
	} else if !os.IsNotExist(e) {
		return "", e
	}
	state, err := openBackupStateWithLease(r.State, false, false, true)
	if err != nil {
		return "", err
	}
	defer state.Close()
	if err = requireCommandReceiverOwner(state, r); err != nil {
		return "", err
	}
	installed, err := commandReceiverSQL(state, r)
	if err != nil {
		return "", err
	}
	if installed && os.IsNotExist(readErr) {
		return "", fmt.Errorf("command receiver receipt missing; recovery required")
	}
	if !installed {
		if readErr == nil && retained.Complete {
			return "", fmt.Errorf("completed command receiver lost SQL authority")
		}
		var existing int
		if err = state.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND (name LIKE 'interactive_%' OR name='backup_command_binding')").Scan(&existing); err != nil {
			return "", err
		}
		if existing != 0 {
			return "", fmt.Errorf("existing command ledger requires explicit adoption or recovery")
		}
	}
	lock, err := d.Lock("commands.lock", true, os.IsNotExist(readErr) && !installed)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if os.IsNotExist(readErr) {
		retained = commandReceiverRecord{Request: r}
		if err = d.WriteJSON(commandReceiverFile, retained, false); err != nil {
			return "", err
		}
	}
	boundary := func(at string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fault != nil {
			return fault(at)
		}
		return nil
	}
	if err = boundary("intent"); err != nil {
		return "", err
	}
	initialize := func(tx *sql.Tx) error {
		present, err := backupCommandsInstalled(tx)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("shared reporting lost backup command evidence tables")
		}
		if _, err = tx.Exec("CREATE TABLE backup_command_binding(singleton INTEGER PRIMARY KEY CHECK(singleton=1),archive_id TEXT NOT NULL,epoch INTEGER NOT NULL)"); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO backup_command_binding VALUES(1,?,?)", r.Archive, r.WriterEpoch); err != nil {
			return err
		}
		if _, err = tx.Exec("CREATE TABLE backup_command_receiver(singleton INTEGER PRIMARY KEY CHECK(singleton=1),request_hash TEXT NOT NULL)"); err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO backup_command_receiver VALUES(1,?)", notify.Hash(r))
		return err
	}
	if _, err = interaction.OpenDomainLedger(ctx, state.db, r.Producer, r.Identity, !installed, initialize); err != nil {
		return "", err
	}
	if err = boundary("ledger"); err != nil {
		return "", err
	}
	if !retained.Complete {
		retained.Complete = true
		if err = d.WriteJSON(commandReceiverFile, retained, true); err != nil {
			return "", err
		}
	}
	if err = boundary("complete"); err != nil {
		return "", err
	}
	return notify.Hash(retained), nil
}
