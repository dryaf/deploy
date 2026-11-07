// Package led implements the pinned pi-status-led v1 protocol. A successful RPC
// proves registry acceptance, never physical LED rendering.
package led

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type State struct {
	ID         string `json:"id"`
	LED        string `json:"led"`
	Pattern    string `json:"pattern"`
	Priority   int    `json:"priority"`
	TTL        *int   `json:"ttl_seconds"`
	Persistent bool   `json:"persistent"`
	Message    string `json:"message"`
}
type Entry struct {
	State
	OwnerUID  int     `json:"owner_uid"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	ExpiresAt *string `json:"expires_at"`
	Winner    bool    `json:"winner"`
}
type Client struct {
	Endpoint *Endpoint
	Socket   string
	Budget   time.Duration
	dial     func(context.Context) (net.Conn, error) // In-memory transport for protocol tests.
}

// DaemonStatus intentionally leaves hardware-specific details opaque while
// inspecting the properties used to qualify a healthy lease.
type DaemonStatus struct {
	Daemon   string                     `json:"daemon,omitempty"`
	Version  string                     `json:"version,omitempty"`
	Degraded bool                       `json:"degraded"`
	LEDs     map[string]json.RawMessage `json:"leds"`
	Patterns map[string]json.RawMessage `json:"patterns,omitempty"`
}

func (c Client) Status(ctx context.Context) (DaemonStatus, error) {
	var raw json.RawMessage
	if err := c.call(ctx, request{Operation: "status.get"}, &raw); err != nil {
		return DaemonStatus{}, err
	}
	var status DaemonStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return status, err
	}
	return status, nil
}

type request struct {
	PlanHash  string `json:"plan_hash,omitempty"`
	Version   int    `json:"v"`
	Operation string `json:"op"`
	State     *State `json:"state,omitempty"`
	ID        string `json:"id,omitempty"`
	Offset    *int   `json:"offset,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// PrepareRemoval is the managed daemon's root-only extension. It atomically
// requires an empty registry and durably fences future state mutations.
func (c Client) PrepareRemoval(ctx context.Context, hash string) error {
	if !safefs.DigestPattern.MatchString(hash) {
		return fmt.Errorf("exact LED removal plan required")
	}
	var result struct {
		Version  int    `json:"version"`
		PlanHash string `json:"plan_hash"`
	}
	if err := c.call(ctx, request{Operation: "deploy.prepare-removal", PlanHash: hash}, &result); err != nil {
		return err
	}
	if result.Version != 1 || result.PlanHash != hash {
		return fmt.Errorf("LED removal fence reply changed")
	}
	return nil
}

type reply struct {
	Version int             `json:"v"`
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

func (c Client) call(ctx context.Context, r request, out any) error {
	if c.Budget <= 0 || c.Budget >= 5*time.Second {
		return fmt.Errorf("LED RPC budget must be below five seconds")
	}
	r.Version = 1
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b)+1 > 4096 {
		return fmt.Errorf("LED request too large")
	}
	ctx, cancel := context.WithTimeout(ctx, c.Budget)
	defer cancel()
	dial := c.dial
	if c.Endpoint != nil {
		if c.Endpoint.Socket != c.Socket || c.dial != nil {
			return fmt.Errorf("LED endpoint differs from selected socket")
		}
		dial = c.Endpoint.dial
	}
	if dial == nil {
		dial = func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
		}
	}
	conn, err := dial(ctx)
	if err != nil {
		return fmt.Errorf("LED daemon unavailable")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err = conn.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("LED mutation outcome uncertain")
	}
	reader := bufio.NewReaderSize(conn, 128<<10)
	line, err := reader.ReadSlice('\n')
	if err != nil || reader.Buffered() != 0 {
		return fmt.Errorf("LED response uncertain")
	}
	var rep reply
	if err = protocol.Decode(line, &rep, 128<<10); err != nil {
		return err
	}
	if rep.Version != 1 || !rep.OK {
		return fmt.Errorf("LED operation rejected")
	}
	if out != nil {
		return protocol.Decode(rep.Result, out, 128<<10)
	}
	return nil
}
func (c Client) Ping(ctx context.Context) error {
	return c.call(ctx, request{Operation: "system.ping"}, nil)
}
func (c Client) Set(ctx context.Context, s State) error {
	if !idPattern.MatchString(s.ID) || (s.LED != "green" && s.LED != "red") || !idPattern.MatchString(s.Pattern) || s.Priority < 0 || s.Priority > 100 || !utf8.ValidString(s.Message) || utf8.RuneCountInString(s.Message) > 256 || (s.TTL != nil && (*s.TTL < 1 || *s.TTL > 2592000)) {
		return fmt.Errorf("invalid LED state")
	}
	return c.call(ctx, request{Operation: "state.set", State: &s}, nil)
}
func (c Client) Clear(ctx context.Context, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid LED state ID")
	}
	return c.call(ctx, request{Operation: "state.clear", ID: id}, nil)
}
func (c Client) List(ctx context.Context) (entries []Entry, err error) {
	// Bound the whole live listing, not each page separately. A changing or
	// malformed registry cannot consume hundreds of per-RPC latency budgets.
	ctx, cancel := context.WithTimeout(ctx, c.Budget)
	defer cancel()
	offset := 0
	for page := 0; page < 1024; page++ {
		var result struct {
			States []Entry `json:"states"`
			Total  int     `json:"total"`
			Next   *int    `json:"next_offset"`
		}
		if err = c.call(ctx, request{Operation: "state.list", Offset: &offset, Limit: 100}, &result); err != nil {
			return nil, err
		}
		if result.Total < 0 || result.Total > 1024 || len(result.States) > 100 || len(entries)+len(result.States) > 1024 {
			return nil, fmt.Errorf("LED listing exceeds bounds")
		}
		for _, entry := range result.States {
			if entry.OwnerUID < 0 || !idPattern.MatchString(entry.ID) || (entry.LED != "green" && entry.LED != "red") || !idPattern.MatchString(entry.Pattern) || entry.Priority < 0 || entry.Priority > 100 {
				return nil, fmt.Errorf("invalid LED registry entry")
			}
			if len(entries) > 0 {
				previous := entries[len(entries)-1]
				if entry.OwnerUID < previous.OwnerUID || (entry.OwnerUID == previous.OwnerUID && entry.ID <= previous.ID) {
					return nil, fmt.Errorf("LED registry changed during pagination")
				}
			}
			entries = append(entries, entry)
		}
		if result.Next == nil {
			if len(entries) != result.Total {
				return nil, fmt.Errorf("incomplete LED registry listing")
			}
			return entries, nil
		}
		if len(result.States) == 0 || *result.Next != offset+len(result.States) || *result.Next >= result.Total || *result.Next > 1024 {
			return nil, fmt.Errorf("invalid LED pagination")
		}
		offset = *result.Next
	}
	return nil, fmt.Errorf("LED pagination did not terminate")
}

// HealthyTTL clamps a relative daemon lease to source evidence. The RPC budget
// and scheduling margin are removed before rounding down, never after sending.
func HealthyTTL(now, deadline time.Time, budget, margin time.Duration) int {
	if budget <= 0 || margin < 0 {
		return 0
	}
	seconds := int((deadline.Sub(now) - budget - margin) / time.Second)
	if seconds < 1 {
		return 0
	}
	return min(seconds, 90)
}
