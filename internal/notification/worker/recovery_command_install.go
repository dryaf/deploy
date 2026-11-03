package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

type commandInstallationIntent struct {
	Version                 int
	Installation            interaction.CommandRecoveryInstallation
	BeforeHash, HistoryHash string
}

func OpenCommandRecoveryInstallSession(ctx context.Context, path string, c Config) (interaction.CommandRecoveryInstallSession, error) {
	s, err := OpenCommandRecoveryCommitSession(ctx, path, c)
	if err != nil {
		return nil, err
	}
	r := s.(*recoveryReconciler)
	if err = r.initializeCommandInstallations(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
func commandInstallationFile(p protocol.Producer) string {
	return "command-install-" + protocol.Hash(p) + ".json"
}
func commandInstallationSchema(tx *sql.Tx) (bool, error) {
	var version string
	err := tx.QueryRow("SELECT value FROM metadata WHERE key='command_installation_schema'").Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	var tables int
	if e := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='reconciliation_command_installations'").Scan(&tables); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && tables == 0 {
		return false, nil
	}
	if err != nil || version != "1" || tables != 1 {
		return false, fmt.Errorf("command installation schema missing or changed")
	}
	return true, nil
}
func (r *recoveryReconciler) initializeCommandInstallations(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		exists, err := commandInstallationSchema(tx)
		if err != nil || exists {
			return err
		}
		_, err = tx.Exec(`CREATE TABLE reconciliation_command_installations(producer TEXT PRIMARY KEY,hash TEXT NOT NULL,body TEXT NOT NULL);
INSERT INTO metadata VALUES('command_installation_schema','1');`)
		return err
	})
}

func (r *recoveryReconciler) InstallCommandRecoveryOutcomes(ctx context.Context, checkpoint interaction.CommandRecoveryCommit) (result interaction.CommandRecoveryInstallation, err error) {
	if err = checkpoint.Validate(); err != nil {
		return
	}
	p := checkpoint.Summary.Manifest.Producer
	if err = r.commandRecoveryEnrollment(p); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		if ok, err := commandInstallationSchema(tx); err != nil {
			return err
		} else if !ok {
			return protocol.ErrRecoveryIncomplete
		}
		var current interaction.CommandRecoveryCommit
		if err := r.prepareCommandRecoveryCommit(tx, p, &current); err != nil {
			return err
		}
		if protocol.Hash(current) != protocol.Hash(checkpoint) {
			return fmt.Errorf("command installation checkpoint changed")
		}
		plan, projection, err := commandInstallationPlan(tx, checkpoint)
		if err != nil {
			return err
		}
		result = plan.Installation
		var intent commandInstallationIntent
		fileErr := r.stage.ReadJSON(commandInstallationFile(p), &intent)
		var raw, hash string
		dbErr := tx.QueryRow("SELECT body,hash FROM reconciliation_command_installations WHERE producer=?", p.ID).Scan(&raw, &hash)
		if dbErr != nil && dbErr != sql.ErrNoRows {
			return dbErr
		}
		if dbErr == sql.ErrNoRows {
			body, err := json.Marshal(plan)
			if err != nil {
				return err
			}
			// Reserve space for the journal and every changed head, including retries
			// whose file intent survived but whose SQL transaction did not.
			if err = r.capacity(tx, len(body)+16384+int(result.Commands)*512); err != nil {
				return err
			}
		}
		if os.IsNotExist(fileErr) {
			if dbErr == nil {
				return fmt.Errorf("command installation lost its file intent")
			}
			if projection != plan.BeforeHash {
				return fmt.Errorf("command projection changed before installation")
			}
			if err = r.stage.WriteJSON(commandInstallationFile(p), plan, false); err != nil {
				return err
			}
			intent = plan
		} else if fileErr != nil {
			return fileErr
		}
		if protocol.Hash(intent) != protocol.Hash(plan) {
			return fmt.Errorf("command installation intent or historical results changed")
		}
		if dbErr == nil {
			var installed interaction.CommandRecoveryInstallation
			if err = protocol.Decode([]byte(raw), &installed, 32768); err != nil {
				return err
			}
			if err = installed.Validate(); err != nil {
				return err
			}
			if protocol.Hash(installed) != hash || hash != protocol.Hash(result) || projection != result.OutcomesHash {
				return fmt.Errorf("command installation projection regressed")
			}
			return nil
		}
		if projection != intent.BeforeHash {
			return fmt.Errorf("uncommitted command installation projection changed")
		}
		if err = recoveryBoundary(r.fault, "command_install_intent"); err != nil {
			return err
		}
		if err = r.checkAuthority(); err != nil {
			return err
		}
		if err = applyCommandInstallation(tx, p); err != nil {
			return err
		}
		afterPlan, after, err := commandInstallationPlan(tx, checkpoint)
		if err != nil {
			return err
		}
		if after != result.OutcomesHash || protocol.Hash(afterPlan) != protocol.Hash(plan) {
			return fmt.Errorf("command installation did not produce expected outcomes")
		}
		body, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO reconciliation_command_installations VALUES(?,?,?)", p.ID, protocol.Hash(result), string(body)); err != nil {
			return err
		}
		return recoveryBoundary(r.fault, "command_install_transaction")
	})
	if err == nil {
		err = recoveryBoundary(r.fault, "command_install_commit")
	}
	return
}
