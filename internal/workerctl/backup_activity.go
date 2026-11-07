package workerctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
)

func activityBackupControl(ctx context.Context, r ControlRequest, reconcile func(context.Context, string, config.DesiredWorkers, string) (backup.ActivityResult, error)) (ControlResult, error) {
	result := ControlResult{Version: 1, State: "setup_required", Code: "ok"}
	if r.Desired.Backup == nil || len(r.Inputs) != 0 {
		return result, fmt.Errorf("backup activity requires YAML policy without credential transfer")
	}
	if _, err := resolveBackupPaths(r.Desired.Backup.Paths, r.Facts); err != nil {
		return result, err
	}
	activity, err := reconcile(ctx, r.Facts.Home, r.Desired, r.HelperSHA256)
	if err != nil {
		var pending backup.ActivityPendingError
		if errors.As(err, &pending) {
			return result, backupActivityPendingError{Server: r.Server, Cause: err}
		}
		return result, err
	}
	if !activity.Handled {
		return result, nil
	}
	result.State = activity.State
	switch activity.State {
	case "not_installed":
		result.Next = "backup is disabled in YAML; no installation created"
	case "disabled":
		result.Next = "backup services disabled; archive, keys, identities and history retained; set enabled: true and apply to stage reactivation"
	case "staged-awaiting-qualification":
		result.State = "qualification_required"
		result.Next = "backup re-enabled in staged mode; timers remain disabled; verify qualification and run a successful manual backup, then deploy backup-worker activate " + r.Server
	case "active":
		result.State = "active_recorded"
		result.Next = "activity is unchanged and activation is recorded; status reports current backup evidence"
	default:
		return result, fmt.Errorf("invalid backup activity response")
	}
	return result, nil
}

type backupActivityPendingError struct {
	Server string
	Cause  error
}

func (e backupActivityPendingError) Error() string { return e.Cause.Error() }
func (e backupActivityPendingError) Unwrap() error { return e.Cause }

// Ordinary setup owns source enrollment and artifact updates. Activity has a
// separate target transaction so disabling never needs source access, credentials
// or a container image. Existing targets are inspected before choosing setup.
func (c CLI) applyBackupActivity(ctx context.Context, v Invocation, server config.ServerConfig, f TargetFacts, p Plan) (bool, error) {
	disabled := !config.WorkerEnabled(server.Desired.Backup.Enabled)
	needed := disabled
	if !needed {
		if inspector, ok := c.Inspector.(StatusInspector); ok {
			status, err := inspector.Status(ctx, server, "backup-worker", *p.Paths)
			if err != nil {
				return false, err
			}
			observed := ApplyStatus(p, status)
			if observed.State == "recovery_required" {
				return false, fmt.Errorf("%s: %s", observed.Code, observed.Next)
			}
			needed = observed.Code != "not_installed"
		}
	}
	if !needed {
		return false, nil
	}
	if p.Artifacts == nil || !f.UserManager || !f.Tools["systemctl"] {
		return true, fmt.Errorf("missing_input: matching helper and target prerequisites required for backup activity")
	}
	if c.Controller == nil {
		c.Controller = SSHController{}
	}
	r := ControlRequest{Version: 1, Worker: "backup-worker", Server: v.Server, Action: v.Operation, Facts: f, Desired: backupDesired(*server.Desired), DesiredHash: p.DesiredHash}
	result, err := c.Controller.Apply(ctx, server, p.Artifacts.Helper, r)
	if err == nil && result.Version == 1 && result.Code == "ok" && result.State == "setup_required" && !disabled {
		return false, nil
	}
	if result.Version != 1 {
		if err != nil {
			return true, err
		}
		return true, fmt.Errorf("invalid backup activity response")
	}
	if result.State == "setup_required" {
		return true, fmt.Errorf("disabled backup did not confirm activity")
	}
	if v.Options.JSON {
		if e := json.NewEncoder(c.Out).Encode(result); e != nil {
			return true, e
		}
	} else {
		if e := printControlResult(c.Out, v.Server, "backup-worker", result); e != nil {
			return true, e
		}
	}
	if err == nil && result.Code != "ok" {
		err = fmt.Errorf("%s: %s", result.Code, result.Next)
	}
	return true, err
}
