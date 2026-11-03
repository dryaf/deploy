package deployment

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/quadlet"
)

func TestQuadletMetadataRestoredWithFailedRelease(t *testing.T) {
	for _, mode := range []config.DeploymentMode{config.ModeBinary, config.ModeImage} {
		t.Run(string(mode), func(t *testing.T) {
			f := newTransactionFixture(t)
			stage := func(id, environment string) string {
				var dir string
				if mode == config.ModeBinary {
					dir = stageBinaryFixture(f, id, map[string]string{"server": id}, false)
				} else {
					dir = f.stage(id, nil, false)
				}
				m := quadlet.QuadletMetadata{Schema: 1, ManagedBy: "deploy", Role: "application", App: "app", Environment: environment, Service: "pdf", Mode: mode, TargetDir: f.target, Backup: &quadlet.QuadletBackupMetadata{Enabled: false}}
				marker, err := quadlet.EncodeMetadata(m)
				if err != nil {
					t.Fatal(err)
				}
				unit := filepath.Join(dir, "candidate.container")
				f.write(unit, marker+f.read(unit), 0600)
				return dir
			}
			f.success(stage("A", "prod"))
			old := f.read(f.unit())
			f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
			f.mark("fail-health")
			if out, err := f.run("release", stage("B", "candidate")); err == nil {
				t.Fatal("expected failed release", out)
			}
			if got := f.read(f.unit()); got != old {
				t.Fatal("rollback failed to restore exact prior marker and unit")
			}
			m, err := quadlet.ParseMetadata(strings.NewReader(f.read(f.unit())))
			if err != nil || m.Environment != "prod" {
				t.Fatal("candidate metadata survived rollback", m, err)
			}
		})
	}
}
