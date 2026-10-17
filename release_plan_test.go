package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls  []string
	failAt int
}

func (r *fakeRunner) record(s string) error {
	r.calls = append(r.calls, s)
	if len(r.calls) == r.failAt {
		return errors.New("injected failure")
	}
	return nil
}
func (r *fakeRunner) SSH(_ Environment, s string) (string, error) { return "", r.record("ssh " + s) }
func (r *fakeRunner) Sync(_ Environment, s []string, d string, a ...string) error {
	return r.record("sync " + strings.Join(s, " ") + " " + d + " " + strings.Join(a, " "))
}
func (r *fakeRunner) Local(c *exec.Cmd) error { return r.record("local " + c.String()) }

func TestImagePlanAndRunner(t *testing.T) {
	n, err := normalizeAndValidateConfig(imageConfig(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	p, err := planImageRelease(n, "v1", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.ReleaseID, "v1-") {
		t.Fatal("release ID does not identify configuration version", p.ReleaseID)
	}
	r := &fakeRunner{}
	if err := executeImageRelease(p, r); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(r.calls, "\n")
	for _, bad := range []string{"go build", "podman build", "Dockerfile", "build/server", "/data"} {
		if strings.Contains(calls, bad) {
			t.Fatalf("unexpected %q in %s", bad, calls)
		}
	}
	if len(r.calls) != 3 {
		t.Fatal(r.calls)
	}
	var b bytes.Buffer
	p.Print(&b)
	if !strings.Contains(b.String(), "<resolved-image-id>") {
		t.Fatal(b.String())
	}
	for i := 1; i <= 3; i++ {
		r := &fakeRunner{failAt: i}
		if err := executeImageRelease(p, r); err == nil {
			t.Fatal("expected failure")
		}
		if len(r.calls) != i {
			t.Fatal(r.calls)
		}
	}
}
func TestLocalArtifactValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	c := imageConfig()
	c.Artifacts.Include = []string{"files"}
	n, err := normalizeAndValidateConfig(c, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planImageRelease(n, "v1", ""); err == nil {
		t.Fatal("missing artifact accepted")
	}
	if err := os.Mkdir("files", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", "files/link"); err != nil {
		t.Fatal(err)
	}
	if _, err := planImageRelease(n, "v1", ""); err == nil {
		t.Fatal("symlink accepted")
	}
}
func TestDryRunNoRunnerOrFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	body := `app_name: pdf
deployment: {mode: image}
environments:
  prod:
    host: example.com
    user: sal
    target_dir: /srv/pdf
    quadlet:
      service_name: pdf
      image: docker.io/library/nginx:1
`
	if err := os.WriteFile("deploy.yaml", []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	dryRun = true
	defer func() { dryRun = false }()
	r := &fakeRunner{failAt: 1}
	if err := release("v1", "prod", r); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(r.calls) != 0 {
		t.Fatal(entries, r.calls)
	}
}
func TestBinaryBuildOrder(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(displayDefault(map[bool]string{true: "custom"}[custom], "default"), func(t *testing.T) {
			t.Chdir(t.TempDir())
			c := imageConfig()
			c.Deployment.Mode = ModeBinary
			c.BinaryName = "server"
			if custom {
				c.Build.Cmd = "echo {{.Version}}"
			}
			c.Artifacts.Include = []string{"static"}
			n, err := normalizeAndValidateConfig(c, "prod")
			if err != nil {
				t.Fatal(err)
			}
			p, err := planBinaryRelease(n, "v1", "abc")
			if err != nil {
				t.Fatal(err)
			}
			r := &fakeRunner{}
			if err := executeBinaryRelease(p, r); err != nil {
				t.Fatal(err)
			}
			buildIndex, syncIndex, activationIndex := -1, -1, -1
			for i, s := range r.calls {
				if strings.HasPrefix(s, "local ") {
					buildIndex = i
				}
				if strings.HasPrefix(s, "sync build/server") {
					syncIndex = i
				}
				if strings.HasPrefix(s, "ssh bash ") && strings.Contains(s, "transaction.sh") {
					activationIndex = i
				}
			}
			if !(buildIndex >= 0 && syncIndex > buildIndex && activationIndex > syncIndex) {
				t.Fatal(r.calls)
			}
			if _, err := os.Stat(filepath.Join("build", "pdf.container")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLegacyBinaryFixtures(t *testing.T) {
	for _, file := range []string{"testdata/binary-default.yaml", "testdata/binary-custom.yaml"} {
		t.Run(file, func(t *testing.T) {
			cfg, err := readConfig(file)
			if err != nil {
				t.Fatal(err)
			}
			n, err := normalizeAndValidateConfig(cfg, "prod")
			if err != nil {
				t.Fatal(err)
			}
			p, err := planBinaryRelease(n, "v1.2.3", "abc")
			if err != nil {
				t.Fatal(err)
			}
			if n.Mode != ModeBinary || !n.Routing || n.Env.Quadlet.Dockerfile != "Dockerfile.vps" {
				t.Fatal(n)
			}
			for _, want := range []string{"Requires=traefik.service", "Restart=on-failure", "traefik.enable=true"} {
				if !strings.Contains(p.Quadlet, want) {
					t.Fatal(p.Quadlet)
				}
			}
			if strings.Contains(file, "default") && (!strings.Contains(p.Quadlet, "custom=preserved") || !strings.Contains(p.Quadlet, "CPUQuota=50%") || !strings.Contains(p.Quadlet, "MemoryMax=512M")) {
				t.Fatal(p.Quadlet)
			}
			if strings.Contains(file, "custom") && strings.Contains(p.Quadlet, "EnvironmentFile=") {
				t.Fatal(p.Quadlet)
			}
		})
	}
}
