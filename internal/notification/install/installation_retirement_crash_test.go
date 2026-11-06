package install

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type retirementCrashInput struct {
	Spec        ManagedSpec
	Manifest    ManagedManifest
	Target      ReplacementTarget
	Certificate InstallationRetirement
	Stage       string
}

func TestInstallationRetirementCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_RETIREMENT_CRASH_INPUT")
	if path == "" {
		t.Skip("child")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var in retirementCrashInput
	if err = json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	fault := func(stage string) error {
		if stage == in.Stage {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	if in.Certificate.Version != 0 {
		_, err = initializeReplacementWithFault(context.Background(), in.Certificate, protocol.Hash(in.Certificate), fault)
	} else {
		f := &fakeManagedServices{t: t, units: in.Spec.UnitDirectory, state: managedServiceState{Load: "not-found", Active: "inactive"}}
		i := managedInstaller{services: f, fault: fault}
		_, err = i.retireInstallation(context.Background(), in.Spec.Root, protocol.Hash(in.Manifest), in.Target)
	}
	t.Fatalf("boundary not killed: %v", err)
}
func TestInstallationRetirementSIGKILL(t *testing.T) {
	for _, stage := range []string{"retirement-target", "retirement-worker", "retirement-published", "replacement-certificate", "replacement-created", "replacement-identity"} {
		t.Run(stage, func(t *testing.T) {
			i, spec, m, target := installationRetirementFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			in := retirementCrashInput{Spec: spec, Manifest: m, Target: target, Stage: stage}
			if stage[:11] == "replacement" {
				var err error
				in.Certificate, err = i.retireInstallation(ctx, spec.Root, protocol.Hash(m), target)
				if err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(in)
			path := filepath.Join(t.TempDir(), "crash.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInstallationRetirementCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_RETIREMENT_CRASH_INPUT="+path)
			output, err := cmd.CombinedOutput()
			e, ok := err.(*exec.ExitError)
			if !ok || e.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("not killed: %v %s", err, output)
			}
			v, err := i.retireInstallation(ctx, spec.Root, protocol.Hash(m), target)
			if err != nil {
				t.Fatal(err)
			}
			id, err := initializeReplacement(ctx, v, protocol.Hash(v))
			if err != nil {
				t.Fatal(err)
			}
			again, err := initializeReplacement(ctx, v, protocol.Hash(v))
			if err != nil || id != again {
				t.Fatal("identity changed after kill", err)
			}
		})
	}
}
