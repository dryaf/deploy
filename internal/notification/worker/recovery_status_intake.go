package worker

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"os"
	"slices"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func (r *recoveryReconciler) reviewRecoveryIntake(ctx context.Context, tx *sql.Tx, out *RecoveryStatus) error {
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM commands WHERE grant<>'' OR token_hash IS NOT NULL OR confirmed<>0 OR preparation<>'' OR state NOT IN ('outcome_unknown','succeeded','failed','rejected','expired')`).Scan(&enabled); err != nil {
		return err
	}
	if enabled != 0 {
		return fmt.Errorf("recovery contains command execution authority")
	}
	if r.config.Interaction == nil {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM command_inbox)+(SELECT count(*) FROM gateway_queries)+(SELECT count(*) FROM provider_inbox)+(SELECT count(*) FROM telegram_cursors)+(SELECT count(*) FROM signal_imports)").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("retained intake history requires interactive recovery")
		}
		out.add("intake", "request_seal", "complete", protocol.Hash("no-intake-history"))
		return nil
	}
	exists, err := requestRecoverySchema(tx)
	if err != nil {
		return err
	}
	sealed := false
	if exists {
		var raw string
		err = tx.QueryRowContext(ctx, "SELECT body FROM reconciliation_request_seal WHERE singleton=1").Scan(&raw)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			sealed = true
			out.add("intake", "request_seal", "complete", protocol.Hash(raw))
		}
	}
	if !sealed {
		out.add("intake", "request_seal", "pending", "")
	}
	accounts := map[string]string{}
	for _, d := range r.config.Interaction.Destinations {
		accounts[d.Provider] = d.Account
	}
	providers := make([]string, 0, len(accounts))
	for p := range accounts {
		providers = append(providers, p)
	}
	slices.Sort(providers)
	for _, provider := range providers {
		account := accounts[provider]
		scope := "intake/" + provider + "/" + account
		hash, err := r.reviewRecoveryCursor(tx, provider, account)
		if err != nil {
			return err
		}
		if hash == "" {
			out.add(scope, "cursor", "pending", "")
		} else {
			out.add(scope, "cursor", "complete", hash)
		}
		// Durable installation is not proof that other receivers are stopped
		// or that the provider cutoff is still fresh at activation time.
		out.add(scope, "activation_recheck", "pending", "")
	}
	return nil
}

func (r *recoveryReconciler) reviewRecoveryCursor(tx *sql.Tx, provider, account string) (string, error) {
	switch provider {
	case "signal":
		exists, err := signalCursorSchema(tx)
		if err != nil {
			return "", err
		}
		if !exists {
			return "", absentOriginalFile(r.stage, signalCursorFile(account))
		}
		installed, err := r.readInstalledSignalCursor(tx, account)
		if err != nil {
			return "", err
		}
		if installed == nil {
			var pending interaction.SignalRecoveryCursor
			e := r.stage.ReadJSON(signalCursorFile(account), &pending)
			if os.IsNotExist(e) {
				return "", nil
			}
			if e != nil {
				return "", e
			}
			if e = pending.Validate(); e != nil {
				return "", e
			}
			handoff, e := r.signalCursorHandoff(tx, account)
			if e != nil {
				return "", e
			}
			if protocol.Hash(pending.Handoff) != protocol.Hash(handoff) {
				return "", fmt.Errorf("pending Signal cursor intent changed")
			}
			return "", nil
		}
		handoff, err := r.signalCursorHandoff(tx, account)
		if err != nil {
			return "", err
		}
		if protocol.Hash(handoff) != protocol.Hash(installed.Handoff) {
			return "", fmt.Errorf("Signal cursor handoff changed")
		}
		proof, err := readSignalRecoveryCursor(tx, account)
		if err != nil {
			return "", err
		}
		if proof == nil || protocol.Hash(*proof) != protocol.Hash(*installed) {
			return "", fmt.Errorf("Signal cursor evidence changed")
		}
		return protocol.Hash(*proof), nil
	case "telegram":
		exists, err := telegramCursorSchema(tx)
		if err != nil {
			return "", err
		}
		if !exists {
			return "", absentOriginalFile(r.stage, telegramCursorFile(account))
		}
		var raw string
		err = tx.QueryRow("SELECT body FROM reconciliation_telegram_cursors WHERE account=?", account).Scan(&raw)
		if err == sql.ErrNoRows {
			var pending TelegramRecoveryCursor
			e := r.stage.ReadJSON(telegramCursorFile(account), &pending)
			if os.IsNotExist(e) {
				return "", nil
			}
			if e != nil {
				return "", e
			}
			drain, e := readTelegramDrain(tx, account)
			if e != nil {
				return "", e
			}
			prepared, e := r.prepareTelegramCursor(tx, drain)
			if e != nil {
				return "", e
			}
			if protocol.Hash(prepared) != protocol.Hash(pending) {
				return "", fmt.Errorf("pending Telegram cursor intent changed")
			}
			return "", nil
		}
		if err != nil {
			return "", err
		}
		proof, err := readTelegramRecoveryCursor(tx, account)
		if err != nil {
			return "", err
		}
		if proof == nil {
			return "", fmt.Errorf("Telegram cursor proof missing")
		}
		var file TelegramRecoveryCursor
		if err = r.stage.ReadJSON(telegramCursorFile(account), &file); err != nil {
			return "", err
		}
		drain, err := readTelegramDrain(tx, account)
		if err != nil {
			return "", err
		}
		prepared, err := r.prepareTelegramCursor(tx, drain)
		if err != nil {
			return "", err
		}
		if protocol.Hash(*proof) != protocol.Hash(file) || protocol.Hash(*proof) != protocol.Hash(prepared) {
			return "", fmt.Errorf("Telegram cursor handoff changed")
		}
		var offset int64
		if err = tx.QueryRow("SELECT offset FROM telegram_cursors WHERE account=?", account).Scan(&offset); err != nil {
			return "", err
		}
		if offset != proof.Offset {
			return "", fmt.Errorf("Telegram cursor changed during quarantine")
		}
		return protocol.Hash(*proof), nil
	default:
		return "", fmt.Errorf("unsupported recovery provider")
	}
}

func (r *recoveryReconciler) reviewRecoveryLED(ctx context.Context, tx *sql.Tx, out *RecoveryStatus) error {
	e, err := r.readLEDAdoption(ctx, tx)
	if err != nil {
		return err
	}
	if e == nil || e.Phase != "committed" {
		out.add("led", "ownership", "pending", "")
		out.add("led", "epoch", "pending", "")
		return nil
	}
	out.add("led", "ownership", "complete", protocol.Hash(e.Record))
	epoch, phase, err := r.readLEDEpoch(ctx, tx, e)
	if err != nil {
		return err
	}
	if phase != "committed" {
		out.add("led", "epoch", "pending", "")
		return nil
	}
	out.add("led", "epoch", "complete", protocol.Hash(epoch))
	imports, err := r.readLEDImports(ctx, tx, e, epoch)
	if err != nil {
		return err
	}
	state := "complete"
	for _, v := range imports {
		if !v.Committed {
			state = "pending"
		}
	}
	out.add("led", "cleanup_imports", state, protocol.Hash(imports))
	inherited := remainingInheritedFaults(e, imports)
	if len(inherited) > 0 {
		out.add("led", "inherited_faults", "retained_fenced", protocol.Hash(inherited))
	}
	// Pending RPCs and ambiguous outcomes stay visible; they are never retried by
	// this audit. Inherited faults do not become healthy because other stages pass.
	clears, err := r.readLEDClears(ctx, tx)
	if err != nil {
		return err
	}
	state = "complete"
	for _, c := range clears {
		if !c.Committed || !c.ResultCommitted {
			state = "pending"
		}
	}
	out.add("led", "cleanup_attempts", state, protocol.Hash(clears))
	return nil
}
