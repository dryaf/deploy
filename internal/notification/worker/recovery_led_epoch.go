package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const ledEpochFile = "led-epoch.json"
const ledEpochPrefix = "reconciliation_led_epoch_"

// These are immutable copies, including the adopted fault union. Active fault
// rows remain untouched. Archiving never grants permission to replay old RPCs.
var ledEpochTables = []string{"metadata", "led_states", "led_operations", "led_operation_heads", "led_migrations", "led_migration_operations", "led_migration_heads"}

func readLEDEpochFaults(ctx context.Context, q ledJournalReader) ([]recoveryLEDFault, error) {
	rows, err := q.QueryContext(ctx, "SELECT id,binding,condition,identity,obsolete FROM led_states ORDER BY id LIMIT 2049")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var faults []recoveryLEDFault
	for rows.Next() {
		var f recoveryLEDFault
		var obsolete int
		if err = rows.Scan(&f.ID, &f.Binding, &f.Condition, &f.Identity, &obsolete); err != nil {
			return nil, err
		}
		if obsolete != 0 && obsolete != 1 {
			return nil, fmt.Errorf("invalid LED epoch cleanup flag")
		}
		f.Obsolete = obsolete == 1
		faults = append(faults, f)
		if len(faults) > 2*maxRecoveryLEDFaults {
			return nil, fmt.Errorf("LED epoch fault union exceeds budget")
		}
	}
	return faults, rows.Err()
}

type recoveryLEDEpoch struct {
	Version                                      int
	Recovery                                     recoveryBinding
	ConfigHash, AdoptionHash, SourceHash, Socket string
	AfterLayout                                  ledLayout
	PreparedAt                                   time.Time
}

type RecoveryLEDEpochView struct {
	Operation         string    `json:"operation"`
	RecordHash        string    `json:"record_hash"`
	AdoptionHash      string    `json:"adoption_hash"`
	State             string    `json:"state"`
	Faults            int       `json:"retained_faults"`
	PreparedAt        time.Time `json:"prepared_at"`
	Quarantined       bool      `json:"quarantined"`
	ActivationAllowed bool      `json:"activation_allowed"`
	ClearAllowed      bool      `json:"clear_allowed"`
}

func (v recoveryLEDEpoch) view(state string, faults int) RecoveryLEDEpochView {
	return RecoveryLEDEpochView{Operation: v.Recovery.Operation, RecordHash: protocol.Hash(v), AdoptionHash: v.AdoptionHash, State: state, Faults: faults, PreparedAt: v.PreparedAt, Quarantined: true}
}

// ledEpochReader redirects only fixed internal LED validation queries. Values
// remain SQL arguments. Reusing the original validators preserves all bounds,
// chain checks and old identity checks rather than trusting an archive checksum.
type ledEpochReader struct{ ledJournalReader }

var ledEpochSQL = strings.NewReplacer(
	"led_migration_operations", ledEpochPrefix+"led_migration_operations",
	"led_migration_heads", ledEpochPrefix+"led_migration_heads",
	"led_operation_heads", ledEpochPrefix+"led_operation_heads",
	"led_operations", ledEpochPrefix+"led_operations",
	"led_migrations", ledEpochPrefix+"led_migrations",
	"led_states", ledEpochPrefix+"led_states",
	"metadata", ledEpochPrefix+"metadata",
)

func (q ledEpochReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.ledJournalReader.QueryContext(ctx, ledEpochSQL.Replace(query), args...)
}
func (q ledEpochReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.ledJournalReader.QueryRowContext(ctx, ledEpochSQL.Replace(query), args...)
}

func ledEpochSchema(ctx context.Context, q ledJournalReader) (bool, error) {
	var version string
	err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-epoch-version'").Scan(&version)
	var count int
	if e := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND (name='reconciliation_led_epoch' OR name GLOB 'reconciliation_led_epoch_*')").Scan(&count); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && count == 0 {
		return false, nil
	}
	if err != nil || version != "1" || count != len(ledEpochTables)+1 {
		return false, fmt.Errorf("LED epoch archive schema missing or changed")
	}
	return true, nil
}

func ensureLEDEpochSchema(ctx context.Context, tx *sql.Tx) error {
	exists, err := ledEpochSchema(ctx, tx)
	if err != nil || exists {
		return err
	}
	if _, err = tx.ExecContext(ctx, "CREATE TABLE reconciliation_led_epoch(singleton INTEGER PRIMARY KEY CHECK(singleton=1),hash TEXT NOT NULL,body TEXT NOT NULL); INSERT INTO metadata VALUES('led-epoch-version','1');"); err != nil {
		return err
	}
	for _, table := range ledEpochTables {
		if _, err = tx.ExecContext(ctx, "CREATE TABLE "+ledEpochPrefix+table+" AS SELECT * FROM "+table+" WHERE 0"); err != nil {
			return err
		}
	}
	return nil
}

func emptyLEDEpochArchive(ctx context.Context, q ledJournalReader) error {
	for _, table := range ledEpochTables {
		var count int
		if err := q.QueryRowContext(ctx, "SELECT count(*) FROM "+ledEpochPrefix+table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("LED epoch archive appeared before atomic commit")
		}
	}
	return nil
}

func (r *recoveryReconciler) makeLEDEpoch(e ledAdoptionEvidence, at time.Time) (recoveryLEDEpoch, error) {
	if e.Phase != "committed" || e.After.Schema != currentSchema || e.After.Layout.UID != os.Getuid() || at.IsZero() {
		return recoveryLEDEpoch{}, fmt.Errorf("LED epoch handoff requires committed current-schema adoption for this UID")
	}
	layout := e.After.Layout
	layout.Version, layout.Revision = 1, ""
	return recoveryLEDEpoch{Version: 1, Recovery: r.journal.binding(), ConfigHash: protocol.Hash(r.config), AdoptionHash: protocol.Hash(e.Record), SourceHash: protocol.Hash(e.After), Socket: e.Record.Socket, AfterLayout: layout, PreparedAt: at}, nil
}

// readLEDEpoch validates immutable authority and both sides of the transition.
// It never repairs, resets or truncates active history on a repeated read.
func (r *recoveryReconciler) readLEDEpoch(ctx context.Context, q ledJournalReader, e *ledAdoptionEvidence) (*recoveryLEDEpoch, string, error) {
	exists, err := ledEpochSchema(ctx, q)
	if err != nil {
		return nil, "", err
	}
	var record recoveryLEDEpoch
	fileErr := r.stage.ReadJSON(ledEpochFile, &record)
	if !exists {
		if !os.IsNotExist(fileErr) {
			return nil, "", fmt.Errorf("LED epoch intent lost its schema")
		}
		return nil, "none", nil
	}
	var count int
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM reconciliation_led_epoch").Scan(&count); err != nil {
		return nil, "", err
	}
	if count > 1 {
		return nil, "", fmt.Errorf("unexpected LED epoch acknowledgements")
	}
	var body, hash string
	dbErr := q.QueryRowContext(ctx, "SELECT body,hash FROM reconciliation_led_epoch WHERE singleton=1").Scan(&body, &hash)
	if dbErr != nil && dbErr != sql.ErrNoRows {
		return nil, "", dbErr
	}
	if dbErr == sql.ErrNoRows && count != 0 {
		return nil, "", fmt.Errorf("LED epoch singleton changed")
	}
	if os.IsNotExist(fileErr) && dbErr == sql.ErrNoRows {
		return nil, "none", emptyLEDEpochArchive(ctx, q)
	}
	if fileErr != nil {
		return nil, "", fileErr
	}
	if e == nil {
		return nil, "", fmt.Errorf("LED epoch lost adoption authority")
	}
	want, err := r.makeLEDEpoch(*e, record.PreparedAt)
	if err != nil || protocol.Hash(want) != protocol.Hash(record) {
		return nil, "", fmt.Errorf("LED epoch authority changed")
	}
	if dbErr == sql.ErrNoRows {
		if err = emptyLEDEpochArchive(ctx, q); err != nil {
			return nil, "", err
		}
		return &record, "prepared", nil
	}
	var saved recoveryLEDEpoch
	if protocol.Decode([]byte(body), &saved, protocol.MaxRequest) != nil || protocol.Hash(saved) != hash || hash != protocol.Hash(record) {
		return nil, "", fmt.Errorf("LED epoch acknowledgement changed")
	}
	archive := ledEpochReader{q}
	var metadataCount int
	if err = archive.QueryRowContext(ctx, "SELECT count(*) FROM metadata").Scan(&metadataCount); err != nil || metadataCount != 2 {
		return nil, "", fmt.Errorf("LED archive metadata changed")
	}
	source, err := r.readLEDSourceLimit(ctx, archive, r.journal.After, 2*maxRecoveryLEDFaults)
	if err != nil {
		return nil, "", err
	}
	if protocol.Hash(source) != record.SourceHash {
		return nil, "", fmt.Errorf("LED epoch archive changed")
	}
	var raw string
	if err = q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil {
		return nil, "", err
	}
	var layout ledLayout
	if protocol.Decode([]byte(raw), &layout, protocol.MaxRequest) != nil || protocol.Hash(layout) != protocol.Hash(record.AfterLayout) {
		return nil, "", fmt.Errorf("LED epoch layout publication changed")
	}
	if err = checkLEDLayout(ctx, q, r.config, r.journal.After, true); err != nil {
		return nil, "", err
	}
	if err = checkLEDOperations(ctx, q, r.journal.After); err != nil {
		return nil, "", err
	}
	if err = checkLEDMigrations(ctx, q, r.config, r.journal.After, false); err != nil {
		return nil, "", err
	}
	return &record, "committed", nil
}

// HandoffRecoveryLED archives old operation/mapping history inside quarantine.
// It performs no daemon calls, clears no faults and never publishes the database.
func HandoffRecoveryLED(ctx context.Context, path string, c Config, socket, adoptionHash string) (RecoveryLEDEpochView, error) {
	if !validLEDSocket(socket) || !safefs.DigestPattern.MatchString(adoptionHash) {
		return RecoveryLEDEpochView{}, fmt.Errorf("LED epoch handoff requires pinned socket and adoption hash")
	}
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return RecoveryLEDEpochView{}, err
	}
	defer r.Close()
	return r.handoffLED(ctx, socket, adoptionHash)
}

func (r *recoveryReconciler) handoffLED(ctx context.Context, socket, adoptionHash string) (out RecoveryLEDEpochView, err error) {
	if !validLEDSocket(socket) || !safefs.DigestPattern.MatchString(adoptionHash) {
		return out, fmt.Errorf("invalid LED epoch handoff request")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Schema preparation is independent of the immutable intent. Empty, unused
	// tables can remain after interruption; rows cannot appear before commit.
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		e, err := r.readLEDAdoption(ctx, tx)
		if err != nil {
			return err
		}
		if e == nil || e.Phase != "committed" || protocol.Hash(e.Record) != adoptionHash || e.Record.Socket != socket {
			return fmt.Errorf("LED epoch handoff requires exact committed adoption")
		}
		if err = r.checkLEDEpochSources(ctx, tx, e); err != nil {
			return err
		}
		if _, err = r.makeLEDEpoch(*e, r.now().UTC()); err != nil {
			return err
		}
		exists, err := ledEpochSchema(ctx, tx)
		if err != nil || exists {
			return err
		}
		if err = r.ledEpochCapacity(ctx, tx); err != nil {
			return err
		}
		return ensureLEDEpochSchema(ctx, tx)
	})
	if err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		e, eErr := r.readLEDAdoptionAuthority(ctx, tx)
		if eErr != nil {
			return eErr
		}
		if e == nil || e.Phase != "committed" || protocol.Hash(e.Record) != adoptionHash || e.Record.Socket != socket {
			return fmt.Errorf("LED epoch adoption changed")
		}
		if eErr = r.checkLEDEpochSources(ctx, tx, e); eErr != nil {
			return eErr
		}
		record, phase, eErr := r.readLEDEpoch(ctx, tx, e)
		if eErr != nil {
			return eErr
		}
		if phase == "committed" {
			out = record.view(phase, len(e.After.Faults))
			return nil
		}
		source, eErr := r.readLEDSourceLimit(ctx, tx, r.journal.After, 2*maxRecoveryLEDFaults)
		if eErr != nil {
			return eErr
		}
		if protocol.Hash(source) != protocol.Hash(e.After) {
			return fmt.Errorf("LED epoch source changed before handoff")
		}
		if eErr = r.ledEpochCapacity(ctx, tx); eErr != nil {
			return eErr
		}
		if record == nil {
			v, eErr := r.makeLEDEpoch(*e, r.now().UTC())
			if eErr != nil {
				return eErr
			}
			record = &v
			if eErr = r.stage.WriteJSON(ledEpochFile, record, false); eErr != nil {
				return eErr
			}
		}
		if eErr = recoveryBoundary(r.fault, "led_epoch_intent"); eErr != nil {
			return eErr
		}
		for _, table := range ledEpochTables {
			query := "INSERT INTO " + ledEpochPrefix + table + " SELECT * FROM " + table
			if table == "metadata" {
				query += " WHERE key IN ('identity','led-layout')"
			}
			if _, eErr = tx.ExecContext(ctx, query); eErr != nil {
				return eErr
			}
		}
		if eErr = recoveryBoundary(r.fault, "led_epoch_archive"); eErr != nil {
			return eErr
		}
		for _, table := range []string{"led_migration_operations", "led_migration_heads", "led_migrations", "led_operations", "led_operation_heads"} {
			if _, eErr = tx.ExecContext(ctx, "DELETE FROM "+table); eErr != nil {
				return eErr
			}
		}
		raw, eErr := json.Marshal(record.AfterLayout)
		if eErr != nil {
			return eErr
		}
		if _, eErr = tx.ExecContext(ctx, "UPDATE metadata SET value=? WHERE key='led-layout'", string(raw)); eErr != nil {
			return eErr
		}
		if eErr = recoveryBoundary(r.fault, "led_epoch_reset"); eErr != nil {
			return eErr
		}
		raw, eErr = json.Marshal(record)
		if eErr != nil {
			return eErr
		}
		if _, eErr = tx.ExecContext(ctx, "INSERT INTO reconciliation_led_epoch VALUES(1,?,?)", protocol.Hash(record), string(raw)); eErr != nil {
			return eErr
		}
		if _, _, eErr = r.readLEDEpoch(ctx, tx, e); eErr != nil {
			return eErr
		}
		if eErr = r.checkLEDEpochSources(ctx, tx, e); eErr != nil {
			return eErr
		}
		out = record.view("committed", len(e.After.Faults))
		return recoveryBoundary(r.fault, "led_epoch_transaction")
	})
	if err == nil {
		err = recoveryBoundary(r.fault, "led_epoch_commit")
	}
	return
}

func (r *recoveryReconciler) checkLEDEpochSources(ctx context.Context, tx *sql.Tx, e *ledAdoptionEvidence) error {
	sources, err := r.collectLEDSources(ctx, tx)
	if err != nil {
		return err
	}
	if protocol.Hash(sources) != e.Record.SourcesHash {
		return fmt.Errorf("LED epoch preserved sources changed")
	}
	return nil
}

func (r *recoveryReconciler) ledEpochCapacity(ctx context.Context, tx *sql.Tx) error {
	var total int64
	for _, query := range []string{
		"SELECT COALESCE(sum(length(value)+length(key)+128),0) FROM metadata WHERE key IN ('identity','led-layout')",
		"SELECT COALESCE(sum(length(id)+length(binding)+length(condition)+length(identity)+128),0) FROM led_states",
		"SELECT COALESCE(sum(length(body)+length(hash)+128),0) FROM led_operations",
		"SELECT COALESCE(sum(length(body)+length(hash)+128),0) FROM led_operation_heads",
		"SELECT COALESCE(sum(length(body)+length(hash)+128),0) FROM led_migrations",
		"SELECT COALESCE(sum(length(body)+length(hash)+128),0) FROM led_migration_operations",
		"SELECT COALESCE(sum(length(body)+length(hash)+128),0) FROM led_migration_heads",
	} {
		var n int64
		if err := tx.QueryRowContext(ctx, query).Scan(&n); err != nil {
			return err
		}
		if n < 0 || n > r.config.MaxDatabaseBytes-total {
			return fmt.Errorf("LED epoch archive exceeds budget")
		}
		total += n
	}
	return r.capacity(tx, int(total)+(64<<10))
}
