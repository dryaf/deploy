package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

func (s *Store) Handle(ctx context.Context, r protocol.Request) (out protocol.Reply) {
	out = protocol.Reply{Version: protocol.Version, Identity: s.identity}
	if r.Version != protocol.Version {
		out.Error = "unsupported_version"
		return
	}
	if r.Operation != "system.ping" && r.Expected != s.identity {
		out.Error = "identity_mismatch"
		return
	}
	var value any
	var err error
	if err = validateRequest(r); err != nil {
		out.Error = "invalid_request"
		return
	}
	switch r.Operation {
	case "system.ping":
		value = struct {
			Alive bool `json:"alive"`
		}{true}
	case "system.status":
		var h Health
		h, err = s.Health(ctx)
		if err == nil {
			h.LEDOperations, err = s.LEDOperations(ctx)
		}
		value = h
	case "led.recovery-review":
		value, err = s.ReviewRuntimeLEDRecovery(ctx)
	case "led.recovery-clear":
		value, err = s.ClearRuntimeLEDRecovery(ctx, r.EventID, r.PayloadHash)
	case "producer.ownership":
		value, err = s.OwnershipStatus(r.Producer, r.Generation)
	case "message.enqueue":
		value, err = s.Enqueue(ctx, *r.Message)
	case "message.get":
		value, err = s.Get(ctx, r.Producer, r.EventID)
	case "message.cancel":
		err = s.Cancel(ctx, r.Producer, r.EventID, r.Sequence, r.PayloadHash)
	case "status.replace":
		err = s.Replace(ctx, *r.Snapshot)
	case "fault-transition.enqueue":
		value, err = s.EnqueueTransitions(ctx, *r.Transitions)
	case "receipt.list":
		value, err = s.Receipts(ctx, *r.Cursor, r.Limit)
	case "receipt.ack":
		err = s.Ack(ctx, *r.Cursor)
	case "inspection.queue":
		value, err = s.InspectQueue(ctx, r.Producer, r.After, r.Limit)
	case "inspection.receipts":
		value, err = s.InspectReceipts(ctx, r.Producer, r.After, r.Limit)
	default:
		if strings.HasPrefix(r.Operation, "command.") {
			value, err = s.handleCommand(ctx, r)
		} else {
			out.Error = "unsupported_operation"
			return
		}
	}
	if err != nil {
		out.Error = "operation_rejected"
		return
	} // Never expose SQL, secret or filesystem diagnostics over the API.
	out.Result, err = json.Marshal(value)
	if err != nil {
		out.Error = "encoding_failed"
		return
	}
	out.OK = true
	return
}

func validateRequest(r protocol.Request) error {
	c := r
	c.Version = 0
	c.Operation = ""
	c.Expected = protocol.Identity{}
	switch r.Operation {
	case "system.ping", "system.status", "led.recovery-review":
	case "led.recovery-clear":
		if len(r.EventID) == 0 || len(r.EventID) > 256 || !safefs.DigestPattern.MatchString(r.PayloadHash) {
			return fmt.Errorf("exact inherited LED review required")
		}
		c.EventID, c.PayloadHash = "", ""
	case "inspection.queue", "inspection.receipts":
		if !r.Producer.Valid() || r.After < 0 || r.Limit < 1 || r.Limit > 128 {
			return fmt.Errorf("invalid inspection range")
		}
		c.Producer = protocol.Producer{}
		c.After = 0
		c.Limit = 0
	case "producer.ownership":
		if !r.Producer.Valid() || !protocol.ValidToken(r.Generation) {
			return fmt.Errorf("producer generation required")
		}
		c.Producer = protocol.Producer{}
		c.Generation = ""
	case "message.enqueue":
		if r.Message == nil || r.Producer != r.Message.Producer {
			return fmt.Errorf("message required")
		}
		c.Message = nil
		c.Producer = protocol.Producer{}
	case "status.replace":
		if r.Snapshot == nil || r.Producer != r.Snapshot.Producer {
			return fmt.Errorf("snapshot required")
		}
		c.Snapshot = nil
		c.Producer = protocol.Producer{}
	case "fault-transition.enqueue":
		if r.Transitions == nil || r.Transitions.Producer != r.Producer {
			return fmt.Errorf("transition batch required")
		}
		c.Transitions = nil
		c.Producer = protocol.Producer{}
	case "message.get":
		if !r.Producer.Valid() || !protocol.ValidToken(r.EventID) {
			return fmt.Errorf("event required")
		}
		c.Producer = protocol.Producer{}
		c.EventID = ""
	case "message.cancel":
		if !r.Producer.Valid() {
			return fmt.Errorf("producer required")
		}
		c.Producer = protocol.Producer{}
		c.EventID = ""
		c.Sequence = 0
		c.PayloadHash = ""
	case "receipt.list", "receipt.ack":
		if r.Cursor == nil || r.Cursor.Producer != r.Producer || r.Cursor.Identity != r.Expected {
			return fmt.Errorf("cursor required")
		}
		c.Producer = protocol.Producer{}
		c.Cursor = nil
		if r.Operation == "receipt.list" {
			c.Limit = 0
		}
	default:
		if !strings.HasPrefix(r.Operation, "command.") || len(r.Interaction) == 0 {
			return fmt.Errorf("unknown operation")
		}
		c.Interaction = nil
		c.Producer = protocol.Producer{}
	}
	if protocol.Hash(c) != protocol.Hash(protocol.Request{}) {
		return fmt.Errorf("unexpected fields")
	}
	return nil
}

func (s *Store) Serve(ctx context.Context, path string) error {
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	info, err := d.File().Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
		return fmt.Errorf("socket directory must be private and owned")
	}
	if info, err = os.Lstat(path); err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("foreign socket path")
		}
		c, e := net.DialTimeout("unix", path, 200*time.Millisecond)
		if e == nil {
			c.Close()
			return fmt.Errorf("socket is live")
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	defer l.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	slots := make(chan struct{}, 32)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); defer func() { <-slots }(); defer c.Close(); s.connection(ctx, c) }()
	}
}

func (s *Store) connection(ctx context.Context, c *net.UnixConn) {
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	raw, err := c.SyscallConn()
	if err != nil {
		return
	}
	var cred *unix.Ucred
	var peerErr error
	if err = raw.Control(func(fd uintptr) { cred, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil || peerErr != nil || cred.Uid != uint32(os.Getuid()) {
		return
	}
	reader := bufio.NewReaderSize(c, protocol.MaxRequest)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > protocol.MaxRequest || reader.Buffered() != 0 {
		return
	}
	var r protocol.Request
	if protocol.Decode(line, &r, protocol.MaxRequest) != nil {
		return
	}
	request, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	b, err := json.Marshal(s.Handle(request, r))
	if err != nil {
		return
	}
	if len(b)+1 > protocol.MaxResponse {
		b, _ = json.Marshal(protocol.Reply{Version: protocol.Version, Identity: s.identity, Error: "response_too_large"})
	}
	_, _ = c.Write(append(b, '\n'))
}
