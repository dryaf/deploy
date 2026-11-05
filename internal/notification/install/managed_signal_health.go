package install

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

func checkManagedSignalHealth(ctx context.Context, m ManagedManifest) error {
	return checkManagedSignalHealthPID(ctx, m, 0)
}
func checkManagedSignalHealthPID(ctx context.Context, m ManagedManifest, pid uint32) error {
	if m.SignalVerification == nil {
		return nil
	}
	p, err := inspectSignalVerificationPublication(m.SignalVerification.SignalVerificationReference)
	if err != nil {
		return err
	}
	next, err := p.gateway()
	if err != nil {
		return err
	}
	c, err := decodeRuntime(next)
	if err != nil {
		return err
	}
	want := SignalVerificationHealth{ServiceHash: m.SignalVerification.ServiceHash, Version: 1, VerificationOnly: true, ManifestHash: p.ManifestHash, InstallationHash: p.Installation.InstallationHash, RecoveryOperation: p.Spec.Recovery.Operation, CommitHash: p.Spec.CommitHash, Signal: *c.Signal}
	socket := filepath.Join(p.Spec.Root, "api.sock")
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		info, err := os.Lstat(socket)
		if err != nil {
			return nil, err
		}
		st := info.Sys().(*syscall.Stat_t)
		if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Getuid()) {
			return nil, fmt.Errorf("unsafe Signal verification socket")
		}
		c, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, err
		}
		raw, err := c.(*net.UnixConn).SyscallConn()
		var cred *unix.Ucred
		var peerErr error
		if err == nil {
			err = raw.Control(func(fd uintptr) { cred, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
		}
		current, e := os.Lstat(socket)
		if err != nil || peerErr != nil || cred == nil || cred.Uid != uint32(os.Getuid()) || pid != 0 && cred.Pid != int32(pid) || e != nil || !os.SameFile(info, current) {
			c.Close()
			return nil, fmt.Errorf("Signal verification socket peer changed")
		}
		return c, nil
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("Signal verification redirect refused") }}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/health", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("Signal verification endpoint unavailable")
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 16385))
		response.Body.Close()
		if response.StatusCode == http.StatusServiceUnavailable && readErr == nil {
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				continue
			}
		}
		if readErr != nil || response.StatusCode != http.StatusOK {
			return fmt.Errorf("Signal verification health failed")
		}
		var actual SignalVerificationHealth
		if safefs.DecodeJSON(raw, &actual, 16384) != nil || actual != want {
			return fmt.Errorf("Signal verification health pins changed")
		}
		return nil
	}
}

func serveManagedSignalRuntime(ctx context.Context, d *safefs.Directory, s *worker.Store, c Runtime, m ManagedManifest, installation *safefs.Lock) error {
	if m.SignalVerification == nil {
		return serveRuntime(ctx, s, c, m.Paths.Socket)
	}
	check := func(ctx context.Context) error {
		if err := checkManagedSignalProfilesRuntime(d, m, installation); err != nil {
			return err
		}
		if _, err := readManagedSignalBinding(d, m); err != nil {
			return err
		}
		return checkManagedSignalHealth(ctx, m)
	}
	if err := check(ctx); err != nil {
		return err
	}
	if err := s.BindSignalVerification(m.SignalVerification.ManifestHash); err != nil {
		return err
	}
	observe, err := s.MonitorRuntimeLoop("signal-verification")
	if err != nil {
		return err
	}
	observe(nil)
	return runRuntimeGroup(ctx, []runtimeService{
		func(ctx context.Context) error { return serveRuntime(ctx, s, c, m.Paths.Socket) },
		func(ctx context.Context) error {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			defer observe(fmt.Errorf("Signal verification stopped"))
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
				}
				err := check(ctx)
				observe(err)
				if err != nil {
					return err
				}
			}
		},
	})
}
