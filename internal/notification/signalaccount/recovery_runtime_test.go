package signalaccount

import (
	"errors"
	"sync"
	"testing"
)

func TestRecoveredRuntimeRequiresAdmissionAndRetainsAccount(t *testing.T) {
	path, status := completedVerificationFixture(t)
	if r, err := OpenRecoveryRuntime(path, status, nil); err == nil {
		r.Close()
		t.Fatal("no admission accepted")
	}
	admitted := true
	check := func() error {
		if !admitted {
			return errors.New("revoked")
		}
		return nil
	}
	r, err := OpenRecoveryRuntime(path, status, check)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if f, err := r.File(); err != nil || f == nil {
		t.Fatal("missing inherited account descriptor", err)
	}
	if other, err := OpenRecoveryRuntime(path, status, check); err == nil {
		other.Close()
		t.Fatal("duplicate account owner")
	}
	if old, err := AcquireRuntime(path); err == nil {
		old.Close()
		t.Fatal("legacy account fence released")
	}
	admitted = false
	if r.Check() == nil {
		t.Fatal("revoked account runtime accepted")
	}
	if _, err = r.File(); err == nil {
		t.Fatal("revoked runtime granted child descriptor")
	}
	r.Close()
	if old, err := AcquireRuntime(path); err == nil {
		old.Close()
		t.Fatal("shutdown released old account fence")
	}
}

func TestRecoveredRuntimeConcurrentClose(t *testing.T) {
	path, status := completedVerificationFixture(t)
	r, err := OpenRecoveryRuntime(path, status, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.Check(); _, _ = r.File(); r.Close() }()
	}
	wg.Wait()
}
