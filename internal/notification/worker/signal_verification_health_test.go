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

func TestFreshSignalHealthRequiresManagedFirstInstall(t *testing.T) {
	s, _, path := fixture(t)
	if err := s.BindFreshSignalService(protocol.Hash("service")); err == nil {
		t.Fatal("unmanaged runtime claimed native service")
	}
	must(t, s.Close())
	v, err := OpenVerification(path, configuration())
	must(t, err)
	defer v.Close()
	if err = v.BindFreshSignalService(protocol.Hash("service")); err == nil {
		t.Fatal("unmanaged verification claimed native service")
	}
	v.mu.Lock()
	v.installationRevision = "fixture-revision"
	v.mu.Unlock()
	must(t, v.BindFreshSignalService(protocol.Hash("service")))
	if err = v.BindSignalVerification(protocol.Hash("recovery")); err == nil {
		t.Fatal("fresh runtime claimed recovered service")
	}
	if err = v.BindFreshSignalService(protocol.Hash("other")); err == nil {
		t.Fatal("fresh runtime replaced service binding")
	}
	h, err := v.Health(context.Background())
	must(t, err)
	if h.FreshSignalServiceHash != protocol.Hash("service") || h.SignalVerificationHash != "" {
		t.Fatal("fresh service health conflated with recovery")
	}
}
