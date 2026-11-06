package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	_ "modernc.org/sqlite"
)

type Inbox struct {
	runtimeAdmission func(context.Context) error
	releaseSource    func()

	installation *SignalRuntimeInstallation // Verification-only recovery installation; never a receiver.
	db           *sql.DB
	dir          *safefs.Directory
	lease        *safefs.Lock
	mu           sync.Mutex
	identity     protocol.Identity
	account      string
	baseline     *interaction.SignalRecoveryCursor
	ready        bool
}
type inboxPin struct {
	Identity          protocol.Identity `json:"identity"`
	Account           string            `json:"account"`
	RecoveryOperation string            `json:"recovery_operation,omitempty"`
	Baseline          string            `json:"baseline,omitempty"`
}

// OpenInbox never recreates missing state during ordinary service startup.
// Creation is a separate, explicit local installation action.
func OpenInbox(path, account string, create bool) (*Inbox, error) {
	return openInbox(path, account, create, nil)
}

func openInbox(path, account string, create bool, installation *SignalRuntimeInstallation) (_ *Inbox, err error) {
	return openInboxLeased(path, account, create, installation, nil, nil)
}

func openInboxLeased(path, account string, create bool, installation *SignalRuntimeInstallation, adoptedDir *safefs.Directory, adoptedLease *safefs.Lock) (_ *Inbox, err error) {
	if !protocol.ValidToken(account) {
		return nil, fmt.Errorf("invalid relay account")
	}
	dir := adoptedDir
	if dir == nil {
		dir, err = safefs.Open(path)
		if err != nil {
			return nil, err
		}
	}
	s := &Inbox{dir: dir, account: account, installation: installation, lease: adoptedLease}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	if err = s.checkRuntimeFence(); err != nil {
		return nil, err
	}
	info, err := dir.File().Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("inbox directory must be private")
	}
	if create {
		for _, name := range []string{"inbox.db", "identity.json", signalRuntimeFile} {
			f, e := dir.Open(name, os.O_RDONLY, 0)
			if e == nil {
				f.Close()
				return nil, fmt.Errorf("relay inbox state already exists")
			}
			if !os.IsNotExist(e) {
				return nil, e
			}
		}
	}
	if s.lease == nil {
		s.lease, err = dir.Lock("inbox.lock", true, create)
		if err != nil {
			return nil, err
		}
	}
	if err = s.checkRuntimeFence(); err != nil {
		return nil, err
	}
	flags := os.O_RDWR
	mode := uint32(0)
	if create {
		flags |= os.O_CREATE | os.O_EXCL
		mode = 0600
	}
	f, err := dir.Open("inbox.db", flags, mode)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok = info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0600 {
		f.Close()
		return nil, fmt.Errorf("unsafe inbox database")
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return nil, err
	}
	if err = dir.File().Sync(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(dir.File().Name(), "inbox.db")}
	u.RawQuery = url.Values{"mode": {"rw"}, "_txlock": {"immediate"}, "_pragma": {"busy_timeout(1500)", "journal_mode(WAL)", "synchronous(FULL)"}}.Encode()
	s.db, err = sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(1)
	if create {
		s.identity.Worker, err = safefs.NewID("inbox")
		if err != nil {
			return nil, err
		}
		s.identity.Recovery, err = safefs.NewID("epoch")
		if err != nil {
			return nil, err
		}
		pin := inboxPin{Identity: s.identity, Account: account}
		if err = dir.WriteJSON("identity.json", pin, false); err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(pin)
		err = s.transaction(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec("CREATE TABLE identity(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL,acked INTEGER NOT NULL DEFAULT 0,listed INTEGER NOT NULL DEFAULT 0);CREATE TABLE events(position INTEGER PRIMARY KEY AUTOINCREMENT,key TEXT NOT NULL UNIQUE,hash TEXT NOT NULL,body TEXT NOT NULL);PRAGMA user_version=1;")
			if err != nil {
				return err
			}
			_, err = tx.Exec("INSERT INTO identity(singleton,body) VALUES(1,?)", string(raw))
			return err
		})
		if err != nil {
			return nil, err
		}
	} else {
		var pin inboxPin
		if err = dir.ReadJSON("identity.json", &pin); err != nil {
			return nil, err
		}
		if !pin.Identity.Valid() || pin.Account != account || pin.RecoveryOperation != "" {
			return nil, fmt.Errorf("inbox identity mismatch")
		}
		s.identity = pin.Identity
		var stored, check string
		var version int
		if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || (version != 1 && version != 2 && version != runtimeInboxSchema) {
			return nil, fmt.Errorf("unsupported inbox schema")
		}
		if version == 2 {
			var count int
			if err = s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('recovery_exports','recovery_records') AND type='table'").Scan(&count); err != nil || count != 2 {
				return nil, fmt.Errorf("relay recovery history missing")
			}
		}
		if err = s.db.QueryRow("SELECT body FROM identity WHERE singleton=1").Scan(&stored); err != nil {
			return nil, err
		}
		var prior inboxPin
		if err = protocol.Decode([]byte(stored), &prior, 4096); err != nil || prior != pin {
			return nil, fmt.Errorf("inbox restore requires reconciliation")
		}
		if err = s.db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
			return nil, fmt.Errorf("inbox integrity failure")
		}
	}
	pin := inboxPin{Identity: s.identity, Account: s.account}
	if err = dir.ReadJSON("identity.json", &pin); err != nil {
		return nil, err
	}
	if s.baseline, err = readInboxBaseline(s.db, pin); err != nil {
		return nil, err
	}
	if err = s.checkBaselineFile(); err != nil {
		return nil, err
	}
	if err = s.checkBaseline(s.db); err != nil {
		return nil, err
	}
	if err = s.verifyBaselineHistory(context.Background()); err != nil {
		return nil, err
	}
	s.ready = true
	return s, nil
}
func (s *Inbox) Close() error {
	if s == nil {
		return nil
	}
	var err error
	if s.releaseSource != nil {
		defer s.releaseSource()
		s.releaseSource = nil
	}
	if s.db != nil {
		err = s.db.Close()
		s.db = nil
	}
	if s.lease != nil {
		s.lease.Close()
		s.lease = nil
	}
	if s.dir != nil {
		s.dir.Close()
		s.dir = nil
	}
	return err
}
func (s *Inbox) Cursor() interaction.SpoolCursor {
	return interaction.SpoolCursor{Identity: s.identity, Account: s.account, Position: s.baselinePosition()}
}
func (s *Inbox) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtimeAdmission != nil {
		if err := s.runtimeAdmission(ctx); err != nil {
			return err
		}
	}
	if err := s.checkLease(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.ready {
		if err = s.checkBaseline(tx); err != nil {
			return err
		}
	}
	if err = fn(tx); err != nil {
		return err
	}
	if err = s.checkLease(); err != nil {
		return err
	}
	if s.ready {
		if err = s.checkBaseline(tx); err != nil {
			return err
		}
	}
	if s.runtimeAdmission != nil {
		if err := s.runtimeAdmission(ctx); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Inbox) checkLease() error {
	if s.lease == nil || s.lease.File() == nil {
		return fmt.Errorf("relay inbox lease missing")
	}
	held, err := s.lease.File().Stat()
	if err != nil {
		return err
	}
	f, err := s.dir.Open("inbox.lock", os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(held, current) {
		return fmt.Errorf("relay inbox lease replaced")
	}
	if err = s.checkRuntimeFence(); err != nil {
		return err
	}
	var pin inboxPin
	if err = s.dir.ReadJSON("identity.json", &pin); err != nil {
		return err
	}
	expectedBaseline := ""
	if s.baseline != nil {
		expectedBaseline = protocol.Hash(*s.baseline)
	}
	if pin.Identity != s.identity || pin.Account != s.account || pin.RecoveryOperation != "" || pin.Baseline != expectedBaseline {
		return fmt.Errorf("relay inbox identity changed or recovery fenced")
	}
	return s.checkBaselineFile()
}

func (s *Inbox) CheckActive(ctx context.Context) error {
	if s.installation != nil && s.runtimeAdmission == nil {
		return fmt.Errorf("Signal intake unavailable in verification mode")
	}
	return s.transaction(ctx, func(*sql.Tx) error { return nil })
}
func (s *Inbox) capacity(tx *sql.Tx, reserve int) error {
	var count int
	if err := tx.QueryRow("SELECT count(*) FROM events WHERE position>max(?,(SELECT acked FROM identity WHERE singleton=1))", s.baselinePosition()).Scan(&count); err != nil {
		return err
	}
	if count+reserve > 1024 {
		return fmt.Errorf("relay inbox full")
	}
	var pages, pageSize int64
	if err := tx.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		return err
	}
	if err := tx.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		return err
	}
	if pages*pageSize+int64(reserve)*(16<<10) > 32<<20 {
		return fmt.Errorf("relay history budget exhausted")
	}
	var disk syscall.Statfs_t
	if err := syscall.Fstatfs(int(s.dir.File().Fd()), &disk); err != nil {
		return err
	}
	if disk.Bavail*uint64(disk.Bsize) < 8<<20 {
		return fmt.Errorf("relay disk reserve exhausted")
	}
	return nil
}
func (s *Inbox) CanReceive(ctx context.Context) error {
	if s.installation != nil && s.runtimeAdmission == nil {
		return fmt.Errorf("Signal intake unavailable in verification mode")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error { return s.capacity(tx, 16) })
}
func (s *Inbox) Enqueue(ctx context.Context, e interaction.SpoolEvent) error {
	if s.installation != nil && s.runtimeAdmission == nil {
		return fmt.Errorf("Signal intake unavailable in verification mode")
	}
	if e.Position != 0 || !protocol.ValidToken(e.Key) || !safefs.DigestPattern.MatchString(e.Hash) {
		return fmt.Errorf("invalid relay event")
	}
	if e.Inbound != nil && (e.Inbound.Provider != "signal" || e.Inbound.Account != s.account || e.Inbound.NativeID != e.Key || len(e.Inbound.Text) > 4096) {
		return fmt.Errorf("relay event identity mismatch")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var hash string
		err := tx.QueryRow("SELECT hash FROM events WHERE key=?", e.Key).Scan(&hash)
		if err == nil {
			if hash != e.Hash {
				return fmt.Errorf("relay event conflict")
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if err = s.capacity(tx, 1); err != nil {
			return err
		}
		body, _ := json.Marshal(e)
		if len(body) > 16384 {
			return fmt.Errorf("relay event too large")
		}
		_, err = tx.Exec("INSERT INTO events(key,hash,body) VALUES(?,?,?)", e.Key, e.Hash, string(body))
		return err
	})
}
func (s *Inbox) List(ctx context.Context, c interaction.SpoolCursor) (page interaction.SpoolPage, err error) {
	if s.installation != nil && s.runtimeAdmission == nil {
		return page, fmt.Errorf("Signal intake unavailable in verification mode")
	}
	if c.Identity != s.identity || c.Account != s.account {
		return page, fmt.Errorf("relay cursor identity mismatch")
	}
	page.Cursor = c
	page.Events = []interaction.SpoolEvent{}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var acked, listed uint64
		if err := tx.QueryRow("SELECT acked,listed FROM identity WHERE singleton=1").Scan(&acked, &listed); err != nil {
			return err
		}
		if c.Position < max(acked, s.baselinePosition()) || c.Position > max(listed, s.baselinePosition()) {
			return fmt.Errorf("relay cursor outside retained boundary")
		}
		rows, err := tx.Query("SELECT position,key,hash,body FROM events WHERE position>? ORDER BY position LIMIT 16", c.Position)
		if err != nil {
			return err
		}
		bytes := 0
		for rows.Next() {
			var e interaction.SpoolEvent
			var body, key, hash string
			var position uint64
			if err = rows.Scan(&position, &key, &hash, &body); err != nil {
				rows.Close()
				return err
			}
			if bytes+len(body) > protocol.MaxRequest-2048 {
				break
			}
			bytes += len(body)
			if err = protocol.Decode([]byte(body), &e, 16384); err != nil {
				rows.Close()
				return err
			}
			if position != page.Cursor.Position+1 || e.Position != 0 || e.Key != key || e.Hash != hash {
				rows.Close()
				return fmt.Errorf("relay listed event changed or stream gap")
			}
			e.Position = position
			if s.baseline != nil {
				if err = interaction.ValidateSignalRecoveryEvent(e, c); err != nil {
					rows.Close()
					return err
				}
			}
			page.Events = append(page.Events, e)
			page.Cursor.Position = position
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page.Events) == 0 {
			return nil
		}
		_, err = tx.Exec("UPDATE identity SET listed=max(listed,?) WHERE singleton=1", page.Cursor.Position)
		return err
	})
	return
}
func (s *Inbox) Ack(ctx context.Context, c interaction.SpoolCursor) error {
	if s.installation != nil && s.runtimeAdmission == nil {
		return fmt.Errorf("Signal intake unavailable in verification mode")
	}
	if c.Identity != s.identity || c.Account != s.account {
		return fmt.Errorf("relay ACK identity mismatch")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var acked, listed uint64
		if err := tx.QueryRow("SELECT acked,listed FROM identity WHERE singleton=1").Scan(&acked, &listed); err != nil {
			return err
		}
		if c.Position < max(acked, s.baselinePosition()) || c.Position > max(listed, s.baselinePosition()) {
			return fmt.Errorf("relay ACK outside listed boundary")
		}
		if s.baseline != nil && c.Position == s.baselinePosition() {
			return nil
		}
		_, err := tx.Exec("UPDATE identity SET acked=? WHERE singleton=1", c.Position)
		return err
	})
}
