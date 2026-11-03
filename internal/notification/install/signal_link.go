package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runSignalLinkCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Use an explicit local Signal account directory")
	state := f.String("state-dir", "", "Private canonical Signal account directory, not the worker/inbox directory")
	operation := f.String("operation", "", "Exact linking operation ID")
	name := f.String("name", "", "Signal device name (link entry point only)")
	reason := f.String("reason", "", "Reason after reviewing possibly changed account state")
	accept := f.Bool("accept-account-state", false, "Explicitly accept reviewed account state without claiming linking succeeded")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || !managedPath(*state) || f.NArg() != 0 {
		return fmt.Errorf("Signal maintenance requires --local and an absolute account --state-dir")
	}
	allowed := map[string]map[string]bool{
		"__signal-link":          {"local": true, "state-dir": true, "operation": true, "name": true},
		"signal-recovery-status": {"local": true, "state-dir": true},
		"signal-service-status":  {"local": true, "state-dir": true},
		"signal-link-status":     {"local": true, "state-dir": true},
		"signal-link-resolve":    {"local": true, "state-dir": true, "operation": true, "reason": true, "accept-account-state": true},
	}
	bad := false
	f.Visit(func(v *flag.Flag) {
		if !allowed[args[0]][v.Name] {
			bad = true
		}
	})
	if bad || allowed[args[0]] == nil {
		return fmt.Errorf("invalid Signal maintenance options")
	}
	if args[0] != "signal-link-status" && args[0] != "signal-service-status" && args[0] != "signal-recovery-status" && !safefs.ValidID(*operation, "n") {
		return fmt.Errorf("an exact --operation ID is required")
	}
	if args[0] == "__signal-link" && !protocol.ValidToken(*name) {
		return fmt.Errorf("a valid device --name is required")
	}
	if args[0] == "signal-link-resolve" && (!*accept || *reason == "") {
		return fmt.Errorf("resolution requires --accept-account-state and --reason after inspection")
	}
	if args[0] == "signal-link-resolve" {
		if err := signalaccount.ValidateResolution(*operation, *reason); err != nil {
			return err
		}
	}
	if ui.DryRun {
		_, err := fmt.Fprintln(out, "[DRY] Signal maintenance arguments validated; no account files, subprocesses, services or messages changed")
		return err
	}
	if args[0] == "signal-recovery-status" {
		status, err := signalaccount.InspectRecovery(*state)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(status)
	}
	if args[0] == "signal-service-status" {
		status, err := signalaccount.InspectMaintenance(*state)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(status)
	}
	var status signalaccount.Status
	var err error
	switch args[0] {
	case "__signal-link":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		status, err = signalaccount.Link(ctx, *state, *name, *operation, out)
	case "signal-link-status":
		status, err = signalaccount.Inspect(*state)
	case "signal-link-resolve":
		status, err = signalaccount.Resolve(*state, *operation, *reason)
	}
	if status.Operation != "" || err == nil {
		if e := json.NewEncoder(out).Encode(status); e != nil {
			return e
		}
	}
	return err
}
