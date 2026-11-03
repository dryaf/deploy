package relay

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

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

const inboxSnapshotLimit int64 = 32 << 20

type InboxSnapshotManifest struct {
	HistoricalEvents              uint64 `json:",omitempty"`
	Version, Schema               int
	ID, Account                   string
	Identity                      protocol.Identity
	DatabaseHash, EventsDigest    string
	DatabaseBytes                 int64
	Highest, Acknowledged, Listed uint64
	CreatedAt                     time.Time
	BaselineHash                  string `json:",omitempty"`
	BaselinePosition              uint64 `json:",omitempty"`
	HistoricalHighest             uint64 `json:",omitempty"`
	RetainedEvents                uint64 `json:",omitempty"`
}

func privateInboxDirectory(path string) (*safefs.Directory, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("relay path must be clean and absolute")
	}
	d, err := safefs.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := d.File().Stat()
	if err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
			err = fmt.Errorf("relay directory must be private and owned")
		}
	}
	if err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}
func inboxArtifactFile(d *safefs.Directory, name string, limit int64) (*os.File, error) {
	f, err := d.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	i, err := f.Stat()
	if err == nil {
		st, ok := i.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || !i.Mode().IsRegular() || i.Mode().Perm() != 0600 || i.Size() <= 0 || i.Size() > limit {
			err = fmt.Errorf("unsafe relay artifact database")
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func inboxArtifactDB(d *safefs.Directory, name string, readOnly bool) (*sql.DB, error) {
	mode, lock := "rw", "immediate"
	if readOnly {
		mode, lock = "ro", "deferred"
	}
	u := url.URL{Scheme: "file", Path: fmt.Sprintf("/proc/self/fd/%d/%s", d.File().Fd(), name)}
	params := url.Values{"mode": {mode}, "_txlock": {lock}, "_pragma": {"busy_timeout(1500)", "synchronous(FULL)", "trusted_schema(OFF)"}}
	// These are offline artifacts, protected by their caller's lease. Even a
	// read-only SQLite connection can create WAL/SHM files. With no sidecars,
	// immutable mode reads the durable main file without changing the directory.
	if readOnly {
		standalone := true
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			f, e := d.Open(name+suffix, os.O_RDONLY|unix.O_NONBLOCK, 0)
			if e == nil {
				f.Close()
				standalone = false
			} else if !os.IsNotExist(e) {
				return nil, e
			}
		}
		if standalone {
			params.Set("immutable", "1")
		}
	}
	u.RawQuery = params.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}
func inboxEventDigest(ctx context.Context, db *sql.DB, account string, highest uint64) (string, error) {
	rows, err := db.QueryContext(ctx, "SELECT position,key,hash,body FROM events ORDER BY position")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var position uint64
		var key, hash, body string
		var e interaction.SpoolEvent
		if err = rows.Scan(&position, &key, &hash, &body); err != nil {
			return "", err
		}
		if err = protocol.Decode([]byte(body), &e, 16384); err != nil {
			return "", err
		}
		if position != uint64(len(hashes))+1 || position > highest || e.Position != 0 || e.Key != key || e.Hash != hash {
			return "", fmt.Errorf("relay event history damaged")
		}
		e.Position = position
		if err = interaction.ValidateSignalRecoveryEvent(e, interaction.SpoolCursor{Account: account}); err != nil {
			return "", err
		}
		hashes = append(hashes, protocol.Hash(e))
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if uint64(len(hashes)) != highest {
		return "", fmt.Errorf("relay event history incomplete")
	}
	return interaction.RecoveryEvidenceDigest(hashes), nil
}
func inspectInboxArtifact(ctx context.Context, db *sql.DB) (m InboxSnapshotManifest, err error) {
	var check, body string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return m, fmt.Errorf("relay artifact integrity unavailable")
	}
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&m.Schema); err != nil || (m.Schema != 1 && m.Schema != 2 && m.Schema != runtimeInboxSchema) {
		return m, fmt.Errorf("unsupported relay artifact schema")
	}
	if m.Schema == 2 {
		var count int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name IN ('recovery_exports','recovery_records') AND type='table'").Scan(&count); err != nil || count != 2 {
			return m, fmt.Errorf("relay artifact lost recovery history")
		}
	}
	if err = db.QueryRowContext(ctx, "SELECT body,acked,listed FROM identity WHERE singleton=1").Scan(&body, &m.Acknowledged, &m.Listed); err != nil {
		return
	}
	var pin inboxPin
	if err = protocol.Decode([]byte(body), &pin, 4096); err != nil {
		return
	}
	if !pin.Identity.Valid() || !protocol.ValidToken(pin.Account) || pin.RecoveryOperation != "" {
		return m, fmt.Errorf("relay artifact identity invalid or fenced")
	}
	m.Identity, m.Account = pin.Identity, pin.Account
	if err = db.QueryRowContext(ctx, "SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='events'),0)").Scan(&m.Highest); err != nil {
		return
	}
	if m.Schema == runtimeInboxSchema {
		err = inspectBaselineSnapshot(ctx, db, &m, pin)
		return
	}
	m.Version = 1
	if err = m.validateLayout(); err != nil {
		return
	}
	if pin.Baseline != "" {
		return m, fmt.Errorf("legacy relay artifact has a baseline pin")
	}
	m.EventsDigest, err = inboxEventDigest(ctx, db, m.Account, m.Highest)
	return
}

// SnapshotInbox takes the normal exclusive relay lease and publishes a new
// standalone artifact. It never stops a running receiver or overwrites an artifact.
func SnapshotInbox(ctx context.Context, path, account, destination string) (m InboxSnapshotManifest, err error) {
	s, err := OpenInbox(path, account, false)
	if err != nil {
		return m, err
	}
	defer s.Close()
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return m, fmt.Errorf("relay snapshot destination must be clean and absolute")
	}
	parent, err := safefs.Open(filepath.Dir(destination))
	if err != nil {
		return m, err
	}
	defer parent.Close()
	if err = parent.Capacity(inboxSnapshotBudget(runtimeInboxSchema) + (8 << 20)); err != nil {
		return
	}
	if err = unix.Mkdirat(int(parent.File().Fd()), filepath.Base(destination), 0700); err != nil {
		return
	}
	if err = parent.File().Sync(); err != nil {
		return
	}
	dir, err := parent.Sub(filepath.Base(destination))
	if err != nil {
		return m, err
	}
	defer dir.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.checkLease(); err != nil {
		return
	}
	if _, err = s.db.ExecContext(ctx, "VACUUM INTO ?", fmt.Sprintf("/proc/self/fd/%d/inbox.db", dir.File().Fd())); err != nil {
		return
	}
	f, err := dir.Open("inbox.db", os.O_RDWR, 0)
	if err != nil {
		return m, err
	}
	err = f.Chmod(0600)
	f.Close()
	if err != nil {
		return
	}
	db, err := inboxArtifactDB(dir, "inbox.db", false)
	if err != nil {
		return m, err
	}
	var mode string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err == nil && mode != "delete" {
		err = fmt.Errorf("relay snapshot not standalone")
	}
	if err == nil {
		m, err = inspectInboxArtifact(ctx, db)
	}
	closeErr := db.Close()
	if err != nil {
		return
	}
	if closeErr != nil {
		return m, closeErr
	}
	expectedBaseline := ""
	if s.baseline != nil {
		expectedBaseline = protocol.Hash(*s.baseline)
	}
	if m.Identity != s.identity || m.Account != s.account || m.BaselineHash != expectedBaseline {
		return m, fmt.Errorf("relay snapshot identity changed")
	}
	f, err = inboxArtifactFile(dir, "inbox.db", inboxSnapshotBudget(m.Schema))
	if err != nil {
		return m, err
	}
	i, err := f.Stat()
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		m.DatabaseHash, err = safefs.DigestFile(f)
	}
	f.Close()
	if err != nil {
		return
	}
	m.DatabaseBytes = i.Size()
	m.CreatedAt = time.Now().UTC()
	if m.ID, err = safefs.NewID("n"); err != nil {
		return
	}
	if err = s.checkLease(); err != nil {
		return
	}
	if err = dir.File().Sync(); err != nil {
		return
	}
	if err = m.validate(account); err != nil {
		return
	}
	err = dir.WriteJSON("snapshot.json", m, false)
	return
}
func VerifyInboxSnapshot(ctx context.Context, path, account string) (InboxSnapshotManifest, error) {
	m, dir, err := readInboxSnapshot(ctx, path, account)
	if dir != nil {
		dir.Close()
	}
	return m, err
}
func readInboxSnapshot(ctx context.Context, path, account string) (m InboxSnapshotManifest, dir *safefs.Directory, err error) {
	dir, err = privateInboxDirectory(path)
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
	if err = m.validate(account); err != nil {
		return
	}
	if err = verifyStandaloneInboxFile(dir, "inbox.db", m.DatabaseBytes, m.DatabaseHash); err != nil {
		return
	}
	db, err := inboxArtifactDB(dir, "inbox.db", true)
	if err != nil {
		return
	}
	defer db.Close()
	observed, err := inspectInboxArtifact(ctx, db)
	if err != nil {
		return
	}
	if observed.Schema != m.Schema || observed.Identity != m.Identity || observed.Account != m.Account || observed.Highest != m.Highest ||
		observed.Version != m.Version || observed.Acknowledged != m.Acknowledged || observed.Listed != m.Listed || observed.EventsDigest != m.EventsDigest || observed.BaselineHash != m.BaselineHash || observed.BaselinePosition != m.BaselinePosition || observed.HistoricalHighest != m.HistoricalHighest || observed.HistoricalEvents != m.HistoricalEvents || observed.RetainedEvents != m.RetainedEvents {
		err = fmt.Errorf("relay snapshot evidence mismatch")
	}
	return
}
func verifyStandaloneInboxFile(dir *safefs.Directory, name string, size int64, hash string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		f, err := dir.Open(name+suffix, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			f.Close()
			return fmt.Errorf("relay artifact has unbound SQLite sidecars")
		}
		if !os.IsNotExist(err) {
			return err
		}
	}
	f, err := inboxArtifactFile(dir, name, size)
	if err != nil {
		return err
	}
	var header [20]byte
	_, err = f.ReadAt(header[:], 0)
	f.Close()
	if err != nil || string(header[:16]) != "SQLite format 3\x00" || header[18] != 1 || header[19] != 1 {
		return fmt.Errorf("relay artifact is not a standalone rollback-mode database")
	}
	return verifyInboxFile(dir, name, size, hash)
}
func verifyInboxFile(dir *safefs.Directory, name string, size int64, hash string) error {
	f, err := inboxArtifactFile(dir, name, size)
	if err != nil {
		return err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return err
	}
	if i.Size() != size {
		return fmt.Errorf("relay artifact length changed")
	}
	actual, err := safefs.DigestFile(f)
	if err != nil {
		return err
	}
	if actual != hash {
		return fmt.Errorf("relay artifact digest changed")
	}
	return nil
}
func copyInboxArtifact(source *safefs.Directory, from string, dest *safefs.Directory, to string, m InboxSnapshotManifest) error {
	in, err := inboxArtifactFile(source, from, m.DatabaseBytes)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := dest.Open(to, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(in, m.DatabaseBytes+1))
	if err != nil {
		return err
	}
	if n != m.DatabaseBytes {
		return fmt.Errorf("relay artifact changed while copying")
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = verifyStandaloneInboxFile(dest, to, m.DatabaseBytes, m.DatabaseHash); err != nil {
		return err
	}
	return dest.File().Sync()
}

// Presence alone fences startup, even if the restore journal is damaged.
func checkInboxRestoreFence(dir *safefs.Directory) error {
	for _, name := range []string{runtimeInstallIntent, runtimeInstallSeal, runtimeInstallComplete} {
		if err := absentRuntimeFile(dir, name); err != nil {
			return fmt.Errorf("Signal runtime installation requires managed verification: %w", err)
		}
	}
	f, err := dir.Open("restore.json", os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err == nil {
		f.Close()
		return fmt.Errorf("relay restore requires reconciliation")
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}
