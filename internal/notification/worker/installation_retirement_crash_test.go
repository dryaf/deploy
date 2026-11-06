package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

type retirementWorkerCrashInput struct {
	Path     string
	Config   Config
	Stage    string
	Registry string
}

func TestInstallationRetirementWorkerCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_WORKER_RETIREMENT_CRASH")
	if path == "" {
		t.Skip("child")
	}
	raw, err := os.ReadFile(path)
	must(t, err)
	var in retirementWorkerCrashInput
	must(t, json.Unmarshal(raw, &in))
	s, err := Open(in.Path, in.Config, false)
	must(t, err)
	defer s.Close()
	api := &fakeLED{}
	raw, err = os.ReadFile(in.Registry)
	must(t, err)
	must(t, json.Unmarshal(raw, &api.states))
	api.onClear = func(string) {
		raw, err := json.Marshal(api.states)
		must(t, err)
		must(t, os.WriteFile(in.Registry, raw, 0600))
	}
	_, err = s.retireInstallation(context.Background(), protocol.Hash("replacement"), migrationSocket, api, func(stage string) error {
		if stage == in.Stage {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatalf("boundary not killed: %v", err)
}
func TestInstallationRetirementWorkerSIGKILL(t *testing.T) {
	for _, stage := range []string{"retirement-intent", "retirement-clear", "retirement-complete"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s, _, path := retirementFixture(t)
			_, err := s.RetireProducer(ctx, producer, "operator", "Retired", true)
			must(t, err)
			cfg := s.config
			baseline := ledBaseline(s.identity.Worker, ledLayoutOutputs(desiredLEDLayout(cfg, s.identity.Worker, os.Getuid()))[0])
			must(t, s.Close())
			registry := filepath.Join(t.TempDir(), "registry.json")
			states := map[string]led.State{baseline.ID: baseline, "foreign": {ID: "foreign", LED: "red", Pattern: "blink-fast", Priority: 100, Persistent: true}}
			raw, err := json.Marshal(states)
			must(t, err)
			must(t, os.WriteFile(registry, raw, 0600))
			input := filepath.Join(t.TempDir(), "input.json")
			raw, err = json.Marshal(retirementWorkerCrashInput{path, cfg, stage, registry})
			must(t, err)
			must(t, os.WriteFile(input, raw, 0600))
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInstallationRetirementWorkerCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_WORKER_RETIREMENT_CRASH="+input)
			output, err := cmd.CombinedOutput()
			e, ok := err.(*exec.ExitError)
			if !ok || e.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("not killed: %v %s", err, output)
			}
			s, err = Open(path, cfg, false)
			must(t, err)
			defer s.Close()
			api := &fakeLED{}
			raw, err = os.ReadFile(registry)
			must(t, err)
			must(t, json.Unmarshal(raw, &api.states))
			_, err = s.RetireInstallation(ctx, protocol.Hash("replacement"), migrationSocket, api)
			must(t, err)
			if len(api.states) != 1 || api.states["foreign"].ID != "foreign" {
				t.Fatal("foreign state lost or owned state remains")
			}
		})
	}
}
