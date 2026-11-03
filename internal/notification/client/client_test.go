package client

import (
	"context"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestCallRejectsConflictingCallerIdentityBeforeConnecting(t *testing.T) {
	c := Client{Socket: "/does/not/exist", Identity: protocol.Identity{Worker: "gateway", Recovery: "one"}}
	_, err := c.Call(context.Background(), protocol.Request{Operation: "message.enqueue", Expected: protocol.Identity{Worker: "different", Recovery: "one"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "identities disagree") {
		t.Fatal("caller identity was overwritten before connecting", err)
	}
}
