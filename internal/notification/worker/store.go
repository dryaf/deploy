package worker

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
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

type Store struct {
	runtimeLEDAPI          LEDAPI
	runtimeLEDCheck        func(context.Context) error
	managedCommandPolicy   *interaction.Config
	managedCommandMode     string
	managedCommandHash     string
	managedCommandCheck    func(context.Context) error
	deliveryActivationHash string
	signalProfilesHash     string
	signalProfileRoutes    map[string]Route
	verificationOnly       bool // Immutable for this open instance; set before exposing any runtime API.
	installationRevision   string
	recoveryCommitHash     string
	recoveryProof          RecoveryCheckpointProof
	recoveryPublication    RecoveryPublicationCommit
	telegramRenewalID      string
	freshSignalServiceHash string
	signalVerificationHash string
	ledMu                  sync.Mutex         // Serializes LED attempts, independent of database state lock.
	ledOperationFault      func(string) error // Private durable-boundary test hook.
	db                     *sql.DB
	dir                    *safefs.Directory
	runtimeEvidenceDir     *safefs.Directory // Read-only ancestor audit overlay; never a runtime authority.
	lease                  *safefs.Lock
	mu                     sync.Mutex // Only local state transitions, never external I/O.
	identity               protocol.Identity
	config                 Config
	session                string
	now                    func() time.Time
	clockLast              time.Time
	clockUncertain         bool
	scanFloor              map[string]uint64
	commandScanFloor       map[string]int64
	runtimeLoops           map[string]RuntimeLoopHealth
	ledObservation         *LEDObservation
	ledOutputs             map[string]LEDObservation
	ownershipFault         func(string) error // Private durable-boundary test hook.
}

const schema = `
CREATE TABLE metadata(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE producers(id TEXT PRIMARY KEY,epoch TEXT NOT NULL,submitted INTEGER NOT NULL DEFAULT 0,acked INTEGER NOT NULL DEFAULT 0,listed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE messages(producer TEXT NOT NULL,id TEXT NOT NULL,seq INTEGER NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,generation TEXT NOT NULL,route_revision TEXT NOT NULL,state TEXT NOT NULL,attempt TEXT NOT NULL DEFAULT '',provider TEXT NOT NULL DEFAULT '',due INTEGER NOT NULL DEFAULT 0,tries INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(producer,id),UNIQUE(producer,seq));
CREATE TABLE receipts(position INTEGER PRIMARY KEY AUTOINCREMENT,producer TEXT NOT NULL,body TEXT NOT NULL);
CREATE INDEX receipt_producer ON receipts(producer,position);
CREATE TABLE status(producer TEXT PRIMARY KEY,seq INTEGER NOT NULL,basis INTEGER NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,received INTEGER NOT NULL,session TEXT NOT NULL);
CREATE TABLE subjects(producer TEXT NOT NULL,id TEXT NOT NULL,body TEXT NOT NULL,latched TEXT NOT NULL DEFAULT '',PRIMARY KEY(producer,id));
CREATE TABLE incidents(producer TEXT NOT NULL,subject TEXT NOT NULL,id TEXT NOT NULL,revision INTEGER NOT NULL,attempt TEXT NOT NULL DEFAULT '',PRIMARY KEY(producer,subject,id));
CREATE TABLE reports(producer TEXT NOT NULL,key TEXT NOT NULL,revision INTEGER NOT NULL,PRIMARY KEY(producer,key));
CREATE TABLE led_states(id TEXT PRIMARY KEY,condition TEXT NOT NULL,obsolete INTEGER NOT NULL DEFAULT 0);
CREATE TABLE provider_cooldowns(provider TEXT PRIMARY KEY,due INTEGER NOT NULL);
PRAGMA user_version=1;`

// Open requires an existing private directory. create exclusively claims a new
// database; an ordinary open never initializes missing state.
func Open(path string, c Config, create bool) (_ *Store, err error) {
	return openWithOwnershipFault(path, c, create, nil)
}

// OpenVerification retains the normal exclusive instance lease and durable API,
// but cannot grant delivery or command execution. Leaving verification requires
// closing this instance; managed activation must separately qualify its successor.
func OpenVerification(path string, c Config) (*Store, error) {
	s, err := Open(path, c, false)
	if err != nil {
		return nil, err
	}
	s.verificationOnly = true
	return s, nil
}

func (s *Store) VerificationOnly() bool { return s.verificationOnly }

func openWithOwnershipFault(path string, c Config, create bool, fault func(string) error) (_ *Store, err error) {
	return openStore(path, c, create, fault, false, protocol.Identity{}, "")
}

// OpenManagedVerification refuses implicit schema upgrades. A managed service or
// installer must use an explicit migration journal before adopting older state.
func OpenManagedVerification(path string, c Config, expected protocol.Identity) (*Store, error) {
	if !expected.Valid() {
		return nil, fmt.Errorf("managed open requires a pinned worker identity")
	}
	s, err := openStore(path, c, false, nil, true, expected, "")
	if err == nil {
		s.verificationOnly = true
	}
	return s, err
}

// OpenManagedRuntime marks health with the exact installation revision. The
// caller must already hold that installation's shared lease and validate pins.
func OpenManagedRuntime(path string, c Config, expected protocol.Identity, revision string) (*Store, error) {
	if !safefs.ValidID(revision, "n") {
		return nil, fmt.Errorf("invalid managed runtime revision")
	}
	s, err := OpenManagedVerification(path, c, expected)
	if err == nil {
		s.installationRevision = revision
	}
	return s, err
}

func openStore(path string, c Config, create bool, fault func(string) error, requireCurrent bool, expected protocol.Identity, recoveryCommit string) (*Store, error) {
	return openStoreWithBootstrap(path, c, create, fault, requireCurrent, expected, recoveryCommit, nil)
}

func openStoreWithBootstrap(path string, c Config, create bool, fault func(string) error, requireCurrent bool, expected protocol.Identity, recoveryCommit string, bootstrap *GatewayBootstrap) (*Store, error) {
	return openStoreWithBootstrapEnrollment(path, c, create, fault, requireCurrent, expected, recoveryCommit, bootstrap, nil)
}

func openStoreWithBootstrapEnrollment(path string, c Config, create bool, fault func(string) error, requireCurrent bool, expected protocol.Identity, recoveryCommit string, bootstrap *GatewayBootstrap, enrollment *bootstrapEnrollment) (_ *Store, err error) {
	return openStoreWithBootstrapTransitions(path, c, create, fault, requireCurrent, expected, recoveryCommit, bootstrap, enrollment, nil)
}

func openStoreWithBootstrapTransitions(path string, c Config, create bool, fault func(string) error, requireCurrent bool, expected protocol.Identity, recoveryCommit string, bootstrap *GatewayBootstrap, enrollment *bootstrapEnrollment, limits *bootstrapLimits) (_ *Store, err error) {
	return openStoreWithPolicyTransitions(path, c, create, fault, requireCurrent, expected, recoveryCommit, bootstrap, enrollment, limits, nil)
}

func openStoreWithPolicyTransitions(path string, c Config, create bool, fault func(string) error, requireCurrent bool, expected protocol.Identity, recoveryCommit string, bootstrap *GatewayBootstrap, enrollment *bootstrapEnrollment, limits *bootstrapLimits, enrolledPolicy *enrolledLimits) (_ *Store, err error) {
	if bootstrap != nil {
		err = c.ValidateBootstrap()
	} else {
		err = c.Validate()
	}
	if err != nil {
		return nil, err
	}
	d, err := safefs.Open(path)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: d, config: c, now: time.Now, ownershipFault: fault, verificationOnly: recoveryCommit != "", recoveryCommitHash: recoveryCommit}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	info, err := d.File().Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("state directory must be owned and private")
	}
	if bootstrap != nil {
		var retained GatewayBootstrap
		if err = d.ReadJSON("bootstrap.json", &retained); err != nil || protocol.Hash(retained) != protocol.Hash(bootstrap) || bootstrap.validate() != nil {
			return nil, fmt.Errorf("unbound gateway reservation changed or unavailable")
		}
		s.verificationOnly = true
	}
	if create {
		// Exclusively claim the instance inode. Reopening or retrying a failed
		// initialization must never replace a missing live worker's lock.
		for _, name := range []string{"state.db", "identity.json"} {
			existing, e := d.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
			if e == nil {
				existing.Close()
				return nil, fmt.Errorf("refusing to initialize existing notification state")
			}
			if !os.IsNotExist(e) {
				return nil, e
			}
		}
		claim, e := d.Open("worker.lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		e = claim.Sync()
		closeErr := claim.Close()
		if e != nil {
			return nil, e
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if e = d.File().Sync(); e != nil {
			return nil, e
		}
	}
	s.lease, err = d.Lock("worker.lock", true, false)
	if err != nil {
		return nil, err
	}
	enrolledTransition, e := checkEnrolledLimitsAdmission(d, c, enrolledPolicy)
	if e != nil {
		return nil, e
	}
	limitsTransition, limitsErr := checkBootstrapLimitsAdmission(d, c, bootstrap, limits)
	if limitsErr != nil {
		return nil, limitsErr
	}
	bootstrapTransition, admissionErr := checkBootstrapEnrollmentAdmission(d, c, bootstrap, enrollment)
	if admissionErr != nil {
		return nil, admissionErr
	}
	if !requireCurrent {
		if bootstrapTransition != nil && bootstrapTransition.Receipt.Version == 3 || enrolledTransition != nil && c.Interaction != nil {
			return nil, fmt.Errorf("enrolled command policy requires managed admission")
		}
		f, e := d.Open(managedDeliveryOwnerFile, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if e == nil {
			f.Close()
			return nil, fmt.Errorf("state is enrolled in managed delivery; use its installation lifecycle")
		}
		if !os.IsNotExist(e) {
			return nil, e
		}
	}
	var recoveryProof RecoveryCheckpointProof
	if recoveryCommit != "" {
		if err = absentOriginalFile(d, nextRecoveryName(recoveryCommit)); err != nil {
			return nil, fmt.Errorf("next recovery is prepared: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		s.recoveryPublication, recoveryProof, err = verifyCompletedPublication(ctx, d, s.lease, c, recoveryCommit)
		if err != nil {
			return nil, err
		}
		if recoveryProof.After != expected {
			return nil, fmt.Errorf("managed recovery identity mismatch")
		}
		s.recoveryProof = recoveryProof
		want := RecoveryVerification{1, recoveryCommit, protocol.Hash(c), recoveryProof.Before, recoveryProof.After, true}
		for _, name := range []string{recoveryVerificationIntent, recoveryVerificationComplete} {
			ok, e := verificationRecord(d, name, want)
			if e != nil {
				return nil, e
			}
			if !ok {
				return nil, fmt.Errorf("managed recovery adoption incomplete")
			}
		}
		if err = verifyRecoveryRuntimeFiles(d, c); err != nil {
			return nil, err
		}
		readDB, e := snapshotDB(d, "state.db", true)
		if e != nil {
			return nil, e
		}
		e = verifyRecoveryRuntimeSQL(ctx, readDB, recoveryProof)
		closeErr := readDB.Close()
		if e != nil {
			return nil, e
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else {
		if gate, e := d.Open(publicationCommitIntent, os.O_RDONLY|unix.O_NONBLOCK, 0); e == nil {
			gate.Close()
			return nil, fmt.Errorf("published recovery remains quarantined; managed activation required")
		} else if !os.IsNotExist(e) {
			return nil, e
		}
		if gate, e := d.Open("recovery.json", os.O_RDONLY|unix.O_NONBLOCK, 0); e == nil {
			gate.Close()
			return nil, fmt.Errorf("notification recovery is quarantined; reconciliation required")
		} else if !os.IsNotExist(e) {
			return nil, e
		}
	}
	if gate, e := d.Open("schema-migration.json", os.O_RDONLY|unix.O_NONBLOCK, 0); e == nil {
		gate.Close()
		return nil, fmt.Errorf("schema migration requires managed resume")
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if gate, e := d.Open("led-migration.json", os.O_RDONLY|unix.O_NONBLOCK, 0); e == nil {
		gate.Close()
		return nil, fmt.Errorf("LED binding migration requires resume")
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	flags := os.O_RDWR
	mode := uint32(0)
	if create {
		flags |= os.O_CREATE | os.O_EXCL
		mode = 0600
	}
	f, err := d.Open("state.db", flags, mode)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	fs, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || fs.Uid != uint32(os.Getuid()) || fi.Mode().Perm() != 0600 {
		f.Close()
		return nil, fmt.Errorf("unsafe database")
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return nil, err
	}
	if err = d.File().Sync(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(d.File().Name(), "state.db")}
	u.RawQuery = url.Values{"mode": {"rw"}, "_txlock": {"immediate"}, "_pragma": {"busy_timeout(1500)", "journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)"}}.Encode()
	s.db, err = sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(1)
	s.session, err = safefs.NewID("n")
	if err != nil {
		return nil, err
	}
	legacyOwnership := false
	if create {
		if bootstrap != nil {
			s.identity = bootstrap.Identity
		} else {
			s.identity.Worker, err = safefs.NewID("n")
			if err != nil {
				return nil, err
			}
			s.identity.Recovery, err = safefs.NewID("n")
			if err != nil {
				return nil, err
			}
		}
		// A partial initialization remains an explicit failed installation, never
		// an invitation for the service to silently recreate state.
		if err = d.WriteJSON("identity.json", s.identity, false); err != nil {
			return nil, err
		}
		tx, e := s.db.Begin()
		if e != nil {
			return nil, e
		}
		defer tx.Rollback()
		if _, err = tx.Exec(schema); err != nil {
			return nil, err
		}
		b, _ := json.Marshal(s.identity)
		if _, err = tx.Exec("INSERT INTO metadata VALUES('identity',?)", string(b)); err != nil {
			return nil, err
		}
		if bootstrap != nil {
			if _, err = tx.Exec("INSERT INTO metadata VALUES('bootstrap',?)", protocol.Hash(bootstrap)); err != nil {
				return nil, err
			}
		}
		for _, p := range c.Producers {
			if _, err = tx.Exec("INSERT INTO producers(id,epoch) VALUES(?,?)", p.Producer.ID, p.Producer.Epoch); err != nil {
				return nil, err
			}
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	} else {
		if err = d.ReadJSON("identity.json", &s.identity); err != nil {
			return nil, err
		}
		if requireCurrent && s.identity != expected {
			return nil, fmt.Errorf("managed worker identity changed")
		}
		var stored string
		var version int
		if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version < 1 || version > currentSchema {
			return nil, fmt.Errorf("unsupported notification schema")
		}
		if requireCurrent && version != currentSchema {
			return nil, fmt.Errorf("managed notification state requires an explicit schema migration")
		}
		if !requireCurrent {
			var enrolled int
			if err = s.db.QueryRow("SELECT count(*) FROM metadata WHERE key=?", managedDeliveryOwnerKey).Scan(&enrolled); err != nil {
				return nil, err
			}
			if enrolled != 0 {
				return nil, fmt.Errorf("database retains managed delivery ownership; use its installation lifecycle")
			}
		}
		legacyOwnership = version < 5
		var recovering int
		if err = s.db.QueryRow("SELECT count(*) FROM metadata WHERE key='recovery'").Scan(&recovering); err != nil {
			return nil, err
		}
		if recovering != 0 && recoveryCommit == "" {
			return nil, fmt.Errorf("restored database requires reconciliation")
		}
		if err = s.db.QueryRow("SELECT value FROM metadata WHERE key='identity'").Scan(&stored); err != nil {
			return nil, err
		}
		var id protocol.Identity
		if err = protocol.Decode([]byte(stored), &id, 4096); err != nil || id != s.identity || !id.Valid() {
			return nil, fmt.Errorf("identity mismatch; recovery required")
		}
		var count int
		if err = s.db.QueryRow("SELECT count(*) FROM producers").Scan(&count); err != nil {
			return nil, err
		}
		if enrollment != nil {
			if err = checkBootstrapEnrollmentRows(s.db, *enrollment); err != nil {
				return nil, err
			}
		} else if count != len(c.Producers) {
			return nil, fmt.Errorf("enrollment change requires migration")
		}
		for _, p := range c.Producers {
			var epoch string
			if err = s.db.QueryRow("SELECT epoch FROM producers WHERE id=?", p.Producer.ID).Scan(&epoch); err != nil || epoch != p.Producer.Epoch {
				return nil, fmt.Errorf("enrollment mismatch")
			}
		}
	}
	if bootstrap != nil {
		var pin string
		if err = s.db.QueryRow("SELECT value FROM metadata WHERE key='bootstrap'").Scan(&pin); err != nil || pin != protocol.Hash(bootstrap) || s.identity != bootstrap.Identity {
			return nil, fmt.Errorf("unbound gateway database reservation mismatch")
		}
	}
	if err = checkEnrolledLimitsSQL(context.Background(), d, s.db, enrolledTransition, enrolledPolicy); err != nil {
		return nil, err
	}
	if enrolledTransition != nil && enrolledTransition.Receipt.Identity != s.identity {
		return nil, fmt.Errorf("enrolled limits identity changed; recovery handoff required")
	}
	if err = checkBootstrapLimitsSQL(s.dir, s.db, limitsTransition, limits); err != nil {
		return nil, err
	}
	if limitsTransition != nil && limitsTransition.Receipt.Identity != s.identity {
		return nil, fmt.Errorf("bootstrap limits identity changed; explicit recovery handoff required")
	}
	if bootstrapTransition != nil {
		if err = checkBootstrapEnrollmentLEDHistory(s.db, *bootstrapTransition); err != nil {
			return nil, err
		}
	}
	if bootstrapTransition != nil && bootstrapTransition.Complete {
		if err = requireBootstrapCommandAliases(s.db, *bootstrapTransition); err != nil {
			return nil, err
		}
		var anchor string
		if err = s.db.QueryRow("SELECT value FROM metadata WHERE key='bootstrap-enrollment'").Scan(&anchor); err != nil || anchor != bootstrapTransition.digest() {
			return nil, fmt.Errorf("bootstrap enrollment SQL authority mismatch")
		}
	}
	if bootstrapTransition == nil {
		var anchor string
		if err = s.db.QueryRow("SELECT value FROM metadata WHERE key='bootstrap-enrollment'").Scan(&anchor); err != sql.ErrNoRows {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("bootstrap enrollment receipt missing")
		}
	}
	if recoveryCommit != "" {
		if err = verifyRecoveryRuntimeSQL(context.Background(), s.db, recoveryProof); err != nil {
			return nil, err
		}
	}
	if err = s.prepareOwnership(create, legacyOwnership); err != nil {
		return nil, err
	}
	if err = s.migrate(); err != nil {
		return nil, err
	}
	if err = s.transaction(context.Background(), s.reserveAliases); err != nil {
		return nil, err
	}
	var check string
	if err = s.db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return nil, fmt.Errorf("state integrity unavailable")
	}
	if err = s.checkMessageAdministration(context.Background()); err != nil {
		return nil, err
	}
	if err = verifyReceiptCompaction(context.Background(), s.db, s.config, s.identity); err != nil {
		return nil, err
	}
	if err = s.recoverAttempts(); err != nil {
		return nil, err
	}
	if err = s.verifyInstallationRetirement(context.Background()); err != nil {
		return nil, err
	}
	if err = s.verifyRetirements(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	if s.lease != nil {
		s.lease.Close()
	}
	if s.dir != nil {
		s.dir.Close()
	}
	return err
}
func (s *Store) Identity() protocol.Identity { return s.identity }

func (s *Store) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) receipt(tx *sql.Tx, p string, r protocol.Receipt) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO receipts(producer,body) VALUES(?,?)", p, string(b))
	return err
}

func (s *Store) capacity(tx *sql.Tx, producer string, bytes int, admission bool) error {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(s.dir.File().Fd()), &fs); err != nil {
		return err
	}
	if fs.Bavail*uint64(fs.Bsize) < uint64(s.config.MinFreeBytes)+(1<<20) {
		return fmt.Errorf("insufficient durable outcome reserve")
	}
	var size int64
	for _, name := range []string{"state.db", "state.db-wal"} {
		f, e := s.dir.Open(name, os.O_RDONLY, 0)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		i, e := f.Stat()
		f.Close()
		if e != nil {
			return e
		}
		size += i.Size()
	}
	if size+int64(bytes)+(256<<10) > s.config.MaxDatabaseBytes {
		return fmt.Errorf("database budget exhausted")
	}
	if !admission {
		return nil
	}
	var count, own int
	var payload int64
	if err := tx.QueryRow("SELECT count(*),COALESCE(sum(length(body)),0) FROM messages WHERE state NOT IN ('provider_accepted','expired','superseded','cancelled')").Scan(&count, &payload); err != nil {
		return err
	}
	if err := tx.QueryRow("SELECT count(*) FROM messages WHERE producer=? AND state NOT IN ('provider_accepted','expired','superseded','cancelled')", producer).Scan(&own); err != nil {
		return err
	}
	limit := 0
	for _, p := range s.config.Producers {
		if p.Producer.ID == producer {
			limit = p.MaxMessages
		}
	}
	if count >= s.config.MaxMessages || own >= limit || payload+int64(bytes) > s.config.MaxPayloadBytes {
		return fmt.Errorf("queue capacity exhausted")
	}
	return nil
}

func (s *Store) recoverAttempts() error {
	return s.transaction(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE command_reply_attempts SET state='ambiguous' WHERE state='sending';UPDATE command_replies SET state='ambiguous',attempt='' WHERE state='sending'"); err != nil {
			return err
		}
		rows, err := tx.Query("SELECT producer,id,seq,attempt,provider FROM messages WHERE attempt<>''")
		if err != nil {
			return err
		}
		type item struct {
			p string
			r protocol.Receipt
		}
		var pending []item
		for rows.Next() {
			var v item
			if err = rows.Scan(&v.p, &v.r.EventID, &v.r.Sequence, &v.r.AttemptID, &v.r.Provider); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, v := range pending {
			v.r.State = "ambiguous"
			v.r.At = s.now().UTC()
			if _, err = tx.Exec("UPDATE messages SET state='ambiguous',attempt='' WHERE producer=? AND id=?", v.p, v.r.EventID); err != nil {
				return err
			}
			if err = s.receipt(tx, v.p, v.r); err != nil {
				return err
			}
		}
		_, err = tx.Exec("UPDATE incidents SET attempt=''")
		return err
	})
}
