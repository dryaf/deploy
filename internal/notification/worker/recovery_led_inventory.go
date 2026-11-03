package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const maxRecoveryLEDInventories = 8
const maxRecoveryLEDInventoryBytes = 4 << 20
const recoveryLEDHead = "led-inventory-head.json"

type recoveryLEDInventorySet struct {
	Records []recoveryLEDInventory
	Head    *recoveryLEDInventory
	Pending bool
}

func recoveryLEDInventoryFile(id string) string { return "led-inventory-" + id + ".json" }

func (r *recoveryReconciler) readLEDInventorySet(ctx context.Context, q ledJournalReader) (set recoveryLEDInventorySet, err error) {
	var version string
	err = q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-inventory-version'").Scan(&version)
	if err == sql.ErrNoRows {
		var tables int
		if err = q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='reconciliation_led_inventories'").Scan(&tables); err != nil {
			return
		}
		var head recoveryLEDInventory
		e := r.stage.ReadJSON(recoveryLEDHead, &head)
		if tables != 0 || !os.IsNotExist(e) {
			return set, fmt.Errorf("unowned LED recovery inventory")
		}
		return set, nil
	}
	if err != nil {
		return
	}
	if version != "1" {
		return set, fmt.Errorf("unsupported LED inventory version")
	}
	rows, err := q.QueryContext(ctx, "SELECT sequence,id,hash,body FROM reconciliation_led_inventories ORDER BY sequence LIMIT 9")
	if err != nil {
		return set, err
	}
	previous := ""
	for rows.Next() {
		var seq int
		var id, hash, body string
		var v recoveryLEDInventory
		if err = rows.Scan(&seq, &id, &hash, &body); err != nil {
			break
		}
		if err = protocol.Decode([]byte(body), &v, maxRecoveryLEDInventoryBytes); err != nil {
			break
		}
		if err = r.validateLEDInventory(v); err != nil {
			break
		}
		if seq != len(set.Records)+1 || v.Sequence != seq || v.ID != id || protocol.Hash(v) != hash || v.PreviousHash != previous {
			err = fmt.Errorf("LED inventory history gap or conflict")
			break
		}
		if len(set.Records) > 0 && v.Socket != set.Records[0].Socket {
			err = fmt.Errorf("LED inventory endpoint changed")
			break
		}
		var saved recoveryLEDInventory
		if err = r.stage.ReadJSON(recoveryLEDInventoryFile(id), &saved); err != nil {
			break
		}
		if protocol.Hash(saved) != hash {
			err = fmt.Errorf("LED inventory archive mismatch")
			break
		}
		set.Records = append(set.Records, v)
		previous = hash
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return
	}
	var head recoveryLEDInventory
	err = r.stage.ReadJSON(recoveryLEDHead, &head)
	if os.IsNotExist(err) && len(set.Records) == 0 {
		return set, nil
	}
	if err != nil {
		return
	}
	if err = r.validateLEDInventory(head); err != nil {
		return
	}
	if len(set.Records) > 0 && head.Socket != set.Records[0].Socket {
		return set, fmt.Errorf("LED inventory endpoint changed")
	}
	switch head.Sequence {
	case len(set.Records):
		if len(set.Records) == 0 || protocol.Hash(head) != previous {
			return set, fmt.Errorf("LED inventory head differs from committed history")
		}
	case len(set.Records) + 1:
		if head.PreviousHash != previous {
			return set, fmt.Errorf("LED inventory intent predecessor changed")
		}
		var saved recoveryLEDInventory
		e := r.stage.ReadJSON(recoveryLEDInventoryFile(head.ID), &saved)
		if e != nil && !os.IsNotExist(e) {
			return set, e
		}
		if e == nil && protocol.Hash(saved) != protocol.Hash(head) {
			return set, fmt.Errorf("LED inventory intent archive changed")
		}
		set.Pending = true
	default:
		return set, fmt.Errorf("LED inventory head was rolled back or skipped history")
	}
	set.Head = &head
	return set, nil
}

func (r *recoveryReconciler) ensureLEDInventory(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := r.readLEDInventorySet(ctx, tx); err != nil {
			return err
		}
		var version string
		err := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-inventory-version'").Scan(&version)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if err = r.capacity(tx, 4096); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `CREATE TABLE reconciliation_led_inventories(sequence INTEGER PRIMARY KEY,id TEXT NOT NULL UNIQUE,hash TEXT NOT NULL,body TEXT NOT NULL);
INSERT INTO metadata VALUES('led-inventory-version','1');`)
		return err
	})
}

func (r *recoveryReconciler) sealLEDInventory(ctx context.Context, v recoveryLEDInventory, expectedHead string, resume bool) error {
	if err := r.validateLEDInventory(v); err != nil {
		return err
	}
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > maxRecoveryLEDInventoryBytes {
		return fmt.Errorf("LED inventory exceeds storage budget")
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		set, err := r.readLEDInventorySet(ctx, tx)
		if err != nil {
			return err
		}
		if protocol.Hash(set.Head) != expectedHead {
			return fmt.Errorf("LED inventory changed during observation")
		}
		if resume {
			if !set.Pending || protocol.Hash(set.Head) != protocol.Hash(v) {
				return fmt.Errorf("LED inventory resume identity changed")
			}
		} else {
			if set.Pending || v.Sequence != len(set.Records)+1 {
				return fmt.Errorf("LED inventory preparation changed")
			}
			sources, err := r.collectLEDSources(ctx, tx)
			if err != nil {
				return err
			}
			if protocol.Hash(sources) != protocol.Hash(v.Sources) {
				return fmt.Errorf("LED ownership changed during registry observation")
			}
			boot, uptime, err := r.recoveryClock()
			if err != nil {
				return err
			}
			if boot != v.BootID || uptime < v.Uptime || uptime-v.Uptime >= 30 || r.now().Before(v.ObservedAt) || !r.now().Before(v.ValidUntil) {
				return fmt.Errorf("LED inventory observation expired before persistence")
			}
		}
		if err = r.capacity(tx, len(body)+4096); err != nil {
			return err
		}
		if !resume {
			if err = r.stage.WriteJSON(recoveryLEDHead, v, set.Head != nil); err != nil {
				return err
			}
		}
		if err = recoveryBoundary(r.fault, "led_inventory_intent"); err != nil {
			return err
		}
		var saved recoveryLEDInventory
		err = r.stage.ReadJSON(recoveryLEDInventoryFile(v.ID), &saved)
		if os.IsNotExist(err) {
			err = r.stage.WriteJSON(recoveryLEDInventoryFile(v.ID), v, false)
		} else if err == nil && protocol.Hash(saved) != protocol.Hash(v) {
			return fmt.Errorf("LED inventory archive collision")
		}
		if err != nil {
			return err
		}
		if err = recoveryBoundary(r.fault, "led_inventory_archive"); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO reconciliation_led_inventories(sequence,id,hash,body) VALUES(?,?,?,?)", v.Sequence, v.ID, protocol.Hash(v), string(body)); err != nil {
			return err
		}
		return recoveryBoundary(r.fault, "led_inventory_before_commit")
	})
	if err != nil {
		return err
	}
	return recoveryBoundary(r.fault, "led_inventory_committed")
}

// ReviewRecoveryLED captures or reads a sealed, diagnostic ownership inventory.
// Default reads reuse the existing observation; refresh explicitly requests a new
// one. An interrupted capture is sealed first, preserving its original age.
func ReviewRecoveryLED(ctx context.Context, path string, c Config, socket string, api LEDRecoveryObserver, refresh bool) (RecoveryLEDReview, error) {
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return RecoveryLEDReview{}, err
	}
	defer r.Close()
	return r.reviewLEDInventory(ctx, socket, api, refresh)
}

func (r *recoveryReconciler) reviewLEDInventory(ctx context.Context, socket string, api LEDRecoveryObserver, refresh bool) (out RecoveryLEDReview, err error) {
	if !validLEDSocket(socket) {
		return out, fmt.Errorf("LED recovery review requires an explicit absolute socket")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err = r.ensureLEDInventory(ctx); err != nil {
		return
	}
	if err = recoveryBoundary(r.fault, "led_inventory_schema"); err != nil {
		return
	}
	var set recoveryLEDInventorySet
	var sources recoveryLEDSources
	var v recoveryLEDInventory
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		var e error
		set, e = r.readLEDInventorySet(ctx, tx)
		if e != nil {
			return e
		}
		if set.Head != nil && set.Head.Socket != socket {
			return fmt.Errorf("LED recovery inventory is pinned to another socket")
		}
		if set.Head != nil && (!refresh || set.Pending) {
			return nil
		}
		if len(set.Records) >= maxRecoveryLEDInventories {
			return fmt.Errorf("LED recovery inventory history budget exhausted")
		}
		sources, e = r.collectLEDSources(ctx, tx)
		if e != nil {
			return e
		}
		boot, uptime, e := r.recoveryClock()
		if e != nil {
			return e
		}
		id, e := safefs.NewID("n")
		if e != nil {
			return e
		}
		now := r.now().UTC()
		v = recoveryLEDInventory{Version: 1, ID: id, Sequence: len(set.Records) + 1, Recovery: r.journal.binding(), ConfigHash: protocol.Hash(r.config), Socket: socket, ObservedAt: now, ValidUntil: now.Add(30 * time.Second), BootID: boot, Uptime: uptime, Sources: sources}
		if set.Head != nil {
			v.PreviousHash = protocol.Hash(set.Head)
		}
		return nil
	})
	if err != nil {
		return
	}
	if set.Pending {
		// This is a historical read/commit, not a renewed daemon observation. Source
		// drift and expiry remain visible when constructing the review below.
		err = r.sealLEDInventory(ctx, *set.Head, protocol.Hash(set.Head), true)
	} else if set.Head == nil || refresh {
		if api == nil {
			return out, fmt.Errorf("LED registry observer is required for a new capture")
		}
		v.Registry, v.Targets, v.Degraded, err = observeRecoveryLED(ctx, api)
		if err != nil {
			return
		}
		err = r.sealLEDInventory(ctx, v, protocol.Hash(set.Head), false)
	}
	if err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		latest, e := r.readLEDInventorySet(ctx, tx)
		if e != nil {
			return e
		}
		if latest.Pending || latest.Head == nil {
			return fmt.Errorf("LED inventory is not sealed")
		}
		current, e := r.collectLEDSources(ctx, tx)
		if e != nil {
			return e
		}
		var previous *recoveryLEDInventory
		if len(latest.Records) > 1 {
			previous = &latest.Records[len(latest.Records)-2]
		}
		out = r.ledInventoryView(*latest.Head, current, previous)
		adoption, e := r.readLEDAdoption(ctx, tx)
		if e != nil {
			return e
		}
		if adoption != nil {
			v := adoption.view()
			v.SourcesMatch = adoption.Record.SourcesHash == protocol.Hash(current)
			out.Adoption = &v
			epoch, phase, e := r.readLEDEpoch(ctx, tx, adoption)
			if e != nil {
				return e
			}
			if epoch != nil {
				v := epoch.view(phase, len(adoption.After.Faults))
				out.EpochHandoff = &v
			}
			imports, e := r.readLEDImports(ctx, tx, adoption, epoch)
			if e != nil {
				return e
			}
			for _, record := range imports {
				out.CleanupImports = append(out.CleanupImports, record.Intent.view(record.Committed))
			}
		}
		if e = r.appendLEDPreservationReview(ctx, tx, current, &out); e != nil {
			return e
		}
		if e = r.appendLEDResolutionReview(ctx, tx, *latest.Head, &out); e != nil {
			return e
		}
		clears, e := r.readLEDClears(ctx, tx)
		if e != nil {
			return e
		}
		out.CleanupAttempts = len(clears)
		if out.EpochHandoff != nil && out.EpochHandoff.State == "committed" {
			inherited := map[string]bool{}
			for _, f := range adoption.After.Faults {
				inherited[f.ID] = true
			}
			for _, v := range out.CleanupImports {
				delete(inherited, v.FaultID)
			}
			for _, record := range clears {
				if record.ResultCommitted && record.Result != nil && inherited[record.Intent.FaultID] && record.Intent.Socket == adoption.Record.Socket && (record.Result.Outcome == "acknowledged" || record.Result.Outcome == "observed_absent") {
					out.ImportableCleanupCount++
					if len(out.ImportableCleanup) < 32 {
						out.ImportableCleanup = append(out.ImportableCleanup, RecoveryLEDImportProposal{record.Intent.FaultID, protocol.Hash(record.Result), record.Result.Outcome})
					}
					delete(inherited, record.Intent.FaultID)
				}
			}
		}
		for i, attempt := range clears {
			v := attempt.Intent.result("uncertain", time.Time{})
			if attempt.Result != nil {
				v = *attempt.Result
			}
			if v.Outcome == "uncertain" {
				out.CleanupUnconfirmed++
			}
			if i >= len(clears)-64 {
				out.RecentCleanup = append(out.RecentCleanup, v)
			}
		}
		return nil
	})
	return
}
