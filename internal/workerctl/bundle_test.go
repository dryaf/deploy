package workerctl

import (
	"errors"
	"github.com/dryaf/deploy/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bundleFixture(t *testing.T) Catalog {
	t.Helper()
	c := Catalog{Version: 1, Release: "fixture", Linux: map[string]ArchitectureArtifacts{}}
	archive := ledArchiveFixture(t)
	for _, arch := range []string{"amd64", "arm64"} {
		helper := elfFixture(t, arch)
		signal := elfFixture(t, arch)
		c.Linux[arch] = ArchitectureArtifacts{Helper: helper, Signal: &signal, LEDs: &archive, BackupImage: "registry.example.test/backup@sha256:" + strings.Repeat("a", 64), RelayImage: "registry.example.test/relay@sha256:" + strings.Repeat("b", 64)}
	}
	return c
}
func TestBundleCompletePublicationAndInterruptedReplacement(t *testing.T) {
	c := bundleFixture(t)
	dir := filepath.Join(t.TempDir(), "bundle")
	catalog, missing, err := AssembleBundle(dir, c, true)
	if err != nil || len(missing) != 0 {
		t.Fatal(missing, err)
	}
	old, err := os.ReadFile(filepath.Join(dir, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		a := catalog.Linux[arch]
		if !strings.Contains(a.Helper.File, a.Helper.SHA256) || filepath.IsAbs(a.Helper.File) {
			t.Fatal("artifact not content addressed")
		}
		resolved, e := ReadCatalog(filepath.Join(dir, "catalog.json"))
		if e != nil {
			t.Fatal(e)
		}
		if _, e = resolveArtifactsForBundleTest(dir, arch); e != nil {
			t.Fatal(e)
		}
		if e = VerifySignalArtifact(*resolved.Linux[arch].Signal, arch); e != nil {
			t.Fatal(e)
		}
	}
	// New artifacts are prepared without altering any file named by the old catalog.
	c.Release = "next"
	for _, arch := range []string{"amd64", "arm64"} {
		a := c.Linux[arch]
		body, e := os.ReadFile(a.Helper.File)
		if e != nil {
			t.Fatal(e)
		}
		body[120] = 7
		if e = os.WriteFile(a.Helper.File, body, 0700); e != nil {
			t.Fatal(e)
		}
		a.Helper.SHA256 = inputDigest(body)
		c.Linux[arch] = a
	}
	stop := errors.New("interrupted publication")
	if _, _, err = assembleBundle(dir, c, true, func(at string) error {
		if at == "amd64" {
			return stop
		}
		return nil
	}); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(filepath.Join(dir, "catalog.json"))
	if err != nil || string(unchanged) != string(old) {
		t.Fatal("interruption replaced catalog", err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if _, err = resolveArtifactsForBundleTest(dir, arch); err != nil {
			t.Fatal("previous release unusable", err)
		}
	}
	for range 2 {
		if _, _, err = AssembleBundle(dir, c, true); err != nil {
			t.Fatal("resume or repeat", err)
		}
	}
	current, err := ReadCatalog(filepath.Join(dir, "catalog.json"))
	if err != nil || current.Release != "next" {
		t.Fatal(current, err)
	}
}

func resolveArtifactsForBundleTest(dir, arch string) (ResolvedArtifacts, error) {
	return ResolveArtifacts(config.WorkerArtifacts{}, arch, filepath.Join(dir, "catalog.json"))
}

func TestBundleRejectsMissingIncompatibleOrChangedInputs(t *testing.T) {
	for _, damage := range []string{"missing-signal", "wrong-signal-arch", "floating-image", "malformed-image", "changed-source", "changed-published", "changed-during-copy", "catalog-symlink"} {
		t.Run(damage, func(t *testing.T) {
			c := bundleFixture(t)
			dir := filepath.Join(t.TempDir(), "bundle")
			published, _, err := AssembleBundle(dir, c, true)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "catalog.json")
			old, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			a := c.Linux["arm64"]
			var boundary func(string) error
			switch damage {
			case "missing-signal":
				a.Signal = nil
			case "wrong-signal-arch":
				a.Signal = c.Linux["amd64"].Signal
			case "floating-image":
				a.BackupImage = "registry.example.test/backup:latest"
			case "malformed-image":
				a.BackupImage = "invalid image@sha256:" + strings.Repeat("a", 64)
			case "changed-source":
				if err = os.WriteFile(a.Signal.File, []byte("changed"), 0700); err != nil {
					t.Fatal(err)
				}
			case "changed-published":
				if err = os.WriteFile(filepath.Join(dir, published.Linux["arm64"].Helper.File), []byte("changed"), 0755); err != nil {
					t.Fatal(err)
				}
			case "changed-during-copy":
				body, e := os.ReadFile(a.Helper.File)
				if e != nil {
					t.Fatal(e)
				}
				body[120] = 9
				if e = os.WriteFile(a.Helper.File, body, 0700); e != nil {
					t.Fatal(e)
				}
				a.Helper.SHA256 = inputDigest(body)
				boundary = func(string) error { return os.WriteFile(a.Helper.File, []byte("changed after verification"), 0700) }
			case "catalog-symlink":
				if err = os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(path+".saved", path); err != nil {
					t.Fatal(err)
				}
			}
			c.Linux["arm64"] = a
			if _, _, err = assembleBundle(dir, c, true, boundary); err == nil {
				t.Fatal("damaged bundle published")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(old) {
				t.Fatal("failed validation replaced catalog", err)
			}
		})
	}
}

func TestBundlePartialDevelopmentIsExplicit(t *testing.T) {
	c := bundleFixture(t)
	a := c.Linux["arm64"]
	a.Signal = nil
	a.BackupImage = ""
	c.Linux["arm64"] = a
	dir := filepath.Join(t.TempDir(), "bundle")
	if _, missing, err := AssembleBundle(dir, c, true); err == nil || len(missing) != 2 {
		t.Fatal(missing, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("incomplete release created output")
	}
	if _, missing, err := AssembleBundle(dir, c, false); err != nil || len(missing) != 2 {
		t.Fatal(missing, err)
	}
}
