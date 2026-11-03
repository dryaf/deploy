package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type ledLayout struct {
	Version  int          `json:"version"`
	Revision string       `json:"revision,omitempty"`
	Worker   string       `json:"worker"`
	UID      int          `json:"uid"`
	Bindings []LEDBinding `json:"bindings"`
	Legacy   []string     `json:"legacy,omitempty"`
}

func desiredLEDLayout(c Config, worker string, uid int) ledLayout {
	l := ledLayout{Version: 1, Worker: worker, UID: uid}
	for _, b := range c.LEDBindings {
		l.Bindings = append(l.Bindings, b.normalized())
	}
	slices.SortFunc(l.Bindings, func(a, b LEDBinding) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return l
}

type ledRowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func checkLEDLayout(ctx context.Context, q ledRowReader, c Config, identity protocol.Identity, live bool) error {
	var body string
	if err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&body); err != nil {
		return err
	}
	var saved ledLayout
	if err := protocol.Decode([]byte(body), &saved, protocol.MaxRequest); err != nil {
		return err
	}
	if err := checkLEDLayoutValue(saved, c, identity, live); err != nil {
		return err
	}
	if saved.Version == 2 {
		migration, err := readLEDMapping(ctx, q, saved.Revision)
		if err != nil {
			return err
		}
		if !migration.Completed || migration.Intent.Identity != identity || protocol.Hash(migration.Intent.After) != protocol.Hash(saved) {
			return fmt.Errorf("LED mapping publication is not confirmed")
		}
	}
	return nil
}

func checkLEDLayoutValue(saved ledLayout, c Config, identity protocol.Identity, live bool) error {
	raw, err := json.Marshal(saved)
	if err != nil || len(raw) > protocol.MaxRequest {
		return fmt.Errorf("LED mapping exceeds storage budget")
	}
	seen := map[string]bool{}
	for _, b := range saved.Bindings {
		namespace := ledBindingNamespace(saved.Worker, b.ID)
		if seen[namespace] {
			return fmt.Errorf("LED namespace collision")
		}
		seen[namespace] = true
	}
	expected := desiredLEDLayout(c, identity.Worker, saved.UID)
	expected.Legacy = saved.Legacy
	if saved.Version == 2 && safefs.ValidID(saved.Revision, "n") {
		expected.Version, expected.Revision = 2, saved.Revision
	}
	if len(saved.Legacy) > 1024 || !slices.IsSorted(saved.Legacy) {
		return fmt.Errorf("invalid legacy LED ownership inventory")
	}
	for i, id := range saved.Legacy {
		if !ledPatternName.MatchString(id) || (i > 0 && saved.Legacy[i-1] == id) {
			return fmt.Errorf("invalid legacy LED state identity")
		}
	}
	if saved.UID < 0 || (live && saved.UID != os.Getuid()) || protocol.Hash(saved) != protocol.Hash(expected) {
		return fmt.Errorf("LED mapping or runtime UID changed; explicit ownership migration required")
	}
	return nil
}

func (s *Store) migrateLED(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE led_states ADD COLUMN binding TEXT NOT NULL DEFAULT '$worker';
ALTER TABLE led_states ADD COLUMN identity TEXT NOT NULL DEFAULT 'legacy';
CREATE INDEX led_binding_states ON led_states(binding,obsolete,condition);`); err != nil {
		return err
	}
	l := desiredLEDLayout(s.config, s.identity.Worker, os.Getuid())
	rows, err := tx.Query("SELECT id FROM led_states ORDER BY id LIMIT 1025")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !ledPatternName.MatchString(id) {
			rows.Close()
			return fmt.Errorf("invalid legacy LED state")
		}
		l.Legacy = append(l.Legacy, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(l.Legacy) > 1024 {
		return fmt.Errorf("legacy LED state inventory exceeds budget")
	}
	// Persist the complete mapping, not just a truncated daemon-state namespace.
	seen := map[string]bool{}
	for _, b := range l.Bindings {
		base := ledBindingNamespace(l.Worker, b.ID)
		if seen[base] {
			return fmt.Errorf("LED namespace collision")
		}
		seen[base] = true
	}
	body, err := json.Marshal(l)
	if err != nil || len(body) > protocol.MaxRequest {
		return fmt.Errorf("LED mapping exceeds storage budget")
	}
	if _, err = tx.Exec("INSERT INTO metadata(key,value) VALUES('led-layout',?)", string(body)); err != nil {
		return err
	}
	return migrateLEDOperations(tx)
}

func ledBindingNamespace(worker, binding string) string {
	if binding == workerLEDBinding {
		return "deploy." + protocol.Hash(worker)[:24]
	}
	return "deploy.b." + protocol.Hash(struct{ Worker, Binding string }{worker, binding})[:24]
}

type ledStateIdentity struct{ Worker, Recovery, Binding, Nonce string }

func (s *Store) checkLEDState(tx *sql.Tx, id, binding, body string) error {
	if body != "legacy" && validateLEDFaultIdentity(id, binding, body, s.identity, nil) == nil {
		return nil
	}
	if inherited, err := s.inheritedLEDState(tx, id, binding, body); err != nil {
		return err
	} else if inherited {
		return errLEDRecoveryPending
	}
	if body == "legacy" && binding == workerLEDBinding {
		var raw string
		if err := tx.QueryRow("SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil {
			return err
		}
		var layout ledLayout
		if err := protocol.Decode([]byte(raw), &layout, protocol.MaxRequest); err != nil {
			return err
		}
		return validateLEDFaultIdentity(id, binding, body, s.identity, layout.Legacy)
	}
	return validateLEDFaultIdentity(id, binding, body, s.identity, nil)
}

func validateLEDFaultIdentity(id, binding, body string, owner protocol.Identity, legacy []string) error {
	if body == "legacy" && binding == workerLEDBinding {
		if !slices.Contains(legacy, id) {
			return fmt.Errorf("unproven legacy LED state")
		}
		return nil
	}
	var identity ledStateIdentity
	if err := protocol.Decode([]byte(body), &identity, 4096); err != nil {
		return err
	}
	if identity.Worker != owner.Worker || identity.Recovery != owner.Recovery || identity.Binding != binding || !safefs.ValidID(identity.Nonce, "n") || id != "deploy.f."+protocol.Hash(identity)[:48] {
		return fmt.Errorf("LED fault ownership mismatch")
	}
	return nil
}

func (s *Store) currentLEDState(tx *sql.Tx, binding, condition string) (string, error) {
	inherited, err := s.recoveredLEDFaults(context.Background(), tx)
	if err != nil {
		return "", err
	}
	var id, body string
	rows, err := tx.Query("SELECT id,identity FROM led_states WHERE binding=? AND obsolete=0 AND condition=? ORDER BY id LIMIT 4097", binding, condition)
	if err != nil {
		return "", err
	}
	count := 0
	for rows.Next() {
		count++
		if count > 4096 {
			rows.Close()
			return "", fmt.Errorf("LED runtime ownership inventory exceeds budget")
		}
		if err = rows.Scan(&id, &body); err != nil {
			rows.Close()
			return "", err
		}
		if _, old := inherited[id]; !old {
			rows.Close()
			return id, s.checkLEDState(tx, id, binding, body)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	if err = s.capacity(tx, "", 4096, false); err != nil {
		return "", err
	}
	nonce, err := safefs.NewID("n")
	if err != nil {
		return "", err
	}
	identity := ledStateIdentity{s.identity.Worker, s.identity.Recovery, binding, nonce}
	id = "deploy.f." + protocol.Hash(identity)[:48]
	raw, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	// The unique key rejects a truncated-hash collision without replacing a row.
	_, err = tx.Exec("INSERT INTO led_states(id,condition,binding,identity) VALUES(?,?,?,?)", id, condition, binding, string(raw))
	return id, err
}

func (s *Store) obsoleteLEDStates(tx *sql.Tx, binding, current string, resolve bool) ([]string, error) {
	// A durable tombstone survives interruption, but unknown/paused observations
	// cannot authorize its clear after a restart or a newer loss of evidence.
	if !resolve {
		return nil, nil
	}
	inherited, err := s.recoveredLEDFaults(context.Background(), tx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query("SELECT id,identity FROM led_states WHERE binding=? AND id<>? ORDER BY id LIMIT 4097", binding, current)
	if err != nil {
		return nil, err
	}
	var ids []string
	count := 0
	for rows.Next() {
		count++
		if count > 4096 {
			rows.Close()
			return nil, fmt.Errorf("LED cleanup inventory exceeds budget")
		}
		var id, body string
		if err = rows.Scan(&id, &body); err != nil {
			rows.Close()
			return nil, err
		}
		if _, old := inherited[id]; old {
			continue
		}
		if err = s.checkLEDState(tx, id, binding, body); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		if len(ids) == 128 {
			break
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err = tx.Exec("UPDATE led_states SET obsolete=1 WHERE id=? AND binding=?", id, binding); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func checkLEDLegacyMapping(ctx context.Context, q ledRowReader) error {
	var raw string
	if err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil {
		return err
	}
	var layout ledLayout
	if err := protocol.Decode([]byte(raw), &layout, protocol.MaxRequest); err != nil {
		return err
	}
	if layout.Version != 1 || layout.Revision != "" {
		return fmt.Errorf("legacy schema cannot authorize a migrated LED layout")
	}
	return nil
}
