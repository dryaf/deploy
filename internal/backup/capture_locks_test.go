//go:build linux

package backup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/sqlite"
	"github.com/dryaf/deploy/internal/testutil"
)

var captureTestLockNames = []string{"export.lock", "service.lock", "target.lock"}

// Real process boundaries and the production SQLite command (prlimit/timeout/
// sqlite3) exercise FD ownership when either Go supervisor disappears.
func TestBackupCaptureLockChild(t *testing.T) {
	role := os.Getenv("DEPLOY_CAPTURE_TEST_ROLE")
	if role == "" {
		t.Skip("subprocess fixture")
	}
	root := os.Getenv("DEPLOY_CAPTURE_TEST_ROOT")
	if role == "snapshot" {
		testutil.Must(t, source.Capture(filepath.Join(root, "source.db"), filepath.Join(root, "snapshot.db"), 1<<20))
		return
	}
	if role == "capture" {
		locks, err := sqlite.InheritSourceCaptureLocks()
		testutil.Must(t, err)
		defer func() {
			for _, lock := range locks {
				lock.Close()
			}
		}()
		sqlite.ConfigureSourceCapture(locks)
		testutil.Must(t, os.WriteFile(filepath.Join(root, "capture-go.pid"), []byte(strconv.Itoa(os.Getpid())), 0600))
		cmd, err := sqlite.Command([]string{"-readonly", filepath.Join(root, "source.db"), "WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000000000) SELECT sum(x) FROM n;"}, 3*time.Second)
		testutil.Must(t, err)
		testutil.Must(t, cmd.Start())
		testutil.Must(t, os.WriteFile(filepath.Join(root, "native.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600))
		_ = cmd.Wait() // The native watchdog ends this deliberately long query.
		return
	}
	dir, err := safefs.Open(root)
	testutil.Must(t, err)
	defer dir.Close()
	var locks []*safefs.Lock
	defer func() {
		for _, lock := range locks {
			lock.Close()
		}
	}()
	for _, name := range captureTestLockNames {
		lock, err := dir.Lock(name, true, true)
		testutil.Must(t, err)
		locks = append(locks, lock)
	}
	args := []string{os.Args[0], "-test.run=^TestBackupCaptureLockChild$"}
	if os.Getenv("DEPLOY_CAPTURE_TEST_UNSHARE") == "1" {
		args = append([]string{"podman", "unshare"}, args...)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "DEPLOY_CAPTURE_TEST_ROLE=capture")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for _, lock := range locks {
		cmd.ExtraFiles = append(cmd.ExtraFiles, lock.File())
	}
	cmd.Stderr = os.Stderr
	testutil.Must(t, cmd.Start())
	testutil.Must(t, os.WriteFile(filepath.Join(root, "capture.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600))
	if role == "close" {
		for _, lock := range locks {
			lock.Close()
		}
	}
	_ = cmd.Wait()
}

func captureFixturePID(t *testing.T, root, name string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(root, name+".pid"))
		if err == nil {
			if pid, e := strconv.Atoi(strings.TrimSpace(string(raw))); e == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s subprocess did not become ready", name)
	return 0
}

func checkBackupCaptureLockLifetime(t *testing.T, unshare bool) {
	for _, mode := range []string{"supervisor-killed", "capture-killed", "parent-closes"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			testutil.CreateDatabase(t, filepath.Join(root, "source.db"), "source")
			role := "supervisor"
			if mode == "parent-closes" {
				role = "close"
			}
			supervisor := exec.Command(os.Args[0], "-test.run=^TestBackupCaptureLockChild$")
			supervisor.Env = append(os.Environ(), "DEPLOY_CAPTURE_TEST_ROOT="+root, "DEPLOY_CAPTURE_TEST_ROLE="+role, fmt.Sprintf("DEPLOY_CAPTURE_TEST_UNSHARE=%d", map[bool]int{false: 0, true: 1}[unshare]))
			supervisor.Stderr = os.Stderr
			testutil.Must(t, supervisor.Start())
			t.Cleanup(func() { supervisor.Process.Kill(); supervisor.Wait() })
			capturePID := captureFixturePID(t, root, "capture")
			t.Cleanup(func() { syscall.Kill(-capturePID, syscall.SIGKILL) })
			captureGoPID := captureFixturePID(t, root, "capture-go")
			nativePID := captureFixturePID(t, root, "native")
			t.Cleanup(func() { syscall.Kill(-nativePID, syscall.SIGKILL) })
			switch mode {
			case "supervisor-killed":
				testutil.Must(t, supervisor.Process.Kill())
				_ = supervisor.Wait()
			case "capture-killed":
				testutil.Must(t, syscall.Kill(captureGoPID, syscall.SIGKILL))
				_ = supervisor.Wait()
			}
			dir, err := safefs.Open(root)
			testutil.Must(t, err)
			defer dir.Close()
			for _, name := range captureTestLockNames {
				lock, err := dir.Lock(name, true, false)
				if err == nil {
					lock.Close()
					t.Fatalf("%s released while capture work was alive", name)
				}
			}
			// No Go watchdog survives the capture-killed case: the native timeout
			// must still stop SQLite, close all inherited FDs and permit deployment.
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				allFree := true
				for _, name := range captureTestLockNames {
					lock, err := dir.Lock(name, true, false)
					if err == nil {
						lock.Close()
					} else {
						allFree = false
					}
				}
				if allFree {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatal("native watchdog failed to release capture locks")
		})
	}
}

func TestBackupCaptureLocksSurviveSupervisorDeath(t *testing.T) {
	checkBackupCaptureLockLifetime(t, false)
}

func checkBackupSnapshotLockHandoff(t *testing.T, unshare bool) {
	t.Helper()
	root := t.TempDir()
	testutil.CreateDatabase(t, filepath.Join(root, "source.db"), "source")
	dir, err := safefs.Open(root)
	testutil.Must(t, err)
	defer dir.Close()
	args := []string{os.Args[0], "-test.run=^TestBackupCaptureLockChild$"}
	if unshare {
		args = append([]string{"podman", "unshare"}, args...)
	}
	// Match the production source capture's independent outer watchdog too.
	args = append([]string{"--signal=TERM", "--kill-after=2s", "15s"}, args...)
	cmd := exec.Command("timeout", args...)
	cmd.Env = append(os.Environ(), "DEPLOY_CAPTURE_TEST_ROLE=snapshot", "DEPLOY_CAPTURE_TEST_ROOT="+root)
	var locks []*safefs.Lock
	defer func() {
		for _, lock := range locks {
			lock.Close()
		}
	}()
	for _, name := range captureTestLockNames {
		lock, e := dir.Lock(name, true, true)
		testutil.Must(t, e)
		locks = append(locks, lock)
		cmd.ExtraFiles = append(cmd.ExtraFiles, lock.File())
	}
	out, err := process.Output(cmd, 20*time.Second)
	if err != nil {
		t.Fatalf("real source capture failed: %v: %s", err, out)
	}
	testutil.Must(t, sqlite.ValidateStandaloneSQLiteSnapshot(filepath.Join(root, "snapshot.db"), time.Second))
	// Parent remains alive, as it would during digesting/streaming the result.
	for _, lock := range locks {
		lock.Close()
	}
	for _, name := range captureTestLockNames {
		lock, e := dir.Lock(name, true, false)
		testutil.Must(t, e)
		lock.Close()
	}
}

func TestBackupCaptureReleasesLocksAfterSnapshot(t *testing.T) {
	checkBackupSnapshotLockHandoff(t, false)
}
