//go:build notificationintegration

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

type runtimeLEDCrashInput struct {
	Path, Revision, Socket, Fault, Hash string
	Config                              Config
	Binding                             ManagedDeliveryRecovery
}

func TestRuntimeLEDRecoveryCrashChild(t *testing.T) {
	name := os.Getenv("DEPLOY_RUNTIME_LED_CRASH")
	if name == "" {
		t.Skip("subprocess fixture")
	}
	raw, e := os.ReadFile(name)
	must(t, e)
	var in runtimeLEDCrashInput
	must(t, json.Unmarshal(raw, &in))
	s, e := OpenManagedRecoveredDelivery(in.Path, in.Config, in.Revision, in.Binding, protocol.Hash(in.Binding.Owner), protocol.Hash("aggregate-activation"))
	must(t, e)
	defer s.Close()
	ctx := context.Background()
	must(t, s.BindManagedLEDRecovery(led.Client{Socket: in.Socket, Budget: time.Second}, func(context.Context) error { return nil }))
	observe, e := s.MonitorRuntimeLoop("delivery-admission")
	must(t, e)
	observe(nil)
	must(t, s.Replace(ctx, snapshot(time.Now().UTC(), 3, 2)))
	s.ledOperationFault = func(at string) error {
		if at == os.Getenv("DEPLOY_RUNTIME_LED_BOUNDARY") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	_, e = s.ClearRuntimeLEDRecovery(ctx, in.Fault, in.Hash)
	t.Fatal("child did not reach crash boundary", e)
}

type runtimeLEDCountingAPI struct {
	LEDAPI
	clears int
}

func (a *runtimeLEDCountingAPI) Clear(ctx context.Context, id string) error {
	a.clears++
	return a.LEDAPI.Clear(ctx, id)
}

func TestRuntimeLEDRecoverySIGKILLPinnedDaemon(t *testing.T) {
	socket := ledfixture.Start(t)
	api := led.Client{Socket: socket, Budget: time.Second}
	for _, point := range []string{"runtime-led-head", "runtime-led-intent", "runtime-led-returned", "runtime-led-result", "runtime-led-recorded"} {
		t.Run(point, func(t *testing.T) {
			f := newRuntimeLEDRecoveryFixture(t)
			ctx := context.Background()
			must(t, api.Set(ctx, f.api.states[f.id]))
			f.s.runtimeLEDAPI = api
			p := f.proposal(t)
			in := runtimeLEDCrashInput{f.path, f.revision, socket, f.id, p.Hash, f.config, f.binding}
			raw, e := json.Marshal(in)
			must(t, e)
			name := filepath.Join(t.TempDir(), "crash.json")
			must(t, os.WriteFile(name, raw, 0600))
			must(t, f.s.Close())
			cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeLEDRecoveryCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_RUNTIME_LED_CRASH="+name, "DEPLOY_RUNTIME_LED_BOUNDARY="+point)
			output, e := cmd.CombinedOutput()
			var died *exec.ExitError
			if !errors.As(e, &died) || died.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal(e, string(output))
			}
			f.open(t)
			counted := &runtimeLEDCountingAPI{LEDAPI: api}
			f.s.runtimeLEDAPI = counted
			result, e := f.s.ClearRuntimeLEDRecovery(ctx, f.id, p.Hash)
			must(t, e)
			if counted.clears != 0 {
				t.Fatal("crashed Clear was resent")
			}
			if point == "runtime-led-result" || point == "runtime-led-recorded" {
				if result.Outcome != "acknowledged" {
					t.Fatal(result)
				}
			} else if result.Outcome != "uncertain" {
				t.Fatal(result)
			}
			entries, e := api.List(ctx)
			must(t, e)
			present := false
			for _, entry := range entries {
				present = present || entry.ID == f.id
			}
			sent := point == "runtime-led-returned" || point == "runtime-led-result" || point == "runtime-led-recorded"
			if present == sent {
				t.Fatal("daemon state disagrees with crash boundary", present, point)
			}
		})
	}
}

func TestRuntimeLEDRecoverySocketPinnedDaemon(t *testing.T) {
	f := newRuntimeLEDRecoveryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := led.Client{Socket: ledfixture.Start(t), Budget: time.Second}
	must(t, api.Set(ctx, f.api.states[f.id]))
	foreign := led.State{ID: "foreign.aggregate-test", LED: "green", Pattern: "blink-fast", Priority: 100, Persistent: true, Message: "another application"}
	must(t, api.Set(ctx, foreign))
	f.s.runtimeLEDAPI = api
	dir, e := os.MkdirTemp("", "led-recovery-api-")
	must(t, e)
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "worker.sock")
	done := make(chan error, 1)
	go func() { done <- f.s.Serve(ctx, socket) }()
	defer func() {
		cancel()
		if e := <-done; e != nil && !errors.Is(e, context.Canceled) {
			t.Error(e)
		}
	}()
	c := client.Client{Socket: socket, Identity: f.s.identity, PeerPID: uint32(os.Getpid())}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, e = c.Call(ctx, protocol.Request{Operation: "system.ping"}, nil)
		if e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(e)
		}
		time.Sleep(5 * time.Millisecond)
	}
	var review RuntimeLEDRecoveryReview
	_, e = c.Call(ctx, protocol.Request{Operation: "led.recovery-review"}, &review)
	must(t, e)
	if review.Condition != "ok" || len(review.Faults) != 1 {
		t.Fatal(review)
	}
	var result RuntimeLEDClearResult
	_, e = c.Call(ctx, protocol.Request{Operation: "led.recovery-clear", EventID: f.id, PayloadHash: review.Faults[0].Hash}, &result)
	must(t, e)
	if result.Outcome != "acknowledged" {
		t.Fatal(result)
	}
	entries, e := api.List(ctx)
	must(t, e)
	preserved := false
	for _, entry := range entries {
		if entry.ID == f.id {
			t.Fatal("inherited fault remains")
		}
		if entry.ID == foreign.ID {
			preserved = protocol.Hash(entry.State) == protocol.Hash(foreign)
		}
	}
	if !preserved {
		t.Fatal("foreign state changed")
	}
}
