package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func outputFixture(t *testing.T) (string, Config, LEDOutputRequest, *fakeLED, string) {
	t.Helper()
	path, c, _, api := mappingFixture(t)
	s, err := Open(path, c, false)
	must(t, err)
	var id string
	must(t, s.transaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		id, err = s.currentLEDState(tx, "selected", "critical")
		return err
	}))
	op, err := safefs.NewID("n")
	must(t, err)
	request := LEDOutputRequest{Identity: s.identity, Operation: op, Socket: migrationSocket, Disabled: true}
	must(t, s.Close())
	api.states[id] = led.State{ID: id, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true, Message: "Notification state: critical"}
	api.states["another-application"] = led.State{ID: "another-application", LED: "green", Pattern: "blink-fast", Priority: 100, Persistent: true}
	api.cleared = nil
	return path, c, request, api, id
}

func TestLEDOutputDisableRetainsFaultAndReenableNeedsFreshEvidence(t *testing.T) {
	path, c, request, api, fault := outputFixture(t)
	ctx := context.Background()
	status, err := ConfigureLEDOutput(ctx, path, c, request, api)
	must(t, err)
	if !status.Complete || !status.Disabled || len(api.states) != 1 {
		t.Fatal("output not disabled", status, api.states)
	}
	if _, ok := api.states["another-application"]; !ok {
		t.Fatal("foreign output cleared")
	}
	s, err := Open(path, c, false)
	must(t, err)
	var condition string
	var obsolete int
	must(t, s.db.QueryRow("SELECT condition,obsolete FROM led_states WHERE id=?", fault).Scan(&condition, &obsolete))
	if condition != "critical" || obsolete != 0 {
		t.Fatal("disable falsely resolved incident")
	}
	clears := len(api.cleared)
	must(t, (&LEDReconciler{Store: s, API: api, Budget: time.Second}).Step(ctx))
	if len(api.states) != 1 || len(api.cleared) != clears {
		t.Fatal("disabled runtime performed LED I/O")
	}
	must(t, s.Close())
	again, err := ConfigureLEDOutput(ctx, path, c, request, api)
	must(t, err)
	if again != status || len(api.cleared) != clears {
		t.Fatal("completed disable replayed")
	}
	request.Operation, err = safefs.NewID("n")
	must(t, err)
	request.Disabled = false
	status, err = ConfigureLEDOutput(ctx, path, c, request, api)
	must(t, err)
	if !status.Complete || status.Disabled || len(api.states) != 1 {
		t.Fatal("re-enable fabricated output")
	}
	s, err = OpenManagedVerification(path, c, request.Identity)
	must(t, err)
	defer s.Close()
	must(t, (&LEDReconciler{Store: s, API: api, Budget: time.Second}).Step(ctx))
	for _, entry := range api.states {
		if entry.Pattern == "heartbeat" {
			t.Fatal("re-enable fabricated fresh healthy evidence")
		}
	}
	must(t, s.db.QueryRow("SELECT condition,obsolete FROM led_states WHERE id=?", fault).Scan(&condition, &obsolete))
	if condition != "critical" || obsolete != 0 {
		t.Fatal("re-enable rewrote incident")
	}
}

func TestLEDOutputLostResponsesResumeWithoutDuplicateClear(t *testing.T) {
	for _, point := range []string{"output-intent", "output-prepared", "output-clear-returned", "output-clear-recorded", "output-complete"} {
		t.Run(point, func(t *testing.T) {
			path, c, request, api, _ := outputFixture(t)
			ctx := context.Background()
			_, err := configureLEDOutput(ctx, path, c, request, api, func(at string) error {
				if at == point {
					return errors.New("lost response")
				}
				return nil
			})
			if err == nil {
				t.Fatal("fault boundary not reached")
			}
			s, err := Open(path, c, false)
			must(t, err)
			before := protocol.Hash(api.states)
			_ = (&LEDReconciler{Store: s, API: api, Budget: time.Second}).Step(ctx)
			if protocol.Hash(api.states) != before {
				t.Fatal("pending/disabled output refreshed")
			}
			must(t, s.Close())
			status, err := ConfigureLEDOutput(ctx, path, c, request, api)
			must(t, err)
			if !status.Complete || len(api.states) != 1 {
				t.Fatal("removal did not converge")
			}
			seen := map[string]bool{}
			for _, id := range api.cleared {
				if seen[id] {
					t.Fatal("lost clear replayed", id)
				}
				seen[id] = true
			}
		})
	}
}

type unavailableOutputLED struct{ *fakeLED }

func (unavailableOutputLED) List(context.Context) ([]led.Entry, error) {
	return nil, errors.New("daemon unavailable")
}

func TestLEDOutputUnavailableDaemonRetainsPendingFence(t *testing.T) {
	path, c, request, api, _ := outputFixture(t)
	if _, err := ConfigureLEDOutput(context.Background(), path, c, request, unavailableOutputLED{api}); err == nil {
		t.Fatal("unreachable daemon reported success")
	}
	s, err := Open(path, c, false)
	must(t, err)
	status, err := s.LEDOutputStatus(context.Background())
	must(t, err)
	if status.Complete || !status.Disabled {
		t.Fatal("missing pending output fence", status)
	}
	before := protocol.Hash(api.states)
	if err = (&LEDReconciler{Store: s, API: api, Budget: time.Second}).Step(context.Background()); err == nil {
		t.Fatal("cleanup-pending runtime admitted")
	}
	if protocol.Hash(api.states) != before {
		t.Fatal("cleanup-pending runtime mutated output")
	}
	must(t, s.Close())
	_, err = ConfigureLEDOutput(context.Background(), path, c, request, api)
	must(t, err)
}

func TestLEDOutputRefusesChangedRegistryAndMissingAuthority(t *testing.T) {
	path, c, request, api, id := outputFixture(t)
	changed := api.states[id]
	changed.Message = "foreign replacement"
	api.states[id] = changed
	if _, err := ConfigureLEDOutput(context.Background(), path, c, request, api); err == nil {
		t.Fatal("foreign replacement cleared")
	}
	if len(api.cleared) != 0 {
		t.Fatal("partial clearing before registry ownership verification")
	}
	changed.Message = "Notification state: critical"
	api.states[id] = changed
	_, err := ConfigureLEDOutput(context.Background(), path, c, request, api)
	must(t, err)
	must(t, os.Remove(filepath.Join(path, ledOutputFile)))
	if _, err = ConfigureLEDOutput(context.Background(), path, c, request, api); err == nil {
		t.Fatal("missing output authority recreated")
	}
	s, err := Open(path, c, false)
	must(t, err)
	defer s.Close()
	if err = (&LEDReconciler{Store: s, API: api, Budget: time.Second}).Step(context.Background()); err == nil {
		t.Fatal("missing output authority admitted refresh")
	}
}

func TestLEDOutputLostSQLHeadCannotRecreateDisposition(t *testing.T) {
	for _, missingFile := range []bool{false, true} {
		path, c, request, api, _ := outputFixture(t)
		_, err := ConfigureLEDOutput(context.Background(), path, c, request, api)
		must(t, err)
		s, err := Open(path, c, false)
		must(t, err)
		_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", ledOutputHead)
		must(t, err)
		must(t, s.Close())
		if missingFile {
			must(t, os.Remove(filepath.Join(path, ledOutputFile)))
		}
		clears := len(api.cleared)
		if _, err = ConfigureLEDOutput(context.Background(), path, c, request, api); err == nil {
			t.Fatal("lost output head recreated")
		}
		if len(api.cleared) != clears {
			t.Fatal("lost head replayed cleanup")
		}
	}
}
