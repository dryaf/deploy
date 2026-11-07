package signalaccount

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const MaintenanceVersion = 2

// Binding must identify the installed service, immutable image/configuration and
// loaded manager definition. Adapters must recheck it on every inspection.
type ServiceState struct {
	Binding string
	Active  bool
}
type Service interface {
	Inspect(context.Context) (ServiceState, error)
	Stop(context.Context) error
	Start(context.Context) error
}
type ServiceRequest struct{ Operation, Name, Binding string }
type serviceCoordinator struct {
	now   func() time.Time
	fault func(string) error
}

// MaintainLink takes no ownership of account credentials or service enablement.
// The adapter must prove its child understands the maintenance gate before entry.
// Launch runs outside account/control leases and must invoke the fenced entry
// point with the exact operation/name. No possibly-executed link is retried.
func MaintainLink(ctx context.Context, path string, request ServiceRequest, svc Service, launch func(context.Context) error) (MaintenanceStatus, error) {
	return (serviceCoordinator{now: time.Now}).run(ctx, path, request, svc, launch)
}

func (c serviceCoordinator) run(ctx context.Context, path string, request ServiceRequest, svc Service, launch func(context.Context) error) (result MaintenanceStatus, resultErr error) {
	if !safefs.ValidID(request.Operation, "n") || !protocol.ValidToken(request.Name) || !safefs.DigestPattern.MatchString(request.Binding) || svc == nil || launch == nil {
		return MaintenanceStatus{}, fmt.Errorf("invalid Signal service maintenance request")
	}
	if err := ctx.Err(); err != nil {
		return MaintenanceStatus{}, err
	}
	d, err := accountDirectory(path)
	if err != nil {
		return MaintenanceStatus{}, err
	}
	defer d.Close()
	if err = requireNoNativeService(d); err != nil {
		return MaintenanceStatus{}, err
	}

	lease, err := d.Lock("deploy-service-link.lock", true, true)
	if err != nil {
		return MaintenanceStatus{}, err
	}
	defer lease.Close()
	// A write may have become durable before its acknowledgement failed. Report
	// the actual persisted gate, including pending publication and link uncertainty,
	// rather than the coordinator's last in-memory phase.
	defer func() {
		view, e := InspectMaintenance(path)
		if e != nil {
			result.RuntimeAllowed = false
			resultErr = errors.Join(resultErr, e)
			return
		}
		result = view
	}()
	inspect := func() (ServiceState, error) {
		if err := ctx.Err(); err != nil {
			return ServiceState{}, err
		}
		s, err := svc.Inspect(ctx)
		if err == nil && s.Binding != request.Binding {
			err = fmt.Errorf("Signal service binding changed")
		}
		return s, err
	}
	state, err := inspect()
	if err != nil {
		return MaintenanceStatus{}, err
	}
	// No manager or child I/O occurs while the short account-control lease is held.
	change := func(fn func(*safefs.Directory, maintenanceHistory) error) (maintenanceHistory, error) {
		q, guard, err := control(path, true)
		if err != nil {
			return maintenanceHistory{}, err
		}
		defer q.Close()
		defer guard.Close()
		if err = requireNoRecovery(q); err != nil {
			return maintenanceHistory{}, err
		}
		h, err := readMaintenance(q)
		if err != nil {
			return h, err
		}
		if last := h.last(); last != nil && last.Operation == request.Operation && (last.Name != request.Name || last.Binding != request.Binding) {
			return h, fmt.Errorf("Signal maintenance request changed")
		}
		if last := h.last(); last != nil && last.Phase != "complete" && (last.Operation != request.Operation || last.Name != request.Name || last.Binding != request.Binding) {
			return h, fmt.Errorf("another Signal maintenance operation is unfinished")
		}
		if h.Pending {
			// Only the exact operation may acknowledge its pending phase; even
			// a pending completion cannot be skipped by a new operation.
			if h.last().Operation != request.Operation {
				return h, fmt.Errorf("resume the pending Signal maintenance acknowledgement first")
			}
			if err = acknowledgeMaintenance(q, h); err != nil {
				return h, err
			}
			h, err = readMaintenance(q)
			if err != nil {
				return h, err
			}
		}
		if err = fn(q, h); err != nil {
			return h, err
		}
		return readMaintenance(q)
	}
	h, err := change(func(q *safefs.Directory, h maintenanceHistory) error {
		if last := h.last(); last != nil && last.Operation == request.Operation {
			if last.Name != request.Name || last.Binding != request.Binding {
				return fmt.Errorf("Signal maintenance request changed")
			}
			links, err := readHistory(q)
			if err != nil {
				return err
			}
			return maintenanceLinkEvidence(h, links)
		}
		for _, old := range h.Records {
			if old.Operation == request.Operation {
				return fmt.Errorf("Signal maintenance ID is historical")
			}
		}
		sequence := 1
		previous := ""
		if last := h.last(); last != nil {
			sequence = last.Sequence + 1
			previous = protocol.Hash(*last)
			if c.now().Before(last.At) {
				return fmt.Errorf("Signal maintenance clock moved backward")
			}
		}
		if sequence > maxLinks {
			return fmt.Errorf("Signal service maintenance history is full")
		}
		links, e := readHistory(q)
		if e != nil {
			return e
		}
		if !links.runtimeAllowed() {
			return fmt.Errorf("finish or review previous Signal linking before service maintenance")
		}
		if len(links.Records) >= maxLinks {
			return fmt.Errorf("Signal link history is full; service was not stopped")
		}
		for _, link := range links.Records {
			if link.Intent.ID == request.Operation {
				return fmt.Errorf("Signal service operation ID already belongs to a link")
			}
		}
		r := maintenanceRecord{Version: 1, Sequence: sequence, Operation: request.Operation, Name: request.Name, Binding: request.Binding, BeforeActive: state.Active, Phase: "prepared", Previous: previous, At: c.now().UTC()}
		return writeMaintenance(q, h, r, c.fault)
	})
	if err != nil {
		return h.status(), err
	}
	advance := func(phase, linkHash string) error {
		var e error
		h, e = change(func(q *safefs.Directory, v maintenanceHistory) error {
			last := v.last()
			if last == nil || last.Operation != request.Operation {
				return fmt.Errorf("Signal maintenance authority disappeared")
			}
			if slices.Index(maintenancePhases, phase) != slices.Index(maintenancePhases, last.Phase)+1 {
				return fmt.Errorf("Signal maintenance phase changed")
			}
			next := *last
			next.Phase, next.Previous, next.At = phase, protocol.Hash(*last), c.now().UTC()
			if next.At.Before(last.At) {
				return fmt.Errorf("Signal maintenance clock moved backward")
			}
			if linkHash != "" {
				next.LinkHash = linkHash
			}
			return writeMaintenance(q, v, next, c.fault)
		})
		return e
	}
	last := h.last()
	if last.Phase == "complete" {
		return h.status(), nil
	}
	if last.Phase == "prepared" {
		if _, err = inspect(); err != nil {
			return h.status(), err
		}
		// Stop even an inactive unit: it cancels pending jobs and descendants.
		if err = svc.Stop(ctx); err != nil {
			return h.status(), err
		}
		if state, err = inspect(); err != nil {
			return h.status(), err
		}
		if state.Active {
			return h.status(), fmt.Errorf("Signal service has not stopped")
		}
		if err = advance("stopped", ""); err != nil {
			return h.status(), err
		}
	}
	if h.last().Phase == "stopped" {
		if state, err = inspect(); err != nil {
			return h.status(), err
		}
		if state.Active {
			return h.status(), fmt.Errorf("Signal service restarted during maintenance")
		}
		status, e := Inspect(path)
		if e != nil {
			return h.status(), e
		}
		if status.Operation == request.Operation && status.State == "uncertain" {
			return h.status(), fmt.Errorf("Signal account state needs explicit review; linking is not repeated")
		}
		if status.Operation != request.Operation || status.State == "prepared" || status.State == "publication_pending" {
			launchErr := launch(ctx)
			if c.fault != nil {
				if e = c.fault("maintenance_child_returned"); e != nil {
					return h.status(), e
				}
			}
			status, e = Inspect(path)
			if e != nil {
				return h.status(), e
			}
			if status.Operation != request.Operation || (status.State != "linked" && status.State != "reviewed") {
				if launchErr != nil {
					return h.status(), fmt.Errorf("Signal linking was not confirmed; resume after account inspection")
				}
				return h.status(), fmt.Errorf("Signal child did not produce the expected durable result")
			}
		}
		if status.Operation != request.Operation || (status.State != "linked" && status.State != "reviewed") {
			return h.status(), fmt.Errorf("Signal linking outcome is unsettled")
		}
		if err = advance("settled", status.RecordHash); err != nil {
			return h.status(), err
		}
	}
	if h.last().Phase == "settled" {
		status, e := Inspect(path)
		if e != nil {
			return h.status(), e
		}
		if status.Operation != request.Operation || status.RecordHash != h.last().LinkHash || (status.State != "linked" && status.State != "reviewed") {
			return h.status(), fmt.Errorf("Signal account review changed before release")
		}
		if state, err = inspect(); err != nil {
			return h.status(), err
		}
		if state.Active {
			return h.status(), fmt.Errorf("Signal service active before release")
		}
		if err = advance("released", ""); err != nil {
			return h.status(), err
		}
	}
	if h.last().Phase == "released" {
		q, guard, e := control(path, false)
		if e != nil {
			return h.status(), e
		}
		links, e := readHistory(q)
		if e == nil {
			e = maintenanceLinkEvidence(h, links)
		}
		guard.Close()
		q.Close()
		if e != nil {
			return h.status(), e
		}
		if state, err = inspect(); err != nil {
			return h.status(), err
		}
		if h.last().BeforeActive && !state.Active {
			if err = svc.Start(ctx); err != nil {
				return h.status(), err
			}
		}
		if !h.last().BeforeActive && state.Active {
			if err = svc.Stop(ctx); err != nil {
				return h.status(), err
			}
		}
		if state, err = inspect(); err != nil {
			return h.status(), err
		}
		if state.Active != h.last().BeforeActive {
			return h.status(), fmt.Errorf("Signal relay did not regain its prior service state")
		}
		if err = advance("complete", ""); err != nil {
			return h.status(), err
		}
	}
	return h.status(), nil
}
