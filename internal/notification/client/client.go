// Package client implements bounded, one-request Unix connections. Identity
// adoption belongs to enrollment/recovery, never to a reconnecting client.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"golang.org/x/sys/unix"
)

type Client struct {
	PeerPID  uint32 // Optional managed readiness pin; zero preserves ordinary clients.
	Socket   string
	Identity protocol.Identity
}

func (c Client) Call(ctx context.Context, r protocol.Request, out any) (protocol.Identity, error) {
	if r.Expected != (protocol.Identity{}) && r.Expected != c.Identity {
		return protocol.Identity{}, fmt.Errorf("request and client notification identities disagree")
	}
	r.Version = protocol.Version
	r.Expected = c.Identity
	b, err := json.Marshal(r)
	if err != nil {
		return protocol.Identity{}, err
	}
	if len(b)+1 > protocol.MaxRequest {
		return protocol.Identity{}, fmt.Errorf("request too large")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var socketInfo os.FileInfo
	if c.PeerPID != 0 {
		info, err := os.Lstat(c.Socket)
		if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
			return protocol.Identity{}, fmt.Errorf("managed notification socket is unsafe")
		}
		socketInfo = info
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return protocol.Identity{}, fmt.Errorf("notification service unavailable")
	}
	defer conn.Close()
	if c.PeerPID != 0 {
		raw, err := conn.(*net.UnixConn).SyscallConn()
		var cred *unix.Ucred
		var peerErr error
		if err == nil {
			err = raw.Control(func(fd uintptr) { cred, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
		}
		current, e := os.Lstat(c.Socket)
		if err != nil || peerErr != nil || cred == nil || cred.Uid != uint32(os.Getuid()) || cred.Pid <= 0 || uint32(cred.Pid) != c.PeerPID || e != nil || !os.SameFile(socketInfo, current) {
			return protocol.Identity{}, fmt.Errorf("managed notification socket peer changed")
		}
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err = conn.Write(append(b, '\n')); err != nil {
		return protocol.Identity{}, fmt.Errorf("notification request outcome unknown")
	}
	reader := bufio.NewReaderSize(conn, protocol.MaxResponse)
	line, err := reader.ReadSlice('\n')
	if err != nil || reader.Buffered() != 0 {
		return protocol.Identity{}, fmt.Errorf("notification response incomplete")
	}
	var reply protocol.Reply
	if err = protocol.Decode(line, &reply, protocol.MaxResponse); err != nil {
		return protocol.Identity{}, err
	}
	if reply.Version != protocol.Version || !reply.Identity.Valid() {
		return protocol.Identity{}, fmt.Errorf("invalid server identity")
	}
	if r.Operation != "system.ping" && reply.Identity != c.Identity {
		return reply.Identity, fmt.Errorf("notification identity changed; reconcile before retry")
	}
	if !reply.OK {
		return reply.Identity, fmt.Errorf("notification request rejected: %s", reply.Error)
	}
	if out != nil {
		err = protocol.Decode(reply.Result, out, protocol.MaxResponse)
	}
	return reply.Identity, err
}
