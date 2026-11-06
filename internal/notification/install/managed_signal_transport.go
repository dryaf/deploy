package install

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type admittedSignalTransport struct {
	check func(context.Context) error
	fail  func(error)
	next  http.RoundTripper
}

func (t admittedSignalTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	err := t.check(r.Context())
	if err == nil && (r.Method != http.MethodPost || r.URL.Scheme != "http" || r.URL.Host != "relay" || r.URL.RawQuery != "" || (r.URL.Path != "/reply" && r.URL.Path != "/inbound/list" && r.URL.Path != "/inbound/ack")) {
		err = fmt.Errorf("Signal request is outside admitted endpoint")
	}
	if err != nil {
		t.fail(err)
		return nil, err
	}
	return t.next.RoundTrip(r)
}

func managedSignalTransport(m ManagedManifest, check func(context.Context) error, fail func(error)) (http.RoundTripper, func(), error) {
	if m.SignalVerification == nil {
		return nil, func() {}, fmt.Errorf("Signal service binding required")
	}
	service, err := inspectSignalService(m.SignalVerification.SignalVerificationReference)
	if err != nil {
		return nil, func() {}, err
	}
	if service.Spec.Runtime == nil {
		return nil, func() {}, fmt.Errorf("Signal transport requires production service")
	}
	socket := filepath.Join(service.Spec.Endpoint.Root, "api.sock")
	t := &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 9 * time.Second, MaxResponseHeaderBytes: 4096, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		info, err := os.Lstat(socket)
		if err != nil {
			return nil, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Getuid()) {
			return nil, fmt.Errorf("unsafe admitted Signal socket")
		}
		conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, err
		}
		raw, err := conn.(*net.UnixConn).SyscallConn()
		var credential *unix.Ucred
		var peerErr error
		if err == nil {
			err = raw.Control(func(fd uintptr) {
				credential, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
			})
		}
		current, e := os.Lstat(socket)
		if err != nil || peerErr != nil || credential == nil || credential.Uid != uint32(os.Getuid()) || credential.Pid <= 0 || e != nil || !os.SameFile(info, current) {
			conn.Close()
			return nil, fmt.Errorf("admitted Signal peer changed")
		}
		if err = checkSignalServiceProcess(uint32(credential.Pid), service); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}}
	return admittedSignalTransport{check: check, fail: fail, next: t}, t.CloseIdleConnections, nil
}
