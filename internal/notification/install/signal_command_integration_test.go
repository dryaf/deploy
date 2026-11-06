//go:build notificationintegration

package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
)

func buildSignalCommandFixture(t *testing.T, source, target string) []byte {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", target, source)
	cmd.Dir = "../../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	// Go may return a cache hardlink; install an independent private executable.
	if err = os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(target, raw, 0700); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNativeSignalCommandSupervisorJoinsDescendants(t *testing.T) {
	root := t.TempDir()
	helperPath, cliPath := filepath.Join(root, "deploy"), filepath.Join(root, "signal-cli")
	buildSignalCommandFixture(t, "./cmd/deploy", helperPath)
	source := filepath.Join(root, "fixture.go")
	const program = `package main
import("os";"os/exec";"os/signal";"path/filepath";"strconv";"syscall";"time")
func main(){
 state, mode:=os.Args[2],os.Args[3]
 f:=os.NewFile(3,"lease"); if _,e:=f.Stat();e!=nil{os.Exit(12)}
 if mode=="ok"{os.Stdout.WriteString("pinned-output");return}
 if mode=="fail"{os.Exit(4)}
 if mode=="child"{
  signal.Ignore(syscall.SIGTERM)
  os.WriteFile(filepath.Join(state,"pid"),[]byte(strconv.Itoa(os.Getpid())),0600)
  for{time.Sleep(time.Second)}
 }
 cmd:=exec.Command("/proc/self/fd/4","--config",state,"child")
 cmd.ExtraFiles=[]*os.File{f,os.NewFile(4,"cli")}
 cmd.SysProcAttr=&syscall.SysProcAttr{Setsid:true}
 if e:=cmd.Start();e!=nil{os.Exit(13)}
 for{if _,e:=os.Stat(filepath.Join(state,"pid"));e==nil{break};time.Sleep(time.Millisecond)}
 if mode=="orphan"{return}
 for{time.Sleep(time.Second)}
}`
	if err := os.WriteFile(source, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	raw := buildSignalCommandFixture(t, source, cliPath)
	helper, err := os.Open(helperPath)
	if err != nil {
		t.Fatal(err)
	}
	defer helper.Close()
	cli, err := os.Open(cliPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	for _, mode := range []string{"ok", "fail", "orphan", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			state := t.TempDir()
			if err := os.Chmod(state, 0700); err != nil {
				t.Fatal(err)
			}
			lease, err := signalaccount.AcquireRuntime(state)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			request := relay.CommandRequest{Version: 1, CLIHash: managedDigest(raw), State: state, Args: []string{"--config", state, mode}}
			var output strings.Builder
			done := make(chan error, 1)
			go func() { done <- relay.RunPinnedCommand(ctx, helper, cli, lease.File(), request, &output) }()
			var pid int
			if mode == "orphan" || mode == "cancel" {
				for deadline := time.Now().Add(5 * time.Second); ; {
					b, err := os.ReadFile(filepath.Join(state, "pid"))
					if err == nil {
						pid, err = strconv.Atoi(string(b))
						if err != nil {
							t.Fatal(err)
						}
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("child did not start", err)
					}
					time.Sleep(5 * time.Millisecond)
				}
				if mode == "cancel" {
					cancel()
				}
			}
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("supervisor did not join")
			}
			if (err == nil) != (mode == "ok") {
				t.Fatal(mode, err)
			}
			if mode == "ok" && output.String() != "pinned-output" {
				t.Fatal(output.String())
			}
			if pid > 0 && syscall.Kill(pid, 0) != syscall.ESRCH {
				t.Fatalf("descendant %d still exists after return", pid)
			}
			lease.Close()
			next, err := signalaccount.AcquireRuntime(state)
			if err != nil {
				t.Fatal("lease retained after joined exit", err)
			}
			next.Close()
		})
	}
	request := relay.CommandRequest{Version: 1, CLIHash: fmt.Sprintf("%064d", 0), State: root, Args: []string{"--config", root, "ok"}}
	if err = relay.RunPinnedCommand(context.Background(), helper, cli, helper, request, io.Discard); err == nil {
		t.Fatal("changed CLI pin accepted")
	}
}
