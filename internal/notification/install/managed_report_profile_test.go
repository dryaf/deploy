package install

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestManagedReportProfileReadOnlyAndPinned(t *testing.T) {
	spec, i, c, m, _ := managedCommandsFixture(t)
	s, err := openManagedStore(m, c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := c.Worker.Producers[0]
	profile, err := InspectManagedReportProfile(spec.Root, m.Identity, p.Producer, p.Routes[0])
	if err != nil {
		t.Fatal(err)
	}
	if profile.ManifestHash != protocol.Hash(m) || profile.Identity != m.Identity || profile.UnitDirectory != m.UnitDirectory || !slices.Contains(profile.Credentials, c.Telegram.TokenFile) {
		t.Fatal("profile lost installation pins or exclusions", profile)
	}
	wrong := m.Identity
	wrong.Recovery = "wrong"
	if _, err = InspectManagedReportProfile(spec.Root, wrong, p.Producer, p.Routes[0]); err == nil {
		t.Fatal("wrong recovered identity accepted")
	}
	if _, err = InspectManagedReportProfile(spec.Root, m.Identity, p.Producer, "unknown"); err == nil {
		t.Fatal("unenrolled route accepted")
	}
	// The live worker lease did not prevent inspection. Installation mutations do.
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = InspectManagedReportProfile(spec.Root, m.Identity, p.Producer, p.Routes[0]); err == nil {
		t.Fatal("unfinished operation was not excluded")
	}
	lock.Close()
	if err = i.services.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(m.UnitDirectory, managedUnit), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = InspectManagedReportProfile(spec.Root, m.Identity, p.Producer, p.Routes[0]); err == nil {
		t.Fatal("foreign unit accepted")
	}
}
