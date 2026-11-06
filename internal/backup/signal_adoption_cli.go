package backup

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runBackupSignalAdoptionCLI(args []string, out io.Writer) error {
	return runBackupSignalAdoptionCLIWith(args, out, prepareBackupSignalAdoption)
}
func runBackupSignalAdoptionCLIWith(args []string, out io.Writer, prepare func(backupSignalAdoptionSpec) (*signalService, error)) error {
	f := flag.NewFlagSet("backup-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Adopt the installed backup Signal account locally")
	path := f.String("spec", "", "Private adoption or published gateway handoff specification")
	home := f.String("home", "", "Installed backup service user's home (review only)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 {
		return fmt.Errorf("legacy Signal adoption requires --local and no positional arguments")
	}
	review := args[0] == "signal-adoption-review"
	var spec backupSignalAdoptionSpec
	var handoff backupSignalHandoffSpec
	var runtime install.Runtime
	if review {
		if !backupRecoveryPath(*home) || *path != "" {
			return fmt.Errorf("Signal adoption review requires --home ABS and no specification")
		}
		c, err := readBackupRecoveryInstallation(filepath.Join(*home, ".config/deploy/backup-worker/runtime.json"))
		if err != nil {
			return err
		}
		// Review uses placeholders only for fields that do not select installed state.
		spec = backupSignalAdoptionSpec{Version: 1, Home: *home, Operation: "n-00000000000000000000000000000000", ConfigHash: notify.Hash(c), ServiceBinding: notify.Hash("review"), Account: "11111111-1111-4111-8111-111111111111", Inbox: filepath.Join(*home, ".local/share/deploy/signal-adoption-review-inbox")}
	} else {
		if !backupRecoveryPath(*path) || *home != "" {
			return fmt.Errorf("Signal adoption requires --spec ABS; home is pinned in the specification")
		}
		raw, err := readBackupSignalPrivate(*path)
		if err != nil {
			return err
		}
		if args[0] == "signal-adoption-handoff" {
			if err = safefs.DecodeJSON(raw, &handoff, safefs.MaxRecordBytes); err != nil {
				return err
			}
			runtime, err = handoff.prepare()
			if err != nil {
				return err
			}
			spec = handoff.Adoption.Spec
		} else {
			if err = safefs.DecodeJSON(raw, &spec, safefs.MaxRecordBytes); err != nil {
				return err
			}
		}
	}
	svc, err := prepare(spec)
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] legacy Signal adoption inputs validated; no account, inbox, manager or provider opened")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	installation, err := signalInstallationLease(spec.Home)
	if err != nil {
		return err
	}
	defer installation.Close()
	current, err := svc.Inspect(ctx)
	if err != nil {
		return err
	}
	if review {
		return json.NewEncoder(out).Encode(map[string]any{"version": 1, "config_hash": spec.ConfigHash, "service_binding": current.Binding, "account_state": spec.accountState(), "service_active": current.Active, "history_complete": false})
	}
	if current.Binding != spec.ServiceBinding {
		return fmt.Errorf("legacy Signal service changed since review")
	}
	state, err := openBackupState(filepath.Join(spec.Home, ".local/state/deploy/backup-worker/state"), false)
	if err != nil {
		return err
	}
	defer state.Close()
	var control ArchiveControl
	if err = state.get("control", &control); err != nil {
		return err
	}
	if control.WriterID != svc.installation.WorkerID || control.ArchiveID != svc.installation.ArchiveID || control.ConfigHash != svc.installation.ConfigHash {
		return fmt.Errorf("legacy Signal backup state differs from installed configuration")
	}
	notifier, err := backupNotifierLease(state)
	if err != nil {
		return err
	}
	defer notifier.Close()
	if err = requireLegacyNotificationOwner(state); err != nil {
		return err
	}
	if args[0] == "signal-adopt" {
		result, err := adoptBackupSignal(ctx, spec, svc)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(result)
	}
	installed, status, err := handoffBackupSignal(ctx, handoff, runtime, svc)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Installation relay.SignalRuntimeInstallation `json:"installation"`
		Account      signalaccount.RecoveryStatus    `json:"account"`
	}{installed, status})
}

func readBackupSignalPrivate(path string) ([]byte, error) {
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	f, err := d.Open(filepath.Base(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || info.Mode().Perm() != 0600 || info.Size() > safefs.MaxRecordBytes {
		return nil, fmt.Errorf("Signal adoption specification must be an owned private regular file")
	}
	return io.ReadAll(io.LimitReader(f, safefs.MaxRecordBytes+1))
}
