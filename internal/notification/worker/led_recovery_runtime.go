package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const runtimeLEDClearPrefix = "led-runtime-clear/"
const runtimeLEDClearHead = "led-runtime-clear-head.json"
const maxRuntimeLEDClears = 128

type RuntimeLEDClearProposal struct {
	Fault        recoveryLEDFault `json:"fault"`
	RegistryHash string           `json:"registry_hash"`
	Hash         string           `json:"hash"`
}
type RuntimeLEDRecoveryReview struct {
	Identity   protocol.Identity         `json:"identity"`
	CommitHash string                    `json:"commit_hash"`
	Next       int                       `json:"next"`
	Condition  string                    `json:"condition"`
	ValidUntil time.Time                 `json:"valid_until"`
	Faults     []RuntimeLEDClearProposal `json:"faults"`
}
type runtimeLEDClearIntent struct {
	Version    int                     `json:"version"`
	Sequence   int                     `json:"sequence"`
	Previous   string                  `json:"previous"`
	Identity   protocol.Identity       `json:"identity"`
	CommitHash string                  `json:"commit_hash"`
	ConfigHash string                  `json:"config_hash"`
	Proposal   RuntimeLEDClearProposal `json:"proposal"`
	HealthHash string                  `json:"health_hash"`
	IssuedAt   time.Time               `json:"issued_at"`
	Deadline   time.Time               `json:"deadline"`
}
type RuntimeLEDClearResult struct {
	Intent  runtimeLEDClearIntent `json:"intent"`
	Outcome string                `json:"outcome"`
}

func runtimeLEDClearKey(n int) string  { return fmt.Sprintf("%s%06d", runtimeLEDClearPrefix, n) }
func runtimeLEDClearFile(n int) string { return fmt.Sprintf("led-runtime-clear-%06d", n) }
func (s *Store) runtimeLEDProposalHash(n int, fault recoveryLEDFault, registry string) string {
	return protocol.Hash(struct {
		Identity       protocol.Identity
		Commit, Config string
		Sequence       int
		Fault          recoveryLEDFault
		Registry       string
	}{s.identity, s.recoveryCommitHash, protocol.Hash(s.config), n, fault, registry})
}

// BindManagedLEDRecovery is installed once before exposing the managed API. The
// admission guard and admitted daemon client remain owned by that runtime.
func (s *Store) BindManagedLEDRecovery(api LEDAPI, check func(context.Context) error) error {
	if s.verificationOnly || s.recoveryCommitHash == "" || s.deliveryActivationHash == "" || api == nil || check == nil || s.runtimeLEDAPI != nil {
		return fmt.Errorf("LED recovery requires admitted managed recovered delivery")
	}
	s.runtimeLEDAPI, s.runtimeLEDCheck = api, check
	return nil
}

func (s *Store) validateRuntimeLEDClear(v RuntimeLEDClearResult, previous string, n int, originals []recoveryLEDFault) error {
	i := v.Intent
	if i.Version != 1 || i.Sequence != n || i.Previous != previous || i.Identity != s.identity || i.CommitHash != s.recoveryCommitHash || i.ConfigHash != protocol.Hash(s.config) || i.Proposal.Fault.Binding != workerLEDBinding || !slices.Contains(originals, i.Proposal.Fault) || i.Proposal.Hash != s.runtimeLEDProposalHash(n, i.Proposal.Fault, i.Proposal.RegistryHash) || !safefs.DigestPattern.MatchString(i.HealthHash) || !safefs.DigestPattern.MatchString(i.CommitHash) || i.IssuedAt.IsZero() || !i.Deadline.After(i.IssuedAt) || !slices.Contains([]string{"pending", "not_sent", "uncertain", "acknowledged", "observed_absent"}, v.Outcome) {
		return fmt.Errorf("invalid runtime LED cleanup evidence")
	}
	return nil
}

// A file head survives rolled-back SQL. Pending work never authorizes removal,
// and replay never repeats a possibly sent Clear. Completed rows require both
// independent records, even after the fault has left the active ledger.
func (s *Store) readRuntimeLEDClears(ctx context.Context, q ledJournalReader, originals []recoveryLEDFault) ([]RuntimeLEDClearResult, error) {
	rows, err := q.QueryContext(ctx, "SELECT key,value FROM metadata WHERE key LIKE ? ORDER BY key LIMIT 129", s.runtimeLEDClearKeyPrefix()+"%")
	if err != nil {
		return nil, err
	}
	var records []RuntimeLEDClearResult
	for rows.Next() {
		var key, raw string
		var v RuntimeLEDClearResult
		if err = rows.Scan(&key, &raw); err != nil {
			break
		}
		if protocol.Decode([]byte(raw), &v, 16384) != nil || key != s.runtimeLEDClearKey(len(records)+1) || len(records) >= maxRuntimeLEDClears {
			err = fmt.Errorf("invalid runtime LED cleanup history")
			break
		}
		records = append(records, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	var head runtimeLEDClearIntent
	err = s.runtimeLEDDirectory().ReadJSON(s.runtimeLEDClearHead(), &head)
	if os.IsNotExist(err) && len(records) == 0 {
		return nil, s.auditRuntimeLEDClearFiles(nil)
	}
	if err != nil {
		return nil, err
	}
	sqlCount := len(records)
	if head.Sequence == len(records)+1 {
		records = append(records, RuntimeLEDClearResult{Intent: head, Outcome: "pending"})
	}
	if len(records) == 0 || len(records) > maxRuntimeLEDClears || head.Sequence != len(records) || protocol.Hash(head) != protocol.Hash(records[len(records)-1].Intent) {
		return nil, fmt.Errorf("runtime LED cleanup head lost or changed")
	}
	previous := ""
	for n, v := range records {
		if err = s.validateRuntimeLEDClear(v, previous, n+1, originals); err != nil {
			return nil, err
		}
		if v.Outcome == "pending" && n != len(records)-1 {
			return nil, fmt.Errorf("unfinished LED cleanup hidden behind later history")
		}
		var intent runtimeLEDClearIntent
		e := s.runtimeLEDDirectory().ReadJSON(s.runtimeLEDClearFile(n+1)+".json", &intent)
		if !(v.Outcome == "pending" && n >= sqlCount && os.IsNotExist(e)) && (e != nil || protocol.Hash(intent) != protocol.Hash(v.Intent)) {
			return nil, fmt.Errorf("runtime LED cleanup intent lost or changed")
		}
		if n >= sqlCount {
			var result RuntimeLEDClearResult
			if e := s.runtimeLEDDirectory().ReadJSON(s.runtimeLEDClearFile(n+1)+"-result.json", &result); !os.IsNotExist(e) {
				return nil, fmt.Errorf("runtime LED cleanup lost SQL intent")
			}
		}
		if v.Outcome != "pending" {
			var result RuntimeLEDClearResult
			if e = s.runtimeLEDDirectory().ReadJSON(s.runtimeLEDClearFile(n+1)+"-result.json", &result); e != nil || protocol.Hash(result) != protocol.Hash(v) {
				return nil, fmt.Errorf("runtime LED cleanup result lost or changed")
			}
		}
		previous = protocol.Hash(v.Intent)
	}
	return records, s.auditRuntimeLEDClearFiles(records)
}

func (s *Store) auditRuntimeLEDClearFiles(records []RuntimeLEDClearResult) error {
	allowed := map[string]bool{s.runtimeLEDClearHead(): len(records) > 0}
	for _, v := range records {
		allowed[s.runtimeLEDClearFile(v.Intent.Sequence)+".json"] = true
		allowed[s.runtimeLEDClearFile(v.Intent.Sequence)+"-result.json"] = true
	}
	d, err := os.Open(s.runtimeLEDDirectory().File().Name())
	if err != nil {
		return err
	}
	defer d.Close()
	held, err := s.runtimeLEDDirectory().File().Stat()
	if err != nil {
		return err
	}
	current, err := d.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(held, current) {
		return fmt.Errorf("LED cleanup directory changed")
	}
	count := 0
	for {
		entries, e := d.ReadDir(128)
		for _, entry := range entries {
			count++
			if count > 16384 {
				return fmt.Errorf("LED cleanup directory audit exceeds budget")
			}
			if strings.HasPrefix(entry.Name(), s.runtimeLEDClearFilePrefix()) && (!allowed[entry.Name()] || !entry.Type().IsRegular()) {
				return fmt.Errorf("unacknowledged or later LED cleanup evidence")
			}
		}
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
	}
}

func (s *Store) remainingRuntimeLEDFaults(ctx context.Context, q ledJournalReader, originals, remaining []recoveryLEDFault) ([]recoveryLEDFault, error) {
	records, err := s.readRuntimeLEDClears(ctx, q, originals)
	if err != nil {
		return nil, err
	}
	completed := map[string]bool{}
	for _, v := range records {
		if v.Outcome == "acknowledged" || v.Outcome == "observed_absent" {
			completed[v.Intent.Proposal.Fault.ID] = true
		}
	}
	var out []recoveryLEDFault
	for _, f := range remaining {
		if !completed[f.ID] {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *Store) runtimeLEDReview(ctx context.Context) (out RuntimeLEDRecoveryReview, h Health, err error) {
	if s.runtimeLEDAPI == nil || s.runtimeLEDCheck == nil || s.verificationOnly {
		return out, h, fmt.Errorf("managed LED recovery unavailable")
	}
	if err = s.runtimeLEDCheck(ctx); err != nil {
		return
	}
	out.Identity, out.CommitHash = s.identity, s.recoveryCommitHash
	var faults []recoveryLEDFault
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		authority, e := s.recoveredLEDAuthority(ctx, tx)
		if e != nil {
			return e
		}
		if authority == nil {
			return fmt.Errorf("no inherited LED ownership")
		}
		if e = s.checkInheritedLEDHistory(ctx, tx); e != nil {
			return e
		}
		records, e := s.readRuntimeLEDClears(ctx, tx, authority.After.Faults)
		if e != nil {
			return e
		}
		out.Next = len(records) + 1
		if len(records) > 0 && records[len(records)-1].Outcome == "pending" {
			return fmt.Errorf("resume pending LED cleanup before another review")
		}
		if out.Next > maxRuntimeLEDClears {
			return fmt.Errorf("runtime LED cleanup history exhausted")
		}
		faults = authority.Remaining
		return nil
	})
	if err != nil {
		return
	}
	h, err = s.health(ctx, true)
	if err != nil {
		return
	}
	out.Condition, out.ValidUntil = h.Condition, h.ValidUntil
	registry, _, _, e := observeRecoveryLED(ctx, s.runtimeLEDAPI)
	if e != nil {
		err = e
		return
	}
	for _, f := range faults {
		if f.Binding != workerLEDBinding {
			continue
		}
		hash := "absent"
		for _, entry := range registry {
			if entry.ID == f.ID && entry.UID == os.Getuid() {
				hash = entry.StateHash
			}
		}
		out.Faults = append(out.Faults, RuntimeLEDClearProposal{Fault: f, RegistryHash: hash, Hash: s.runtimeLEDProposalHash(out.Next, f, hash)})
	}
	slices.SortFunc(out.Faults, func(a, b RuntimeLEDClearProposal) int {
		if a.Fault.ID < b.Fault.ID {
			return -1
		}
		if a.Fault.ID > b.Fault.ID {
			return 1
		}
		return 0
	})
	return
}
func (s *Store) ReviewRuntimeLEDRecovery(ctx context.Context) (RuntimeLEDRecoveryReview, error) {
	if !s.ledMu.TryLock() {
		return RuntimeLEDRecoveryReview{}, fmt.Errorf("LED reconciliation busy")
	}
	defer s.ledMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _, err := s.runtimeLEDReview(ctx)
	return out, err
}

func (s *Store) finishRuntimeLEDClear(ctx context.Context, v RuntimeLEDClearResult) error {
	if v.Outcome == "pending" {
		return fmt.Errorf("cleanup result must be historical")
	}
	name := s.runtimeLEDClearFile(v.Intent.Sequence) + "-result.json"
	var saved RuntimeLEDClearResult
	e := s.runtimeLEDDirectory().ReadJSON(name, &saved)
	if os.IsNotExist(e) {
		if e = s.runtimeLEDDirectory().WriteJSON(name, v, false); e != nil {
			return e
		}
	} else if e != nil || protocol.Hash(saved) != protocol.Hash(v) {
		return fmt.Errorf("LED cleanup result conflict")
	}
	if e = recoveryBoundary(s.ledOperationFault, "runtime-led-result"); e != nil {
		return e
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var intent runtimeLEDClearIntent
		if e := s.runtimeLEDDirectory().ReadJSON(s.runtimeLEDClearHead(), &intent); e != nil || protocol.Hash(intent) != protocol.Hash(v.Intent) {
			return fmt.Errorf("LED cleanup head changed")
		}
		raw, _ := json.Marshal(v)
		if _, e := tx.Exec("INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", s.runtimeLEDClearKey(v.Intent.Sequence), string(raw)); e != nil {
			return e
		}
		if v.Outcome == "acknowledged" || v.Outcome == "observed_absent" {
			f := v.Intent.Proposal.Fault
			if _, e := tx.Exec("DELETE FROM led_states WHERE id=? AND binding=? AND condition=? AND identity=?", f.ID, f.Binding, f.Condition, f.Identity); e != nil {
				return e
			}
		}
		return recoveryBoundary(s.ledOperationFault, "runtime-led-recorded")
	})
}

// ClearRuntimeLEDRecovery clears one reviewed inherited aggregate incarnation.
// It cannot clear producer bindings, foreign/current faults or publish healthy
// output. Fresh producer and loop evidence is required again before the RPC.
func (s *Store) ClearRuntimeLEDRecovery(ctx context.Context, id, hash string) (out RuntimeLEDClearResult, err error) {
	if !s.ledMu.TryLock() {
		return out, fmt.Errorf("LED reconciliation busy")
	}
	defer s.ledMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if s.runtimeLEDAPI == nil || s.runtimeLEDCheck == nil || s.verificationOnly {
		return out, fmt.Errorf("managed LED recovery unavailable")
	}
	if err = s.runtimeLEDCheck(ctx); err != nil {
		return
	}
	// Resume historical intent/result first, including a crash before SQL intent.
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		authority, e := s.recoveredLEDAuthority(ctx, tx)
		if e != nil {
			return e
		}
		if authority == nil {
			return fmt.Errorf("no inherited ownership")
		}
		records, e := s.readRuntimeLEDClears(ctx, tx, authority.After.Faults)
		if e != nil {
			return e
		}
		for _, v := range records {
			if v.Intent.Proposal.Fault.ID == id && v.Intent.Proposal.Hash == hash {
				out = v
				return nil
			}
		}
		if len(records) > 0 && records[len(records)-1].Outcome == "pending" {
			return fmt.Errorf("another LED cleanup requires resume")
		}
		return nil
	})
	if err != nil {
		return
	}
	if out.Intent.Sequence != 0 {
		if out.Outcome != "pending" {
			return
		}
		var result RuntimeLEDClearResult
		e := s.runtimeLEDDirectory().ReadJSON(s.runtimeLEDClearFile(out.Intent.Sequence)+"-result.json", &result)
		if os.IsNotExist(e) {
			result = out
			result.Outcome = "uncertain"
		} else if e != nil || protocol.Hash(result.Intent) != protocol.Hash(out.Intent) || !slices.Contains([]string{"not_sent", "uncertain", "acknowledged", "observed_absent"}, result.Outcome) {
			return out, fmt.Errorf("pending LED cleanup result changed")
		}
		name := s.runtimeLEDClearFile(out.Intent.Sequence) + ".json"
		var intent runtimeLEDClearIntent
		if e = s.runtimeLEDDirectory().ReadJSON(name, &intent); os.IsNotExist(e) {
			e = s.runtimeLEDDirectory().WriteJSON(name, out.Intent, false)
		}
		if e != nil {
			return out, e
		}
		out = result
		err = s.finishRuntimeLEDClear(ctx, out)
		return
	}
	review, h, e := s.runtimeLEDReview(ctx)
	if e != nil {
		return out, e
	}
	var proposal RuntimeLEDClearProposal
	for _, p := range review.Faults {
		if p.Fault.ID == id && p.Hash == hash {
			proposal = p
		}
	}
	if proposal.Hash == "" || h.Condition != "ok" || !h.ValidUntil.After(s.now().Add(2500*time.Millisecond)) {
		return out, fmt.Errorf("LED cleanup requires exact review and fresh aggregate health")
	}
	// Registry ownership is checked using the recorded original condition/style.
	r := LEDReconciler{Store: s, API: s.runtimeLEDAPI, Budget: 2 * time.Second}
	if e = r.checkLEDClearRegistry(ctx, ledOperation{Kind: "clear", ID: id, Condition: proposal.Fault.Condition}); e != nil {
		return out, e
	}
	previous := ""
	if review.Next > 1 {
		var head runtimeLEDClearIntent
		if e = s.runtimeLEDDirectory().ReadJSON(s.runtimeLEDClearHead(), &head); e != nil {
			return out, e
		}
		previous = protocol.Hash(head)
	}
	out = RuntimeLEDClearResult{Intent: runtimeLEDClearIntent{1, review.Next, previous, s.identity, s.recoveryCommitHash, protocol.Hash(s.config), proposal, protocol.Hash(h), s.now().UTC(), h.ValidUntil}, Outcome: "pending"}
	if err = s.runtimeLEDDirectory().WriteJSON(s.runtimeLEDClearHead(), out.Intent, review.Next > 1); err != nil {
		return
	}
	if err = recoveryBoundary(s.ledOperationFault, "runtime-led-head"); err != nil {
		return
	}
	if err = s.runtimeLEDDirectory().WriteJSON(s.runtimeLEDClearFile(review.Next)+".json", out.Intent, false); err != nil {
		return
	}
	raw, _ := json.Marshal(out)
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec("INSERT INTO metadata(key,value) VALUES(?,?)", s.runtimeLEDClearKey(review.Next), string(raw))
		return e
	})
	if err != nil {
		return
	}
	if err = recoveryBoundary(s.ledOperationFault, "runtime-led-intent"); err != nil {
		return
	}
	out.Outcome = "not_sent"
	latest, e := s.health(ctx, true)
	if e == nil && (!ledHealthCurrent(h, latest) || !out.Intent.Deadline.After(s.now().Add(2500*time.Millisecond))) {
		e = errLEDObservationChanged
	}
	if e == nil {
		e = s.runtimeLEDCheck(ctx)
	}
	if e == nil {
		e = r.checkLEDClearRegistry(ctx, ledOperation{Kind: "clear", ID: id, Condition: proposal.Fault.Condition})
	}
	if e == nil {
		// Repeat the exact registry observation after journaling. A replacement
		// with the same style is still a changed reviewed incarnation.
		registry, _, _, readErr := observeRecoveryLED(ctx, s.runtimeLEDAPI)
		e = readErr
		actual := "absent"
		for _, entry := range registry {
			if entry.ID == id && entry.UID == os.Getuid() {
				actual = entry.StateHash
			}
		}
		if e == nil && actual != proposal.RegistryHash {
			e = fmt.Errorf("LED registry changed after review")
		}
	}
	if e == nil {
		latest, e = s.health(ctx, true)
		if e == nil && (!ledHealthCurrent(h, latest) || !out.Intent.Deadline.After(s.now().Add(2500*time.Millisecond))) {
			e = errLEDObservationChanged
		}
	}
	if e == nil && proposal.RegistryHash == "absent" {
		out.Outcome = "observed_absent"
	} else if e == nil {
		rpc, stop := context.WithTimeout(ctx, 2*time.Second)
		e = s.runtimeLEDAPI.Clear(rpc, id)
		if e == nil {
			e = rpc.Err()
		}
		stop()
		out.Outcome = "uncertain"
		if e == nil {
			out.Outcome = "acknowledged"
		}
		if err = recoveryBoundary(s.ledOperationFault, "runtime-led-returned"); err != nil {
			return
		}
	}
	finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	err = errors.Join(e, s.finishRuntimeLEDClear(finish, out))
	return
}
