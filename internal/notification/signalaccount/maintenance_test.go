package signalaccount

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type fixtureService struct {
	path, binding       string
	active              bool
	lease               *Lease
	starts, stops       int
	lostStart, lostStop bool
}

func (s *fixtureService) Inspect(context.Context) (ServiceState, error) {
	return ServiceState{s.binding, s.active}, nil
}
func (s *fixtureService) Stop(context.Context) error {
	s.stops++
	if s.lease != nil {
		s.lease.Close()
		s.lease = nil
	}
	s.active = false
	if s.lostStop {
		s.lostStop = false
		return errors.New("lost stop reply")
	}
	return nil
}
func (s *fixtureService) Start(context.Context) error {
	s.starts++
	l, err := AcquireRuntime(s.path)
	if err != nil {
		return err
	}
	s.lease = l
	s.active = true
	if s.lostStart {
		s.lostStart = false
		return errors.New("lost start reply")
	}
	return nil
}
func maintenanceFixture(t *testing.T, active bool) (string, ServiceRequest, *fixtureService, func(context.Context) error, *int) {
	t.Helper()
	path, id, l, calls := fixture(t)
	request := ServiceRequest{id, "fixture-device", protocol.Hash("service-image-and-config")}
	s := &fixtureService{path: path, binding: request.Binding}
	if active {
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		s.starts = 0
	}
	t.Cleanup(func() {
		if s.lease != nil {
			s.lease.Close()
		}
	})
	launch := func(ctx context.Context) error {
		_, err := l.link(ctx, path, request.Name, request.Operation, io.Discard)
		return err
	}
	return path, request, s, launch, calls
}

func TestSignalMaintenanceRestoresOriginalActivity(t *testing.T) {
	for _, active := range []bool{false, true} {
		path, request, svc, launch, calls := maintenanceFixture(t, active)
		view, err := MaintainLink(context.Background(), path, request, svc, launch)
		if err != nil || view.Phase != "complete" || !view.RuntimeAllowed || view.BeforeActive != active || svc.active != active || *calls != 1 {
			t.Fatal(view, err, svc.active, *calls)
		}
		stops, starts := svc.stops, svc.starts
		view, err = MaintainLink(context.Background(), path, request, svc, launch)
		if err != nil || view.Phase != "complete" || svc.stops != stops || svc.starts != starts || *calls != 1 {
			t.Fatal("repeat changed service/account", view, err)
		}
	}
}

func TestSignalMaintenanceInterruptedPhases(t *testing.T) {
	var boundaries []string
	for _, phase := range maintenancePhases {
		boundaries = append(boundaries, "maintenance_"+phase+"_record", "maintenance_"+phase+"_head")
	}
	boundaries = append(boundaries, "maintenance_child_returned")
	for _, point := range boundaries {
		t.Run(point, func(t *testing.T) {
			path, request, svc, launch, calls := maintenanceFixture(t, true)
			c := serviceCoordinator{now: time.Now, fault: func(p string) error {
				if p == point {
					return errors.New("interrupted")
				}
				return nil
			}}
			returned, runErr := c.run(context.Background(), path, request, svc, launch)
			if runErr == nil {
				t.Fatal("boundary missed")
			}
			view, err := InspectMaintenance(path)
			if err != nil {
				t.Fatal(err)
			}
			if view.Operation != request.Operation {
				t.Fatal(view)
			}
			if returned != view {
				t.Fatal("returned stale durable status", returned, view)
			}
			if view.Phase != "released" && view.Phase != "complete" || view.Pending {
				requireRuntime(t, path, false)
			}
			// New operations cannot steal an incomplete or unacknowledged journal.
			if view.Phase != "complete" || view.Pending {
				other := request
				other.Operation, _ = safefs.NewID("n")
				if _, err = MaintainLink(context.Background(), path, other, svc, launch); err == nil {
					t.Fatal("unfinished operation superseded")
				}
			}
			c.fault = nil
			view, err = c.run(context.Background(), path, request, svc, launch)
			if err != nil || view.Phase != "complete" || !svc.active || *calls != 1 {
				t.Fatal("resume", view, err, svc.active, *calls)
			}
		})
	}
}

func TestSignalMaintenanceUncertainLinkRequiresReview(t *testing.T) {
	path, request, svc, _, _ := maintenanceFixture(t, true)
	calls := 0
	l := linker{now: time.Now, run: func(context.Context, *Lease, string, io.Writer) error { calls++; return errors.New("CLI unavailable") }}
	launch := func(ctx context.Context) error {
		_, err := l.link(ctx, path, request.Name, request.Operation, io.Discard)
		return err
	}
	if _, err := MaintainLink(context.Background(), path, request, svc, launch); err == nil {
		t.Fatal("failed child accepted")
	}
	if svc.active || calls != 1 {
		t.Fatal("service resumed after failure")
	}
	requireRuntime(t, path, false)
	if _, err := MaintainLink(context.Background(), path, request, svc, launch); err == nil || calls != 1 {
		t.Fatal("uncertain CLI retried", err, calls)
	}
	if _, err := Resolve(path, request.Operation, "Checked account after interrupted maintenance"); err != nil {
		t.Fatal(err)
	}
	// Account review alone must not bypass the service coordinator's gate.
	requireRuntime(t, path, false)
	view, err := MaintainLink(context.Background(), path, request, svc, launch)
	if err != nil || !svc.active || calls != 1 || view.Phase != "complete" {
		t.Fatal(view, err, svc.active, calls)
	}
}

func TestSignalMaintenanceLostRepliesDoNotRepeatLink(t *testing.T) {
	for _, failure := range []string{"stop", "child", "start"} {
		t.Run(failure, func(t *testing.T) {
			path, request, svc, launch, calls := maintenanceFixture(t, true)
			svc.lostStop = failure == "stop"
			svc.lostStart = failure == "start"
			if failure == "child" {
				original := launch
				launch = func(ctx context.Context) error {
					if err := original(ctx); err != nil {
						return err
					}
					return errors.New("reply lost after durable success")
				}
			}
			_, err := MaintainLink(context.Background(), path, request, svc, launch)
			if failure == "child" && err != nil {
				t.Fatal("durable child result not recovered", err)
			}
			view, err := MaintainLink(context.Background(), path, request, svc, launch)
			if err != nil || view.Phase != "complete" || !svc.active || *calls != 1 {
				t.Fatal(view, err, *calls)
			}
		})
	}
}

func TestSignalMaintenanceGuards(t *testing.T) {
	path, request, svc, launch, calls := maintenanceFixture(t, false)
	c := serviceCoordinator{now: time.Now, fault: func(p string) error {
		if p == "maintenance_prepared_head" {
			return errors.New("stop")
		}
		return nil
	}}
	if _, err := c.run(context.Background(), path, request, svc, launch); err == nil {
		t.Fatal("missing boundary")
	}
	requireRuntime(t, path, false)
	_, _, l, _ := fixture(t)
	if _, err := l.link(context.Background(), path, request.Name, request.Operation, io.Discard); err == nil {
		t.Fatal("child ran before stopped phase")
	}
	for _, change := range []string{"name", "binding", "service"} {
		other := request
		switch change {
		case "name":
			other.Name = "different"
		case "binding":
			other.Binding = protocol.Hash("different")
		case "service":
			svc.binding = protocol.Hash("different")
		}
		if _, err := MaintainLink(context.Background(), path, other, svc, launch); err == nil {
			t.Fatal("changed authority accepted", change)
		}
		svc.binding = request.Binding
	}
	if *calls != 0 || svc.stops != 0 {
		t.Fatal("guard failure changed service", *calls, svc.stops)
	}
	d, err := accountDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	lock, err := d.Lock("deploy-service-link.lock", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = MaintainLink(context.Background(), path, request, svc, launch); err == nil {
		t.Fatal("coordinator lease bypassed")
	}
	lock.Close()
	if _, err = MaintainLink(context.Background(), path, request, svc, launch); err != nil {
		t.Fatal(err)
	}
}

func TestSignalMaintenanceCorruptEvidenceFencesStartup(t *testing.T) {
	for _, damage := range []string{"missing-head", "missing-phase", "changed-record", "foreign-file", "missing-link-history"} {
		t.Run(damage, func(t *testing.T) {
			path, request, svc, launch, _ := maintenanceFixture(t, false)
			if _, err := MaintainLink(context.Background(), path, request, svc, launch); err != nil {
				t.Fatal(err)
			}
			var err error
			switch damage {
			case "missing-head":
				err = os.Remove(filepath.Join(path, maintenanceHeadFile))
			case "missing-phase":
				err = os.Remove(filepath.Join(path, maintenanceDirectory, "0001.2.json"))
			case "changed-record":
				file := filepath.Join(path, maintenanceDirectory, "0001.4.json")
				raw, e := os.ReadFile(file)
				if e != nil {
					t.Fatal(e)
				}
				err = os.WriteFile(file, []byte(strings.Replace(string(raw), "complete", "released", 1)), 0600)
			case "foreign-file":
				err = os.WriteFile(filepath.Join(path, maintenanceDirectory, "foreign.json"), []byte("{}"), 0600)
			case "missing-link-history":
				err = os.Remove(filepath.Join(path, historyDirectory, recordFile(1, "result")))
			}
			if err != nil {
				t.Fatal(err)
			}
			requireRuntime(t, path, false)
			if _, err = InspectMaintenance(path); err == nil {
				t.Fatal("corrupt evidence accepted")
			}
		})
	}
}

func TestSignalMaintenanceRejectsExhaustionAndReusedLinkBeforeStop(t *testing.T) {
	for _, full := range []bool{false, true} {
		path, request, svc, launch, _ := maintenanceFixture(t, false)
		l := linker{now: time.Now, run: func(context.Context, *Lease, string, io.Writer) error { return nil }}
		count := 1
		if full {
			count = maxLinks
		}
		for i := 0; i < count; i++ {
			id := request.Operation
			if full {
				id, _ = safefs.NewID("n")
			}
			if _, err := l.link(context.Background(), path, request.Name, id, io.Discard); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := MaintainLink(context.Background(), path, request, svc, launch); err == nil {
			t.Fatal("exhausted/reused history accepted", full)
		}
		if svc.stops != 0 {
			t.Fatal("preflight stopped service")
		}
		view, err := InspectMaintenance(path)
		if err != nil || view.Phase != "none" || !view.RuntimeAllowed {
			t.Fatal(view, err)
		}
	}
}
