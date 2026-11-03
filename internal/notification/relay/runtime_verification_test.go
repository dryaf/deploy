package relay

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestSignalRuntimeVerificationLeasesAndReceipt(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "restored"}[restored], func(t *testing.T) {
			ctx := context.Background()
			src, dst, cursor := runtimeInstallFixture(t, restored)
			p, err := InstallSignalRuntime(ctx, src, dst, cursor)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dst, "inbox.db"))
			if err != nil {
				t.Fatal(err)
			}
			v, err := OpenSignalRuntimeVerification(ctx, dst, p.InstallationHash)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			for _, path := range []string{src, dst} {
				d, e := safefs.Open(path)
				if e != nil {
					t.Fatal(e)
				}
				l, e := d.Lock("inbox.lock", true, false)
				d.Close()
				if e == nil {
					l.Close()
					t.Fatal("verification did not retain lease", path)
				}
			}
			copy := v.Installation()
			copy.Cursor.Handoff.Target.Position++
			if err = v.Check(ctx); err != nil {
				t.Fatal("caller changed proof", err)
			}
			root := t.TempDir()
			receipt := SignalVerificationEndpoint{Version: 1, Root: root, Device: 1, Inode: 2, ManifestHash: protocol.Hash("endpoint")}
			if err = v.BindVerificationEndpoint(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			if err = v.BindVerificationEndpoint(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			got, ok, err := v.VerificationEndpoint()
			if err != nil || !ok || got != receipt {
				t.Fatal(got, ok, err)
			}
			receipt.ManifestHash = protocol.Hash("another")
			if err = v.BindVerificationEndpoint(ctx, receipt); err == nil {
				t.Fatal("endpoint rebound")
			}
			after, err := os.ReadFile(filepath.Join(dst, "inbox.db"))
			if err != nil || string(before) != string(after) {
				t.Fatal("verification changed inbox", err)
			}
			v.Close()
			if err = v.Check(ctx); err == nil {
				t.Fatal("closed verification accepted")
			}
			v, err = OpenSignalRuntimeVerification(ctx, dst, p.InstallationHash)
			if err != nil {
				t.Fatal(err)
			}
			v.Close()
		})
	}
}
func TestSignalRuntimeVerificationDetectsReplacement(t *testing.T) {
	for _, name := range []string{"inbox.db", runtimeInstallImage, "inbox.lock", runtimeInstallComplete} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			src, dst, p := runtimeInstallFixture(t, false)
			installed, err := InstallSignalRuntime(ctx, src, dst, p)
			if err != nil {
				t.Fatal(err)
			}
			v, err := OpenSignalRuntimeVerification(ctx, dst, installed.InstallationHash)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			path := filepath.Join(dst, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if name != runtimeInstallComplete {
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err = v.Check(ctx); err == nil {
				t.Fatal("replaced or missing evidence accepted")
			}
		})
	}
}

func TestSignalRuntimeVerificationDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	src, dst, cursor := runtimeInstallFixture(t, false)
	p, err := InstallSignalRuntime(ctx, src, dst, cursor)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	for _, path := range []string{src, dst} {
		if _, err = unix.InotifyAddWatch(fd, path, unix.IN_CREATE|unix.IN_MODIFY|unix.IN_DELETE|unix.IN_MOVED_FROM|unix.IN_MOVED_TO); err != nil {
			t.Fatal(err)
		}
	}
	v, err := OpenSignalRuntimeVerification(ctx, dst, p.InstallationHash)
	if err != nil {
		t.Fatal(err)
	}
	if err = v.Check(ctx); err != nil {
		v.Close()
		t.Fatal(err)
	}
	v.Close()
	buf := make([]byte, 4096)
	n, err := unix.Read(fd, buf)
	if n > 0 || !errors.Is(err, unix.EAGAIN) {
		t.Fatal("verification changed inbox files or sidecars", n, err)
	}
	// Uncheckpointed sidecars are never ignored or deleted by this endpoint.
	sidecar := filepath.Join(dst, "inbox.db-wal")
	if err = os.WriteFile(sidecar, []byte("uncheckpointed"), 0600); err != nil {
		t.Fatal(err)
	}
	if other, e := OpenSignalRuntimeVerification(ctx, dst, p.InstallationHash); e == nil {
		other.Close()
		t.Fatal("sidecar accepted")
	}
	if raw, e := os.ReadFile(sidecar); e != nil || string(raw) != "uncheckpointed" {
		t.Fatal("sidecar repaired", e)
	}
}
