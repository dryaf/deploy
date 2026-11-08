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

// EnrolledLimitsReceipt keeps the original enrollment and all queued work intact.
// Its independent file/SQL chain prevents an older runtime from silently opening
// the store with different budgets. Callers stop verification under its lease.
type EnrolledLimitsReceipt struct {
	CommandHistoryHash string                `json:"command_history_hash,omitempty"`
	Version            int                   `json:"version"`
	DeliveryOwner      *ManagedDeliveryOwner `json:"delivery_owner,omitempty"`
	LEDOutputHash      string                `json:"led_output_hash,omitempty"`
	LEDOutputOperation string                `json:"led_output_operation,omitempty"`
	Operation          string                `json:"operation"`
	Identity           protocol.Identity     `json:"identity"`
	Previous           string                `json:"previous,omitempty"`
	Before             Config                `json:"before"`
	After              Config                `json:"after"`
}
type enrolledLimits struct {
	Receipt  EnrolledLimitsReceipt `json:"receipt"`
	Complete bool                  `json:"complete"`
}

const enrolledLimitsFile = "enrolled-limits.json"
const enrolledLimitsKey = "enrolled-limits"

func ValidateEnrolledLimitsChange(before, after Config) error {
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if len(before.Producers) != len(after.Producers) {
		return fmt.Errorf("limit changes cannot change enrollment")
	}
	next := before
	next.MaxMessages, next.MaxPayloadBytes = after.MaxMessages, after.MaxPayloadBytes
	next.MaxDatabaseBytes, next.MinFreeBytes = after.MaxDatabaseBytes, after.MinFreeBytes
	next.ReceiptAuditDays = after.ReceiptAuditDays
	next.Producers = append([]Enrollment(nil), before.Producers...)
	for i := range next.Producers {
		next.Producers[i].MaxMessages = after.Producers[i].MaxMessages
	}
	if protocol.Hash(next) != protocol.Hash(after) {
		return fmt.Errorf("change limits separately from routes, identities and freshness")
	}
	return nil
}
func (v enrolledLimits) digest() string { return protocol.Hash(v.Receipt) }
func (v enrolledLimits) validate() error {
	r := v.Receipt
	if (r.Version < 1 || r.Version > 10 && r.Version != 11 && r.Version != 16 && r.Version != 12 && r.Version != 17 && r.Version != 13 && r.Version != 18 && r.Version != 21 && r.Version != 26 && r.Version != 24 && r.Version != 29 && r.Version != 23 && r.Version != 28 && r.Version != 25 && r.Version != 30 && r.Version != 31 && r.Version != 36 && r.Version != 32 && r.Version != 37 && r.Version != 33 && r.Version != 38 && r.Version != 34 && r.Version != 39 && r.Version != 35 && r.Version != 40) || !safefs.ValidID(r.Operation, "n") || !r.Identity.Valid() || r.Previous != "" && !safefs.DigestPattern.MatchString(r.Previous) || r.Version != 5 && r.Version != 10 && r.Version != 25 && r.Version != 30 && r.Version != 35 && r.Version != 40 && protocol.Hash(r.Before) == protocol.Hash(r.After) {
		return fmt.Errorf("invalid enrolled limits intent")
	}
	version := r.Version
	commandPolicy := version == 31 || version == 36 || version == 32 || version == 37 || version == 33 || version == 38 || version == 34 || version == 39 || version == 35 || version == 40
	if commandPolicy {
		if !safefs.DigestPattern.MatchString(r.CommandHistoryHash) || r.Before.Interaction == nil || r.After.Interaction == nil || r.DeliveryOwner == nil || r.DeliveryOwner.validate() != nil || r.DeliveryOwner.Identity != r.Identity {
			return fmt.Errorf("command policy migration lost retained authority")
		}
		version -= 30
	} else if r.CommandHistoryHash != "" {
		return fmt.Errorf("unexpected command authority in policy migration")
	}
	if version == 21 || version == 26 || version == 24 || version == 29 || version == 23 || version == 28 || version == 25 || version == 30 {
		if r.DeliveryOwner == nil || r.DeliveryOwner.validate() != nil || r.DeliveryOwner.Identity != r.Identity || r.Before.Interaction != nil || r.After.Interaction != nil {
			return fmt.Errorf("production limits require unchanged direct-alert delivery ownership")
		}
		version -= 20
	} else if !commandPolicy && r.DeliveryOwner != nil {
		return fmt.Errorf("unexpected delivery ownership on verification policy")
	}
	if version > 5 && version != 11 && version != 12 && version != 13 {
		version -= 5
		if !safefs.DigestPattern.MatchString(r.LEDOutputHash) || !safefs.ValidID(r.LEDOutputOperation, "n") {
			return fmt.Errorf("invalid enrolled policy output pin")
		}
	} else if r.LEDOutputHash != "" || r.LEDOutputOperation != "" {
		return fmt.Errorf("unexpected enrolled policy output pin")
	}
	return validateEnrolledPolicyChange(version, r.Before, r.After)
}
func readEnrolledLimits(d *safefs.Directory) (*enrolledLimits, error) {
	var v enrolledLimits
	if err := d.ReadJSON(enrolledLimitsFile, &v); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := v.validate(); err != nil {
		return nil, err
	}
	current := v
	for count := 0; ; count++ {
		if count >= 64 {
			return nil, fmt.Errorf("enrolled limits history bound reached")
		}
		if current.Receipt.Previous == "" {
			break
		}
		var prior enrolledLimits
		if err := d.ReadJSON("enrolled-limits-history/"+current.Receipt.Previous+".json", &prior); err != nil {
			return nil, err
		}
		if prior.validate() != nil || !prior.Complete || prior.digest() != current.Receipt.Previous || prior.Receipt.Identity != v.Receipt.Identity || protocol.Hash(prior.Receipt.After) != protocol.Hash(current.Receipt.Before) && ValidateLEDBindingChange(prior.Receipt.After, current.Receipt.Before) != nil {
			return nil, fmt.Errorf("enrolled limits predecessor changed")
		}
		current = prior
	}
	return &v, nil
}
func checkEnrolledLimitsAdmission(d *safefs.Directory, c Config, pending *enrolledLimits) (*enrolledLimits, error) {
	v, err := readEnrolledLimits(d)
	if err != nil {
		return nil, err
	}
	if v == nil {
		if pending != nil {
			return nil, fmt.Errorf("enrolled limits intent missing")
		}
		return nil, nil
	}
	want := v.Receipt.After
	if pending != nil {
		if v.Complete || protocol.Hash(v) != protocol.Hash(pending) {
			return nil, fmt.Errorf("enrolled limits intent changed")
		}
		want = v.Receipt.Before
	} else if !v.Complete {
		return nil, fmt.Errorf("enrolled limits migration requires resume")
	}
	// The policy history pins budgets, routes and enrolled producer policy.
	// Command and output transitions retain their independent admission.
	projected := c
	projected.Routes = want.Routes
	projected.MaxMessages, projected.MaxPayloadBytes = want.MaxMessages, want.MaxPayloadBytes
	projected.MaxDatabaseBytes, projected.MinFreeBytes = want.MaxDatabaseBytes, want.MinFreeBytes
	projected.ReceiptAuditDays = want.ReceiptAuditDays
	projected.Producers = want.Producers
	projected.Interaction = want.Interaction
	if protocol.Hash(projected) != protocol.Hash(c) {
		return nil, fmt.Errorf("enrolled limits differ from retained policy")
	}
	return v, nil
}
func checkEnrolledLimitsSQL(ctx context.Context, d *safefs.Directory, db ledRowReader, v, pending *enrolledLimits) error {
	var anchor string
	err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", enrolledLimitsKey).Scan(&anchor)
	if errors.Is(err, sql.ErrNoRows) {
		anchor = ""
	} else if err != nil {
		return err
	}
	if v == nil {
		if anchor != "" {
			return fmt.Errorf("enrolled limits file missing; recovery required")
		}
		return nil
	}
	if anchor == v.digest() || pending != nil && anchor == v.Receipt.Previous {
		if err := checkEnrolledLimitsLEDHistory(ctx, d, db, v); err != nil {
			return err
		}
		if err := checkEnrolledCommandAliases(ctx, d, db, v, anchor); err != nil {
			return err
		}
		return checkEnrolledPolicyMappingHistory(ctx, d, db, v)
	}
	return fmt.Errorf("enrolled limits SQL authority mismatch")
}
func EnrolledLimitsRequired(state string) (bool, error) {
	d, err := safefs.Open(state)
	if err != nil {
		return false, err
	}
	defer d.Close()
	v, err := readEnrolledLimits(d)
	if err != nil {
		return false, err
	}
	db, err := snapshotDB(d, "state.db", true)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var pending *enrolledLimits
	if v != nil && !v.Complete {
		pending = v
	}
	if err = checkEnrolledLimitsSQL(context.Background(), d, db, v, pending); err != nil {
		return false, err
	}
	return v != nil, nil
}
func checkEnrolledLimitsCapacity(ctx context.Context, s *Store, after Config) error {
	return checkLimitsCapacity(ctx, s, after, false)
}
func checkLimitsCapacity(ctx context.Context, s *Store, after Config, delivery bool, commandLimits ...bool) error {
	commands := len(commandLimits) == 1 && commandLimits[0]
	if err := checkLEDMigrations(ctx, s.db, s.config, s.identity, false); err != nil {
		return err
	}
	if delivery {
		if (!commands && (s.config.Interaction != nil || after.Interaction != nil)) || s.recoveryCommitHash != "" {
			return fmt.Errorf("production limits require unrecovered direct-alert state")
		}
		var owner ManagedDeliveryOwner
		if err := s.dir.ReadJSON(managedDeliveryOwnerFile, &owner); err != nil {
			return err
		}
		if err := checkManagedDeliveryOwnerRecord(s.dir, s.db, s.config, owner, protocol.Hash(owner), s.identity); err != nil {
			return err
		}
		var recovered int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE key='recovery'").Scan(&recovered); err != nil {
			return err
		}
		if recovered != 0 {
			return fmt.Errorf("production limit changes require unrecovered state")
		}
	} else if err := absentOriginalFile(s.dir, managedDeliveryOwnerFile); err != nil {
		return fmt.Errorf("enrolled limits require no delivery owner: %w", err)
	}
	if _, _, err := s.completedLEDOutputPin(ctx); err != nil {
		return err
	}
	var count int
	var payload int64
	if err := s.db.QueryRowContext(ctx, "SELECT count(*),COALESCE(sum(length(body)),0) FROM messages WHERE state NOT IN ('provider_accepted','expired','superseded','cancelled')").Scan(&count, &payload); err != nil {
		return err
	}
	if count > after.MaxMessages || payload > after.MaxPayloadBytes {
		return fmt.Errorf("requested limits are smaller than retained queued work")
	}
	for _, p := range after.Producers {
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE producer=? AND state NOT IN ('provider_accepted','expired','superseded','cancelled')", p.Producer.ID).Scan(&count); err != nil {
			return err
		}
		if count > p.MaxMessages {
			return fmt.Errorf("requested producer limit is smaller than retained queued work")
		}
	}
	if !delivery {
		if err := s.RequireInactiveProducers(); err != nil {
			return err
		}
		// Delivery authority is immutable here. Completed LED history is retained
		// independently and validated before the policy transaction.
		for _, query := range []string{"SELECT count(*) FROM metadata WHERE key = 'managed-delivery-owner'"} {
			if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("enrolled policy requires no delivery history")
			}
		}
	}
	if err := verifyReceiptCompaction(ctx, s.db, after, s.identity); err != nil {
		return err
	}
	prior := s.config
	s.config = after
	defer func() { s.config = prior }()
	return s.capacity(nil, "", 0, false)
}
func PreflightEnrolledLimits(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightEnrolledPolicy(ctx, state, id, before, after, 1)
}
func preflightEnrolledPolicy(ctx context.Context, state string, id protocol.Identity, before, after Config, version int) error {
	if err := validateEnrolledPolicyChange(version, before, after); err != nil {
		return err
	}
	return inspectEnrolledPolicyState(ctx, state, id, before, func(s *Store) error { return checkEnrolledPolicyCapacity(ctx, s, after, version) })
}
func inspectEnrolledPolicyState(ctx context.Context, state string, id protocol.Identity, before Config, check func(*Store) error) error {
	d, err := privateDirectory(state)
	if err != nil {
		return err
	}
	defer d.Close()
	var current protocol.Identity
	if err = d.ReadJSON("identity.json", &current); err != nil {
		return err
	}
	if current != id {
		return fmt.Errorf("enrolled limits identity changed")
	}
	v, err := checkEnrolledLimitsAdmission(d, before, nil)
	if err != nil {
		return err
	}
	db, err := snapshotDB(d, "state.db", true)
	if err != nil {
		return err
	}
	defer db.Close()
	var raw string
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&raw); err != nil {
		return err
	}
	var stored protocol.Identity
	if protocol.Decode([]byte(raw), &stored, 4096) != nil || stored != id {
		return fmt.Errorf("enrolled limits database identity changed")
	}
	if err = checkEnrolledLimitsSQL(ctx, d, db, v, nil); err != nil {
		return err
	}
	if v != nil && v.Receipt.Identity != id {
		return fmt.Errorf("enrolled limits history identity changed")
	}
	return check(&Store{dir: d, db: db, config: before, identity: id})
}
func MigrateEnrolledLimits(ctx context.Context, state string, id protocol.Identity, before, after Config, operation string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledLimits(ctx, state, id, before, after, operation, nil)
}
func migrateEnrolledLimits(ctx context.Context, state string, id protocol.Identity, before, after Config, operation string, fault func(string) error) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, operation, 1, fault)
}
func migrateEnrolledPolicy(ctx context.Context, state string, id protocol.Identity, before, after Config, operation string, version int, fault func(string) error, commandHistory ...string) (EnrolledLimitsReceipt, error) {
	var zero EnrolledLimitsReceipt
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	wanted := enrolledLimits{Receipt: EnrolledLimitsReceipt{Version: version, Operation: operation, Identity: id, Before: before, After: after}}
	if version == 31 || version == 32 || version == 33 || version == 34 || version == 35 {
		if len(commandHistory) != 1 || !safefs.DigestPattern.MatchString(commandHistory[0]) {
			return zero, fmt.Errorf("command policy migration requires its exact retained command head")
		}
		wanted.Receipt.CommandHistoryHash = commandHistory[0]
	} else if len(commandHistory) != 0 {
		return zero, fmt.Errorf("unexpected command policy authority")
	}

	if version == 21 || version == 24 || version == 23 || version == 25 || version == 31 || version == 32 || version == 33 || version == 34 || version == 35 {
		owner, err := InspectManagedDeliveryOwner(state)
		if err != nil {
			return zero, err
		}
		wanted.Receipt.DeliveryOwner = &owner
	}
	if err := wanted.validate(); err != nil {
		return zero, err
	}
	d, err := safefs.Open(state)
	if err != nil {
		return zero, err
	}
	defer d.Close()
	lock, err := d.Lock("enrolled-limits.lock", true, true)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	current, err := readEnrolledLimits(d)
	if err != nil {
		return zero, err
	}
	var s *Store
	if current != nil && current.Receipt.Operation == operation {
		wanted.Receipt.Previous = current.Receipt.Previous
		wanted.Receipt.Version = current.Receipt.Version
		wanted.Receipt.LEDOutputHash, wanted.Receipt.LEDOutputOperation = current.Receipt.LEDOutputHash, current.Receipt.LEDOutputOperation
		if wanted.validate() != nil || wanted.digest() != current.digest() {
			return zero, fmt.Errorf("resume the exact enrolled limits intent")
		}
		if current.Complete {
			s, err = OpenManagedVerification(state, after, id)
			if err != nil {
				return zero, err
			}
			if err = s.requireCompletedLEDOutputPin(ctx, current.Receipt.LEDOutputOperation, current.Receipt.LEDOutputHash); err != nil {
				s.Close()
				return zero, err
			}
			if current.Receipt.DeliveryOwner != nil {
				if err = s.RequireManagedDeliveryOwner(*current.Receipt.DeliveryOwner, protocol.Hash(current.Receipt.DeliveryOwner)); err != nil {
					s.Close()
					return zero, err
				}
			}
			if current.Receipt.CommandHistoryHash != "" {
				if err = checkDisabledDeliveryCommandHead(s, current.Receipt.CommandHistoryHash); err != nil {
					s.Close()
					return zero, err
				}
			}
			return current.Receipt, s.Close()
		}
		s, err = openStoreWithPolicyTransitions(state, before, false, nil, true, id, "", nil, nil, nil, current)
	} else {
		if current != nil {
			if !current.Complete {
				return zero, fmt.Errorf("enrolled limits predecessor changed")
			}
			wanted.Receipt.Previous = current.digest()
			ancestor := *current
			for count := 1; ; count++ {
				if count >= 64 {
					return zero, fmt.Errorf("enrolled limits history full")
				}
				if ancestor.Receipt.Previous == "" {
					break
				}
				var prior enrolledLimits
				if err = d.ReadJSON("enrolled-limits-history/"+ancestor.Receipt.Previous+".json", &prior); err != nil {
					return zero, err
				}
				ancestor = prior
			}
		}
		s, err = OpenManagedVerification(state, before, id)
		if err == nil {
			if current != nil {
				if err = verifyEnrolledPolicyMappingGap(ctx, s.db, id, current.Receipt.After, before); err != nil {
					s.Close()
					return zero, err
				}
			}
			if version == 31 || version == 32 || version == 33 || version == 34 || version == 35 {
				if err = checkDisabledDeliveryCommandHead(s, wanted.Receipt.CommandHistoryHash); err != nil {
					s.Close()
					return zero, err
				}
			}
			if err = checkEnrolledPolicyCapacity(ctx, s, after, version); err != nil {
				s.Close()
				return zero, err
			}
			outputOperation, pin, e := s.completedLEDOutputPin(ctx)
			if e != nil {
				s.Close()
				return zero, e
			}
			wanted.Receipt.LEDOutputOperation, wanted.Receipt.LEDOutputHash = outputOperation, pin
			if pin != "" {
				wanted.Receipt.Version += 5
			}
			if e = wanted.validate(); e != nil {
				s.Close()
				return zero, e
			}
			if current != nil {
				if err = d.Mkdir("enrolled-limits-history"); err == nil {
					err = d.WriteJSON("enrolled-limits-history/"+current.digest()+".json", current, false)
				}
				if errors.Is(err, os.ErrExist) {
					var saved enrolledLimits
					err = d.ReadJSON("enrolled-limits-history/"+current.digest()+".json", &saved)
					if err == nil && protocol.Hash(saved) != protocol.Hash(current) {
						err = fmt.Errorf("enrolled limits history changed")
					}
				}
			}
			if err == nil {
				err = d.WriteJSON(enrolledLimitsFile, wanted, current != nil)
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
		return zero, err
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
	if current.Receipt.CommandHistoryHash != "" {
		if err = checkDisabledDeliveryCommandHead(s, current.Receipt.CommandHistoryHash); err != nil {
			return current.Receipt, err
		}
	}
	if err = boundary("intent"); err != nil {
		return current.Receipt, err
	}
	if err = checkEnrolledPolicyCapacity(ctx, s, after, version); err != nil {
		return current.Receipt, err
	}
	if err = s.transaction(ctx, func(tx *sql.Tx) error {
		if version == 11 || version == 12 || version == 13 {
			prior := s.config
			s.config = after
			e := s.reserveAliases(tx)
			s.config = prior
			if e != nil {
				return e
			}
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", enrolledLimitsKey, current.digest())
		return e
	}); err != nil {
		return current.Receipt, err
	}
	if err = boundary("committed"); err != nil {
		return current.Receipt, err
	}
	current.Complete = true
	if err = d.WriteJSON(enrolledLimitsFile, current, true); err != nil {
		return current.Receipt, err
	}
	return current.Receipt, boundary("complete")
}
