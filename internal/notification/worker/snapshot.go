package worker

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type SnapshotManifest struct {
	Version       int               `json:"version"`
	ID            string            `json:"id"`
	Identity      protocol.Identity `json:"identity"`
	Schema        int               `json:"schema"`
	ConfigHash    string            `json:"config_hash"`
	DatabaseHash  string            `json:"database_hash"`
	DatabaseBytes int64             `json:"database_bytes"`
	CreatedAt     time.Time         `json:"created_at"`
}

// VerifySnapshot validates a completed artifact without opening a worker or
// acquiring delivery authority.
func VerifySnapshot(ctx context.Context, path string, c Config) (SnapshotManifest, error) {
	m, dir, err := readSnapshot(ctx, path, c)
	if err == nil {
		err = dir.Close()
	}
	return m, err
}

func privateDirectory(path string) (*safefs.Directory, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("private path must be clean and absolute")
	}
	d, err := safefs.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := d.File().Stat()
	if err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
			err = fmt.Errorf("directory must be owned and private (0700)")
		}
	}
	if err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func snapshotDB(dir *safefs.Directory, name string, readOnly bool) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: fmt.Sprintf("/proc/self/fd/%d/%s", dir.File().Fd(), name)}
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	u.RawQuery = url.Values{"mode": {mode}, "_txlock": {"immediate"}, "_pragma": {"busy_timeout(1500)", "synchronous(FULL)", "foreign_keys(ON)"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

func snapshotFile(dir *safefs.Directory, name string, limit int64) (*os.File, error) {
	f, err := dir.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	i, err := f.Stat()
	if err == nil {
		st, ok := i.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || i.Mode().Perm() != 0600 || i.Size() <= 0 || i.Size() > limit {
			err = fmt.Errorf("unsafe or oversized snapshot database")
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func verifySnapshotDB(ctx context.Context, db *sql.DB, m SnapshotManifest, c Config) error {
	var check, body string
	var version, count int
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return fmt.Errorf("snapshot integrity unavailable")
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != m.Schema || !supportedSnapshotSchema(version) {
		return fmt.Errorf("unsupported snapshot schema")
	}
	if err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&body); err != nil {
		return err
	}
	var id protocol.Identity
	if err := protocol.Decode([]byte(body), &id, 4096); err != nil || id != m.Identity {
		return fmt.Errorf("snapshot identity mismatch")
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE key='recovery'").Scan(&count); err != nil || count != 0 {
		return fmt.Errorf("snapshot is already quarantined")
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM producers").Scan(&count); err != nil || count != len(c.Producers) {
		return fmt.Errorf("snapshot enrollment mismatch")
	}
	for _, p := range c.Producers {
		var epoch string
		if err := db.QueryRowContext(ctx, "SELECT epoch FROM producers WHERE id=?", p.Producer.ID).Scan(&epoch); err != nil || epoch != p.Producer.Epoch {
			return fmt.Errorf("snapshot producer epoch mismatch")
		}
	}
	if version >= 7 && version < 9 {
		if err := checkLEDLegacyMapping(ctx, db); err != nil {
			return err
		}
	}
	if err := verifyAdministrativeHistory(ctx, db, m.Identity); err != nil {
		return err
	}
	if err := verifyReceiptCompaction(ctx, db, c, m.Identity); err != nil {
		return err
	}
	if version >= 7 {
		if err := checkLEDLayout(ctx, db, c, m.Identity, false); err != nil {
			return err
		}
		if version >= 8 {
			if err := checkLEDOperations(ctx, db, m.Identity); err != nil {
				return err
			}
			if version >= 9 {
				return checkLEDMigrations(ctx, db, c, m.Identity, false)
			}
			return nil
		}
		return nil
	}
	if len(c.LEDBindings) != 0 {
		return fmt.Errorf("legacy snapshot cannot contain LED bindings")
	}
	return nil
}

func readSnapshot(ctx context.Context, path string, c Config) (m SnapshotManifest, dir *safefs.Directory, err error) {
	if err = c.Validate(); err != nil {
		return
	}
	dir, err = privateDirectory(path)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			dir.Close()
			dir = nil
		}
	}()
	if err = dir.ReadJSON("snapshot.json", &m); err != nil {
		return
	}
	if m.Version != 1 || !supportedSnapshotSchema(m.Schema) || !safefs.ValidID(m.ID, "n") || !m.Identity.Valid() || m.ConfigHash != protocol.Hash(c) ||
		!safefs.DigestPattern.MatchString(m.DatabaseHash) || m.DatabaseBytes <= 0 || m.DatabaseBytes > c.MaxDatabaseBytes || m.CreatedAt.IsZero() {
		err = fmt.Errorf("invalid snapshot manifest or configuration mismatch")
		return
	}
	f, e := snapshotFile(dir, "state.db", c.MaxDatabaseBytes)
	if e != nil {
		err = e
		return
	}
	i, e := f.Stat()
	if e == nil && i.Size() != m.DatabaseBytes {
		e = fmt.Errorf("snapshot length mismatch")
	}
	var hash string
	if e == nil {
		hash, e = safefs.DigestFile(f)
	}
	f.Close()
	if e != nil {
		err = e
		return
	}
	if hash != m.DatabaseHash {
		err = fmt.Errorf("snapshot checksum mismatch")
		return
	}
	// A snapshot is a standalone database, never a DB plus unaccounted WAL.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		extra, e := dir.Open("state.db"+suffix, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if e == nil {
			extra.Close()
			err = fmt.Errorf("snapshot has SQLite sidecars")
			return
		}
		if !os.IsNotExist(e) {
			err = e
			return
		}
	}
	db, e := snapshotDB(dir, "state.db", true)
	if e != nil {
		err = e
		return
	}
	err = verifySnapshotDB(ctx, db, m, c)
	closeErr := db.Close()
	if err == nil {
		err = closeErr
	}
	return
}

// Snapshot creates an exclusive, standalone SQLite artifact. snapshot.json is
// published last; an interrupted directory is not a valid snapshot.
func (s *Store) Snapshot(ctx context.Context, destination string) (m SnapshotManifest, err error) {
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return m, fmt.Errorf("snapshot destination must be clean and absolute")
	}
	parent, err := safefs.Open(filepath.Dir(destination))
	if err != nil {
		return m, err
	}
	defer parent.Close()
	if err = parent.Capacity(s.config.MaxDatabaseBytes + s.config.MinFreeBytes); err != nil {
		return m, err
	}
	if err = unix.Mkdirat(int(parent.File().Fd()), filepath.Base(destination), 0700); err != nil {
		return m, err
	}
	if err = parent.File().Sync(); err != nil {
		return m, err
	}
	dir, err := parent.Sub(filepath.Base(destination))
	if err != nil {
		return m, err
	}
	defer dir.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err = s.db.ExecContext(ctx, "VACUUM INTO ?", fmt.Sprintf("/proc/self/fd/%d/state.db", dir.File().Fd())); err != nil {
		return m, err
	}
	f, err := dir.Open("state.db", os.O_RDWR, 0)
	if err != nil {
		return m, err
	}
	if err = f.Chmod(0600); err == nil {
		err = f.Sync()
	}
	i, statErr := f.Stat()
	if err == nil {
		err = statErr
	}
	if err == nil {
		m.DatabaseHash, err = safefs.DigestFile(f)
	}
	f.Close()
	if err != nil {
		return m, err
	}
	if i.Size() > s.config.MaxDatabaseBytes {
		return m, fmt.Errorf("snapshot exceeds database budget")
	}
	m.Version, m.Schema, m.Identity, m.ConfigHash, m.DatabaseBytes, m.CreatedAt = 1, currentSchema, s.identity, protocol.Hash(s.config), i.Size(), s.now().UTC()
	m.ID, err = safefs.NewID("n")
	if err != nil {
		return m, err
	}
	db, err := snapshotDB(dir, "state.db", true)
	if err != nil {
		return m, err
	}
	err = verifySnapshotDB(ctx, db, m, s.config)
	closeErr := db.Close()
	if err != nil {
		return m, err
	}
	if closeErr != nil {
		return m, closeErr
	}
	if err = dir.File().Sync(); err != nil {
		return m, err
	}
	err = dir.WriteJSON("snapshot.json", m, false)
	return
}

func copySnapshotFile(source *safefs.Directory, from string, dest *safefs.Directory, to string, m SnapshotManifest) error {
	in, err := snapshotFile(source, from, m.DatabaseBytes)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := dest.Open(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, m.DatabaseBytes+1))
	if err == nil && n != m.DatabaseBytes {
		err = fmt.Errorf("snapshot changed during copy")
	}
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	check, err := snapshotFile(dest, to, m.DatabaseBytes)
	if err != nil {
		return err
	}
	hash, err := safefs.DigestFile(check)
	check.Close()
	if err != nil {
		return err
	}
	if hash != m.DatabaseHash {
		return fmt.Errorf("snapshot changed during copy")
	}
	return dest.File().Sync()
}
