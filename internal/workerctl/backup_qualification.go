package workerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
)

func operateBackupControl(ctx context.Context, r ControlRequest, qualify func(context.Context, string, string, string, string, install.QualificationObservation) (install.QualificationSession, error), activate func(context.Context, string, string, string) (backup.ActivationResult, error)) (ControlResult, error) {
	result := ControlResult{Version: 1}
	if r.Desired.Backup == nil {
		return result, fmt.Errorf("backup declaration is missing")
	}
	if _, _, err := CompileBackup(*r.Desired.Backup, r.Facts); err != nil {
		return result, err
	}
	if r.Action == "activate" {
		if !r.Facts.Linger {
			return result, fmt.Errorf("lingering is required before schedule activation")
		}
		observed, err := activate(ctx, r.Facts.Home, r.DesiredHash, r.HelperSHA256)
		if err != nil {
			return result, err
		}
		if observed.State != "active" {
			return result, fmt.Errorf("invalid activation acknowledgement")
		}
		result.Code, result.State = "ok", "active"
		if observed.Unchanged {
			result.State = "unchanged"
		}
		result.Next = "schedule units are enabled and active; status reports retained backup observations"
		return result, nil
	}
	var observation install.QualificationObservation
	if r.Observation != nil {
		observation = *r.Observation
	}
	session, err := qualify(ctx, r.Facts.Home, r.DesiredHash, r.HelperSHA256, strings.TrimPrefix(r.Action, "qualify-"), observation)
	if err != nil {
		return result, err
	}
	result.Code, result.State = "ok", "qualification_draft"
	result.QualificationSession = &session
	result.Next = "continue recording actual observations; incomplete checks remain pending"
	if session.Recorded {
		result.State = "qualification_recorded"
		result.Next = "complete remaining observations with deploy backup-worker qualify " + r.Server
		if session.EvidenceComplete {
			result.Next = "after a successful manual backup (deploy backup-worker run " + r.Server + "), enable the schedule with deploy backup-worker activate " + r.Server
		}
	}
	return result, nil
}

func (c CLI) activateBackup(ctx context.Context, v Invocation, inv config.ServerInventory, facts TargetFacts, p Plan) error {
	if p.Artifacts == nil {
		return fmt.Errorf("matching helper bundle required for activation")
	}
	if c.Controller == nil {
		c.Controller = SSHController{}
	}
	r := ControlRequest{Version: 1, Worker: "backup-worker", Server: v.Server, Action: "activate", Facts: facts, Desired: *inv.Servers[v.Server].Desired, DesiredScope: p.DesiredScope, DesiredHash: p.DesiredHash}
	if r.Desired.Backup.Notifications.Worker == "" {
		r.Desired.Notification = nil
	}
	targets, err := declaredSourceTargets(inv, r.Desired)
	if err != nil {
		return err
	}
	r.SourceTargets = targets
	result, err := c.Controller.Apply(ctx, inv.Servers[v.Server], p.Artifacts.Helper, r)
	if err != nil {
		return err
	}
	if result.Version != 1 || result.Code != "ok" || (result.State != "active" && result.State != "unchanged") {
		return fmt.Errorf("activation outcome unconfirmed; repeat activate to reconcile retained state")
	}
	if v.Options.JSON {
		return json.NewEncoder(c.Out).Encode(result)
	}
	return printControlResult(c.Out, v.Server, "backup-worker", result)
}
