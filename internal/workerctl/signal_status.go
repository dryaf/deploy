package workerctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
)

// The Signal child precedes the gateway journal. Inspect that retained child
// during first-install interruptions instead of reporting an absent gateway as
// an absent installation. This function never creates directories or leases.
func inspectDeclaredSignalProgress(ctx context.Context, r ControlRequest, paths ResolvedPaths) (ControlResult, bool, error) {
	result := ControlResult{Version: 1, State: "resumable", Code: "signal_setup_pending", Next: "run deploy notification-worker resume " + r.Server + " with the same declaration"}
	if _, err := os.Lstat(filepath.Join(paths.Control, "operation.json")); err == nil {
		return result, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, false, err
	}
	d, err := safefs.Open(filepath.Join(paths.Control, "signal"))
	if errors.Is(err, os.ErrNotExist) {
		return result, false, nil
	}
	if err != nil {
		return result, true, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if errors.Is(err, os.ErrNotExist) {
		result.State, result.Code, result.Next = "recovery_required", "signal_authority_missing", "Signal staging exists without its coordinator lease; inspect retained state before retrying"
		return result, true, nil
	}
	if err != nil {
		return result, true, err
	}
	defer lock.Close()
	var record OperationJournal
	if err = d.ReadJSON("operation.json", &record); err != nil {
		return result, true, err
	}
	if err = validateOperationJournal(record); err != nil {
		return result, true, err
	}
	if record.Pins.Worker != "notification-worker" || record.Pins.Action != "apply" || record.Pins.UID != r.Facts.UID || record.Pins.Home != r.Facts.Home || record.Pins.Artifacts["helper"] != r.HelperSHA256 || len(record.Children) != 1 || record.Children[0].Name != "signal-service" {
		return result, true, fmt.Errorf("Signal coordinator target or helper changed")
	}
	if record.Pins.DesiredHash != r.DesiredHash {
		result.State, result.Code, result.Next = "needs_action", "policy_changed", "finish the retained Signal setup with its original declaration before changing policy"
		return result, true, nil
	}
	p, ok := r.Desired.Notification.Providers["signal"]
	if !ok {
		return result, true, fmt.Errorf("retained Signal setup has no declaration")
	}
	resolved, err := ResolveSignalPaths(p, paths, r.Facts.Home, record.Pins.Artifacts["signal"])
	if err != nil {
		return result, true, err
	}
	expected := map[string]string{"signal-service": resolved.Service, "signal-account": resolved.Account, "signal-inbox": resolved.Inbox}
	if !reflect.DeepEqual(record.Pins.Installations, expected) {
		return result, true, fmt.Errorf("Signal setup paths changed")
	}
	var pins signalSetupPins
	if err = d.ReadJSON("inputs.json", &pins); err != nil {
		if errors.Is(err, os.ErrNotExist) && !record.Complete {
			for _, path := range []string{resolved.Account, resolved.Inbox, resolved.Service} {
				if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
					return result, true, fmt.Errorf("Signal input authority missing from existing state")
				}
			}
			return result, true, nil
		}
		return result, true, err
	}
	if pins.Version != 1 || pins.Operation != record.Children[0].Operation || pins.DesiredHash != r.DesiredHash || pins.Paths != resolved || pins.HelperHash != r.HelperSHA256 || pins.CLIHash != record.Pins.Artifacts["signal"] || !validDigest(pins.AccountHash) || !validDigest(pins.RecipientHash) {
		return result, true, fmt.Errorf("Signal input authority changed")
	}
	var saved signalSetupRecord
	err = d.ReadJSON("service.json", &saved)
	if err == nil {
		if saved.Version != 1 || saved.Spec.Operation != pins.Operation || saved.Account.Path != resolved.Account || saved.Account.Operation != pins.Operation || saved.Spec.Root != resolved.Service || saved.Spec.AccountState != resolved.Account || saved.Spec.Inbox.State != resolved.Inbox || saved.Spec.BinarySHA256 != pins.HelperHash || saved.Spec.CLISHA256 != pins.CLIHash || saved.Spec.AccountSHA256 != pins.AccountHash || saved.Spec.RecipientSHA256 != pins.RecipientHash {
			return result, true, fmt.Errorf("Signal service setup authority changed")
		}
		if err = signalaccount.InspectNativeAccountReservation(saved.Account); err != nil {
			return result, true, err
		}
		m, complete, err := install.InspectFreshSignalSetup(ctx, saved.Spec, false)
		if err != nil {
			return result, true, err
		}
		if record.Complete && (!complete || protocol.Hash(m) != record.Children[0].Receipt) {
			return result, true, fmt.Errorf("completed Signal child receipt is missing or changed")
		}
		if complete {
			result.Code, result.Next = "signal_gateway_pending", "Signal service is configured; resume the same declaration to finish its gateway"
		}
		return result, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return result, true, err
	}
	if record.Complete {
		return result, true, fmt.Errorf("completed Signal setup record missing")
	}
	if service, e := safefs.Open(resolved.Service); e == nil {
		names, readErr := service.File().Readdirnames(1)
		service.Close()
		if len(names) != 0 || readErr != io.EOF {
			return result, true, fmt.Errorf("Signal service exists without its coordinator receipt")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return result, true, e
	}
	if _, err = os.Lstat(resolved.Account); errors.Is(err, os.ErrNotExist) {
		return result, true, nil
	} else if err != nil {
		return result, true, err
	}
	status, err := signalaccount.Inspect(resolved.Account)
	if err != nil {
		return result, true, err
	}
	if status.Operation != "" && status.Operation != pins.Operation {
		return result, true, fmt.Errorf("Signal link operation changed")
	}
	switch status.State {
	case "none", "prepared", "publication_pending":
		result.State, result.Code, result.Next = "needs_action", "signal_link_required", "run deploy notification-worker resume "+r.Server+" in a terminal to finish linking"
	case "uncertain":
		result.State, result.Code, result.SignalReview = "needs_action", "signal_link_review_required", status.Operation
		result.Next = "inspect linked devices in Signal, then resume in a terminal to review the retained attempt"
	case "linked", "reviewed":
		result.Code, result.Next = "signal_service_pending", "account linking is retained; resume the same declaration to verify and install its service"
	default:
		return result, true, fmt.Errorf("Signal linking state requires domain review")
	}
	return result, true, nil
}
