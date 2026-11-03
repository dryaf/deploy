package backup

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/dryaf/deploy/internal/safefs"
)

type archiveTakeover struct {
	Schema   int             `json:"schema"`
	Before   ArchiveIdentity `json:"before"`
	After    ArchiveIdentity `json:"after"`
	Previous ArchiveControl  `json:"previous"`
	Control  ArchiveControl  `json:"control"`
	At       time.Time       `json:"at"`
}

// Explicit setup --recover --fenced is the authority for this transition. The
// exclusive archive lease drains every reader/writer before changing its epoch.
func (a *backupArchive) claimRecoveredWriter(state *backupState, c WorkerInstallation) error {
	var recovered ArchiveControl
	if err := state.get("recovered_control", &recovered); err != nil {
		return fmt.Errorf("recovery lacks complete control/pin evidence")
	}
	if recovered.ArchiveID != a.identity.ID || c.ArchiveID != a.identity.ID {
		return fmt.Errorf("recovery archive mismatch")
	}
	if a.maintenance != nil {
		a.maintenance.Close()
	}
	a.maintenance = nil
	exclusive, err := a.dir.Lock(".locks/maintenance.lock", true, false)
	if err != nil {
		return err
	}
	a.maintenance = exclusive
	if err = a.dir.Mkdir("control/takeovers"); err != nil {
		return err
	}
	path := "control/takeovers/" + c.WorkerID + ".json"
	var intent archiveTakeover
	err = a.dir.ReadJSON(path, &intent)
	if os.IsNotExist(err) {
		checkpoint, e := a.checkpoint()
		if e != nil || !reflect.DeepEqual(checkpoint, recovered) {
			return fmt.Errorf("recovered control differs from current archive checkpoint")
		}
		filesystem, e := a.dir.FilesystemID()
		if e != nil {
			return e
		}
		next := a.identity
		next.WriterID = c.WorkerID
		next.Epoch++
		next.FilesystemID = filesystem
		var control ArchiveControl
		original, _ := json.Marshal(recovered)
		if err = safefs.DecodeJSON(original, &control, safefs.MaxRecordBytes); err != nil {
			return err
		}
		control.WriterID = c.WorkerID
		control.Epoch = next.Epoch
		control.Revision++
		control.ConfigHash = c.ConfigHash
		control.Retention = c.Policy.Retention
		for key, value := range control.Instances {
			value.LastSuccess = time.Time{}
			control.Instances[key] = value
		}
		intent = archiveTakeover{1, a.identity, next, recovered, control, time.Now().UTC()}
		if err = a.dir.WriteJSON(path, intent, false); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if intent.Schema != 1 || intent.After.WriterID != c.WorkerID || intent.Control.ConfigHash != c.ConfigHash || intent.Control.ArchiveID != c.ArchiveID || (a.identity != intent.Before && a.identity != intent.After) {
		return fmt.Errorf("takeover intent conflicts with installation or newer writer")
	}
	var checkpoint archiveCheckpoint
	if err = a.dir.ReadJSON("control/recovery.json", &checkpoint); err != nil {
		return err
	}
	body, _ := json.Marshal(checkpoint.Control)
	if fmt.Sprintf("%x", sha256.Sum256(body)) != checkpoint.SHA256 || (!reflect.DeepEqual(checkpoint.Control, intent.Previous) && !reflect.DeepEqual(checkpoint.Control, intent.Control)) {
		return fmt.Errorf("takeover checkpoint conflict")
	}
	if err = a.dir.WriteJSON("archive.json", intent.After, true); err != nil {
		return err
	}
	a.identity = intent.After
	body, _ = json.Marshal(intent.Control)
	if err = a.dir.WriteJSON("control/recovery.json", archiveCheckpoint{intent.Control, fmt.Sprintf("%x", sha256.Sum256(body))}, true); err != nil {
		return err
	}
	if err = state.put("control", intent.Control); err != nil {
		return err
	}
	return state.put("mode", "writer")
}
