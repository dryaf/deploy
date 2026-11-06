//go:build notificationintegration

package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/provider"
)

// Only manager calls and the native Signal CLI are simulated. The installed
// deploy process, descriptor supervisor, recovery leases, socket peer identity,
// health and independent service authority are exercised together.
const nativeSignalCLIStub = `package main
import("fmt";"os";"slices")
func main(){
 if _,e:=os.NewFile(3,"lease").Stat();e!=nil{os.Exit(7)}
 if slices.Contains(os.Args,"--version"){fmt.Println("signal-cli 0.14.9");return}
 if slices.Contains(os.Args,"listAccounts"){
  account,e:=os.ReadFile(os.Args[2]+"/test-account");if e!=nil{os.Exit(8)}
  fmt.Printf("[{\"number\":%q,\"aci\":%q}]",string(account),string(account));return
 }
 if slices.Contains(os.Args,"receive"){b,e:=os.ReadFile(os.Args[2]+"/test-envelope");if e==nil{os.Remove(os.Args[2]+"/test-envelope");os.Stdout.Write(b)};return}
 if slices.Contains(os.Args,"send"){os.WriteFile(os.Args[2]+"/test-sent",[]byte("sent"),0600);fmt.Print("{\"timestamp\":987654321}");return}
 os.Exit(9)
}`

func TestNativeSignalProductionService(t *testing.T) {
	s, _, _ := productionSignalFixture(t)
	s.BinarySHA256 = managedDigest(buildSignalCommandFixture(t, "./cmd/deploy", s.BinaryFile))
	cli := filepath.Join(t.TempDir(), "signal-cli")
	source := filepath.Join(t.TempDir(), "signal.go")

	if err := os.WriteFile(source, []byte(nativeSignalCLIStub), 0600); err != nil {
		t.Fatal(err)
	}
	s.Runtime.CLIFile = cli
	s.Runtime.CLISHA256 = managedDigest(buildSignalCommandFixture(t, source, cli))
	p, err := inspectSignalVerificationPublication(s.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.gateway()
	if err != nil {
		t.Fatal(err)
	}
	c, err := decodeRuntime(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(p.Spec.AccountState, "test-account"), []byte(c.Signal.Account), 0600); err != nil {
		t.Fatal(err)
	}
	service := &executableSignalService{fakeSignalServiceManager: &fakeSignalServiceManager{units: s.UnitDirectory}, t: t}
	t.Cleanup(func() {
		if service.cmd != nil {
			_ = service.Stop(context.Background())
		}
	})
	i := signalServiceInstaller{manager: service}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	m, err := i.setup(ctx, s)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(s.Root, "test-runtime.log"))
		t.Fatal(err, string(log))
	}
	result := (provider.ConversationAdapter{Signal: c.Signal}).SendReply(ctx, c.Worker.Interaction.Destinations[0], "synthetic production reply")
	if !result.Accepted || result.NativeID != "987654321" {
		t.Fatal(result)
	}
	if _, err = i.resume(ctx, s.Root); err != nil {
		t.Fatal("live resume", err)
	}
	if err = service.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = i.resume(ctx, s.Root); err != nil {
		t.Fatal("restart", err)
	}
	if service.starts != 2 {
		t.Fatal("missing restart", service.starts)
	}
	// An explicit service hash binds production health, while the gateway's
	// verification mode still grants no command or delivery authority.
	ref := ManagedManifest{SignalVerification: &ManagedSignalBinding{SignalVerificationReference: SignalVerificationReference{Root: s.Endpoint.Root, ManifestHash: s.Endpoint.ManifestHash, ServiceRoot: s.Root, ServiceHash: m.ManifestHash}}}
	if err = checkManagedSignalHealth(ctx, ref); err != nil {
		t.Fatal("pinned production health", err)
	}
	if _, err = i.uninstall(ctx, s.Root); err != nil {
		t.Fatal(err)
	}
	if _, err = i.resume(ctx, s.Root); err != nil {
		t.Fatal(err)
	}
	if service.starts != 2 {
		t.Fatal("revoked service restarted")
	}
}
