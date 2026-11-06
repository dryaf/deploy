package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

// This command uses the running admitted process. Stopping/reopening the store
// would lose the current producer session and runtime-loop evidence needed to
// resolve an inherited aggregate fault.
func runRuntimeLEDRecoveryCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	f.SetOutput(out)
	local := f.Bool("local", false, "Use the running local managed gateway")
	root := f.String("root", "", "Private managed installation root")
	manifest := f.String("manifest-hash", "", "Exact current installation hash")
	fault := f.String("fault", "", "One inherited worker fault from led-recovery-review")
	review := f.String("review-hash", "", "Exact proposed fault review hash")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	clear := args[0] == "led-recovery-clear"
	if !*local || !managedPath(*root) || !safefs.DigestPattern.MatchString(*manifest) || f.NArg() != 0 || (!clear && args[0] != "led-recovery-review") || clear && (len(*fault) == 0 || len(*fault) > 256 || !safefs.DigestPattern.MatchString(*review)) || !clear && (*fault != "" || *review != "") {
		return fmt.Errorf("LED recovery requires local root and manifest pins; clear also requires one fault and review hash")
	}
	if ui.DryRun {
		_, err := fmt.Fprintln(out, "[DRY] arguments validated; no service, worker API or daemon accessed")
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := privateManagedDir(*root, true)
	if err != nil {
		return err
	}
	defer d.Close()
	lease, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	m, c, err := qualificationInstallation(d, *root)
	if err != nil {
		return err
	}
	if protocol.Hash(m) != *manifest || m.Mode != "delivery" || m.RecoveryCommitHash == "" || c.LEDSocket == "" {
		return fmt.Errorf("LED recovery requires exact admitted recovered delivery with LED output")
	}
	pid, err := (userDeliveryInspector{}).inspect(ctx, m, false)
	if err != nil {
		return err
	}
	if pid == 0 {
		return fmt.Errorf("recovered delivery process is not running")
	}
	api := client.Client{Socket: m.Paths.Socket, Identity: m.Identity, PeerPID: pid}
	var result any
	if clear {
		var v worker.RuntimeLEDClearResult
		_, err = api.Call(ctx, protocol.Request{Operation: "led.recovery-clear", EventID: *fault, PayloadHash: *review}, &v)
		result = v
	} else {
		var v worker.RuntimeLEDRecoveryReview
		_, err = api.Call(ctx, protocol.Request{Operation: "led.recovery-review"}, &v)
		result = v
	}
	if err != nil {
		return err
	}
	if err = managedLeaseMatches(d, "operation.lock", lease); err != nil {
		return err
	}
	if err = checkManagedDirectoryPath(d, *root, true); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
