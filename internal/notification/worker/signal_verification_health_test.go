package worker

import (
	"context"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"testing"
)

func TestSignalVerificationHealthRequiresManagedRecovery(t *testing.T) {
	s, _, path := fixture(t)
	if err := s.BindSignalVerification(protocol.Hash("endpoint")); err == nil {
		t.Fatal("ordinary runtime claimed managed endpoint")
	}
	must(t, s.Close())
	v, err := OpenVerification(path, configuration())
	must(t, err)
	defer v.Close()
	if err = v.BindSignalVerification(protocol.Hash("endpoint")); err == nil {
		t.Fatal("unmanaged verifier claimed managed endpoint")
	}
	h, err := v.Health(context.Background())
	must(t, err)
	if h.SignalVerificationHash != "" {
		t.Fatal("rejected binding leaked into health")
	}
}
