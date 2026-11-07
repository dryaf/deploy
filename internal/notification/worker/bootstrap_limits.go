package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// BootstrapLimitsReceipt is a forward-only policy change. The original gateway
// reservation remains immutable; the database anchors this separate history.
type BootstrapLimitsReceipt struct {
	Kind               string            `json:"kind,omitempty"`
	Version            int               `json:"version"`
	Operation          string            `json:"operation"`
	Identity           protocol.Identity `json:"identity"`
	Previous           string            `json:"previous,omitempty"`
	LEDOutputHash      string            `json:"led_output_hash,omitempty"`
	LEDOutputOperation string            `json:"led_output_operation,omitempty"`
	Before             Config            `json:"before"`
	After              Config            `json:"after"`
}
type bootstrapLimits struct {
	Receipt  BootstrapLimitsReceipt `json:"receipt"`
	Complete bool                   `json:"complete"`
}

const bootstrapLimitsFile = "bootstrap-limits.json"
const bootstrapLimitsKey = "bootstrap-limits"

// BootstrapLimitsRequired verifies file/database agreement even when a missing
// file would otherwise make an executable downgrade appear compatible.
func BootstrapLimitsRequired(state string) (bool, error) {
	d, err := safefs.Open(state)
	if err != nil {
		return false, err
	}
	defer d.Close()
	v, err := readBootstrapLimits(d)
	if err != nil {
		return false, err
	}
	db, err := snapshotDB(d, "state.db", true)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var pending *bootstrapLimits
	if v != nil && !v.Complete {
		pending = v
	}
	if err = checkBootstrapLimitsSQL(d, db, v, pending); err != nil {
		return false, err
	}
	return v != nil, nil
}

// ValidateBootstrapLimitsChange excludes route, audience, enrollment and command
// changes. Those have separate admission and disposition requirements.
func ValidateBootstrapLimitsChange(before, after Config) error {
	if err := before.ValidateBootstrap(); err != nil {
		return err
	}
	if err := after.ValidateBootstrap(); err != nil {
		return err
	}
	next := before
	next.MaxMessages, next.MaxPayloadBytes = after.MaxMessages, after.MaxPayloadBytes
	next.MaxDatabaseBytes, next.MinFreeBytes = after.MaxDatabaseBytes, after.MinFreeBytes
	next.ReceiptAuditDays = after.ReceiptAuditDays
	if protocol.Hash(next) != protocol.Hash(after) {
		return fmt.Errorf("limit migration cannot change routes, identities or enrollment")
	}
	return nil
}
func (v bootstrapLimits) digest() string { return protocol.Hash(v.Receipt) }
func (v bootstrapLimits) validate() error {
	r := v.Receipt
	if !safefs.ValidID(r.Operation, "n") || !r.Identity.Valid() || r.Previous != "" && !safefs.DigestPattern.MatchString(r.Previous) || protocol.Hash(r.Before) == protocol.Hash(r.After) {
		return fmt.Errorf("invalid bootstrap policy intent")
	}
	base := 1
	if r.Kind == "routes" {
		base = 3
	} else if r.Kind != "" {
		return fmt.Errorf("unknown bootstrap policy transition")
	}
	if r.Version != base && r.Version != base+1 {
		return fmt.Errorf("bootstrap policy version differs from kind")
	}
	if r.Version == base && (r.LEDOutputHash != "" || r.LEDOutputOperation != "") || r.Version == base+1 && (!safefs.DigestPattern.MatchString(r.LEDOutputHash) || !safefs.ValidID(r.LEDOutputOperation, "n")) {
		return fmt.Errorf("invalid bootstrap policy output pin")
	}
	return validateBootstrapPolicyChange(r.Kind, r.Before, r.After)
}

func readBootstrapLimits(d *safefs.Directory) (*bootstrapLimits, error) {
	var v bootstrapLimits
	if err := d.ReadJSON(bootstrapLimitsFile, &v); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := v.validate(); err != nil {
		return nil, err
	}
	var b GatewayBootstrap
	if err := d.ReadJSON("bootstrap.json", &b); err != nil {
		return nil, err
	}
	if b.validate() != nil || b.Identity != v.Receipt.Identity {
		return nil, fmt.Errorf("limits bootstrap authority changed")
	}
	current := v
	for count := 0; ; count++ {
		if count >= 32 {
			return nil, fmt.Errorf("bootstrap limits history bound reached")
		}
		if current.Receipt.Previous == "" {
			if protocol.Hash(current.Receipt.Before) != protocol.Hash(b.Config) {
				return nil, fmt.Errorf("limits history lost its original reservation")
			}
			break
		}
		var prior bootstrapLimits
		if err := d.ReadJSON("bootstrap-limits-history/"+current.Receipt.Previous+".json", &prior); err != nil {
			return nil, err
		}
		if prior.validate() != nil || !prior.Complete || prior.digest() != current.Receipt.Previous || prior.Receipt.Identity != b.Identity || protocol.Hash(prior.Receipt.After) != protocol.Hash(current.Receipt.Before) {
			return nil, fmt.Errorf("bootstrap limits predecessor changed")
		}
		current = prior
	}
	return &v, nil
}

func bootstrapEffectiveConfig(d *safefs.Directory, b GatewayBootstrap) (Config, error) {
	v, err := readBootstrapLimits(d)
	if err != nil {
		return Config{}, err
	}
	if v == nil {
		return b.Config, nil
	}
	if !v.Complete || v.Receipt.Identity != b.Identity {
		return Config{}, fmt.Errorf("bootstrap limits migration must finish before enrollment")
	}
	return v.Receipt.After, nil
}

func checkBootstrapLimitsAdmission(d *safefs.Directory, c Config, b *GatewayBootstrap, pending *bootstrapLimits) (*bootstrapLimits, error) {
	v, err := readBootstrapLimits(d)
	if err != nil {
		return nil, err
	}
	if v == nil {
		if pending != nil {
			return nil, fmt.Errorf("bootstrap limits intent missing")
		}
		if b != nil && protocol.Hash(c) != protocol.Hash(b.Config) {
			return nil, fmt.Errorf("unbound gateway policy changed without migration")
		}
		return nil, nil
	}
	want := v.Receipt.After
	if pending != nil {
		if b == nil || v.Complete || protocol.Hash(v) != protocol.Hash(pending) {
			return nil, fmt.Errorf("bootstrap limits intent changed")
		}
		want = v.Receipt.Before
	} else if !v.Complete {
		return nil, fmt.Errorf("bootstrap limits migration requires resume")
	}
	if b != nil && (b.Identity != v.Receipt.Identity || protocol.Hash(c) != protocol.Hash(want)) {
		return nil, fmt.Errorf("bootstrap limits policy mismatch")
	}
	return v, nil
}

func checkBootstrapLimitsSQL(d *safefs.Directory, db *sql.DB, v, pending *bootstrapLimits) error {
	var anchor string
	err := db.QueryRow("SELECT value FROM metadata WHERE key=?", bootstrapLimitsKey).Scan(&anchor)
	if errors.Is(err, sql.ErrNoRows) {
		anchor = ""
	} else if err != nil {
		return err
	}
	if v == nil {
		if anchor != "" {
			return fmt.Errorf("bootstrap limits file missing; recovery required")
		}
		return nil
	}
	if anchor == v.digest() || pending != nil && anchor == v.Receipt.Previous {
		return checkBootstrapLimitsLEDHistory(d, db, v)
	}
	return fmt.Errorf("bootstrap limits SQL authority mismatch")
}

// MigrateBootstrapLimits requires a stopped unbound gateway and its managed
// installation lease. Pending intent fences normal opens until exact resume.
func MigrateBootstrapLimits(ctx context.Context, state string, id protocol.Identity, before, after Config, operation string) (BootstrapLimitsReceipt, error) {
	return migrateBootstrapLimits(ctx, state, id, before, after, operation, nil)
}
func migrateBootstrapLimits(ctx context.Context, state string, id protocol.Identity, before, after Config, operation string, fault func(string) error) (BootstrapLimitsReceipt, error) {
	return migrateBootstrapPolicy(ctx, state, id, before, after, operation, "", fault)
}
func migrateBootstrapPolicy(ctx context.Context, state string, id protocol.Identity, before, after Config, operation, kind string, fault func(string) error) (BootstrapLimitsReceipt, error) {
	var zero BootstrapLimitsReceipt
	if err := validateBootstrapPolicyChange(kind, before, after); err != nil {
		return zero, err
	}
	d, err := safefs.Open(state)
	if err != nil {
		return zero, err
	}
	defer d.Close()
	lock, err := d.Lock("bootstrap-limits.lock", true, true)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	var b GatewayBootstrap
	if err = d.ReadJSON("bootstrap.json", &b); err != nil {
		return zero, err
	}
	if b.validate() != nil || b.Identity != id || b.State != state {
		return zero, fmt.Errorf("bootstrap limits reservation changed")
	}
	current, err := readBootstrapLimits(d)
	if err != nil {
		return zero, err
	}
	wanted := bootstrapLimits{Receipt: BootstrapLimitsReceipt{Kind: kind, Version: 1, Operation: operation, Identity: id, Before: before, After: after}}
	if kind == "routes" {
		wanted.Receipt.Version = 3
	}
	var s *Store
	if current != nil && current.Receipt.Operation == operation {
		wanted.Receipt.Previous = current.Receipt.Previous
		wanted.Receipt.Version, wanted.Receipt.LEDOutputHash = current.Receipt.Version, current.Receipt.LEDOutputHash
		wanted.Receipt.LEDOutputOperation = current.Receipt.LEDOutputOperation
		if wanted.validate() != nil || wanted.digest() != current.digest() {
			return zero, fmt.Errorf("bootstrap limits resume changed policy")
		}
		if current.Complete {
			s, err = OpenBootstrapVerification(state, after, id, "")
			if err != nil {
				return zero, err
			}
			if err = s.requireCompletedLEDOutputPin(ctx, current.Receipt.LEDOutputOperation, current.Receipt.LEDOutputHash); err != nil {
				s.Close()
				return zero, err
			}
			return current.Receipt, s.Close()
		}
		s, err = openStoreWithBootstrapTransitions(state, before, false, nil, true, id, "", &b, nil, current)
	} else {
		if current != nil {
			if !current.Complete || protocol.Hash(current.Receipt.After) != protocol.Hash(before) {
				return zero, fmt.Errorf("unfinished or mismatched bootstrap limits predecessor")
			}
			wanted.Receipt.Previous = current.digest()
			// Keep room for the successor before publishing its intent. Counting
			// the validated chain avoids treating orphaned staging as authority.
			ancestor := *current
			for count := 1; ; count++ {
				if count >= 32 {
					return zero, fmt.Errorf("bootstrap limits history full; archive through a reviewed migration")
				}
				if ancestor.Receipt.Previous == "" {
					break
				}
				name := "bootstrap-limits-history/" + ancestor.Receipt.Previous + ".json"
				ancestor = bootstrapLimits{}
				if err = d.ReadJSON(name, &ancestor); err != nil {
					return zero, err
				}
			}
		}
		if err = wanted.validate(); err != nil {
			return zero, err
		}
		s, err = OpenBootstrapVerification(state, before, id, "")
		if err == nil {
			if err = preflightBootstrapLimits(s, after); err != nil {
				s.Close()
				return zero, err
			}
			ledOperation, pin, e := s.completedLEDOutputPin(ctx)
			if e != nil {
				s.Close()
				return zero, e
			}
			wanted.Receipt.LEDOutputHash = pin
			wanted.Receipt.LEDOutputOperation = ledOperation
			if pin != "" {
				wanted.Receipt.Version++
			}
			if current != nil {
				if err = d.Mkdir("bootstrap-limits-history"); err == nil {
					err = d.WriteJSON("bootstrap-limits-history/"+current.digest()+".json", current, false)
				}
				if errors.Is(err, os.ErrExist) {
					var saved bootstrapLimits
					err = d.ReadJSON("bootstrap-limits-history/"+current.digest()+".json", &saved)
					if err == nil && protocol.Hash(saved) != protocol.Hash(current) {
						err = fmt.Errorf("limits history changed")
					}
				}
			}
			if err == nil {
				err = d.WriteJSON(bootstrapLimitsFile, wanted, current != nil)
			}
			if err != nil {
				s.Close()
				return zero, err
			}
			current = &wanted
		}
	}
	if err != nil {
		return zero, err
	}
	defer s.Close()
	if err = s.requireCompletedLEDOutputPin(ctx, current.Receipt.LEDOutputOperation, current.Receipt.LEDOutputHash); err != nil {
		return current.Receipt, err
	}
	boundary := func(at string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fault != nil {
			return fault(at)
		}
		return nil
	}
	if err = boundary("intent"); err != nil {
		return current.Receipt, err
	}
	if err = preflightBootstrapLimits(s, after); err != nil {
		return current.Receipt, err
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", bootstrapLimitsKey, current.digest())
		return err
	})
	if err != nil {
		return current.Receipt, err
	}
	if err = boundary("committed"); err != nil {
		return current.Receipt, err
	}
	current.Complete = true
	if err = d.WriteJSON(bootstrapLimitsFile, current, true); err != nil {
		return current.Receipt, err
	}
	return current.Receipt, boundary("complete")
}

// PreflightBootstrapLimits inspects a live database without acquiring its
// exclusive runtime lease. The stopped transaction repeats every capacity check.
func PreflightBootstrapLimits(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightBootstrapPolicy(ctx, state, id, before, after, "")
}
func preflightBootstrapPolicy(ctx context.Context, state string, id protocol.Identity, before, after Config, kind string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateBootstrapPolicyChange(kind, before, after); err != nil {
		return err
	}
	d, err := safefs.Open(state)
	if err != nil {
		return err
	}
	defer d.Close()
	var b GatewayBootstrap
	if err = d.ReadJSON("bootstrap.json", &b); err != nil {
		return err
	}
	if b.validate() != nil || b.Identity != id || b.State != state {
		return fmt.Errorf("bootstrap limits reservation changed")
	}
	v, err := checkBootstrapLimitsAdmission(d, before, &b, nil)
	if err != nil {
		return err
	}
	if _, err = checkBootstrapEnrollmentAdmission(d, before, &b, nil); err != nil {
		return err
	}
	db, err := snapshotDB(d, "state.db", true)
	if err != nil {
		return err
	}
	defer db.Close()
	var identity, bootstrap string
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&identity); err != nil {
		return err
	}
	var stored protocol.Identity
	if protocol.Decode([]byte(identity), &stored, 4096) != nil || stored != id {
		return fmt.Errorf("bootstrap limits database identity changed")
	}
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='bootstrap'").Scan(&bootstrap); err != nil || bootstrap != protocol.Hash(b) {
		return fmt.Errorf("bootstrap limits database reservation changed")
	}
	if err = checkBootstrapLimitsSQL(d, db, v, nil); err != nil {
		return err
	}
	s := &Store{dir: d, db: db, config: before, identity: id}
	if _, _, err = s.completedLEDOutputPin(ctx); err != nil {
		return err
	}
	return preflightBootstrapLimits(s, after)
}

func preflightBootstrapLimits(s *Store, after Config) error {
	for _, table := range []string{"producers", "messages", "receipts", "status", "subjects", "incidents", "reports"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("unbound limits migration found production history")
		}
	}
	// Use the same database/WAL and outcome reserve checks as runtime admission.
	prior := s.config
	s.config = after
	defer func() { s.config = prior }()
	return s.capacity(nil, "", 0, false)
}
