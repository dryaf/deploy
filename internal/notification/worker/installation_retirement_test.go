package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestInstallationRetirementResumesAndPreservesForeignLED(t *testing.T) {
	for _, boundary := range []string{"retirement-intent", "retirement-clear", "retirement-complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			s, _, path := retirementFixture(t)
			_, err := s.RetireProducer(ctx, producer, "operator", "Retire old worker", true)
			must(t, err)
			api := &fakeLED{states: map[string]led.State{}}
			baseline := ledBaseline(s.identity.Worker, ledLayoutOutputs(desiredLEDLayout(s.config, s.identity.Worker, os.Getuid()))[0])
			api.states[baseline.ID] = baseline
			foreign := led.State{ID: "foreign-fault", LED: "red", Pattern: "blink-fast", Priority: 100, Persistent: true}
			api.states[foreign.ID] = foreign
			target := protocol.Hash("reviewed replacement")
			_, err = s.retireInstallation(ctx, target, migrationSocket, api, func(stage string) error {
				if stage == boundary {
					return errors.New("interruption")
				}
				return nil
			})
			if err == nil {
				t.Fatal("fault not reached")
			}
			if _, err = s.retireInstallation(ctx, protocol.Hash("changed"), migrationSocket, api, nil); err == nil {
				t.Fatal("changed target resumed")
			}
			cfg := s.config
			must(t, s.Close())
			s, err = Open(path, cfg, false)
			must(t, err)
			defer s.Close()
			complete, err := s.RetireInstallation(ctx, target, migrationSocket, api)
			must(t, err)
			again, err := s.RetireInstallation(ctx, target, migrationSocket, api)
			must(t, err)
			if protocol.Hash(complete) != protocol.Hash(again) || protocol.Hash(api.states[foreign.ID]) != protocol.Hash(foreign) || len(api.states) != 1 {
				t.Fatal("retirement changed audit or foreign state")
			}
			if err = (&LEDReconciler{Store: s, API: api, Budget: time.Second}).Step(ctx); err == nil {
				t.Fatal("archived runtime recreated LED output")
			}
			if err = s.Stage(producer, "late"); err == nil {
				t.Fatal("archived producer revived")
			}
			if _, err = os.Stat(filepath.Join(path, "ownership")); err != nil {
				t.Fatal("ownership history lost", err)
			}
		})
	}
}
func TestInstallationRetirementRefusesUnsettledAndChangedLED(t *testing.T) {
	for _, kind := range []string{"unretired", "pending-input", "fault", "baseline", "socket", "wrong-uid"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, _, _ := retirementFixture(t)
			if kind != "unretired" {
				_, err := s.RetireProducer(ctx, producer, "operator", "Retired", true)
				must(t, err)
			}
			api := &fakeLED{states: map[string]led.State{}}
			baseline := ledBaseline(s.identity.Worker, ledLayoutOutputs(desiredLEDLayout(s.config, s.identity.Worker, os.Getuid()))[0])
			switch kind {
			case "pending-input":
				_, err := s.db.Exec("INSERT INTO provider_inbox(account,id,hash,body,state) VALUES('account',1,'hash','{}','pending')")
				must(t, err)
			case "fault":
				api.states[ledBindingNamespace(s.identity.Worker, workerLEDBinding)+".fault"] = led.State{ID: ledBindingNamespace(s.identity.Worker, workerLEDBinding) + ".fault", LED: "green", Pattern: "blink-fast", Persistent: true, Priority: 90}
			case "baseline":
				baseline.Pattern = "off"
				api.states[baseline.ID] = baseline
			case "wrong-uid":
				_, err := s.db.Exec("UPDATE metadata SET value=json_set(value,'$.uid',?) WHERE key='led-layout'", os.Getuid()+1)
				must(t, err)
			}
			var endpoint LEDAPI = api
			if kind == "socket" {
				endpoint = nil
			}
			if _, err := s.RetireInstallation(ctx, protocol.Hash("target"), migrationSocket, endpoint); err == nil {
				t.Fatal("unsafe retirement completed")
			}
		})
	}
}

func TestInstallationRetirementMissingIndependentProofRefusesOpen(t *testing.T) {
	for _, missing := range []string{"installation-retirement-intent.json", "sql"} {
		t.Run(missing, func(t *testing.T) {
			ctx := context.Background()
			s, _, path := retirementFixture(t)
			_, err := s.RetireProducer(ctx, producer, "operator", "Retired", true)
			must(t, err)
			_, err = s.RetireInstallation(ctx, protocol.Hash("target"), "", nil)
			must(t, err)
			cfg := s.config
			if missing == "sql" {
				_, err = s.db.Exec("DELETE FROM metadata WHERE key='installation-retirement'")
				must(t, err)
			} else {
				must(t, os.Remove(filepath.Join(path, missing)))
			}
			must(t, s.Close())
			if opened, err := Open(path, cfg, false); err == nil {
				opened.Close()
				t.Fatal("lost proof reopened")
			}
		})
	}
}
