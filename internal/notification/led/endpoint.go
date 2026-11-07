package led

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Endpoint is an existing daemon's declared local trust boundary. Socket inode
// and PID are deliberately not retained: systemd may replace them on restart.
// Each connection rechecks the path ownership and the kernel-provided peer UID.
type Endpoint struct {
	Version  int    `json:"version"`
	Socket   string `json:"socket"`
	OwnerUID int    `json:"owner_uid"`
	PeerUID  int    `json:"peer_uid"`
}

func (p Endpoint) Validate() error {
	if p.Version != 1 || !filepath.IsAbs(p.Socket) || filepath.Clean(p.Socket) != p.Socket || len(p.Socket) > 100 || p.OwnerUID < 0 || p.PeerUID < 0 {
		return fmt.Errorf("invalid LED daemon endpoint")
	}
	return nil
}

func endpointOwner(socket string) (int, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || len(socket) > 100 {
		return 0, fmt.Errorf("invalid LED socket path")
	}
	for path := filepath.Dir(socket); ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil {
			return 0, fmt.Errorf("LED socket parent unavailable")
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || (st.Uid != 0 && st.Uid != uint32(os.Getuid())) || info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return 0, fmt.Errorf("LED socket parent is not trusted")
		}
		if path == "/" {
			break
		}
	}
	info, err := os.Lstat(socket)
	if err != nil {
		return 0, fmt.Errorf("LED socket unavailable")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0002 != 0 {
		return 0, fmt.Errorf("LED socket has unsafe type or permissions")
	}
	return int(st.Uid), nil
}

func endpointPeer(conn net.Conn) (int, error) {
	u, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("LED endpoint requires a Unix peer")
	}
	raw, err := u.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var peerErr error
	if err = raw.Control(func(fd uintptr) { cred, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return 0, err
	}
	if peerErr != nil || cred == nil || cred.Pid <= 0 {
		return 0, fmt.Errorf("LED peer identity unavailable")
	}
	return int(cred.Uid), nil
}

func (p Endpoint) dial(ctx context.Context) (net.Conn, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	owner, err := endpointOwner(p.Socket)
	if err != nil || owner != p.OwnerUID {
		return nil, fmt.Errorf("LED socket ownership changed")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", p.Socket)
	if err != nil {
		return nil, err
	}
	peer, err := endpointPeer(conn)
	after, afterErr := endpointOwner(p.Socket)
	if err != nil || peer != p.PeerUID || afterErr != nil || after != owner {
		conn.Close()
		return nil, fmt.Errorf("LED daemon peer changed")
	}
	return conn, nil
}

// InspectEndpoint performs only protocol and ownership reads. It never changes
// registry state, daemon configuration, permissions, or hardware settings.
func InspectEndpoint(ctx context.Context, socket string) (Endpoint, DaemonStatus, error) {
	var p Endpoint
	var status DaemonStatus
	owner, err := endpointOwner(socket)
	if err != nil {
		return p, status, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return p, status, fmt.Errorf("LED daemon unavailable")
	}
	peer, err := endpointPeer(conn)
	conn.Close()
	if err != nil {
		return p, status, err
	}
	p = Endpoint{Version: 1, Socket: socket, OwnerUID: owner, PeerUID: peer}
	c := Client{Socket: socket, Budget: time.Second, Endpoint: &p}
	if err = c.Ping(ctx); err != nil {
		return p, status, err
	}
	status, err = c.Status(ctx)
	if err == nil {
		_, err = c.List(ctx)
	}
	return p, status, err
}
