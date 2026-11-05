//go:build notificationintegration

package install

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

func TestActivationReviewNativeExecutable(t *testing.T) {
	spec, i, _, report := qualificationFixture(t)
	f := i.services.(*fakeManagedServices)
	if err := f.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	// Only the manager's read-only show command is simulated. Any attempt to
	// mutate it or call another command fails the actual CLI subprocess.
	bin := t.TempDir()
	manager := fmt.Sprintf(`#!/bin/sh
if [ "$#" -ne 5 ] || [ "$1" != --user ] || [ "$2" != --no-pager ] || [ "$3" != show ] || [ "$4" != deploy-notification-worker.service ] || [ "$5" != --property=LoadState,ActiveState,FragmentPath,DropInPaths,UnitFileState,Names ]; then exit 71; fi
cat <<'PROPERTIES'
LoadState=loaded
ActiveState=inactive
FragmentPath=%s
DropInPaths=
UnitFileState=enabled
Names=deploy-notification-worker.service
PROPERTIES
`, filepath.Join(spec.UnitDirectory, managedUnit))
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(manager), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	before := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
	command := exec.Command(binary, "notification-worker", "activation-review", "--local", "--root", spec.Root)
	data, err := process.LimitedOutput(command, 30*time.Second, 64<<10)
	if err != nil {
		t.Fatal(err, string(data))
	}
	var review ActivationReview
	if err := json.Unmarshal(data, &review); err != nil {
		t.Fatal(err, string(data))
	}
	activationCheck(t, review, "runtime", "drain", "satisfied")
	activationCheck(t, review, "runtime", "state", "satisfied")
	activationCheck(t, review, "installation", "activation", "satisfied")
	if review.Identity != report.Binding.Manifest.Identity || activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) != before {
		t.Fatal("native review changed installation or identity")
	}
}
