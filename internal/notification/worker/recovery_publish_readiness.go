package worker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Publication installs a fenced database. Fresh service/provider handoffs are
// activation requirements and must not be mistaken for durable preparation.
func (r *recoveryReconciler) publicationReady(ctx context.Context, review RecoveryStatus) error {
	absent := map[string]bool{}
	for _, c := range review.Checks {
		if strings.HasPrefix(c.Scope, "producer/") && c.Stage == "ownership" && c.State == "retained_fenced" {
			absent[c.Scope] = true
		}
	}
	noLED := false
	for _, c := range review.Checks {
		if c.State != "pending" {
			continue
		}
		if strings.HasPrefix(c.Scope, "intake/") && c.Stage == "activation_recheck" {
			continue
		}
		if absent[c.Scope] && (c.Stage == "delivery_reconciliation" || c.Stage == "commands") {
			continue
		}
		if c.Scope == "led" && (c.Stage == "ownership" || c.Stage == "epoch") {
			if !noLED {
				var err error
				noLED, err = r.noPublicationLEDHistory(ctx)
				if err != nil {
					return err
				}
			}
			if noLED {
				continue
			}
		}
		return fmt.Errorf("publication requires completed recovery stage %s/%s", c.Scope, c.Stage)
	}
	return nil
}

// An installation without configured bindings may skip LED adoption only when
// none of its original, restored or reconciled evidence contains LED history.
func (r *recoveryReconciler) noPublicationLEDHistory(ctx context.Context) (bool, error) {
	if len(r.config.LEDBindings) != 0 {
		return false, nil
	}
	files, err := r.recoveryEvidenceInventory(ctx)
	if err != nil {
		return false, err
	}
	for _, f := range files {
		if strings.Contains(f.Path, "led-") || strings.Contains(f.Path, "led_") {
			return false, nil
		}
	}
	for _, name := range []string{"source.db", originalArchiveFile, "reconciled.db"} {
		var db *sql.DB
		if name == "reconciled.db" {
			db = r.db
		} else if db, err = immutableOriginalDB(r.stage, name); err != nil {
			return false, err
		}
		empty, e := emptyPublicationLEDHistory(ctx, db)
		if name != "reconciled.db" {
			closeErr := db.Close()
			if e == nil {
				e = closeErr
			}
		}
		if e != nil || !empty {
			return false, e
		}
	}
	return true, nil
}

func emptyPublicationLEDHistory(ctx context.Context, db *sql.DB) (bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND instr(lower(name),'led')>0")
	if err != nil {
		return false, err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			break
		}
		tables = append(tables, name)
		if len(tables) > 1024 {
			err = fmt.Errorf("LED history exceeds schema budget")
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, table := range tables {
		var count int
		query := `SELECT EXISTS(SELECT 1 FROM "` + strings.ReplaceAll(table, `"`, `""`) + `")`
		if err = db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return false, err
		}
		if count != 0 {
			return false, nil
		}
	}
	return true, nil
}
