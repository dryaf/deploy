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

	"github.com/dryaf/deploy/internal/process"
)

type publicationInstallCrash struct {
	Path, Point string
	Config      Config
	Image       RecoveryPublicationImage
}

func TestRecoveryPublishCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_PUBLICATION_INSTALL_CRASH")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	must(t, err)
	var in publicationInstallCrash
	must(t, json.Unmarshal(raw, &in))
	fault := func(at string) error {
		if at == in.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	_, err = publishImage(context.Background(), in.Path, in.Config, in.Image, fault)
	t.Fatal("crash boundary missed", err)
}

func TestRecoveryPublishSIGKILL(t *testing.T) {
	for _, point := range publicationInstallBoundaries {
		t.Run(point, func(t *testing.T) {
			path, c, image := publishFixture(t)
			raw, err := json.Marshal(publicationInstallCrash{path, point, c, image})
			must(t, err)
			input := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(input, raw, 0600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryPublishCrashHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "DEPLOY_PUBLICATION_INSTALL_CRASH="+input)
			out, err := process.LimitedOutput(cmd, 50*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected exit", cmd.ProcessState, string(out))
			}
			one, err := publishImage(context.Background(), path, c, image, nil)
			must(t, err)
			two, err := publishImage(context.Background(), path, c, image, nil)
			must(t, err)
			if one != two || !one.CanonicalInstalled || one.DeliveryEnabled || one.ExecutionEnabled {
				t.Fatal(one, two)
			}
		})
	}
}
