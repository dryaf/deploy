package source

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type sourceSpoolOwner struct {
	Schema     int       `json:"schema"`
	SourceID   string    `json:"source_id"`
	SnapshotID string    `json:"snapshot_id"`
	Nonce      string    `json:"nonce"`
	CreatedAt  time.Time `json:"created_at"`
	BootID     string    `json:"boot_id"`
	Uptime     float64   `json:"uptime"`
}

func WriteSourceSpoolOwner(path, source, id, nonce string) error {
	root, err := safefs.Open(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		return err
	}
	defer root.Close()
	boot, uptime, err := process.HostClock()
	if err != nil {
		return err
	}
	owner := sourceSpoolOwner{1, source, id, nonce, time.Now().UTC(), boot, uptime}
	if err = root.Mkdir("spool-owners"); err != nil {
		return err
	}
	if err = root.WriteJSON("spool-owners/"+id+".json", owner, false); err != nil {
		return err
	}
	if err = root.Mkdir("exports/" + id); err != nil {
		return err
	}
	dir, err := safefs.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.WriteJSON("owner.json", owner, false)
}

func readSourceSpoolOwner(path string, dir *safefs.Directory, owner *sourceSpoolOwner) error {
	err := dir.ReadJSON("owner.json", owner)
	if !os.IsNotExist(err) {
		return err
	}
	root, e := safefs.Open(filepath.Dir(filepath.Dir(path)))
	if e != nil {
		return e
	}
	defer root.Close()
	return root.ReadJSON("spool-owners/"+filepath.Base(path)+".json", owner)
}

func CleanupSourceSpool(path string) error {
	dir, err := safefs.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	var owner sourceSpoolOwner
	if err = readSourceSpoolOwner(path, dir, &owner); err != nil {
		return err
	}
	if owner.Schema != 1 || owner.SnapshotID != filepath.Base(path) || !safefs.ValidID(owner.SourceID, "s") || !safefs.ValidID(owner.Nonce, "n") {
		return fmt.Errorf("source spool ownership is not established")
	}
	entries, err := dir.File().ReadDir(7)
	if err != nil && err != io.EOF {
		return err
	}
	allowed := map[string]bool{"owner.json": true, "database.db": true, "database.db-wal": true, "database.db-shm": true, "database.db-journal": true}
	for _, entry := range entries {
		temporary := safefs.IsJSONTemporary(entry.Name())
		if (!allowed[entry.Name()] && !temporary) || !entry.Type().IsRegular() {
			return fmt.Errorf("foreign source spool contents retained")
		}
	}
	for _, entry := range entries {
		if entry.Name() == "owner.json" {
			continue
		}
		if err = unix.Unlinkat(int(dir.File().Fd()), entry.Name(), 0); err != nil {
			return err
		}
	}
	if err = unix.Unlinkat(int(dir.File().Fd()), "owner.json", 0); err != nil && err != unix.ENOENT {
		return err
	}
	if err = dir.File().Sync(); err != nil {
		return err
	}
	parent, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	current, err := parent.Sub(filepath.Base(path))
	if err != nil {
		return err
	}
	oldInfo, _ := dir.File().Stat()
	newInfo, _ := current.File().Stat()
	current.Close()
	if oldInfo == nil || newInfo == nil || !os.SameFile(oldInfo, newInfo) {
		return fmt.Errorf("source spool directory changed")
	}
	if err = unix.Unlinkat(int(parent.File().Fd()), filepath.Base(path), unix.AT_REMOVEDIR); err != nil {
		return err
	}
	if err = parent.File().Sync(); err != nil {
		return err
	}
	root, err := safefs.Open(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		return err
	}
	defer root.Close()
	owners, err := root.Sub("spool-owners")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer owners.Close()
	if err = unix.Unlinkat(int(owners.File().Fd()), filepath.Base(path)+".json", 0); err != nil && err != unix.ENOENT {
		return err
	}
	return owners.File().Sync()
}

func cleanupExpiredSourceSpools(dir *safefs.Directory, sourceID string) error {
	exports, err := dir.Sub("exports")
	if err != nil {
		return err
	}
	defer exports.Close()
	entries, err := exports.File().ReadDir(129)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) > 128 {
		return fmt.Errorf("source spool inventory requires operator reconciliation")
	}
	boot, uptime, err := process.HostClock()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !safefs.ValidID(entry.Name(), "b") {
			return fmt.Errorf("foreign source spool requires reconciliation")
		}
		spool, err := exports.Sub(entry.Name())
		if err != nil {
			return err
		}
		var owner sourceSpoolOwner
		err = readSourceSpoolOwner(filepath.Join(dir.File().Name(), "exports", entry.Name()), spool, &owner)
		spool.Close()
		if err != nil || owner.Schema != 1 || owner.SourceID != sourceID || owner.SnapshotID != entry.Name() {
			return fmt.Errorf("source spool lacks ownership evidence")
		}
		if owner.BootID != boot || uptime-owner.Uptime > (sourceSessionCap+time.Minute).Seconds() {
			if err = CleanupSourceSpool(filepath.Join(dir.File().Name(), "exports", entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
