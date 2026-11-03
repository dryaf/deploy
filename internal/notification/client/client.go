// Package client implements bounded, one-request Unix connections. Identity
// adoption belongs to enrollment/recovery, never to a reconnecting client.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type Client struct {
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
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return protocol.Identity{}, fmt.Errorf("notification service unavailable")
	}
	defer conn.Close()
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
