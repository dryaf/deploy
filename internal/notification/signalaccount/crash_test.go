package signalaccount

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestSignalLinkCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_SIGNAL_LINK_CRASH_STATE")
	if path == "" {
		t.Skip("subprocess helper")
	}
	var input struct{ ID, Boundary string }
	raw, err := os.ReadFile(filepath.Join(path, "test-input.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	l := linker{now: time.Now, run: func(_ context.Context, lease *Lease, _ string, _ io.Writer) error {
		if input.Boundary == "surviving-child" {
			cmd := exec.Command("sleep", "30")
			cmd.ExtraFiles = []*os.File{lease.File()}
			if err := cmd.Start(); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(path, "test-child.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
				return err
			}
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}, fault: func(point string) error {
		if point == input.Boundary {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}}
	_, err = l.link(context.Background(), path, "device", input.ID, io.Discard)
	t.Fatal("SIGKILL boundary not reached", err)
}

func TestSignalLinkSIGKILL(t *testing.T) {
	for _, point := range []string{"link_intent", "link_head", "link_started", "link_child_finished", "link_result", "surviving-child"} {
		t.Run(point, func(t *testing.T) {
			path, id, l, calls := fixture(t)
			raw, _ := json.Marshal(struct{ ID, Boundary string }{id, point})
			if err := os.WriteFile(filepath.Join(path, "test-input.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run=^TestSignalLinkCrashHelper$", "-test.timeout=15s")
			cmd.Env = append(os.Environ(), "DEPLOY_SIGNAL_LINK_CRASH_STATE="+path)
			out, err := process.LimitedOutput(cmd, 20*time.Second, 8192)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("child did not crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong exit", cmd.ProcessState, string(out))
			}
			if point == "surviving-child" {
				raw, err = os.ReadFile(filepath.Join(path, "test-child.pid"))
				if err != nil {
					t.Fatal(err)
				}
				pid, e := strconv.Atoi(string(raw))
				if e != nil || pid <= 1 {
					t.Fatal("invalid test child", e)
				}
				t.Cleanup(func() {
					if pid > 1 {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				})
				if lease, e := acquire(path); e == nil {
					lease.Close()
					t.Fatal("parent death released child's lease")
				}
				if _, e = Resolve(path, id, "Inspected"); e == nil {
					t.Fatal("review bypassed surviving child")
				}
				if _, e = l.link(context.Background(), path, "device", id, io.Discard); e == nil || *calls != 0 {
					t.Fatal("second linker ran", e, *calls)
				}
				if e = syscall.Kill(pid, syscall.SIGKILL); e != nil {
					t.Fatal(e)
				}
				deadline := time.Now().Add(3 * time.Second)
				for {
					lease, e := acquire(path)
					if e == nil {
						lease.Close()
						pid = 0
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("child lease not released", e)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			view, err := Inspect(path)
			if err != nil {
				t.Fatal(err)
			}
			if view.RuntimeAllowed != (point == "link_result") {
				t.Fatal(view)
			}
			if point == "link_intent" || point == "link_head" {
				if _, err = l.link(context.Background(), path, "device", id, io.Discard); err != nil || *calls != 1 {
					t.Fatal(err, *calls)
				}
			} else if point == "link_result" {
				if _, err = l.link(context.Background(), path, "device", id, io.Discard); err != nil || *calls != 0 {
					t.Fatal("completed child replayed", err, *calls)
				}
			} else {
				if _, err = l.link(context.Background(), path, "device", id, io.Discard); err == nil || *calls != 0 {
					t.Fatal("uncertain child replayed", err, *calls)
				}
				if _, err = Resolve(path, id, "Inspected interrupted account state"); err != nil {
					t.Fatal(err)
				}
			}
			requireRuntime(t, path, true)
		})
	}
}

func TestLinkNativeChildInheritsDescriptor(t *testing.T) {
	path, id, l, _ := fixture(t)
	// The real runner resolves a synthetic executable in this private directory.
	// FD 3 must refer to the exact leased inode; the process may not reopen it.
	bin := t.TempDir()
	script := "#!/bin/sh\nset -eu\ntest -f /proc/self/fd/3\n/usr/bin/flock -n 3\nprintf 'synthetic-private-link-uri\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "signal-cli"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	l.run = runLink
	if _, err := l.link(context.Background(), path, "device", id, io.Discard); err != nil {
		t.Fatal(err)
	}
	d, err := safefs.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	h, err := readHistory(d)
	if err != nil || !h.runtimeAllowed() {
		t.Fatal(h, err)
	}
}
