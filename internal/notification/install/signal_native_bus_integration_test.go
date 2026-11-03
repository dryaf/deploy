//go:build notificationintegration

package install

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

// Check the real busctl JSON representation of the typed nested D-Bus value.
// dbus-run-session supplies a private bus; no request reaches the user's manager.
func TestNativeSignalExecPrivateBus(t *testing.T) {
	for _, tool := range []string{"dbus-run-session", "busctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	if err := exec.Command("/usr/bin/python3", "-c", "import dbus, dbus.service, gi.repository.GLib").Run(); err != nil {
		t.Skip("Python D-Bus/GLib fixture dependencies unavailable")
	}
	s, _ := nativeSignalSpecFixture(t)
	dir := t.TempDir()
	config := filepath.Join(dir, "spec.json")
	b, _ := json.Marshal(s)
	if err := os.WriteFile(config, b, 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "service.py")
	const fixture = `import json, subprocess, sys, threading
import dbus, dbus.service
from dbus.mainloop.glib import DBusGMainLoop
from gi.repository import GLib
DBusGMainLoop(set_as_default=True)
with open(sys.argv[1]) as f: spec=json.load(f)
flags=[] if sys.argv[2]=='plain' else ['ignore-failure']
class Service(dbus.service.Object):
 @dbus.service.method('org.freedesktop.DBus.Properties', in_signature='ss', out_signature='v')
 def Get(self, interface, prop):
  if interface!='org.freedesktop.systemd1.Service' or prop!='ExecStartEx': raise ValueError('unexpected property')
  argv=[spec['executable'],'notification-worker','__signal-relay','--local','--config',spec['relay_config']]
  row=dbus.Struct((spec['executable'],dbus.Array(argv,signature='s'),dbus.Array(flags,signature='s'),dbus.UInt64(0),dbus.UInt64(0),dbus.UInt64(0),dbus.UInt64(0),dbus.UInt32(0),dbus.Int32(0),dbus.Int32(0)),signature='sasasttttuii')
  return dbus.Array([row],signature='(sasasttttuii)',variant_level=1)
bus=dbus.SessionBus()
name=dbus.service.BusName('org.freedesktop.systemd1',bus)
obj=Service(bus,'/org/freedesktop/systemd1/unit/deploy_2dnotification_2dsignal_2eservice')
loop=GLib.MainLoop()
result=[]
def request():
 try:
  p=subprocess.run(['busctl','--user','--json=short','get-property','org.freedesktop.systemd1','/org/freedesktop/systemd1/unit/deploy_2dnotification_2dsignal_2eservice','org.freedesktop.systemd1.Service','ExecStartEx'],capture_output=True,timeout=10)
  result.append(p)
 finally: GLib.idle_add(loop.quit)
thread=threading.Thread(target=request)
thread.start()
loop.run()
thread.join()
if not result or result[0].returncode: sys.exit(1)
sys.stdout.buffer.write(result[0].stdout)
`
	if err := os.WriteFile(script, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"plain", "flagged"} {
		b, err := process.LimitedOutput(exec.Command("dbus-run-session", "--", "/usr/bin/python3", script, config, mode), 20*time.Second, 16384)
		if err != nil {
			t.Fatal(err)
		}
		err = parseNativeSignalExec(b, s)
		if mode == "plain" && err != nil {
			t.Fatal(err, string(b))
		}
		if mode == "flagged" && err == nil {
			t.Fatal("real D-Bus execution flags ignored", string(b))
		}
	}
}
