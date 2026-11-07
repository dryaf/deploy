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

// BootstrapEnrollmentReceipt identifies a one-way transition from unbound to
// real consumers. It retains the gateway identity and leaves every generation
// inactive. Managed callers must hold their installation lease and stop service.
type BootstrapEnrollmentReceipt struct {
	Version            int               `json:"version"`
	Operation          string            `json:"operation"`
	Identity           protocol.Identity `json:"identity"`
	Before             string            `json:"before"`
	After              string            `json:"after"`
	LEDOutputOperation string            `json:"led_output_operation,omitempty"`
	LEDOutputHash      string            `json:"led_output_hash,omitempty"`
}
type bootstrapEnrollment struct {
	Receipt  BootstrapEnrollmentReceipt `json:"receipt"`
	Config   Config                     `json:"config"`
	Complete bool                       `json:"complete"`
}

func (e bootstrapEnrollment) digest() string { return protocol.Hash(e.Receipt) }
func (e bootstrapEnrollment) validate() error {
	r := e.Receipt
	if r.Version < 1 || r.Version > 3 || (r.Version == 1 && (r.LEDOutputHash != "" || r.LEDOutputOperation != "")) || ((r.Version == 2 || r.LEDOutputHash != "" || r.LEDOutputOperation != "") && (!safefs.ValidID(r.LEDOutputOperation, "n") || !safefs.DigestPattern.MatchString(r.LEDOutputHash))) || !safefs.ValidID(r.Operation, "n") || !r.Identity.Valid() || !safefs.DigestPattern.MatchString(r.Before) || r.After != protocol.Hash(e.Config) {
		return fmt.Errorf("invalid bootstrap enrollment intent")
	}
	if (e.Config.Interaction != nil) != (r.Version == 3) || len(e.Config.LEDBindings) != 0 {
		return fmt.Errorf("bootstrap command enrollment requires version 3; LED mappings require separate migration")
	}
	return e.Config.Validate()
}
func checkBootstrapEnrollmentAdmission(d *safefs.Directory, c Config, b *GatewayBootstrap, pending *bootstrapEnrollment) (*bootstrapEnrollment, error) {
	var e bootstrapEnrollment
	err := d.ReadJSON("bootstrap-enrollment.json", &e)
	if errors.Is(err, os.ErrNotExist) {
		if pending != nil {
			return nil, fmt.Errorf("bootstrap enrollment intent missing")
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = e.validate(); err != nil {
		return nil, err
	}
	if pending != nil {
		if b == nil {
			return nil, fmt.Errorf("bootstrap enrollment reservation missing")
		}
		baseline, err := bootstrapEffectiveConfig(d, *b)
		if err != nil {
			return nil, err
		}
		if protocol.Hash(e) != protocol.Hash(pending) || e.Receipt.Identity != b.Identity || e.Receipt.Before != protocol.Hash(baseline) {
			return nil, fmt.Errorf("bootstrap enrollment intent changed")
		}
	} else if b != nil || !e.Complete {
		return nil, fmt.Errorf("bootstrap enrollment requires its exact forward migration")
	}
	return &e, nil
}
func checkBootstrapEnrollmentRows(db *sql.DB, e bootstrapEnrollment) error {
	rows, err := db.Query("SELECT id,epoch,submitted,acked,listed FROM producers")
	if err != nil {
		return err
	}
	defer rows.Close()
	found := 0
	for rows.Next() {
		var id, epoch string
		var submitted, acked, listed int64
		if err = rows.Scan(&id, &epoch, &submitted, &acked, &listed); err != nil {
			return err
		}
		expected := false
		for _, p := range e.Config.Producers {
			if p.Producer.ID == id && p.Producer.Epoch == epoch {
				expected = true
				break
			}
		}
		if !expected || submitted != 0 || acked != 0 || listed != 0 {
			return fmt.Errorf("bootstrap enrollment contains unexpected producer activity")
		}
		found++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if found != 0 && found != len(e.Config.Producers) {
		return fmt.Errorf("partial bootstrap producer set")
	}
	return nil
}

func EnrollBootstrap(ctx context.Context, state string, expected protocol.Identity, after Config, operation string) (BootstrapEnrollmentReceipt, error) {
	return enrollBootstrap(ctx, state, expected, after, operation, nil)
}
func enrollBootstrap(ctx context.Context, state string, expected protocol.Identity, after Config, operation string, fault func(string) error) (BootstrapEnrollmentReceipt, error) {
	var zero BootstrapEnrollmentReceipt
	if !expected.Valid() || !safefs.ValidID(operation, "n") {
		return zero, fmt.Errorf("bootstrap enrollment requires exact identity and operation")
	}
	if err := after.Validate(); err != nil {
		return zero, err
	}
	d, err := safefs.Open(state)
	if err != nil {
		return zero, err
	}
	defer d.Close()
	var b GatewayBootstrap
	if err = d.ReadJSON("bootstrap.json", &b); err != nil {
		return zero, err
	}
	if b.validate() != nil || b.Identity != expected || b.State != state {
		return zero, fmt.Errorf("gateway bootstrap reservation mismatch")
	}
	baseline := after
	baseline.Producers = nil
	baseline.Interaction = nil
	effective, err := bootstrapEffectiveConfig(d, b)
	if err != nil {
		return zero, err
	}
	if protocol.Hash(baseline) != protocol.Hash(effective) {
		return zero, fmt.Errorf("bootstrap enrollment may only add real consumers and initial command policy; route and budget changes require separate migration")
	}
	wanted := bootstrapEnrollment{Receipt: BootstrapEnrollmentReceipt{Version: 1, Operation: operation, Identity: expected, Before: protocol.Hash(effective), After: protocol.Hash(after)}, Config: after}
	if after.Interaction != nil {
		wanted.Receipt.Version = 3
	}
	if err = wanted.validate(); err != nil {
		return zero, err
	}
	// Serialize intent creation before acquiring the instance lease. Existing
	// managed installation/outer locks remain outside these worker locks.
	lock, err := d.Lock("bootstrap-enrollment.lock", true, true)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	var current GatewayBootstrap
	if err = d.ReadJSON("bootstrap.json", &current); err != nil || protocol.Hash(current) != protocol.Hash(b) {
		return zero, fmt.Errorf("bootstrap reservation changed before enrollment")
	}
	var e bootstrapEnrollment
	err = d.ReadJSON("bootstrap-enrollment.json", &e)
	var s *Store
	if errors.Is(err, os.ErrNotExist) {
		s, err = OpenBootstrapVerification(state, effective, expected, "")
		if err != nil {
			return zero, err
		}
		if err = checkBootstrapCommandEnrollment(s.db, wanted); err != nil {
			s.Close()
			return zero, err
		}
		wanted.Receipt.LEDOutputOperation, wanted.Receipt.LEDOutputHash, err = s.completedLEDOutputPin(ctx)
		if err != nil {
			s.Close()
			return zero, err
		}
		if wanted.Receipt.LEDOutputHash != "" && wanted.Receipt.Version == 1 {
			wanted.Receipt.Version = 2
		}
		if err = d.WriteJSON("bootstrap-enrollment.json", wanted, false); err != nil {
			s.Close()
			return zero, err
		}
		e = wanted
	} else if err == nil {
		wanted.Receipt.Version = e.Receipt.Version
		wanted.Receipt.LEDOutputOperation, wanted.Receipt.LEDOutputHash = e.Receipt.LEDOutputOperation, e.Receipt.LEDOutputHash
		if e.validate() != nil || protocol.Hash(e.Receipt) != protocol.Hash(wanted.Receipt) {
			return zero, fmt.Errorf("pending bootstrap enrollment differs from requested consumers")
		}
		if e.Complete {
			verified, openErr := OpenManagedVerification(state, after, expected)
			if openErr != nil {
				return e.Receipt, openErr
			}
			if err = verified.requireCompletedLEDOutputPin(ctx, e.Receipt.LEDOutputOperation, e.Receipt.LEDOutputHash); err != nil {
				verified.Close()
				return e.Receipt, err
			}
			return e.Receipt, verified.Close()
		}
		s, err = openStoreWithBootstrapEnrollment(state, effective, false, nil, true, expected, "", &b, &e)
	}
	if err != nil {
		return zero, err
	}
	defer s.Close()
	if err = s.requireCompletedLEDOutputPin(ctx, e.Receipt.LEDOutputOperation, e.Receipt.LEDOutputHash); err != nil {
		return e.Receipt, err
	}
	boundary := func(stage string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fault != nil {
			return fault(stage)
		}
		return nil
	}
	if err = boundary("intent"); err != nil {
		return e.Receipt, err
	}
	// Empty gateways have no legitimate queue or producer observations. Any such
	// evidence blocks enrollment instead of being discarded or overwritten.
	for _, table := range []string{"messages", "receipts", "status", "subjects", "incidents", "reports"} {
		var count int
		if err = s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			return e.Receipt, err
		}
		if count != 0 {
			return e.Receipt, fmt.Errorf("unbound gateway contains production history")
		}
	}
	if err = checkBootstrapEnrollmentRows(s.db, e); err != nil {
		return e.Receipt, err
	}
	if err = checkBootstrapCommandEnrollment(s.db, e); err != nil {
		return e.Receipt, err
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var anchor string
		readErr := tx.QueryRow("SELECT value FROM metadata WHERE key='bootstrap-enrollment'").Scan(&anchor)
		if readErr == nil {
			if anchor != e.digest() {
				return fmt.Errorf("enrollment SQL intent conflict")
			}
			return nil
		}
		if readErr != sql.ErrNoRows {
			return readErr
		}
		for _, p := range after.Producers {
			if _, err := tx.Exec("INSERT INTO producers(id,epoch) VALUES(?,?)", p.Producer.ID, p.Producer.Epoch); err != nil {
				return err
			}
		}
		// Reserve aliases in the same transaction as the producer identities.
		// This projected store contains configuration only, never copied locks.
		projection := &Store{config: after}
		if err := projection.reserveAliases(tx); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO metadata VALUES('bootstrap-enrollment',?)", e.digest())
		return err
	})
	if err != nil {
		return e.Receipt, err
	}
	if err = boundary("producers"); err != nil {
		return e.Receipt, err
	}
	for _, p := range after.Producers {
		if err = initializeBootstrapOwnership(s, p.Producer); err != nil {
			return e.Receipt, err
		}
		if err = boundary("owner/" + p.Producer.ID); err != nil {
			return e.Receipt, err
		}
	}
	if !e.Complete {
		e.Complete = true
		if err = d.WriteJSON("bootstrap-enrollment.json", e, true); err != nil {
			return e.Receipt, err
		}
	}
	return e.Receipt, boundary("complete")
}

func initializeBootstrapOwnership(s *Store, p protocol.Producer) error {
	name := ownershipName(p)
	var current ownership
	ownerErr := s.dir.ReadJSON(name+".json", &current)
	var intent ownershipIntent
	intentErr := s.dir.ReadJSON(name+".journal.json", &intent)
	if ownerErr != nil && !errors.Is(ownerErr, os.ErrNotExist) {
		return ownerErr
	}
	if intentErr != nil && !errors.Is(intentErr, os.ErrNotExist) {
		return intentErr
	}
	present := ownerErr == nil || intentErr == nil
	lock, err := s.dir.Lock(name+".lock", true, !present)
	if err != nil {
		return err
	}
	defer lock.Close()
	if !present {
		return s.prepareProducerOwnership(p, true, false)
	}
	if intentErr != nil {
		return fmt.Errorf("bootstrap producer ownership intent missing")
	}
	empty := emptyOwnership(s.identity, p)
	if !sameOwnership(intent.Before, empty) || intent.Transition.Operation != "initialize" || intent.Transition.Generation != "" {
		return fmt.Errorf("bootstrap producer has unexpected ownership history")
	}
	after, err := applyOwnershipTransition(empty, intent.Transition)
	if err != nil {
		return err
	}
	if err = s.checkOwnershipHistoryTail(p, 1); err != nil {
		return err
	}
	if err = s.checkOwnershipHistory(intent.Transition, true); err != nil {
		return err
	}
	if errors.Is(ownerErr, os.ErrNotExist) {
		return s.dir.WriteJSON(name+".json", after, false)
	}
	if !sameOwnership(current, after) {
		return fmt.Errorf("bootstrap producer ownership changed")
	}
	return nil
}
