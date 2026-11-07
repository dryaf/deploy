package workerctl

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/config"
)

func omittedDeclarationPlan(p Plan, d config.DesiredWorkers) (Plan, error) {
	switch p.Operation {
	case "apply", "resume", "plan", "status", "uninstall":
	default:
		return p, fmt.Errorf("%s requires a %s declaration; restore its YAML block before this operation", p.Operation, p.Worker)
	}
	p.Omitted = true
	p.Code = "declaration_omitted"
	p.State = "planned"
	p.Actions = []string{"inspect retained worker paths and service without changing activity", "review existing installation before explicit removal"}
	p.Unresolved = []string{"target presence and any historical custom paths require read-only inspection"}
	p.Next = "worker is absent from YAML; omission requests no installation and grants no removal authority"
	var err error
	if p.Worker == "notification-worker" {
		p.DesiredScope = 1
		p.DesiredHash, err = d.NotificationFingerprint()
	} else {
		p.DesiredHash, err = d.Fingerprint()
	}
	return p, err
}

// Omission is always a read-only review, including for apply and uninstall.
// Restore the previous block to select and authorize the normal domain removal.
// Presence is deliberately not treated as ownership or as an applied snapshot.
func (c CLI) reviewOmittedWorker(ctx context.Context, v Invocation, server config.ServerConfig, p Plan) error {
	if c.Inspector == nil {
		c.Inspector = SSHInspector{}
	}
	facts, err := c.Inspector.Inspect(ctx, server)
	if err != nil {
		return err
	}
	paths, err := ResolvePaths(p.Worker, config.WorkerPaths{}, facts)
	if err != nil {
		return err
	}
	inspector, ok := c.Inspector.(StatusInspector)
	if !ok {
		return fmt.Errorf("read-only target status inspection is unavailable")
	}
	observed, err := inspector.Status(ctx, server, p.Worker, paths)
	if err != nil {
		return err
	}
	p.Paths = &paths
	p = ApplyStatus(p, observed)
	p.Actions = nil
	p.Unresolved = append(p.Unresolved, "historical custom paths are not inferred from missing YAML; restore their explicit declaration to inspect them")
	switch p.Code {
	case "not_installed":
		if observed.LoadState == "not-found" && observed.ActiveState == "inactive" {
			p.Code = "not_requested"
			p.Next = "worker is absent from YAML and no installation is observed at default paths; nothing was created"
		} else {
			p.State, p.Code = "needs_action", "presence_unverified"
			p.Next = "worker is absent from YAML; default paths are empty but service presence is unverified; restore access before removal review"
		}
	case "admission_unverified":
		p.State, p.Code = "needs_action", "removal_review"
		p.Next = "worker is absent from YAML but retained files or a service exist; restore its previous declaration, inspect status, then use deploy " + p.Worker + " uninstall " + p.Server + " for reviewed removal"
	default:
		p.Next = "worker is absent from YAML and retained paths need recovery review; restore the previous declaration and inspect ownership before removal"
	}
	return PrintPlan(c.Out, p, v.Options.JSON)
}
