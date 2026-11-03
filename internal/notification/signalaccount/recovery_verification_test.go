package signalaccount

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func completedVerificationFixture(t *testing.T) (string, RecoveryStatus) {
	t.Helper()
	path, req, svc := recoveryFixture(t)
	apply, verify, _ := recoveryCallbacks(t, path)
	status, err := FenceRecovery(context.Background(), path, req, svc, apply, verify)
	if err != nil {
		t.Fatal(err)
	}
	return path, status
}
func TestRecoveryVerificationOwnsAccount(t *testing.T) {
	path, status := completedVerificationFixture(t)
	v, err := OpenRecoveryVerification(path, status)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err = v.Check(); err != nil {
		t.Fatal(err)
	}
	if other, e := OpenRecoveryVerification(path, status); e == nil {
		other.Close()
		t.Fatal("duplicate account verifier")
	}
	if l, e := AcquireRuntime(path); e == nil {
		l.Close()
		t.Fatal("verifier released runtime fence")
	}
	d, e := accountDirectory(path)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if l, e := d.Lock("deploy-service-link.lock", true, false); e == nil {
		l.Close()
		t.Fatal("maintenance entered")
	}
	v.Close()
	if err = v.Check(); err == nil {
		t.Fatal("closed verifier accepted")
	}
	v, err = OpenRecoveryVerification(path, status)
	if err != nil {
		t.Fatal(err)
	}
	v.Close()
}
func TestRecoveryVerificationRejectsLostOrChangedEvidence(t *testing.T) {
	for _, name := range []string{"deploy-account.lock", "deploy-service-link.lock", "deploy-account-control.lock", recoveryIntentFile, recoveryCompleteFile} {
		t.Run(name, func(t *testing.T) {
			path, status := completedVerificationFixture(t)
			v, err := OpenRecoveryVerification(path, status)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			target := filepath.Join(path, name)
			raw, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(target, target+".old"); err != nil {
				t.Fatal(err)
			}
			if err = v.Check(); err == nil {
				t.Fatal("lost evidence accepted")
			}
			v.Close()
			if other, e := OpenRecoveryVerification(path, status); e == nil {
				other.Close()
				t.Fatal("missing evidence repaired")
			}
			if _, e := os.Stat(target); !os.IsNotExist(e) {
				t.Fatal("missing file created", e)
			}
			if name == "deploy-account.lock" || name == "deploy-service-link.lock" {
				// Replacement account lease is also rejected by persistent recovery pins.
				if err = os.WriteFile(target, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if name == "deploy-account.lock" {
					if other, e := OpenRecoveryVerification(path, status); e == nil {
						other.Close()
						t.Fatal("replaced account lock accepted")
					}
				}
			}
		})
	}
}
func TestRecoveryVerificationPinsAndCloseRace(t *testing.T) {
	path, status := completedVerificationFixture(t)
	for _, change := range []func(*RecoveryStatus){func(s *RecoveryStatus) { s.RuntimeAllowed = true }, func(s *RecoveryStatus) { s.Phase = "result" }, func(s *RecoveryStatus) { s.Target = s.Binding }, func(s *RecoveryStatus) { s.ResultHash = s.Target }} {
		bad := status
		change(&bad)
		if v, e := OpenRecoveryVerification(path, bad); e == nil {
			v.Close()
			t.Fatal("wrong pin accepted")
		}
	}
	v, err := OpenRecoveryVerification(path, status)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = v.Check(); v.Close() }()
	}
	wg.Wait()
}

func TestRecoveryVerificationDetectsCoordinationReplacement(t *testing.T) {
	for _, name := range []string{"deploy-service-link.lock", "deploy-account-control.lock"} {
		t.Run(name, func(t *testing.T) {
			path, status := completedVerificationFixture(t)
			v, err := OpenRecoveryVerification(path, status)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			target := filepath.Join(path, name)
			if err = os.Rename(target, target+".old"); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(target, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err = v.Check(); err == nil {
				t.Fatal("same-byte coordination replacement accepted")
			}
		})
	}
}
