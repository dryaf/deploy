package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
	_ "modernc.org/sqlite"
)

const workerStateSchema = 1

type backupState struct {
	db          *sql.DB
	directory   string
	schemaLease *safefs.Lock
}

type ExpectedInstance struct {
	SourceID         string    `json:"source_id"`
	Alias            string    `json:"alias"`
	Service          string    `json:"service"`
	Generation       string    `json:"generation"`
	BindingHash      string    `json:"binding_hash"`
	Retired          bool      `json:"retired"`
	RetirementReason string    `json:"retirement_reason,omitempty"`
	LastSuccess      time.Time `json:"last_success"`
	CaptureSequence  uint64    `json:"capture_sequence"`
}

type ArchiveControl struct {
	RetentionPasses map[string]AutomaticRetentionPass `json:"retention_passes,omitempty"`
	LatestSnapshots map[string]LatestSnapshot         `json:"latest_snapshots,omitempty"`
	Schema          int                               `json:"schema"`
	ArchiveID       string                            `json:"archive_id"`
	WriterID        string                            `json:"writer_id"`
	Epoch           uint64                            `json:"epoch"`
	Revision        uint64                            `json:"revision"`
	ConfigHash      string                            `json:"config_hash"`
	Instances       map[string]ExpectedInstance       `json:"instances"`
	RetiredSources  map[string]string                 `json:"retired_sources"`
	Pins            map[string]string                 `json:"pins"`
	Retention       *config.RetentionConfig           `json:"retention"`
}

func instanceKey(source, service string) string { return source + "/" + safefs.LockName(service) }

func openBackupState(directory string, create bool) (*backupState, error) {
	return openBackupStateBootstrap(directory, create, false)
}

// resume is permitted only by a durable installation journal that claimed an
// absent state directory before creation. Ordinary opens never initialize it.
func openBackupStateBootstrap(directory string, create, resume bool) (*backupState, error) {
	return openBackupStateWithLease(directory, create, resume, false)
}

func openBackupStateWithLease(directory string, create, resume, installer bool) (*backupState, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	// Never silently initialize an absent or corrupt state database on normal opens.
	if create {
		if err := os.Mkdir(directory, 0700); err != nil && !(resume && os.IsExist(err)) {
			return nil, err
		}
	}
	root, err := safefs.Open(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var schemaLease *safefs.Lock
	if !installer {
		schemaLease, err = root.Lock("schema.lock", false, true)
		if err != nil {
			return nil, err
		}
		defer func() {
			if schemaLease != nil {
				schemaLease.Close()
			}
		}()
		var gate installationGate
		if e := root.ReadJSON("installation.pending", &gate); e == nil {
			return nil, fmt.Errorf("worker installation recovery is pending")
		} else if !os.IsNotExist(e) {
			return nil, e
		}
	}
	info, err := root.File().Stat()
	if err != nil || info.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("worker state directory must be private (0700)")
	}
	file := filepath.Join(directory, "worker.db")
	if create {
		f, err := root.Open("worker.db", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if os.IsExist(err) && resume {
			f, err = root.Open("worker.db", os.O_RDWR, 0)
		}
		if err != nil {
			return nil, err
		}
		if err = f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
		f.Close()
		if err = root.File().Sync(); err != nil {
			return nil, err
		}
	}
	check, err := root.Open("worker.db", os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	fileInfo, statErr := check.Stat()
	check.Close()
	if statErr != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("unsafe worker state database permissions/type")
	}
	if stat, ok := fileInfo.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("unsafe state database owner or hardlink")
	}
	uri := url.URL{Scheme: "file", Path: file}
	q := url.Values{"mode": {"rw"}, "_txlock": {"immediate"}, "_pragma": {"busy_timeout(5000)", "foreign_keys(ON)", "journal_mode(WAL)", "synchronous(FULL)"}}
	uri.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	state := &backupState{db: db, directory: directory}
	good := false
	defer func() {
		if !good {
			db.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, fmt.Errorf("worker state unreadable; explicit recovery required: %w", err)
	}
	if create && !(resume && version == workerStateSchema) {
		if version != 0 {
			return nil, fmt.Errorf("refusing to overwrite existing state schema")
		}
		var tables int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil || tables != 0 {
			return nil, fmt.Errorf("refusing to initialize a nonempty state schema")
		}
		schema := `BEGIN IMMEDIATE;
CREATE TABLE meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE instances(id TEXT PRIMARY KEY,record TEXT NOT NULL);
CREATE TABLE attempts(id TEXT PRIMARY KEY,instance TEXT NOT NULL,status TEXT NOT NULL,record TEXT NOT NULL);
CREATE TABLE bundles(id TEXT PRIMARY KEY,path TEXT NOT NULL,record TEXT NOT NULL);
CREATE TABLE incidents(id TEXT PRIMARY KEY,class TEXT NOT NULL,instance TEXT NOT NULL,opened TEXT NOT NULL,last_observed TEXT NOT NULL,resolved TEXT,message TEXT NOT NULL);
CREATE UNIQUE INDEX active_incident ON incidents(class,instance) WHERE resolved IS NULL;
CREATE TABLE outbox(id TEXT PRIMARY KEY,kind TEXT NOT NULL,created TEXT NOT NULL,due TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,accepted TEXT,covered_by TEXT,record TEXT NOT NULL);
PRAGMA user_version=1; COMMIT;`
		if _, err = db.ExecContext(ctx, schema); err != nil {
			return nil, err
		}
	} else if version != workerStateSchema {
		return nil, fmt.Errorf("unsupported/missing worker state schema; explicit recovery required")
	}
	var result string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil || result != "ok" {
		return nil, fmt.Errorf("worker state integrity could not be established")
	}
	good = true
	state.schemaLease = schemaLease
	schemaLease = nil
	return state, nil
}

func (s *backupState) Close() error {
	err := s.db.Close()
	if s.schemaLease != nil {
		s.schemaLease.Close()
	}
	return err
}

func (s *backupState) get(key string, dest any) error {
	var data string
	if err := s.db.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&data); err != nil {
		return err
	}
	return safefs.DecodeJSON([]byte(data), dest, safefs.MaxRecordBytes)
}

func (s *backupState) put(key string, value any) error {
	data, err := json.Marshal(value)
	if len(data) > safefs.MaxRecordBytes {
		return fmt.Errorf("worker state record exceeds its supported bound")
	}
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(data))
	return err
}

func (s *backupState) recordAttempt(id, instance, status string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO attempts(id,instance,status,record) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,record=excluded.record", id, instance, status, string(data))
	return err
}
