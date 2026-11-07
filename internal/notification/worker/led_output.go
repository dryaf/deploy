package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const ledOutputFile = "led-output.json"
const ledOutputHead = "led-output-head"
const ledOutputPrefix = "led-output-operation:"

// LEDOutputRequest changes output admission, not binding identities or incident
// state. The caller stops the gateway and holds its installation lifecycle lease.
type LEDOutputRequest struct {
	Identity  protocol.Identity `json:"identity"`
	Operation string            `json:"operation"`
	Socket    string            `json:"socket"`
	Disabled  bool              `json:"disabled"`
}
type LEDOutputStatus struct {
	Operation string `json:"operation"`
	Disabled  bool   `json:"disabled"`
	Complete  bool   `json:"complete"`
	Cleared   int    `json:"cleared"`
	Total     int    `json:"total"`
}
type ledOutputTarget struct {
	ID, Binding, Condition, Ownership string
}
type ledOutputIntent struct {
	Version           int               `json:"version"`
	Request           LEDOutputRequest  `json:"request"`
	UID               int               `json:"uid"`
	ConfigHash        string            `json:"config_hash"`
	LayoutHash        string            `json:"layout_hash"`
	PreviousOperation string            `json:"previous_operation,omitempty"`
	PreviousHash      string            `json:"previous_hash,omitempty"`
	Targets           []ledOutputTarget `json:"targets"`
}
type ledOutputRecord struct {
	Intent   ledOutputIntent `json:"intent"`
	Next     int             `json:"next"`
	Complete bool            `json:"complete"`
}

func (v ledOutputIntent) validate(c Config, identity protocol.Identity) error {
	if v.Version != 1 || v.Request.Identity != identity || !identity.Valid() || !safefs.ValidID(v.Request.Operation, "n") || !validLEDSocket(v.Request.Socket) || v.UID != os.Getuid() || v.ConfigHash != protocol.Hash(c) || !safefs.DigestPattern.MatchString(v.LayoutHash) || len(v.Targets) > 1090 {
		return fmt.Errorf("LED output disposition authority changed")
	}
	if (v.PreviousOperation == "") != (v.PreviousHash == "") || v.PreviousOperation != "" && (!safefs.ValidID(v.PreviousOperation, "n") || !safefs.DigestPattern.MatchString(v.PreviousHash)) {
		return fmt.Errorf("invalid LED output predecessor")
	}
	last := ""
	for _, target := range v.Targets {
		if !ledPatternName.MatchString(target.ID) || target.ID <= last || target.Binding == "" || target.Condition == "" {
			return fmt.Errorf("invalid LED output ownership inventory")
		}
		last = target.ID
	}
	return nil
}

// readLEDOutput refuses missing either half of the file/SQL authority. A file
// published before its SQL transaction is a pending fence, never enabled output.
func (s *Store) readLEDOutput(ctx context.Context, q ledRowReader) (ledOutputRecord, bool, bool, error) {
	var result ledOutputRecord
	var intent ledOutputIntent
	err := s.dir.ReadJSON(ledOutputFile, &intent)
	var head string
	headErr := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", ledOutputHead).Scan(&head)
	if errors.Is(err, os.ErrNotExist) && headErr == sql.ErrNoRows {
		var retained int
		if e := q.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE key LIKE ?", ledOutputPrefix+"%").Scan(&retained); e != nil {
			return result, true, false, e
		}
		if retained != 0 {
			return result, true, false, fmt.Errorf("LED output head authority missing from retained history")
		}
		return result, false, false, nil
	}
	if err != nil {
		return result, true, false, fmt.Errorf("LED output file authority unavailable")
	}
	validationConfig, e := s.ledOutputValidationConfig(ctx, q, intent)
	if e != nil {
		return result, true, false, e
	}
	if err = intent.validate(validationConfig, s.identity); err != nil {
		return result, true, false, err
	}
	if headErr != nil && headErr != sql.ErrNoRows {
		return result, true, false, headErr
	}
	result.Intent = intent
	if head != intent.Request.Operation {
		if _, e := readLEDOutputRecord(ctx, q, intent.Request.Operation); e != sql.ErrNoRows {
			return result, true, false, fmt.Errorf("LED output head missing or moved from an existing operation")
		}
		if head != intent.PreviousOperation {
			return result, true, false, fmt.Errorf("LED output head differs from retained intent")
		}
		if head != "" {
			previous, err := readLEDOutputRecord(ctx, q, head)
			if err != nil || !previous.Complete || protocol.Hash(previous) != intent.PreviousHash {
				return result, true, false, fmt.Errorf("LED output predecessor changed")
			}
		}
		return result, true, false, nil
	}
	result, err = readLEDOutputRecord(ctx, q, head)
	if err != nil {
		return result, true, false, err
	}
	if protocol.Hash(result.Intent) != protocol.Hash(intent) || result.Next < 0 || result.Next > len(intent.Targets) || result.Complete && result.Next != len(intent.Targets) {
		return result, true, false, fmt.Errorf("LED output SQL authority changed")
	}
	if intent.PreviousOperation != "" {
		previous, err := readLEDOutputRecord(ctx, q, intent.PreviousOperation)
		if err != nil || !previous.Complete || protocol.Hash(previous) != intent.PreviousHash {
			return result, true, false, fmt.Errorf("LED output history missing or changed")
		}
	}
	return result, true, true, nil
}
func readLEDOutputRecord(ctx context.Context, q ledRowReader, op string) (r ledOutputRecord, err error) {
	var raw string
	err = q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", ledOutputPrefix+op).Scan(&raw)
	if err == nil {
		err = protocol.Decode([]byte(raw), &r, 256<<10)
	}
	return
}
func (s *Store) writeLEDOutputRecord(ctx context.Context, tx *sql.Tx, r ledOutputRecord) error {
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 256<<10 {
		return fmt.Errorf("LED output disposition exceeds budget")
	}
	if err = s.capacity(tx, "", len(raw), false); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", ledOutputPrefix+r.Intent.Request.Operation, string(raw))
	if err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", ledOutputHead, r.Intent.Request.Operation)
	}
	return err
}

// output admission is checked again at individual mutation boundaries. The
// suspended ledger remains intact; disabling output never marks a fault resolved.
func (s *Store) ledOutputAdmission(ctx context.Context, q ledRowReader) (bool, error) {
	r, present, recorded, err := s.readLEDOutput(ctx, q)
	if err != nil || !present {
		return false, err
	}
	if !recorded || !r.Complete {
		return true, fmt.Errorf("LED output cleanup or re-enable is pending")
	}
	return r.Intent.Request.Disabled, nil
}

func (s *Store) LEDOutputStatus(ctx context.Context) (LEDOutputStatus, error) {
	r, present, recorded, err := s.readLEDOutput(ctx, s.db)
	if err != nil || !present {
		return LEDOutputStatus{}, err
	}
	return ledOutputStatus(r, recorded), nil
}

func ledOutputStatus(r ledOutputRecord, recorded bool) LEDOutputStatus {
	return LEDOutputStatus{r.Intent.Request.Operation, r.Intent.Request.Disabled, recorded && r.Complete, r.Next, len(r.Intent.Targets)}
}

// RequireLEDOutput checks the installation's exact completed disposition. Empty
// operation requires absence, so deleting a manifest pin cannot re-admit output.
func (s *Store) RequireLEDOutput(ctx context.Context, operation string, disabled bool) error {
	r, present, recorded, err := s.readLEDOutput(ctx, s.db)
	if err != nil {
		return err
	}
	if operation == "" {
		if present {
			return fmt.Errorf("LED output disposition has no installation pin")
		}
		return nil
	}
	if !present || !recorded || !r.Complete || r.Intent.Request.Operation != operation || r.Intent.Request.Disabled != disabled {
		return fmt.Errorf("LED output disposition is missing, pending or changed")
	}
	return nil
}

// ConfigureLEDOutput clears only proven owned output, including active faults
// under this explicit output-disable disposition. Re-enable admits future fresh
// reconciliation; it neither refreshes healthy leases nor rewrites fault state.
func ConfigureLEDOutput(ctx context.Context, path string, c Config, request LEDOutputRequest, api LEDAPI) (LEDOutputStatus, error) {
	return configureLEDOutput(ctx, path, c, request, api, nil)
}
func configureLEDOutput(ctx context.Context, path string, c Config, request LEDOutputRequest, api LEDAPI, fault func(string) error) (LEDOutputStatus, error) {
	var zero LEDOutputStatus
	if !request.Identity.Valid() || !safefs.ValidID(request.Operation, "n") || !validLEDSocket(request.Socket) || api == nil {
		return zero, fmt.Errorf("exact LED output disposition and daemon required")
	}
	var s *Store
	var err error
	if len(c.Producers) == 0 {
		s, err = OpenBootstrapVerification(path, c, request.Identity, "")
	} else {
		s, err = OpenManagedVerification(path, c, request.Identity)
	}
	if err != nil {
		return zero, err
	}
	defer s.Close()
	if err = s.requireInstallationNotRetired(); err != nil {
		return zero, err
	}
	boundary := func(at string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return recoveryBoundary(fault, at)
	}
	current, present, recorded, err := s.readLEDOutput(ctx, s.db)
	if err != nil {
		return zero, err
	}
	if !present || current.Intent.Request.Operation != request.Operation {
		if present && (!recorded || !current.Complete) {
			return zero, fmt.Errorf("resume the retained LED output operation before changing it")
		}
		if !request.Disabled && (!present || !current.Intent.Request.Disabled) {
			return zero, fmt.Errorf("LED re-enable requires a completed output-disable disposition")
		}
		var prior int
		if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE key LIKE ?", ledOutputPrefix+"%").Scan(&prior); err != nil {
			return zero, err
		}
		if prior >= 64 {
			return zero, fmt.Errorf("LED output history budget exhausted; explicit compaction required")
		}
		var old string
		if e := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", ledOutputPrefix+request.Operation).Scan(&old); e != sql.ErrNoRows {
			return zero, fmt.Errorf("LED output operation is historical or unavailable")
		}
		intent, err := s.prepareLEDOutput(ctx, request)
		if err != nil {
			return zero, err
		}
		if present {
			intent.PreviousOperation, intent.PreviousHash = current.Intent.Request.Operation, protocol.Hash(current)
		}
		if err = intent.validate(c, s.identity); err != nil {
			return zero, err
		}
		// Fence refresh before daemon I/O. An unavailable daemon leaves a durable
		// cleanup-pending operation and cannot permit an old healthy refresh.
		if err = s.dir.WriteJSON(ledOutputFile, intent, present); err != nil {
			return zero, err
		}
		current, recorded = ledOutputRecord{Intent: intent}, false
		if err = boundary("output-intent"); err != nil {
			return zero, err
		}
	}
	if current.Intent.Request != request {
		return zero, fmt.Errorf("LED output request changed")
	}
	if current.Complete {
		if request.Disabled {
			if err = s.checkLEDOutputRegistry(ctx, api, current.Intent, true); err != nil {
				return zero, err
			}
		}
		return s.LEDOutputStatus(ctx)
	}
	if err = s.checkLEDOutputInventory(ctx, current.Intent); err != nil {
		return zero, err
	}
	if !recorded {
		if err = s.transaction(ctx, func(tx *sql.Tx) error { return s.writeLEDOutputRecord(ctx, tx, current) }); err != nil {
			return zero, err
		}
		if err = boundary("output-prepared"); err != nil {
			return zero, err
		}
	}
	for current.Next < len(current.Intent.Targets) {
		target := current.Intent.Targets[current.Next]
		// Every retry inspects the registry. Already absent entries need no RPC;
		// changed entries never inherit clear authority from an old acknowledgement.
		entries, err := s.ledOutputRegistry(ctx, api, current.Intent, false)
		if err != nil {
			return zero, err
		}
		for _, entry := range entries {
			if entry.OwnerUID == current.Intent.UID && entry.ID == target.ID {
				if !request.Disabled {
					return zero, fmt.Errorf("owned LED output reappeared before re-enable")
				}
				bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
				err = api.Clear(bounded, target.ID)
				cancel()
				if err != nil {
					return zero, err
				}
				if err = boundary("output-clear-returned"); err != nil {
					return zero, err
				}
			}
		}
		current.Next++
		if err = s.transaction(ctx, func(tx *sql.Tx) error { return s.writeLEDOutputRecord(ctx, tx, current) }); err != nil {
			return zero, err
		}
		if err = boundary("output-clear-recorded"); err != nil {
			return zero, err
		}
	}
	if err = s.checkLEDOutputRegistry(ctx, api, current.Intent, true); err != nil {
		return zero, err
	}
	current.Complete = true
	if err = s.transaction(ctx, func(tx *sql.Tx) error { return s.writeLEDOutputRecord(ctx, tx, current) }); err != nil {
		return zero, err
	}
	if err = boundary("output-complete"); err != nil {
		return zero, err
	}
	return s.LEDOutputStatus(ctx)
}

func (s *Store) prepareLEDOutput(ctx context.Context, request LEDOutputRequest) (ledOutputIntent, error) {
	v := ledOutputIntent{Version: 1, Request: request, UID: os.Getuid(), ConfigHash: protocol.Hash(s.config)}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := checkLEDLayout(ctx, tx, s.config, s.identity, true); err != nil {
			return err
		}
		if err := s.checkInheritedLEDHistory(ctx, tx); err != nil {
			return err
		}
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil {
			return err
		}
		var layout ledLayout
		if err := protocol.Decode([]byte(raw), &layout, protocol.MaxRequest); err != nil {
			return err
		}
		v.LayoutHash = protocol.Hash(layout)
		for _, b := range ledLayoutOutputs(layout) {
			base := ledBindingNamespace(s.identity.Worker, b.ID)
			v.Targets = append(v.Targets, ledOutputTarget{ID: base + ".baseline", Binding: b.ID, Condition: "baseline"}, ledOutputTarget{ID: base + ".healthy", Binding: b.ID, Condition: "ok"})
		}
		rows, err := tx.QueryContext(ctx, "SELECT id,binding,condition,identity FROM led_states ORDER BY id LIMIT 1025")
		if err != nil {
			return err
		}
		var faults []ledOutputTarget
		for rows.Next() {
			var target ledOutputTarget
			if err = rows.Scan(&target.ID, &target.Binding, &target.Condition, &target.Ownership); err != nil {
				rows.Close()
				return err
			}
			faults = append(faults, target)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(faults) > 1024 {
			return fmt.Errorf("LED output fault inventory exceeds budget")
		}
		for _, target := range faults {
			if err := s.checkLEDState(tx, target.ID, target.Binding, target.Ownership); err != nil {
				return err
			}
		}
		v.Targets = append(v.Targets, faults...)
		sort.Slice(v.Targets, func(i, j int) bool { return v.Targets[i].ID < v.Targets[j].ID })
		return nil
	})
	return v, err
}
func (s *Store) checkLEDOutputInventory(ctx context.Context, intent ledOutputIntent) error {
	actual, err := s.prepareLEDOutput(ctx, intent.Request)
	if err != nil {
		return err
	}
	actual.PreviousOperation, actual.PreviousHash = intent.PreviousOperation, intent.PreviousHash
	if protocol.Hash(actual) != protocol.Hash(intent) {
		return fmt.Errorf("LED output ownership inventory changed")
	}
	return nil
}
func (s *Store) checkLEDOutputRegistry(ctx context.Context, api LEDAPI, intent ledOutputIntent, absent bool) error {
	_, err := s.ledOutputRegistry(ctx, api, intent, absent)
	return err
}
func (s *Store) ledOutputRegistry(ctx context.Context, api LEDAPI, intent ledOutputIntent, absent bool) ([]led.Entry, error) {
	entries, err := mappingRegistry(ctx, api)
	if err != nil {
		return nil, err
	}
	bindings := map[string]LEDBinding{}
	for _, b := range ledLayoutOutputs(desiredLEDLayout(s.config, s.identity.Worker, os.Getuid())) {
		bindings[b.ID] = b
	}
	targets := map[string]ledOutputTarget{}
	for _, target := range intent.Targets {
		targets[target.ID] = target
	}
	for _, entry := range entries {
		if entry.OwnerUID != intent.UID {
			continue
		}
		target, owned := targets[entry.ID]
		if !owned {
			continue
		}
		if absent {
			return nil, fmt.Errorf("owned LED output remains visible in daemon registry")
		}
		binding, ok := bindings[target.Binding]
		if !ok {
			return nil, fmt.Errorf("LED output binding no longer exists")
		}
		style, ok := binding.Styles[target.Condition]
		if !ok || entry.LED != binding.LED || entry.Pattern != style.Pattern || entry.Priority != style.Priority {
			return nil, fmt.Errorf("LED registry entry differs from owned output")
		}
		message := "Notification state: " + target.Condition
		if target.Condition == "baseline" {
			message = "Notification observations unavailable"
		}
		if target.Condition == "ok" {
			message = "Required producers have fresh evidence"
		}
		if target.Ownership != "legacy" && entry.Message != message {
			return nil, fmt.Errorf("LED registry message differs from owned output")
		}
		leased := target.Condition == "ok" || target.Condition == "paused"
		if leased && (entry.Persistent || entry.TTL == nil || *entry.TTL < 1 || *entry.TTL > 90) || !leased && (!entry.Persistent || entry.TTL != nil) {
			return nil, fmt.Errorf("LED registry persistence differs from owned output")
		}
	}
	return entries, nil
}
