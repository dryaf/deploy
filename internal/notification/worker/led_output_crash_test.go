package worker

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type ledOutputCrashInput struct {
	Path     string
	Config   Config
	Request  LEDOutputRequest
	Boundary string
	States   map[string]led.State
}
type ledOutputCrashDaemon struct {
	States  map[string]led.State
	Cleared []string
}

func TestLEDOutputCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_LED_OUTPUT_CRASH_INPUT")
	if path == "" {
		t.Skip("subprocess only")
	}
	d, err := safefs.Open(path)
	must(t, err)
	defer d.Close()
	var input ledOutputCrashInput
	must(t, d.ReadJSON("input.json", &input))
	api := &fakeLED{states: input.States}
	_, err = configureLEDOutput(context.Background(), input.Path, input.Config, input.Request, api, func(at string) error {
		if at != input.Boundary {
			return nil
		}
		must(t, d.WriteJSON("daemon.json", ledOutputCrashDaemon{api.states, api.cleared}, false))
		return syscall.Kill(os.Getpid(), syscall.SIGKILL)
	})
	t.Fatal("crash helper survived", err)
}

func TestLEDOutputSIGKILLPreservesOutputDisposition(t *testing.T) {
	for _, boundary := range []string{"output-intent", "output-prepared", "output-clear-returned", "output-clear-recorded", "output-complete"} {
		t.Run(boundary, func(t *testing.T) {
			path, c, request, api, fault := outputFixture(t)
			dir := t.TempDir()
			d, err := safefs.Open(dir)
			must(t, err)
			defer d.Close()
			must(t, d.WriteJSON("input.json", ledOutputCrashInput{path, c, request, boundary, api.states}, false))
			command := exec.Command(os.Args[0], "-test.run=^TestLEDOutputCrashHelper$", "-test.timeout=30s")
			command.Env = append(os.Environ(), "DEPLOY_LED_OUTPUT_CRASH_INPUT="+dir)
			out, err := process.LimitedOutput(command, 35*time.Second, 16384)
			if err == nil || command.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong helper exit", string(out))
			}
			var retained ledOutputCrashDaemon
			must(t, d.ReadJSON("daemon.json", &retained))
			api.states, api.cleared = retained.States, retained.Cleared
			result, err := ConfigureLEDOutput(context.Background(), path, c, request, api)
			must(t, err)
			if !result.Complete || !result.Disabled || len(api.states) != 1 {
				t.Fatal("removal failed to converge")
			}
			seen := map[string]bool{}
			for _, id := range api.cleared {
				if seen[id] {
					t.Fatal("crash replayed clear", id)
				}
				seen[id] = true
			}
			s, err := Open(path, c, false)
			must(t, err)
			defer s.Close()
			var obsolete int
			must(t, s.db.QueryRow("SELECT obsolete FROM led_states WHERE id=?", fault).Scan(&obsolete))
			if obsolete != 0 {
				t.Fatal("crash recovery resolved an active fault")
			}
		})
	}
}
