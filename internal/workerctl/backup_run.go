package workerctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
)

func requestBackupRunControl(ctx context.Context, r ControlRequest, run func(context.Context, string, string, string) (backup.RunRequestResult, error)) (ControlResult, error) {
	result := ControlResult{Version: 1}
	if r.Desired.Backup == nil || !config.WorkerEnabled(r.Desired.Backup.Enabled) || len(r.Inputs) != 0 {
		return result, fmt.Errorf("manual backup requires an enabled YAML declaration without credential transfer")
	}
	if _, _, err := CompileBackup(*r.Desired.Backup, r.Facts); err != nil {
		return result, err
	}
	observed, err := run(ctx, r.Facts.Home, r.DesiredHash, r.HelperSHA256)
	result.State = observed.State
	if errors.Is(err, backup.ErrSourceRetirementPending) || errors.Is(err, backup.ErrSourceReplacementPending) {
		return controlFailure(err), err
	}
	if errors.Is(err, backup.ErrSourceEndpointMismatch) {
		return ControlResult{Version: 1, State: "needs_action", Code: "source_migration_required", Next: err.Error()}, err
	}
	if err != nil {
		result.Code = "run_unconfirmed"
		result.Next = "inspect backup status and logs before requesting another run; an interrupted reply is not proof that capture did not start"
		return result, err
	}
	if observed.State != "start_requested" && observed.State != "running_or_busy" {
		return result, fmt.Errorf("invalid manual backup acknowledgement")
	}
	result.Code = "ok"
	result.Next = "deploy backup-worker status " + r.Server + " and inspect logs for completion; a queued start is not a successful backup or schedule activation"
	return result, nil
}

func (c CLI) requestBackupRun(ctx context.Context, v Invocation, inv config.ServerInventory, facts TargetFacts, p Plan) error {
	if p.Artifacts == nil {
		return fmt.Errorf("matching helper bundle required to verify manual backup ownership")
	}
	if c.Controller == nil {
		c.Controller = SSHController{}
	}
	r := ControlRequest{Version: 1, Worker: "backup-worker", Server: v.Server, Action: "run", Facts: facts, Desired: backupDesired(*inv.Servers[v.Server].Desired), DesiredHash: p.DesiredHash}
	targets, err := declaredSourceTargets(inv, r.Desired)
	if err != nil {
		return err
	}
	r.SourceTargets = targets
	result, err := c.Controller.Apply(ctx, inv.Servers[v.Server], p.Artifacts.Helper, r)
	if result.Version != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("manual backup outcome unconfirmed; inspect status before another request")
	}
	if v.Options.JSON {
		if e := json.NewEncoder(c.Out).Encode(result); e != nil {
			return e
		}
	} else if e := printControlResult(c.Out, v.Server, "backup-worker", result); e != nil {
		return e
	}
	if err == nil && (result.Code != "ok" || result.State != "start_requested" && result.State != "running_or_busy") {
		return fmt.Errorf("manual backup outcome unconfirmed; inspect status before another request")
	}
	return err
}
