package signalaccount

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/testutil"
)

func nativeRuntimeFixture(t *testing.T) (string, NativeServiceBinding, linker) {
	t.Helper()
	path, id, l, _ := fixture(t)
	l.native = &NativeCLI{Path: filepath.Join(t.TempDir(), "signal-cli"), SHA256: strings.Repeat("a", 64), Version: "0.14.9"}
	status, err := l.link(context.Background(), path, "device", id, io.Discard)
	testutil.Must(t, err)
	b := NativeServiceBinding{Version: 1, ServiceRoot: filepath.Join(t.TempDir(), "service"), ServiceHash: strings.Repeat("b", 64), Account: "account-fixture", LinkOperation: id, LinkHash: status.RecordHash, CLISHA256: l.native.SHA256, CLIVersion: l.native.Version}
	return path, b, l
}

func TestNativeRuntimeBindingInterruptionAndSingleOwner(t *testing.T) {
	for _, point := range []string{"intent", "receipt", "complete"} {
		t.Run(point, func(t *testing.T) {
			path, b, linker := nativeRuntimeFixture(t)
			err := bindNativeService(path, b, func(at string) error {
				if at == point {
					return errors.New("interruption")
				}
				return nil
			})
			if err == nil {
				t.Fatal("missing interruption")
			}
			requireRuntime(t, path, false)
			if _, err = linker.link(context.Background(), path, "device", b.LinkOperation, io.Discard); err == nil {
				t.Fatal("native owner allowed standalone linking")
			}
			testutil.Must(t, BindNativeService(path, b))
			testutil.Must(t, BindNativeService(path, b))
			testutil.Must(t, InspectNativeService(path, b))
			allowed := true
			r, err := OpenNativeRuntime(path, b, func() error {
				if !allowed {
					return errors.New("admission lost")
				}
				return nil
			})
			testutil.Must(t, err)
			defer r.Close()
			testutil.Must(t, InspectNativeService(path, b))
			if _, err = OpenNativeRuntime(path, b, func() error { return nil }); err == nil {
				t.Fatal("two account owners")
			}
			if _, err = r.FileFor(path + "-other"); err == nil {
				t.Fatal("retargeted leased account")
			}
			if _, err = r.FileFor(path); err != nil {
				t.Fatal(err)
			}
			allowed = false
			if _, err = r.FileFor(path); err == nil {
				t.Fatal("lost admission could execute CLI")
			}
			r.Close()
			if err = r.Check(); err == nil {
				t.Fatal("closed runtime still valid")
			}
		})
	}
}

func TestNativeRuntimeLostAuthorityAndChangedBinding(t *testing.T) {
	for _, name := range []string{nativeOwnerFile, nativeOwnerComplete, nativeOwnerLock, "deploy-account.lock", "deploy-account-control.lock", "deploy-service-link.lock"} {
		t.Run(name, func(t *testing.T) {
			path, b, _ := nativeRuntimeFixture(t)
			testutil.Must(t, BindNativeService(path, b))
			r, err := OpenNativeRuntime(path, b, func() error { return nil })
			testutil.Must(t, err)
			testutil.Must(t, os.Rename(filepath.Join(path, name), filepath.Join(path, name+".saved")))
			if err = r.Check(); err == nil {
				t.Fatal("live runtime ignored lost authority")
			}
			r.Close()
			if _, err = OpenNativeRuntime(path, b, func() error { return nil }); err == nil {
				t.Fatal("startup recreated missing authority")
			}
			if _, err = os.Lstat(filepath.Join(path, name)); !os.IsNotExist(err) {
				t.Fatal("missing authority recreated")
			}
			requireRuntime(t, path, false)
			if err = BindNativeService(path, b); err == nil {
				t.Fatal("binding recreated lost completed authority")
			}
			if _, err = os.Lstat(filepath.Join(path, name)); !os.IsNotExist(err) {
				t.Fatal("legacy runtime or binding recreated authority")
			}
		})
	}
	path, b, _ := nativeRuntimeFixture(t)
	testutil.Must(t, BindNativeService(path, b))
	changed := b
	changed.ServiceHash = strings.Repeat("c", 64)
	if err := BindNativeService(path, changed); err == nil {
		t.Fatal("native owner rebound without lifecycle")
	}
	if _, err := OpenNativeRuntime(path, changed, func() error { return nil }); err == nil {
		t.Fatal("changed service acquired native runtime")
	}
	if _, err := OpenNativeRuntime(path, b, nil); err == nil {
		t.Fatal("unadmitted native runtime")
	}
}

func TestNativeRuntimeRequiresActualNativeLinkAndPreservesRecoveryFence(t *testing.T) {
	path, b, _ := nativeRuntimeFixture(t)
	changed := b
	changed.LinkHash = strings.Repeat("0", 64)
	if err := BindNativeService(path, changed); err == nil {
		t.Fatal("accepted different link evidence")
	}
	d, err := accountDirectory(path)
	testutil.Must(t, err)
	present, err := nativeServicePresent(d)
	d.Close()
	testutil.Must(t, err)
	if present {
		t.Fatal("bad link evidence wrote ownership intent")
	}
	plain, id, l, _ := fixture(t)
	status, err := l.link(context.Background(), plain, "device", id, io.Discard)
	testutil.Must(t, err)
	b.LinkOperation, b.LinkHash = id, status.RecordHash
	if err = BindNativeService(plain, b); err == nil {
		t.Fatal("legacy link treated as pinned native link")
	}
	// Existing recovered accounts retain their original stronger admission path.
	recovered, _ := completedVerificationFixture(t)
	if err = BindNativeService(recovered, b); err == nil {
		t.Fatal("fresh binding bypassed recovery")
	}
}
