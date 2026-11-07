package ledartifact

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

func archiveFixture(t *testing.T, change func(map[string][]byte, *Metadata), duplicate bool) []byte {
	t.Helper()
	files := map[string][]byte{"__main__.py": launcher, "pistatus/deploy_removal.py": removalGuard, "LICENSE": []byte("fixture"), "pistatus/__init__.py": []byte("__version__ = '0.1.0'\n"), "pistatus/daemon.py": []byte("fixture"), "pistatus/sysfs.py": []byte("fixture")}
	metadata := Metadata{Version: 1, DaemonVersion: "0.1.0", SourceCommit: SourceCommit, RestoreGuard: 1, RemovalGuard: 1, Files: map[string]string{}}
	for name, body := range files {
		metadata.Files[name] = hash(body)
	}
	if change != nil {
		change(files, &metadata)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	files["deploy-led-artifact.json"] = raw
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for name, body := range files {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = entry.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if duplicate {
		entry, err := w.Create("pistatus/daemon.py")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = entry.Write([]byte("duplicate"))
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestArtifactInspectionRejectsChangedAndUnsafeArchives(t *testing.T) {
	valid := archiveFixture(t, nil, false)
	if _, err := Inspect(valid); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"traversal", "duplicate", "launcher", "missing", "untracked", "changed", "version"} {
		t.Run(kind, func(t *testing.T) {
			raw := archiveFixture(t, func(files map[string][]byte, m *Metadata) {
				switch kind {
				case "traversal":
					files["pistatus/../../escape.py"] = []byte("escape")
				case "launcher":
					files["__main__.py"] = []byte("print('changed')")
					m.Files["__main__.py"] = hash(files["__main__.py"])
				case "missing":
					delete(files, "pistatus/sysfs.py")
					delete(m.Files, "pistatus/sysfs.py")
				case "untracked":
					files["pistatus/extra.py"] = []byte("untracked")
				case "changed":
					files["pistatus/daemon.py"] = []byte("changed")
				case "version":
					m.DaemonVersion = "unreviewed"
				}
			}, kind == "duplicate")
			if _, err := Inspect(raw); err == nil {
				t.Fatal("unsafe artifact accepted")
			}
		})
	}
	file := filepath.Join(t.TempDir(), "daemon.pyz")
	if err := os.WriteFile(file, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(file, hash(valid)); err != nil {
		t.Fatal(err)
	}
	valid[len(valid)-1] ^= 1
	if err := os.WriteFile(file, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(file, hash([]byte("wrong"))); err == nil {
		t.Fatal("changed artifact digest accepted")
	}
}

func TestLauncherRestoresOnlyStillOwnedHardware(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python unavailable")
	}
	file := filepath.Join(t.TempDir(), "launcher.py")
	if err = os.WriteFile(file, launcher, 0600); err != nil {
		t.Fatal(err)
	}
	// Fake modules exercise the actual embedded launcher without importing a
	// daemon or opening hardware. Only ordinary temporary files represent sysfs.
	script := `import sys,types,pathlib,tempfile
class Driver:
 calls=0
 def restore_original(self): self.calls+=1
 def inspect(self): return types.SimpleNamespace(trigger=self.current)
daemon=types.ModuleType('pistatus.daemon');daemon.main=lambda:0
hardware=types.ModuleType('pistatus.sysfs');hardware.SysfsLEDDriver=Driver
sys.modules['pistatus.daemon']=daemon;sys.modules['pistatus.sysfs']=hardware
api=types.ModuleType('pistatus.api');api.Application=object
guard=types.ModuleType('pistatus.deploy_removal');guard.install=lambda cls:None
sys.modules['pistatus.api']=api;sys.modules['pistatus.deploy_removal']=guard
scope={}
try: exec(compile(pathlib.Path(sys.argv[1]).read_text(),sys.argv[1],'exec'),scope)
except SystemExit as e: assert e.code==0
with tempfile.TemporaryDirectory() as root:
 d=Driver();d.path=pathlib.Path(root);d.original=('mmc0',1);d._trigger='none';d.current='none';d._brightness=0
 (d.path/'brightness').write_text('0')
 d.restore_original();assert d.calls==1
 for changed in ('trigger','brightness','timer'):
  d.current='none';d._trigger='none';d._brightness=0;(d.path/'brightness').write_text('0')
  if changed=='trigger': d.current='mmc0'
  if changed=='brightness': (d.path/'brightness').write_text('1')
  if changed=='timer':
   d._trigger=d.current='timer';d._timer=(100,100)
   (d.path/'delay_on').write_text('200');(d.path/'delay_off').write_text('100')
  try: d.restore_original()
  except OSError: pass
  else: raise AssertionError('foreign hardware change overwritten: '+changed)
  assert d.calls==1
 d._trigger=d.current='timer';d._timer=(200,100);d.restore_original();assert d.calls==2
`
	cmd := exec.Command(python, "-I", "-c", script, file)
	if out, err := process.LimitedOutput(cmd, 5*time.Second, 4096); err != nil {
		t.Fatal(err, string(out))
	}
}
