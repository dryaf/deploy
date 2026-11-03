package relay

import (
	"os"
	"os/exec"
	"testing"

	"github.com/dryaf/deploy/internal/safefs"
)

func TestAccountLeaseSurvivesParentReference(t *testing.T) {
	d, err := safefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	lease, err := d.Lock("deploy-account.lock", true, true)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.ExtraFiles = []*os.File{lease.File()}
	if err = cmd.Start(); err != nil {
		lease.Close()
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	lease.Close()
	if contender, err := d.Lock("deploy-account.lock", true, true); err == nil {
		contender.Close()
		t.Fatal("surviving child lost account lease")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	contender, err := d.Lock("deploy-account.lock", true, true)
	if err != nil {
		t.Fatal(err)
	}
	contender.Close()
}
