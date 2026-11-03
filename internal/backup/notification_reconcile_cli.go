package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	notifyinstall "github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

type backupReconciliationOptions struct {
	Config, State, NotificationConfig, NotificationState, Operation string
	SummaryHash                                                     string
}

type backupReconciliationOutput struct {
	notify.RecoverySummary
	SummaryHash string `json:"summary_hash"`
}

func runBackupReconciliationCLI(args []string, out io.Writer) error {
	return runBackupRecoveryCoordinatorCLI("recovery-notifications", args, out)
}

func runBackupRecoveryCoordinatorCLI(op string, args []string, out io.Writer) error {
	f := flag.NewFlagSet("backup-worker "+op, flag.ContinueOnError)
	local := f.Bool("local", false, "Reconcile existing local backup state with an offline quarantined notification worker")
	var o backupReconciliationOptions
	f.StringVar(&o.Config, "config", "", "Private installed backup worker JSON")
	f.StringVar(&o.State, "state-dir", "", "Existing backup state directory")
	f.StringVar(&o.NotificationConfig, "notification-config", "", "Private notification runtime JSON")
	f.StringVar(&o.NotificationState, "notification-state", "", "Existing quarantined notification state directory")
	f.StringVar(&o.Operation, "operation", "", "Exact notification recovery operation ID")
	f.StringVar(&o.SummaryHash, "summary-hash", "", "Exact reviewed notification reconciliation summary digest (handoff only)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !safefs.ValidID(o.Operation, "n") {
		return fmt.Errorf("notification reconciliation requires --local, exact --operation and no positional arguments")
	}
	switch op {
	case "recovery-handoff":
		if !safefs.DigestPattern.MatchString(o.SummaryHash) {
			return fmt.Errorf("handoff requires the reviewed --summary-hash")
		}
	case "recovery-notifications":
		var invalid bool
		f.Visit(func(v *flag.Flag) {
			if v.Name == "summary-hash" {
				invalid = true
			}
		})
		if invalid {
			return fmt.Errorf("notification reconciliation does not accept handoff options")
		}
	default:
		return fmt.Errorf("unknown backup recovery coordinator")
	}
	for _, path := range []string{o.Config, o.State, o.NotificationConfig, o.NotificationState} {
		if !backupRecoveryPath(path) {
			return fmt.Errorf("notification reconciliation requires clean absolute configuration/state paths")
		}
	}
	if o.State == o.NotificationState || strings.HasPrefix(o.State, o.NotificationState+"/") || strings.HasPrefix(o.NotificationState, o.State+"/") {
		return fmt.Errorf("backup and notification state directories must be separate")
	}
	c, err := readBackupRecoveryInstallation(o.Config)
	if err != nil {
		return err
	}
	runtime, err := notifyinstall.ReadRuntime(o.NotificationConfig)
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintf(out, "[DRY] backup %s arguments/configurations validated; no state opened or services changed\n", op)
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if op == "recovery-handoff" {
		return runBackupRecoveryHandoff(ctx, o, c, runtime.Worker, out, nil)
	}
	return runBackupReconciliation(ctx, o, c, runtime.Worker, out, nil)
}

// Pin every response before the producer can fence ownership or persist
// evidence. The session holds the exclusive worker lease throughout the exchange.
type pinnedBackupRecoverySession struct {
	notify.RecoverySession
	operation string
}

func (s pinnedBackupRecoverySession) Challenge(ctx context.Context, p notify.Producer) (ch notify.RecoveryChallenge, err error) {
	ch, err = s.RecoverySession.Challenge(ctx, p)
	if err == nil && (ch.Operation != s.operation || ch.Producer != p) {
		err = fmt.Errorf("notification recovery operation or producer changed")
	}
	return
}

func (s pinnedBackupRecoverySession) Summary(ctx context.Context, p notify.Producer) (v notify.RecoverySummary, err error) {
	v, err = s.RecoverySession.Summary(ctx, p)
	if err == nil && (v.Challenge.Operation != s.operation || v.Challenge.Producer != p) {
		err = fmt.Errorf("notification recovery operation or producer changed")
	}
	return
}

func runBackupReconciliation(ctx context.Context, o backupReconciliationOptions, c WorkerInstallation, config worker.Config, out io.Writer, fault func(string) error) (err error) {
	s, err := openBackupState(o.State, false)
	if err != nil {
		return err
	}
	defer s.Close()
	var producer notify.Producer
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		owner, e := readBackupNotificationOwner(tx)
		if e != nil {
			return e
		}
		producer = owner.Binding.Producer
		return requireBackupRecoveryConfig(tx, owner.Binding, c)
	})
	if err != nil {
		return err
	}
	enrolled := false
	for _, p := range config.Producers {
		if p.Producer == producer {
			enrolled = true
		}
	}
	if !enrolled {
		return fmt.Errorf("backup producer is not enrolled in the notification configuration")
	}
	session, err := worker.OpenRecoverySession(ctx, o.NotificationState, config)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	summary, err := reconcileBackupNotificationsWith(ctx, s, c, pinnedBackupRecoverySession{session, o.Operation}, reportHostClock, fault)
	if err != nil {
		return err
	}
	if err = backupRecoveryBoundary(fault, "recovery_cli_result"); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(backupReconciliationOutput{RecoverySummary: summary, SummaryHash: notify.Hash(summary)})
}
