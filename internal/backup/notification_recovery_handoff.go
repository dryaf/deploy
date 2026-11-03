package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type backupRecoveryHandoffResult struct {
	Version          int                                      `json:"version"`
	Operation        string                                   `json:"operation"`
	SummaryHash      string                                   `json:"summary_hash"`
	Delivery         backupRecoveryCommitRecord               `json:"delivery"`
	Commands         *interaction.CommandRecoveryInstallation `json:"commands,omitempty"`
	DeliveryEnabled  bool                                     `json:"delivery_enabled"`
	ExecutionEnabled bool                                     `json:"execution_enabled"`
}

type pinnedBackupCommandRecovery struct {
	interaction.CommandRecoverySession
	before, after notify.Identity
	producer      notify.Producer
	operation     string
	check         func() error
}

func (s pinnedBackupCommandRecovery) validate(ch notify.RecoveryChallenge) error {
	if ch.Operation != s.operation || ch.Identity != s.after || ch.Producer != s.producer {
		return fmt.Errorf("command exchange belongs to another recovery")
	}
	return s.check()
}

func (s pinnedBackupCommandRecovery) CommandChallenge(ctx context.Context, p notify.Producer) (ch notify.RecoveryChallenge, err error) {
	if err = s.check(); err != nil {
		return
	}
	ch, err = s.CommandRecoverySession.CommandChallenge(ctx, p)
	if err == nil {
		err = s.validate(ch)
	}
	return
}

func (s pinnedBackupCommandRecovery) CommandSummary(ctx context.Context, p notify.Producer) (summary interaction.CommandRecoverySummary, err error) {
	if err = s.check(); err != nil {
		return
	}
	summary, err = s.CommandRecoverySession.CommandSummary(ctx, p)
	if err == nil {
		err = s.validate(summary.Challenge)
		if err == nil && (summary.Manifest.Identity != s.before || summary.Manifest.Producer != s.producer || summary.ExecutionEnabled) {
			err = fmt.Errorf("command recovery summary binding changed")
		}
	}
	return
}

// Both source streams stay fenced throughout. Individual stages retain their
// own durable authority; a lost coordinator response never invents a new epoch,
// command, receipt acknowledgement or delivery generation.
func runBackupRecoveryHandoff(ctx context.Context, o backupReconciliationOptions, c WorkerInstallation, config worker.Config, out io.Writer, fault func(string) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	state, err := openBackupState(o.State, false)
	if err != nil {
		return err
	}
	defer state.Close()
	var original backupNotificationRecovery
	var summary notify.RecoverySummary
	err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
		var e error
		original, e = readBackupRecoveryAuthority(tx, c)
		if e != nil {
			return e
		}
		if e = backupEvaluationMeta(tx, "notification_recovery_summary", &summary); e != nil {
			return e
		}
		if e = validateBackupRecoverySummary(tx, original, summary); e != nil {
			return e
		}
		if summary.Challenge.Operation != o.Operation || notify.Hash(summary) != o.SummaryHash {
			return fmt.Errorf("reviewed backup reconciliation changed")
		}
		return nil
	})
	if err != nil {
		return err
	}
	p := original.Binding.Producer
	interactive, enrolled := false, false
	for _, e := range config.Producers {
		if e.Producer == p {
			enrolled = true
		}
	}
	if !enrolled {
		return fmt.Errorf("backup recovery producer is not enrolled")
	}
	if config.Interaction != nil {
		for _, w := range config.Interaction.Workers {
			if w.Producer == p {
				interactive = true
			}
		}
	}
	// Drain the command runner. The shared schema lease already held by state
	// excludes concurrent commands-init, including for a noninteractive producer.
	dir, err := safefs.Open(o.State)
	if err != nil {
		return err
	}
	defer dir.Close()
	var lease *safefs.Lock
	if interactive {
		lease, err = dir.Lock("commands.lock", true, false)
		if err != nil {
			return err
		}
		defer lease.Close()
	}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		locks := map[string]*safefs.Lock{"schema.lock": state.schemaLease}
		if lease != nil {
			locks["commands.lock"] = lease
		}
		for name, heldLease := range locks {
			if heldLease == nil {
				return fmt.Errorf("backup recovery source lease missing")
			}
			held, err := heldLease.File().Stat()
			if err != nil {
				return err
			}
			f, err := dir.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			current, err := f.Stat()
			f.Close()
			if err != nil {
				return err
			}
			if !os.SameFile(held, current) {
				return fmt.Errorf("backup recovery lease changed: %s", name)
			}
		}
		return nil
	}
	boundary := func(at string) error {
		if err := check(); err != nil {
			return err
		}
		if err := backupRecoveryBoundary(fault, at); err != nil {
			return err
		}
		return check()
	}
	var ledger *interaction.Ledger
	if interactive {
		// Commands must have been initialized against this exact backup archive.
		var archive string
		var epoch uint64
		if err = state.db.QueryRowContext(ctx, "SELECT archive_id,epoch FROM backup_command_binding WHERE singleton=1").Scan(&archive, &epoch); err != nil {
			return err
		}
		if archive != original.Binding.Archive || epoch != original.Binding.Epoch {
			return fmt.Errorf("backup command archive binding changed")
		}
		ledger, err = interaction.OpenRecoveryLedger(ctx, state.db, p, original.Binding.Identity, summary.Challenge.Identity, o.Operation)
		if err != nil {
			return err
		}
	} else {
		var count int
		if err = state.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name LIKE 'interactive_%' OR name='backup_command_binding'").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("existing backup commands require command recovery enrollment")
		}
	}
	session, err := worker.OpenRecoveryHandoffSession(ctx, o.NotificationState, config)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	remote, err := session.Summary(ctx, p)
	if err != nil {
		return err
	}
	if notify.Hash(remote) != o.SummaryHash {
		return fmt.Errorf("notification reconciliation no longer matches the reviewed summary")
	}
	if err = boundary("handoff_preflight"); err != nil {
		return err
	}
	if !interactive {
		if err = session.RequireCommandRecoveryAbsent(ctx, p); err != nil {
			return err
		}
	}
	if interactive {
		pinned := pinnedBackupCommandRecovery{CommandRecoverySession: session, before: original.Binding.Identity, after: summary.Challenge.Identity, producer: p, operation: o.Operation, check: check}
		commandSummary, e := interaction.ReconcileCommands(ctx, ledger, pinned)
		if e != nil {
			return e
		}
		if commandSummary.Challenge.Operation != o.Operation || commandSummary.Challenge.Identity != summary.Challenge.Identity || commandSummary.Manifest.Identity != original.Binding.Identity || commandSummary.Manifest.Producer != p {
			return fmt.Errorf("command recovery differs from notification recovery")
		}
		if err = boundary("handoff_commands_reconciled"); err != nil {
			return err
		}
	}
	if _, err = prepareBackupRecoveryOwnershipWith(ctx, state, c, session, boundary); err != nil {
		return err
	}
	if err = boundary("handoff_ownership"); err != nil {
		return err
	}
	delivery, err := commitBackupNotificationRecoveryWith(ctx, state, c, session, boundary)
	if err != nil {
		return err
	}
	if err = boundary("handoff_delivery_commit"); err != nil {
		return err
	}
	result := backupRecoveryHandoffResult{Version: 1, Operation: o.Operation, SummaryHash: o.SummaryHash, Delivery: delivery}
	if interactive {
		if _, err = interaction.CommitCommandRecovery(ctx, ledger, session); err != nil {
			return err
		}
		if err = boundary("handoff_command_commit"); err != nil {
			return err
		}
		current, e := interaction.OpenRecoveryLedger(ctx, state.db, p, original.Binding.Identity, summary.Challenge.Identity, o.Operation)
		if e != nil {
			return e
		}
		installed, e := interaction.InstallCommandRecovery(ctx, current, session)
		if e != nil {
			return e
		}
		result.Commands = &installed
		if err = boundary("handoff_outcomes_installed"); err != nil {
			return err
		}
	}
	if err = boundary("handoff_result"); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
