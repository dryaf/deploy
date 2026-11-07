package signalaccount

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/dryaf/deploy/internal/testutil"
)

func nativeBootstrapParent(t *testing.T) string {
	t.Helper()
	p := t.TempDir()
	testutil.Must(t, os.Chmod(p, 0700))
	return p
}
func TestNativeAccountBootstrapResumesAndLinks(t *testing.T) {
	native := nativeLinkFixture(t)
	for _, at := range []string{"reserved", "staging", "ready", "published", "complete"} {
		t.Run(at, func(t *testing.T) {
			path := filepath.Join(nativeBootstrapParent(t), "account")
			op := testutil.ID(t, "n")
			first, err := bootstrapNativeAccount(context.Background(), path, op, func(phase string) error {
				if phase == at {
					return errors.New("interrupted")
				}
				return nil
			})
			if err == nil {
				t.Fatal("missing fault")
			}
			done, err := BootstrapNativeAccount(context.Background(), path, op)
			testutil.Must(t, err)
			if done.Operation != first.Operation || (at == "ready" || at == "published" || at == "complete") && first != done {
				t.Fatal("reservation changed")
			}
			testutil.Must(t, InspectNativeAccountReservation(done))
			link := testutil.ID(t, "n")
			linked, err := LinkNative(context.Background(), path, "fixture", link, native, io.Discard)
			testutil.Must(t, err)
			repeated, err := BootstrapNativeAccount(context.Background(), path, op)
			testutil.Must(t, err)
			if repeated != done {
				t.Fatal("linked directory replaced")
			}
			actual, err := Inspect(path)
			testutil.Must(t, err)
			if actual.RecordHash != linked.RecordHash {
				t.Fatal("bootstrap changed link evidence")
			}
		})
	}
}
func TestNativeAccountBootstrapRefusesForeignOrMissingAuthority(t *testing.T) {
	for _, damage := range []string{"foreign", "state", "marker", "lock", "journal", "inode"} {
		t.Run(damage, func(t *testing.T) {
			parent := nativeBootstrapParent(t)
			path := filepath.Join(parent, "account")
			op := testutil.ID(t, "n")
			if damage == "foreign" {
				testutil.Must(t, os.Mkdir(path, 0700))
			} else {
				_, err := BootstrapNativeAccount(context.Background(), path, op)
				testutil.Must(t, err)
				switch damage {
				case "state":
					testutil.Must(t, os.Rename(path, path+"-retained"))
				case "marker":
					testutil.Must(t, os.Remove(filepath.Join(path, "deploy-native-bootstrap.json")))
				case "lock":
					testutil.Must(t, os.Remove(filepath.Join(parent, nativeAccountBootstrapName(path)+".lock")))
				case "journal":
					testutil.Must(t, os.Remove(filepath.Join(parent, nativeAccountBootstrapName(path))))
				case "inode":
					marker, err := os.ReadFile(filepath.Join(path, "deploy-native-bootstrap.json"))
					testutil.Must(t, err)
					testutil.Must(t, os.Rename(path, path+"-retained"))
					testutil.Must(t, os.Mkdir(path, 0700))
					testutil.Must(t, os.WriteFile(filepath.Join(path, "deploy-native-bootstrap.json"), marker, 0600))
				}
			}
			if _, err := BootstrapNativeAccount(context.Background(), path, op); err == nil {
				t.Fatal("foreign/lost authority accepted")
			}
		})
	}
}
func TestNativeAccountBootstrapCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_NATIVE_ACCOUNT_BOOTSTRAP_PATH")
	if path == "" {
		t.Skip("subprocess only")
	}
	_, err := bootstrapNativeAccount(context.Background(), path, os.Getenv("DEPLOY_NATIVE_ACCOUNT_BOOTSTRAP_OPERATION"), func(at string) error {
		if at == os.Getenv("DEPLOY_NATIVE_ACCOUNT_BOOTSTRAP_BOUNDARY") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("helper survived", err)
}
func TestNativeAccountBootstrapSIGKILL(t *testing.T) {
	for _, at := range []string{"reserved", "staging", "ready", "published", "complete"} {
		t.Run(at, func(t *testing.T) {
			path := filepath.Join(nativeBootstrapParent(t), "account")
			op := testutil.ID(t, "n")
			cmd := exec.Command(os.Args[0], "-test.run=^TestNativeAccountBootstrapCrashHelper$", "-test.timeout=15s")
			cmd.Env = append(os.Environ(), "DEPLOY_NATIVE_ACCOUNT_BOOTSTRAP_PATH="+path, "DEPLOY_NATIVE_ACCOUNT_BOOTSTRAP_OPERATION="+op, "DEPLOY_NATIVE_ACCOUNT_BOOTSTRAP_BOUNDARY="+at)
			out, err := cmd.CombinedOutput()
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			st, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !st.Signaled() || st.Signal() != syscall.SIGKILL {
				t.Fatal("wrong helper exit", string(out))
			}
			r, err := BootstrapNativeAccount(context.Background(), path, op)
			testutil.Must(t, err)
			testutil.Must(t, InspectNativeAccountReservation(r))
		})
	}
}
