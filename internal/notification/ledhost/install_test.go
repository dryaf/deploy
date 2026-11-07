package ledhost

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/ledartifact"
)

type fakeServices struct {
	active          bool
	starts, reloads int
	foreign         bool
}

func (f *fakeServices) Inspect(_ context.Context, _ Spec, owned bool) error {
	if f.foreign || !owned && f.active {
		return errors.New("foreign system service")
	}
	return nil
}
func (f *fakeServices) Reload(context.Context) error { f.reloads++; return nil }
func (f *fakeServices) Start(context.Context, Spec) error {
	if !f.active {
		f.starts++
	}
	f.active = true
	return nil
}
func (f *fakeServices) Verify(context.Context, Spec) error {
	if !f.active {
		return errors.New("service inactive")
	}
	return nil
}

func fixture(t *testing.T) (manager, Spec, []byte, *fakeServices) {
	t.Helper()
	launcher, err := os.ReadFile("../ledartifact/launcher.py")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"__main__.py": launcher, "LICENSE": []byte("fixture"), "pistatus/__init__.py": []byte("fixture"), "pistatus/daemon.py": []byte("fixture"), "pistatus/sysfs.py": []byte("fixture")}
	files["pistatus/deploy_removal.py"], err = os.ReadFile("../ledartifact/removal_guard.py")
	if err != nil {
		t.Fatal(err)
	}
	meta := ledartifact.Metadata{Version: 1, DaemonVersion: "0.1.0", SourceCommit: ledartifact.SourceCommit, RestoreGuard: 1, RemovalGuard: 1, Files: map[string]string{}}
	for name, body := range files {
		meta.Files[name] = bytesHash(body)
	}
	files["deploy-led-artifact.json"], err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	z := zip.NewWriter(&buffer)
	for name, body := range files {
		f, e := z.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(body); e != nil {
			t.Fatal(e)
		}
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := buffer.Bytes()
	f := &fakeServices{}
	m := manager{prefix: t.TempDir(), owner: os.Getuid(), services: f, validateHost: func(context.Context, Spec) error { return nil }, verifyDaemon: func(context.Context, Spec) error { return nil }}
	m.prepareRemoval = func(_ context.Context, s Spec, hash string) error { return fakeRemovalFence(m, s, hash) }
	if err = os.Chmod(m.prefix, 0700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/var/lib", "/etc/systemd/system", "/run"} {
		if err = os.MkdirAll(m.path(p), 0755); err != nil {
			t.Fatal(err)
		}
	}
	s := Spec{Version: 1, UID: 1000, Device: "ACT", DevicePath: "/sys/devices/platform/leds/leds/ACT", ArtifactSHA256: bytesHash(artifact)}
	return m, s, artifact, f
}

func TestHostInstallationResumesEveryBoundaryAndNoops(t *testing.T) {
	for _, boundary := range []string{"prepared", "published:config.toml", "published:deploy-led-host-1000.service", "published:deploy-led-host-1000.socket", "files", "started", "receipt", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			m, s, artifact, f := fixture(t)
			p, err := Build(s)
			if err != nil {
				t.Fatal(err)
			}
			m.boundary = func(at string) error {
				if at == boundary {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err = m.install(context.Background(), s, p.Hash, artifact); err == nil {
				t.Fatal("did not interrupt")
			}
			m.boundary = nil
			r, err := m.install(context.Background(), s, p.Hash, artifact)
			if err != nil {
				t.Fatal(err)
			}
			if !r.Complete || r.Spec != s || f.starts != 1 {
				t.Fatalf("bad completion: %+v %+v", r, f)
			}
			reloads := f.reloads
			again, err := m.install(context.Background(), s, p.Hash, artifact)
			if err != nil || again != r || f.starts != 1 || f.reloads != reloads {
				t.Fatalf("retry caused effects: %+v %v", f, err)
			}
		})
	}
}

func TestHostInstallationRefusesDriftWithoutReplacingFiles(t *testing.T) {
	for _, kind := range []string{"approval", "artifact", "host", "foreign-unit", "foreign-file", "symlink", "missing-journal", "changed-unit", "missing-artifact", "missing-receipt", "inactive"} {
		t.Run(kind, func(t *testing.T) {
			m, s, artifact, f := fixture(t)
			p, _ := Build(s)
			approved := p.Hash
			unit := m.path("/etc/systemd/system/" + Unit(s.UID) + ".service")
			switch kind {
			case "approval":
				approved = strings.Repeat("0", 64)
			case "artifact":
				artifact = append(artifact, 1)
			case "host":
				m.validateHost = func(context.Context, Spec) error { return errors.New("device moved") }
			case "foreign-unit":
				f.foreign = true
			case "foreign-file":
				if err := os.WriteFile(unit, []byte("administrator file"), 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("/does-not-exist", unit); err != nil {
					t.Fatal(err)
				}
			default:
				if _, err := m.install(context.Background(), s, approved, artifact); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "missing-journal":
					if err := os.Remove(m.path(Root(s.UID) + "/journal.json")); err != nil {
						t.Fatal(err)
					}
				case "changed-unit":
					if err := os.WriteFile(unit, []byte("administrator edit"), 0644); err != nil {
						t.Fatal(err)
					}
				case "missing-artifact":
					if err := os.Remove(m.path(Root(s.UID) + "/daemon.pyz")); err != nil {
						t.Fatal(err)
					}
				case "missing-receipt":
					if err := os.Remove(m.path(Root(s.UID) + "/receipt.json")); err != nil {
						t.Fatal(err)
					}
				case "inactive":
					f.active = false
				}
			}
			starts := f.starts
			if _, err := m.install(context.Background(), s, approved, artifact); err == nil {
				t.Fatal("unsafe installation accepted")
			}
			if f.starts != starts {
				t.Fatal("started despite failed preflight")
			}
			if kind == "foreign-file" || kind == "changed-unit" {
				b, err := os.ReadFile(unit)
				if err != nil || !strings.HasPrefix(string(b), "administrator") {
					t.Fatal("foreign file overwritten")
				}
			}
			if kind == "approval" || kind == "artifact" || kind == "host" {
				if _, err := os.Stat(m.path("/var/lib/deploy-led-host")); !os.IsNotExist(err) {
					t.Fatal("mutated before approval/artifact/host checks")
				}
			}
		})
	}
}

func TestPlanDeviceSelectionAndRestrictedGeneratedFiles(t *testing.T) {
	for _, test := range []struct {
		devices        []string
		selected, want string
	}{{[]string{"ACT", "PWR"}, "", "ACT"}, {[]string{"green:act"}, "", "green:act"}, {[]string{"led0"}, "led0", "led0"}, {[]string{"led0"}, "", ""}, {[]string{"ACT", "green:act"}, "", ""}, {[]string{"PWR"}, "PWR", ""}} {
		got, err := SelectDevice(test.devices, test.selected)
		if got != test.want || (err == nil) != (test.want != "") {
			t.Fatalf("selection %+v: %q %v", test, got, err)
		}
	}
	_, s, _, _ := fixture(t)
	p, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.Socket != Socket(s.UID) || len(p.Files) != 3 || p.Hash == "" {
		t.Fatal(p)
	}
	for path, body := range p.Files {
		if strings.HasSuffix(path, ".service") && !strings.Contains(body, "ReadWritePaths="+Root(s.UID)+"/state "+s.DevicePath+"\n") {
			t.Fatal("unbounded writable sysfs")
		}
		if strings.HasSuffix(path, "config.toml") && strings.Contains(body, "solid") {
			t.Fatal("positive default")
		}
	}
	for _, device := range []string{"ACT\nExecStart=evil", "../ACT", "PWR"} {
		bad := s
		bad.Device = device
		if _, err = Build(bad); err == nil {
			t.Fatal("unsafe device accepted")
		}
	}
	bad := s
	bad.DevicePath = "/sys/devices/../class/ACT"
	if _, err = Build(bad); err == nil {
		t.Fatal("traversal accepted")
	}
	bad = s
	bad.UID = 0
	if _, err = Build(bad); err == nil {
		t.Fatal("root client accepted")
	}
	if filepath.Clean(Root(s.UID)) != Root(s.UID) {
		t.Fatal("unclean path")
	}
}
