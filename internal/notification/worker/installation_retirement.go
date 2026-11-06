package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// InstallationRetirement retains the old identity and permanent producer fences.
// It grants no authority in the replacement installation.
type InstallationRetirement struct {
	Version    int                  `json:"version"`
	Identity   protocol.Identity    `json:"identity"`
	UID        int                  `json:"uid"`
	ConfigHash string               `json:"config_hash"`
	TargetHash string               `json:"target_hash"`
	Socket     string               `json:"socket,omitempty"`
	Producers  []ProducerRetirement `json:"producers"`
}

func (s *Store) retirementInstallationEvidence(ctx context.Context, target, socket string) (out InstallationRetirement, err error) {
	if !safefs.DigestPattern.MatchString(target) || (socket != "" && !validLEDSocket(socket)) {
		return out, fmt.Errorf("invalid replacement target or LED socket")
	}
	if err = s.checkRetirementLease(); err != nil {
		return
	}
	out = InstallationRetirement{Version: 1, Identity: s.identity, UID: os.Getuid(), ConfigHash: protocol.Hash(s.config), TargetHash: target, Socket: socket}
	for _, e := range s.config.Producers {
		r, e := s.RetireProducer(ctx, e.Producer, "archive", "Review permanent retirement", false)
		if e != nil {
			return out, e
		}
		if !r.Applied || r.Retirement == nil {
			return out, fmt.Errorf("every producer must be permanently retired before installation retirement")
		}
		out.Producers = append(out.Producers, *r.Retirement)
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{"SELECT count(*) FROM led_states", "SELECT count(*) FROM provider_inbox WHERE state='pending'", "SELECT count(*) FROM command_replies WHERE state NOT IN ('provider_accepted','expired','superseded') OR attempt<>''"} {
			var count int
			if e := tx.QueryRowContext(ctx, q).Scan(&count); e != nil {
				return e
			}
			if count != 0 {
				return fmt.Errorf("installation retirement has outstanding LED, input or reply obligations")
			}
		}
		return checkLEDLayout(ctx, tx, s.config, s.identity, true)
	})
	return
}

// ReviewInstallationRetirement verifies settlement without publishing a fence.
func (s *Store) ReviewInstallationRetirement(ctx context.Context, target, socket string, api LEDAPI) error {
	_, err := s.retirementInstallationEvidence(ctx, target, socket)
	if err != nil {
		return err
	}
	if socket != "" {
		return s.reviewRetirementLED(ctx, api)
	}
	return nil
}

// RetireInstallation is an offline terminal transition. Call only after the
// service is uninstalled. The caller holds its installation lease. It never
// deletes history or clears a fault. A lost clear result is reconciled by listing
// the same UID's exact reserved baseline/healthy IDs before retrying.
func (s *Store) RetireInstallation(ctx context.Context, target, socket string, api LEDAPI) (InstallationRetirement, error) {
	return s.retireInstallation(ctx, target, socket, api, nil)
}
func (s *Store) retireInstallation(ctx context.Context, target, socket string, api LEDAPI, fault func(string) error) (out InstallationRetirement, err error) {
	out, err = s.retirementInstallationEvidence(ctx, target, socket)
	if err != nil {
		return
	}
	if socket != "" {
		if err = s.reviewRetirementLED(ctx, api); err != nil {
			return
		}
	}
	if err = s.verifyInstallationRetirement(ctx); err != nil {
		return
	}
	var intent InstallationRetirement
	err = s.dir.ReadJSON("installation-retirement-intent.json", &intent)
	if err == nil {
		if protocol.Hash(intent) != protocol.Hash(out) {
			return out, fmt.Errorf("installation retirement target or evidence changed")
		}
	} else if os.IsNotExist(err) {
		if err = s.dir.WriteJSON("installation-retirement-intent.json", out, false); err != nil {
			return
		}
	} else {
		return
	}

	body, e := json.Marshal(out)
	if e != nil {
		return out, e
	}
	if err = s.transaction(ctx, func(tx *sql.Tx) error {
		var saved string
		e := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='installation-retirement'").Scan(&saved)
		if e == nil {
			if saved != string(body) {
				return fmt.Errorf("retirement SQL evidence changed")
			}
			return nil
		}
		if e != sql.ErrNoRows {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES('installation-retirement',?)", string(body))
		return e
	}); err != nil {
		return
	}
	if err = recoveryBoundary(fault, "retirement-intent"); err != nil {
		return
	}
	if socket != "" {
		if api == nil {
			return out, fmt.Errorf("original LED socket is required")
		}
		layout := desiredLEDLayout(s.config, s.identity.Worker, os.Getuid())
		for _, b := range ledLayoutOutputs(layout) {
			for _, suffix := range []string{"healthy", "baseline"} {
				entries, e := mappingRegistry(ctx, api)
				if e != nil {
					return out, e
				}
				id := ledBindingNamespace(layout.Worker, b.ID) + "." + suffix
				for _, entry := range entries {
					if entry.OwnerUID != layout.UID || entry.ID != id {
						continue
					}
					if entry.LED != b.LED {
						return out, fmt.Errorf("retired LED target changed")
					}
					if suffix == "baseline" && protocol.Hash(entry.State) != protocol.Hash(ledBaseline(layout.Worker, b)) {
						return out, fmt.Errorf("retired LED baseline changed")
					}
					if suffix == "healthy" && (entry.Persistent || entry.Pattern != b.Styles["ok"].Pattern || entry.Priority != b.Styles["ok"].Priority) {
						return out, fmt.Errorf("retired LED lease changed")
					}
					if err = api.Clear(ctx, id); err != nil {
						return out, err
					}
					if err = recoveryBoundary(fault, "retirement-clear"); err != nil {
						return
					}
				}
			}
		}
		entries, e := mappingRegistry(ctx, api)
		if e != nil {
			return out, e
		}
		for _, entry := range entries {
			if entry.OwnerUID != layout.UID {
				continue
			}
			if strings.HasPrefix(entry.ID, ledBindingNamespace(layout.Worker, workerLEDBinding)+".") {
				return out, fmt.Errorf("retired worker LED state remains")
			}
			for _, b := range layout.Bindings {
				if strings.HasPrefix(entry.ID, ledBindingNamespace(layout.Worker, b.ID)+".") {
					return out, fmt.Errorf("retired binding LED state remains")
				}
			}
		}
	}
	var complete InstallationRetirement
	if e := s.dir.ReadJSON("installation-retirement.json", &complete); e == nil {
		if protocol.Hash(complete) != protocol.Hash(out) {
			return out, fmt.Errorf("retirement completion changed")
		}
		return out, nil
	} else if !os.IsNotExist(e) {
		return out, e
	}
	if err = s.dir.WriteJSON("installation-retirement.json", out, false); err != nil {
		return
	}
	err = recoveryBoundary(fault, "retirement-complete")
	return
}

// Refuse LED publication after terminal archival, including through an unmanaged
// invocation of a retained executable. Inspection and receipt history remain open.
func (s *Store) requireInstallationNotRetired() error {
	f, err := s.dir.Open("installation-retirement-intent.json", os.O_RDONLY, 0)
	if err == nil {
		f.Close()
		return fmt.Errorf("installation permanently retired; retain state for audit")
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RequireFreshReplacement checks that interrupted initialization has not gained
// work or ownership before publishing its independently retained identity.
func (s *Store) RequireFreshReplacement(ctx context.Context) error {
	for _, e := range s.config.Producers {
		o, err := s.readOwnership(e.Producer)
		if err != nil {
			return err
		}
		if len(o.Generations) != 0 || o.Retirement != nil || o.RecoveryPending != nil {
			return fmt.Errorf("replacement identity publication requires untouched producer ownership")
		}
	}
	for _, table := range []string{"messages", "receipts", "status", "commands", "provider_inbox", "led_states"} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("replacement has work before identity publication")
		}
	}
	return nil
}

func (s *Store) verifyInstallationRetirement(ctx context.Context) error {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='installation-retirement'").Scan(&raw)
	if err == sql.ErrNoRows {
		f, e := s.dir.Open("installation-retirement.json", os.O_RDONLY, 0)
		if e == nil {
			f.Close()
			return fmt.Errorf("completed retirement lost SQL evidence")
		}
		if !os.IsNotExist(e) {
			return e
		}
		return nil
	}
	if err != nil {
		return err
	}
	var record, file InstallationRetirement
	if err = protocol.Decode([]byte(raw), &record, safefs.MaxRecordBytes); err != nil {
		return err
	}
	if err = s.dir.ReadJSON("installation-retirement-intent.json", &file); err != nil {
		return err
	}
	if record.Identity != s.identity || record.ConfigHash != protocol.Hash(s.config) || protocol.Hash(record) != protocol.Hash(file) {
		return fmt.Errorf("retirement identity or independent evidence changed")
	}
	if e := s.dir.ReadJSON("installation-retirement.json", &file); e == nil {
		if protocol.Hash(record) != protocol.Hash(file) {
			return fmt.Errorf("retirement completion changed")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	return nil
}

func (s *Store) reviewRetirementLED(ctx context.Context, api LEDAPI) error {
	if api == nil {
		return fmt.Errorf("original LED socket is required")
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil {
		return err
	}
	var layout ledLayout
	if err := protocol.Decode([]byte(raw), &layout, protocol.MaxRequest); err != nil {
		return err
	}
	entries, err := mappingRegistry(ctx, api)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.OwnerUID != layout.UID {
			continue
		}
		for _, legacy := range layout.Legacy {
			if entry.ID == legacy {
				return fmt.Errorf("legacy LED state must be reconciled before retirement")
			}
		}
		for _, b := range ledLayoutOutputs(layout) {
			prefix := ledBindingNamespace(layout.Worker, b.ID) + "."
			if !strings.HasPrefix(entry.ID, prefix) {
				continue
			}
			if entry.LED != b.LED {
				return fmt.Errorf("retired LED target changed")
			}
			switch strings.TrimPrefix(entry.ID, prefix) {
			case "baseline":
				if protocol.Hash(entry.State) != protocol.Hash(ledBaseline(layout.Worker, b)) {
					return fmt.Errorf("retired LED baseline changed")
				}
			case "healthy":
				if entry.Persistent || entry.Pattern != b.Styles["ok"].Pattern || entry.Priority != b.Styles["ok"].Priority {
					return fmt.Errorf("retired LED lease changed")
				}
			default:
				return fmt.Errorf("fault or unfamiliar LED state requires reconciliation before retirement")
			}
		}
	}
	return nil
}
