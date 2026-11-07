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

func bootstrapConsumerConfig(c Config) Config {
	c.Producers = []Enrollment{{Producer: protocol.Producer{ID: "backup-worker", Epoch: "reserved-one"}, Required: true, Routes: []string{"alerts"}, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, MaxMessages: 100}}
	return c
}
func TestBootstrapEnrollmentRetainsIdentityAndInactiveOwnership(t *testing.T) {
	path, op := bootstrapTarget(t)
	ctx := context.Background()
	c := bootstrapConfig()
	b, err := BootstrapGateway(ctx, path, op, c)
	if err != nil {
		t.Fatal(err)
	}
	next := bootstrapConsumerConfig(c)
	enroll, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	r, err := EnrollBootstrap(ctx, path, b.Identity, next, enroll)
	if err != nil {
		t.Fatal(err)
	}
	if r.Identity != b.Identity {
		t.Fatal("enrollment changed gateway identity")
	}
	s, err := OpenManagedVerification(path, next, b.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RequireInactiveProducers(); err != nil {
		t.Fatal(err)
	}
	owner, err := s.readOwnership(next.Producers[0].Producer)
	if err != nil || owner.Revision != 1 || owner.Active != "" {
		t.Fatal("consumer granted authority", owner, err)
	}
	s.Close()
	again, err := EnrollBootstrap(ctx, path, b.Identity, next, enroll)
	if err != nil || again != r {
		t.Fatal("retry changed receipt", err)
	}
	if _, err = OpenBootstrapVerification(path, c, b.Identity, ""); err == nil {
		t.Fatal("bound gateway reopened as unbound")
	}
	next.Producers[0].Producer.Epoch = "different"
	if _, err = EnrollBootstrap(ctx, path, b.Identity, next, enroll); err == nil {
		t.Fatal("retargeted existing operation")
	}
}
func TestBootstrapEnrollmentResumesAtDurableBoundaries(t *testing.T) {
	for _, boundary := range []string{"intent", "producers", "owner/backup-worker", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			path, op := bootstrapTarget(t)
			ctx := context.Background()
			c := bootstrapConfig()
			b, err := BootstrapGateway(ctx, path, op, c)
			if err != nil {
				t.Fatal(err)
			}
			after := bootstrapConsumerConfig(c)
			enroll, _ := safefs.NewID("n")
			stop := errors.New("interruption")
			r, err := enrollBootstrap(ctx, path, b.Identity, after, enroll, func(stage string) error {
				if stage == boundary {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal("boundary not reached", err)
			}
			if boundary != "complete" {
				if opened, e := Open(path, after, false); e == nil {
					opened.Close()
					t.Fatal("pending enrollment admitted ordinary runtime")
				}
			}
			again, err := EnrollBootstrap(ctx, path, b.Identity, after, enroll)
			if err != nil || again != r {
				t.Fatal("cannot resume", err)
			}
			s, err := OpenManagedVerification(path, after, b.Identity)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.RequireInactiveProducers(); err != nil {
				t.Fatal(err)
			}
			s.Close()
		})
	}
}
func TestBootstrapEnrollmentDoesNotHideOtherPolicyChanges(t *testing.T) {
	path, op := bootstrapTarget(t)
	c := bootstrapConfig()
	b, err := BootstrapGateway(context.Background(), path, op, c)
	if err != nil {
		t.Fatal(err)
	}
	after := bootstrapConsumerConfig(c)
	after.MaxMessages++
	enroll, _ := safefs.NewID("n")
	if _, err = EnrollBootstrap(context.Background(), path, b.Identity, after, enroll); err == nil {
		t.Fatal("changed unrelated budgets")
	}
	if _, err = os.Stat(filepath.Join(path, "bootstrap-enrollment.json")); !os.IsNotExist(err) {
		t.Fatal("invalid intent mutated state", err)
	}
}
