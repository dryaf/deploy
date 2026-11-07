package workerctl

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

func testNotificationControl(ctx context.Context, r ControlRequest, paths ResolvedPaths, send func(context.Context, string, string) (install.DiagnosticResult, error)) (ControlResult, error) {
	result := ControlResult{Version: 1}
	d, err := safefs.Open(paths.Control)
	if err != nil {
		return result, fmt.Errorf("declarative installation receipt is required before testing")
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	var record OperationJournal
	if err = d.ReadJSON("operation.json", &record); err != nil {
		return result, err
	}
	if err = validateOperationJournal(record); err != nil {
		return result, err
	}
	receipt, canonical := canonicalNotificationReceipt(record)
	if r.Desired.Notification == nil || !config.WorkerEnabled(r.Desired.Notification.Enabled) || !record.Complete || !canonical || record.Pins.Action != "apply" || record.Pins.DesiredHash != r.DesiredHash || record.Pins.DesiredScope != r.DesiredScope || record.Pins.UID != r.Facts.UID || record.Pins.Home != r.Facts.Home || record.Pins.Installations["notification-worker"] != paths.Installation || len(record.Children) != 1 {
		return result, fmt.Errorf("test requires the unchanged completed declaration")
	}
	if adoptionJournal(record) {
		var v notificationAdoptionIntent
		if err = d.ReadJSON("adoption.json", &v); err != nil {
			return result, err
		}
		if err = verifyAdoptionReceipt(d, record, v.Manifest); err != nil {
			return result, err
		}
	}
	if err = verifyRetainedSignalAdoption(d); err != nil {
		return result, err
	}
	if r.DiagnosticProvider != "" {
		p, ok := r.Desired.Notification.Providers[r.DiagnosticProvider]
		if !ok || p.Conversation != "" {
			return result, fmt.Errorf("diagnostic provider is not a configured fixed destination")
		}
	}
	diagnostic, err := send(ctx, paths.Installation, receipt)
	if err != nil {
		return result, err
	}
	if r.DiagnosticProvider != "" && diagnostic.Provider != r.DiagnosticProvider {
		return result, fmt.Errorf("diagnostic response differs from the requested provider; inspect retained outcomes before another send")
	}
	result.Diagnostic = &diagnostic
	result.Identity = &diagnostic.Identity
	result.State = diagnostic.Status
	if diagnostic.Status == "provider_accepted" {
		result.Code = "ok"
		result.Next = "provider accepted the diagnostic; this does not qualify backup delivery or activate production"
		if diagnostic.Provider != "" {
			result.Next = diagnostic.Provider + " accepted the selected-destination diagnostic; this does not qualify delivery or activate production"
		}
	} else {
		result.Code = diagnostic.Status
		result.Next = "diagnostic outcome retained; inspect provider state before any new send"
	}
	return result, nil
}
