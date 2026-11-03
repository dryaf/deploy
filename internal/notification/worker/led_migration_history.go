package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const maxLEDMigrations = 16

type ledMappingStep struct {
	Kind  string     `json:"kind"`
	ID    string     `json:"id"`
	LED   string     `json:"led"`
	State *led.State `json:"state,omitempty"`
}

type ledMappingIntent struct {
	Version          int               `json:"version"`
	Position         int               `json:"position"`
	ID               string            `json:"id"`
	Identity         protocol.Identity `json:"identity"`
	Socket           string            `json:"socket"`
	BaseConfigHash   string            `json:"base_config_hash"`
	BeforeConfigHash string            `json:"before_config_hash"`
	AfterConfigHash  string            `json:"after_config_hash"`
	Before           ledLayout         `json:"before"`
	After            ledLayout         `json:"after"`
	HistoryHash      string            `json:"history_hash"`
	Steps            []ledMappingStep  `json:"steps"`
}

type ledMappingRecord struct {
	Reconciliations int64            `json:"reconciliations"`
	Intent          ledMappingIntent `json:"intent"`
	Next            int              `json:"next"`
	Attempts        int64            `json:"attempts"`
	Outcome         string           `json:"outcome,omitempty"`
	Completed       bool             `json:"completed"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// LEDMigrationResult describes a completed local mapping transition, not hardware
// qualification. A retry of an interrupted transition uses the same operation ID.
type LEDMigrationResult struct {
	Operation  string `json:"operation"`
	BeforeHash string `json:"before_hash"`
	AfterHash  string `json:"after_hash"`
	Steps      int    `json:"acknowledged_steps"`
}

func migrateLEDMappings(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE led_migrations(position INTEGER PRIMARY KEY,id TEXT NOT NULL UNIQUE,body TEXT NOT NULL,hash TEXT NOT NULL);
CREATE TABLE led_migration_heads(migration TEXT NOT NULL REFERENCES led_migrations(id),binding TEXT NOT NULL,body TEXT NOT NULL,hash TEXT NOT NULL,PRIMARY KEY(migration,binding));
CREATE TABLE led_migration_operations(migration TEXT NOT NULL REFERENCES led_migrations(id),binding TEXT NOT NULL,revision INTEGER NOT NULL,body TEXT NOT NULL,hash TEXT NOT NULL,PRIMARY KEY(migration,binding,revision));
PRAGMA user_version=9;`)
	return err
}

func ledBaseConfigHash(c Config) string { c.LEDBindings = nil; return protocol.Hash(c) }

func ledLayoutOutputs(layout ledLayout) []LEDBinding {
	out := []LEDBinding{{ID: workerLEDBinding, LED: "green", Styles: standardLEDStyles()}}
	return append(out, layout.Bindings...)
}

func ledBaseline(worker string, b LEDBinding) led.State {
	style := b.Styles["baseline"]
	return led.State{ID: ledBindingNamespace(worker, b.ID) + ".baseline", LED: b.LED, Pattern: style.Pattern, Priority: style.Priority, Persistent: true, Message: "Notification observations unavailable"}
}

func changedLEDBindings(before, after ledLayout) []string {
	var changed []string
	for _, old := range before.Bindings {
		i := slices.IndexFunc(after.Bindings, func(b LEDBinding) bool { return b.ID == old.ID })
		if i < 0 || protocol.Hash(old) != protocol.Hash(after.Bindings[i]) {
			changed = append(changed, old.ID)
		}
	}
	return changed
}

func ledMappingSteps(before, after ledLayout) []ledMappingStep {
	var steps []ledMappingStep
	// Revoke every old healthy lease before changing any baseline. No step sets
	// a healthy lease or clears a fault ID, including after interrupted migration.
	for _, b := range ledLayoutOutputs(before) {
		steps = append(steps, ledMappingStep{Kind: "clear", ID: ledBindingNamespace(before.Worker, b.ID) + ".healthy", LED: b.LED})
	}
	for _, id := range changedLEDBindings(before, after) {
		i := slices.IndexFunc(before.Bindings, func(b LEDBinding) bool { return b.ID == id })
		steps = append(steps, ledMappingStep{Kind: "clear", ID: ledBindingNamespace(before.Worker, id) + ".baseline", LED: before.Bindings[i].LED})
	}
	for _, b := range ledLayoutOutputs(after) {
		state := ledBaseline(after.Worker, b)
		steps = append(steps, ledMappingStep{Kind: "set", ID: state.ID, LED: b.LED, State: &state})
	}
	return steps
}

func validateLEDMapping(m ledMappingRecord, c Config, id protocol.Identity) error {
	v := m.Intent
	if v.Version != 1 || v.Position < 1 || v.Position > maxLEDMigrations || !safefs.ValidID(v.ID, "n") || v.Identity != id || !safefs.DigestPattern.MatchString(v.BaseConfigHash) || !safefs.DigestPattern.MatchString(v.BeforeConfigHash) || !safefs.DigestPattern.MatchString(v.AfterConfigHash) || !safefs.DigestPattern.MatchString(v.HistoryHash) || !validLEDSocket(v.Socket) {
		return fmt.Errorf("invalid LED migration identity")
	}
	for _, layout := range []ledLayout{v.Before, v.After} {
		config := c
		config.LEDBindings = layout.Bindings
		if err := config.Validate(); err != nil {
			return err
		}
		if err := checkLEDLayoutValue(layout, config, id, false); err != nil {
			return err
		}
	}
	if v.After.Version != 2 || v.After.Revision != v.ID || v.Before.UID != v.After.UID || protocol.Hash(v.Before.Legacy) != protocol.Hash(v.After.Legacy) || protocol.Hash(v.Before.Bindings) == protocol.Hash(v.After.Bindings) || protocol.Hash(v.Steps) != protocol.Hash(ledMappingSteps(v.Before, v.After)) {
		return fmt.Errorf("invalid LED migration mapping")
	}
	if m.Reconciliations < 0 || m.Next < 0 || m.Next > len(v.Steps) || m.Attempts < int64(m.Next) || m.UpdatedAt.IsZero() {
		return fmt.Errorf("invalid LED migration progress")
	}
	switch m.Outcome {
	case "":
		if m.Next != 0 || m.Attempts != 0 || m.Completed {
			return fmt.Errorf("invalid empty LED migration")
		}
	case "pending", "uncertain":
		if m.Next == len(v.Steps) || m.Attempts <= int64(m.Next) || m.Completed {
			return fmt.Errorf("invalid unconfirmed LED migration")
		}
	case "acknowledged":
		if m.Next == 0 {
			return fmt.Errorf("invalid LED migration acknowledgement")
		}
	default:
		return fmt.Errorf("unknown LED migration outcome")
	}
	if m.Completed && m.Next != len(v.Steps) {
		return fmt.Errorf("premature LED migration completion")
	}
	return nil
}

func readLEDMapping(ctx context.Context, q ledRowReader, id string) (m ledMappingRecord, err error) {
	var body, hash string
	if err = q.QueryRowContext(ctx, "SELECT body,hash FROM led_migrations WHERE id=?", id).Scan(&body, &hash); err != nil {
		return
	}
	if err = protocol.Decode([]byte(body), &m, 4*protocol.MaxRequest); err != nil {
		return
	}
	if protocol.Hash(m) != hash {
		err = fmt.Errorf("LED migration digest mismatch")
	}
	return
}

func writeLEDMapping(ctx context.Context, tx *sql.Tx, m ledMappingRecord) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(body) > 4*protocol.MaxRequest {
		return fmt.Errorf("LED migration exceeds journal budget")
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO led_migrations(position,id,body,hash) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body,hash=excluded.hash", m.Intent.Position, m.Intent.ID, string(body), protocol.Hash(m))
	return err
}

func ledHistoryHash(ctx context.Context, q ledJournalReader, archive string) (string, error) {
	heads, entries, args := ledHistoryQueries(archive)
	type row struct {
		Kind, Binding string
		Revision      int64
		Body, Hash    string
	}
	var records []row
	for i, query := range []string{heads, entries} {
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			r := row{Kind: "head"}
			if i == 0 {
				err = rows.Scan(&r.Binding, &r.Body, &r.Hash)
			} else {
				r.Kind = "operation"
				err = rows.Scan(&r.Binding, &r.Revision, &r.Body, &r.Hash)
			}
			if err != nil {
				break
			}
			records = append(records, r)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return "", err
		}
	}
	return protocol.Hash(records), nil
}

// Completed transitions and archived operation history are snapshot data. A
// missing link or orphan archive cannot silently reset the current mapping.
func checkLEDMigrations(ctx context.Context, q ledJournalReader, c Config, identity protocol.Identity, allowPending bool) error {
	var raw string
	if err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil {
		return err
	}
	var current ledLayout
	if err := protocol.Decode([]byte(raw), &current, protocol.MaxRequest); err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, "SELECT position,id,body,hash FROM led_migrations ORDER BY position LIMIT 17")
	if err != nil {
		return err
	}
	var records []ledMappingRecord
	for rows.Next() {
		var position int
		var id, body, hash string
		var m ledMappingRecord
		if err = rows.Scan(&position, &id, &body, &hash); err != nil {
			break
		}
		if err = protocol.Decode([]byte(body), &m, 4*protocol.MaxRequest); err != nil {
			break
		}
		if protocol.Hash(m) != hash || m.Intent.Position != position || m.Intent.ID != id || position != len(records)+1 {
			err = fmt.Errorf("LED migration history gap or conflict")
			break
		}
		if err = validateLEDMapping(m, c, identity); err != nil {
			break
		}
		records = append(records, m)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	expected := current
	if len(records) > 0 {
		expected = records[0].Intent.Before
		if expected.Version != 1 {
			return fmt.Errorf("missing initial LED migration history")
		}
	}
	for i, m := range records {
		if protocol.Hash(m.Intent.Before) != protocol.Hash(expected) {
			return fmt.Errorf("broken LED mapping chain")
		}
		if !m.Completed {
			if !allowPending || i != len(records)-1 {
				return fmt.Errorf("LED binding migration requires resume")
			}
			var count int
			if err = q.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM led_migration_heads WHERE migration=?)+(SELECT count(*) FROM led_migration_operations WHERE migration=?)", m.Intent.ID, m.Intent.ID).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("premature LED history archive")
			}
			hash, e := ledHistoryHash(ctx, q, "")
			if e != nil {
				return e
			}
			if hash != m.Intent.HistoryHash {
				return fmt.Errorf("active LED history changed during migration")
			}
			break
		}
		if _, err = readLEDOperationHistory(ctx, q, identity, m.Intent.Before, m.Intent.ID); err != nil {
			return err
		}
		hash, e := ledHistoryHash(ctx, q, m.Intent.ID)
		if e != nil {
			return e
		}
		if hash != m.Intent.HistoryHash {
			return fmt.Errorf("LED operation archive mismatch")
		}
		expected = m.Intent.After
	}
	if protocol.Hash(expected) != protocol.Hash(current) || (len(records) == 0 && current.Version != 1) {
		return fmt.Errorf("LED mapping lacks migration authority")
	}
	var orphans int
	if err = q.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM led_migration_heads a LEFT JOIN led_migrations m ON a.migration=m.id WHERE m.id IS NULL)+(SELECT count(*) FROM led_migration_operations a LEFT JOIN led_migrations m ON a.migration=m.id WHERE m.id IS NULL)").Scan(&orphans); err != nil {
		return err
	}
	if orphans != 0 {
		return fmt.Errorf("orphan LED migration history")
	}
	return nil
}
