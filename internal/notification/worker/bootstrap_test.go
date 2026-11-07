package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func bootstrapConfig() Config {
	return Config{Routes: map[string]Route{"alerts": {Revision: "first", Primary: "telegram"}}, MaxMessages: 100, MaxPayloadBytes: 1 << 20, MaxDatabaseBytes: 16 << 20}
}
func bootstrapTarget(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	id, e := safefs.NewID("n")
	if e != nil {
		t.Fatal(e)
	}
	return filepath.Join(dir, "state"), id
}

func TestGatewayBootstrapUnboundAuthority(t *testing.T) {
	ctx := context.Background()
	path, op := bootstrapTarget(t)
	c := bootstrapConfig()
	if c.Validate() == nil {
		t.Fatal("normal enrollment accepts no producers")
	}
	b, e := BootstrapGateway(ctx, path, op, c)
	if e != nil {
		t.Fatal(e)
	}
	s, e := OpenBootstrapVerification(path, c, b.Identity, "")
	if e != nil {
		t.Fatal(e)
	}
	h, e := s.Health(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if !s.VerificationOnly() || h.Condition != "unknown" || !h.VerificationOnly {
		t.Fatalf("unbound health: %+v", h)
	}
	if e = s.Activate(protocol.Producer{ID: "invented", Epoch: "one"}, "generation"); e == nil {
		t.Fatal("unbound gateway activated")
	}
	var n int
	if e = s.db.QueryRow("SELECT count(*) FROM producers").Scan(&n); e != nil || n != 0 {
		t.Fatal("fabricated producer", n, e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = Open(path, c, false); e == nil {
		t.Fatal("ordinary runtime admitted bootstrap")
	}
	again, e := BootstrapGateway(ctx, path, op, c)
	if e != nil || again.Identity != b.Identity {
		t.Fatal("no-op changed identity", e)
	}
	c.MaxMessages++
	if _, e = BootstrapGateway(ctx, path, op, c); e == nil {
		t.Fatal("changed policy silently adopted")
	}
	if _, e = OpenBootstrapVerification(path, c, b.Identity, ""); e == nil {
		t.Fatal("bootstrap runtime changed policy")
	}
}

func TestGatewayBootstrapInterruptedPublication(t *testing.T) {
	for _, stage := range []string{"reserved", "staging", "ready", "published", "complete"} {
		t.Run(stage, func(t *testing.T) {
			path, op := bootstrapTarget(t)
			c := bootstrapConfig()
			injected := errors.New("power loss")
			b, e := bootstrapGateway(context.Background(), path, op, c, func(s string) error {
				if s == stage {
					return injected
				}
				return nil
			})
			if !errors.Is(e, injected) {
				t.Fatal("boundary not reached", e)
			}
			resumed, e := BootstrapGateway(context.Background(), path, op, c)
			if e != nil {
				t.Fatal(e)
			}
			if resumed.Identity != b.Identity {
				t.Fatal("retry replaced reserved identity")
			}
			s, e := OpenBootstrapVerification(path, c, b.Identity, "")
			if e != nil {
				t.Fatal(e)
			}
			s.Close()
		})
	}
}

func TestGatewayBootstrapRefusesMissingAndForeignState(t *testing.T) {
	t.Run("foreign", func(t *testing.T) {
		path, op := bootstrapTarget(t)
		if e := os.Mkdir(path, 0700); e != nil {
			t.Fatal(e)
		}
		if _, e := BootstrapGateway(context.Background(), path, op, bootstrapConfig()); e == nil {
			t.Fatal("adopted unowned state")
		}
	})
	t.Run("missing", func(t *testing.T) {
		path, op := bootstrapTarget(t)
		_, e := BootstrapGateway(context.Background(), path, op, bootstrapConfig())
		if e != nil {
			t.Fatal(e)
		}
		if e = os.Rename(path, path+".lost"); e != nil {
			t.Fatal(e)
		}
		if _, e = BootstrapGateway(context.Background(), path, op, bootstrapConfig()); e == nil {
			t.Fatal("reinitialized lost state")
		}
	})
	t.Run("reservation", func(t *testing.T) {
		path, op := bootstrapTarget(t)
		_, e := bootstrapGateway(context.Background(), path, op, bootstrapConfig(), func(stage string) error {
			if stage == "reserved" {
				return errors.New("stop")
			}
			return nil
		})
		if e == nil {
			t.Fatal("no fault")
		}
		name := ".gateway-" + safefs.LockName(filepath.Base(path)) + ".json"
		if e = os.Remove(filepath.Join(filepath.Dir(path), name)); e != nil {
			t.Fatal(e)
		}
		if _, e = BootstrapGateway(context.Background(), path, op, bootstrapConfig()); e == nil {
			t.Fatal("recreated missing reservation")
		}
	})
}
