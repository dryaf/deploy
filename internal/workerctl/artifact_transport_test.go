package workerctl

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
)

// Exercise the real bounded SSH bootstrap script and stdin ordering. A local
// ssh adapter runs its remote command; no remote account/provider is contacted.
func TestSSHBootstrapTransfersPinnedLEDAndSignalArtifacts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux native helpers")
	}
	dir := t.TempDir()
	source := `package main
import("crypto/sha256";"encoding/binary";"encoding/json";"fmt";"io";"os")
func main(){
 var h [4]byte;if _,e:=io.ReadFull(os.Stdin,h[:]);e!=nil{panic(e)}
 n:=binary.BigEndian.Uint32(h[:]);if n==0||n>1<<20{panic("frame size")}
 raw:=make([]byte,n);if _,e:=io.ReadFull(os.Stdin,raw);e!=nil{panic(e)}
 var r map[string]any;if e:=json.Unmarshal(raw,&r);e!=nil{panic(e)}
 for env,field:=range map[string]string{"DEPLOY_CONTROL_SIGNAL_ARTIFACT":"signal_sha256","DEPLOY_CONTROL_LED_ARTIFACT":"led_sha256"}{
  path:=os.Getenv(env);want,_:=r[field].(string)
  if want==""{if path!=""{panic("inherited artifact environment")};continue}
  body,e:=os.ReadFile(path);if e!=nil||fmt.Sprintf("%x",sha256.Sum256(body))!=want{panic("artifact transfer changed")}
  info,e:=os.Stat(path);if e!=nil||info.Mode().Perm()!=0700{panic("unsafe transfer permissions")}
 }
 code:="ok";if r["action"]=="plan"{code="policy_changed"};if r["server"]=="invalid"{code="operation_failed"}
 out,_:=json.Marshal(map[string]any{"version":1,"state":"configured","code":code,"next":"verified transferred artifacts"})
 binary.BigEndian.PutUint32(h[:],uint32(len(out)));os.Stdout.Write(h[:]);os.Stdout.Write(out)
}`
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	helperFile := filepath.Join(dir, "helper")
	cmd := exec.Command("go", "build", "-o", helperFile, file)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := process.LimitedOutput(cmd, 30*time.Second, 4096); err != nil {
		t.Fatal(err, string(out))
	}
	raw, err := os.ReadFile(helperFile)
	if err != nil {
		t.Fatal(err)
	}
	helper := Artifact{helperFile, inputDigest(raw)}
	shim := []byte("#!/bin/sh\nfor argument do selected=$argument; done\nexec /bin/sh -c \"$selected\"\n")
	if err = os.WriteFile(filepath.Join(dir, "ssh"), shim, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_CONTROL_SIGNAL_ARTIFACT", "/must-not-be-inherited")
	t.Setenv("DEPLOY_CONTROL_LED_ARTIFACT", "/must-not-be-inherited")
	a := ledArchiveFixture(t)
	native, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	nativeFile := filepath.Join(dir, "native")
	if err = os.WriteFile(nativeFile, native, 0700); err != nil {
		t.Fatal(err)
	}
	signal := Artifact{nativeFile, inputDigest(native)}
	server := config.ServerConfig{Host: "offline-fixture.invalid", User: "deploy"}
	for _, artifacts := range []bool{false, true} {
		r := ControlRequest{Version: 1, Worker: "notification-worker", Action: "apply", Facts: TargetFacts{Arch: runtime.GOARCH}, Desired: config.DesiredWorkers{Notification: &config.NotificationDeclaration{Providers: map[string]config.ProviderDeclaration{"signal": {}}, StatusLEDs: config.LEDDeclaration{Enabled: true, Daemon: config.LEDDaemon{Mode: "managed"}}}}}
		if artifacts {
			r.LocalLED = &a
			r.LocalSignal = &signal
		}
		result, err := (SSHController{}).Apply(context.Background(), server, helper, r)
		if err != nil || result.Code != "ok" || result.Next != "verified transferred artifacts" {
			t.Fatal("artifact/frame bootstrap failed", result, err)
		}
	}
	// Read-only plans may report changes/required actions without becoming a
	// transport error; actual canonical inspection failures must still fail.
	r := ControlRequest{Version: 1, Worker: "notification-worker", Action: "plan", Facts: TargetFacts{Arch: runtime.GOARCH}}
	if result, err := (SSHController{}).Apply(context.Background(), server, helper, r); err != nil || result.Code != "policy_changed" {
		t.Fatal("plan changes rejected by transport", result, err)
	}
	r.Server = "invalid"
	if _, err := (SSHController{}).Apply(context.Background(), server, helper, r); err == nil {
		t.Fatal("plan failure hidden by transport")
	}
}
