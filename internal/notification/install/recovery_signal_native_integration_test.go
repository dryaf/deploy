//go:build notificationintegration

package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

// Native processes and real inherited OS locks, but no user manager, account
// credential, Signal CLI or provider endpoint. The fake manager accepts only the
// fixed relay unit and can simulate a lost response after completing its stop.
func TestNativeSignalHandoffNativeExecutable(t *testing.T) {
	buildRoot := t.TempDir()
	deploy := filepath.Join(buildRoot, "deploy")
	build := exec.Command("go", "build", "-o", deploy, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	fixture := filepath.Join(buildRoot, "relay.go")
	const source = `package main
import("encoding/json"; "os"; "path/filepath"; "syscall"; "time")
func main() {
 if len(os.Args)==3 && os.Args[1]=="notification-worker" && os.Args[2]=="__signal-recovery-probe" { os.Stdout.WriteString("{\"version\":1}\n"); return }
 if len(os.Args)!=6 || os.Args[2]!="__signal-relay" { os.Exit(2) }
 var c struct { State string ` + "`json:\"state_dir\"`" + `; Interactive struct { Inbox string ` + "`json:\"inbox_dir\"`" + ` } ` + "`json:\"interactive\"`" + ` }
 b,e:=os.ReadFile(os.Args[5]); if e!=nil || json.Unmarshal(b,&c)!=nil { os.Exit(3) }
 f,e:=os.OpenFile(filepath.Join(c.State,"deploy-account.lock"),os.O_RDWR,0600); if e!=nil { os.Exit(4) }; defer f.Close()
 if syscall.Flock(int(f.Fd()),syscall.LOCK_EX|syscall.LOCK_NB)!=nil { os.Exit(5) }
 inbox,e:=os.OpenFile(filepath.Join(c.Interactive.Inbox,"inbox.lock"),os.O_RDWR,0600); if e!=nil { os.Exit(7) }; defer inbox.Close()
 if syscall.Flock(int(inbox.Fd()),syscall.LOCK_EX|syscall.LOCK_NB)!=nil { os.Exit(8) }
 if os.WriteFile(filepath.Join(c.State,"fixture-ready"),[]byte("ready"),0600)!=nil { os.Exit(6) }
 for { time.Sleep(time.Hour) }
}`
	if err := os.WriteFile(fixture, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(buildRoot, "relay")
	if out, err := process.LimitedOutput(exec.Command("go", "build", "-o", helper, fixture), time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	for _, lostStop := range []bool{false, true} {
		t.Run(strconv.FormatBool(lostStop), func(t *testing.T) {
			flags := signalRuntimeCLIArguments(t)
			c, err := ReadRuntime(flags[2])
			if err != nil {
				t.Fatal(err)
			}
			s, rc := nativeSignalSpecFixture(t)
			base := filepath.Dir(s.Executable)
			rc.StateDir, rc.Interactive.InboxDir, rc.Socket = filepath.Join(base, "account-state"), flags[8], c.Signal.Socket
			if err = os.Mkdir(rc.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			lease, err := signalaccount.AcquireRuntime(rc.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			lease.Close()
			write := func(path string, b []byte, mode os.FileMode) string {
				t.Helper()
				if e := os.WriteFile(path, b, mode); e != nil {
					t.Fatal(e)
				}
				return managedDigest(b)
			}
			b, _ := json.Marshal(rc)
			s.RelaySHA256 = write(s.RelayConfig, b, 0600)
			b, err = os.ReadFile(helper)
			if err != nil {
				t.Fatal(err)
			}
			s.ExecutableSHA256 = write(s.Executable, b, 0700)
			b, err = os.ReadFile(flags[2])
			if err != nil {
				t.Fatal(err)
			}
			op, _ := safefs.NewID("n")
			spec := NativeSignalRecoverySpec{Version: 1, Operation: op, GatewayConfig: flags[2], GatewaySHA256: managedDigest(b), GatewayState: flags[4], CommitHash: flags[6], AccountState: rc.StateDir, RelaySource: flags[8], RelayDestination: flags[10], Service: s}
			b, _ = json.Marshal(spec)
			specPath := filepath.Join(base, "handoff.json")
			write(specPath, b, 0600)
			proc := exec.Command(s.Executable, s.argv()[1:]...)
			if err = proc.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { _ = proc.Wait(); close(done) }()
			t.Cleanup(func() { _ = proc.Process.Kill(); <-done })
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, e := os.Stat(filepath.Join(rc.StateDir, "fixture-ready")); e == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("relay did not acquire fixture account")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err = checkNativeSignalProcess(uint32(proc.Process.Pid), s); err != nil {
				t.Fatal(err)
			}
			wrong := s
			wrong.RelayConfig += ".different"
			if err = checkNativeSignalProcess(uint32(proc.Process.Pid), wrong); err == nil {
				t.Fatal("wrong process configuration accepted")
			}
			// Identical bytes at a replacement executable path do not prove the
			// running process uses that inode. Restore the original afterwards.
			retired := s.Executable + ".retired"
			if err = os.Rename(s.Executable, retired); err != nil {
				t.Fatal(err)
			}
			replacement, err := os.ReadFile(retired)
			if err != nil {
				t.Fatal(err)
			}
			write(s.Executable, replacement, 0700)
			if err = checkNativeSignalProcess(uint32(proc.Process.Pid), s); err == nil {
				t.Fatal("replacement executable inode accepted")
			}
			if err = os.Remove(s.Executable); err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(retired, s.Executable); err != nil {
				t.Fatal(err)
			}
			// Same config argv but a different on-disk account must fail too.
			other := t.TempDir()
			if err = os.Chmod(other, 0700); err != nil {
				t.Fatal(err)
			}
			otherLease, err := signalaccount.AcquireRuntime(other)
			if err != nil {
				t.Fatal(err)
			}
			otherLease.Close()
			wrongRC := rc
			wrongRC.StateDir = other
			b, _ = json.Marshal(wrongRC)
			write(s.RelayConfig, b, 0600)
			if err = checkNativeSignalProcess(uint32(proc.Process.Pid), s); err == nil {
				t.Fatal("different running account accepted")
			}
			b, _ = json.Marshal(rc)
			write(s.RelayConfig, b, 0600)
			wrongInbox := *rc.Interactive
			wrongInbox.InboxDir = other
			wrongRC = rc
			wrongRC.Interactive = &wrongInbox
			write(filepath.Join(other, "inbox.lock"), nil, 0600)
			b, _ = json.Marshal(wrongRC)
			write(s.RelayConfig, b, 0600)
			if err = checkNativeSignalProcess(uint32(proc.Process.Pid), s); err == nil {
				t.Fatal("different running inbox accepted")
			}
			b, _ = json.Marshal(rc)
			write(s.RelayConfig, b, 0600)
			manager := filepath.Join(base, "manager")
			if err = os.Mkdir(manager, 0700); err != nil {
				t.Fatal(err)
			}
			active := nativeSignalManagerProperties(s, true)
			active["MainPID"] = strconv.Itoa(proc.Process.Pid)
			write(filepath.Join(manager, "active"), nativeSignalManagerOutput(active), 0600)
			write(filepath.Join(manager, "inactive"), nativeSignalManagerOutput(nativeSignalManagerProperties(s, false)), 0600)
			write(filepath.Join(manager, "exec.json"), nativeSignalExecOutput(s), 0600)
			write(filepath.Join(manager, "pid"), []byte(strconv.Itoa(proc.Process.Pid)), 0600)
			// Derive the private fixture directory from the script path. This is
			// deliberately not a general systemctl implementation.
			write(filepath.Join(manager, "systemctl"), []byte(`#!/bin/sh
set -eu
d=${0%/*}
[ "$1" = --user ] && [ "$2" = --no-pager ] && [ "$4" = deploy-notification-signal.service ] || exit 90
case "$3" in
show)
 [ "$#" = 6 ] && [ "$5" = --all ] || exit 91
 case "$6" in --property=*) ;; *) exit 92;; esac
 if [ -f "$d/stopped" ]; then cat "$d/inactive"; else cat "$d/active"; fi;;
stop)
 [ "$#" = 4 ] || exit 93
 pid=$(cat "$d/pid")
 kill -TERM "$pid"
 n=0
 while kill -0 "$pid" 2>/dev/null; do n=$((n+1)); [ "$n" -lt 500 ] || exit 94; sleep 0.01; done
 echo stop >> "$d/stops"
 touch "$d/stopped"
 if [ -f "$d/lose-stop" ]; then rm "$d/lose-stop"; echo private-manager-error >&2; exit 95; fi;;
*) exit 96;;
esac
`), 0700)
			write(filepath.Join(manager, "busctl"), []byte(`#!/bin/sh
set -eu
[ "$#" = 7 ] && [ "$1" = --user ] && [ "$2" = --json=short ] && [ "$3" = get-property ] && [ "$4" = org.freedesktop.systemd1 ] && [ "$5" = /org/freedesktop/systemd1/unit/deploy_2dnotification_2dsignal_2eservice ] && [ "$6" = org.freedesktop.systemd1.Service ] && [ "$7" = ExecStartEx ] || exit 97
cat "${0%/*}/exec.json"
`), 0700)
			if lostStop {
				write(filepath.Join(manager, "lose-stop"), []byte("1"), 0600)
			}
			t.Setenv("PATH", manager+":"+os.Getenv("PATH"))
			run := func() ([]byte, error) {
				return process.LimitedOutput(exec.Command(deploy, "notification-worker", "recovery-signal-handoff", "--local", "--spec", specPath), time.Minute, 32768)
			}
			one, err := run()
			if lostStop {
				if err == nil || bytes.Contains(one, []byte("private-manager-error")) {
					t.Fatal("lost stop response", err, string(one))
				}
				status, e := signalaccount.InspectRecovery(rc.StateDir)
				if e != nil || status.Phase != "fenced" || status.RuntimeAllowed {
					t.Fatal(status, e)
				}
				one, err = run()
			}
			if err != nil {
				t.Fatal(err, string(one))
			}
			var result struct {
				Installation relay.SignalRuntimeInstallation `json:"installation"`
				Account      signalaccount.RecoveryStatus    `json:"account"`
			}
			if err = json.Unmarshal(one, &result); err != nil || result.Account.Phase != "complete" || result.Account.RuntimeAllowed || result.Installation.IntakeEnabled {
				t.Fatal(result, err, string(one))
			}
			if _, err = relay.VerifyInstalledSignalRuntime(context.Background(), spec.RelayDestination, result.Installation.InstallationHash); err != nil {
				t.Fatal(err)
			}
			two, err := run()
			if err != nil || !bytes.Equal(one, two) {
				t.Fatal("resume changed result", err, string(one), string(two))
			}
			b, err = os.ReadFile(filepath.Join(manager, "stops"))
			if err != nil || string(b) != "stop\n" {
				t.Fatal("service stopped more than once", err, string(b))
			}
			if l, e := signalaccount.AcquireRuntime(rc.StateDir); e == nil {
				l.Close()
				t.Fatal("account fence released")
			}
			if err = os.WriteFile(rc.RecipientFile, []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			if out, e := run(); e == nil || strings.Contains(string(out), "private-account-fixture") {
				t.Fatal("changed credential accepted or exposed", e, string(out))
			}
		})
	}
}
