package signalaccount

import (
	"fmt"
	"os"
	"sync"
)

// RecoveryRuntime retains the original account recovery fence. A managed
// service must supply its independently journaled admission check; ordinary
// AcquireRuntime and account maintenance remain fenced. CLI children inherit
// File and must finish before Close releases the parent's account reference.
type RecoveryRuntime struct {
	mu           sync.Mutex
	verification *RecoveryVerification
	admission    func() error
}

func OpenRecoveryRuntime(path string, expected RecoveryStatus, admission func() error) (*RecoveryRuntime, error) {
	if admission == nil {
		return nil, fmt.Errorf("managed Signal admission required")
	}
	if err := admission(); err != nil {
		return nil, err
	}
	v, err := OpenRecoveryVerification(path, expected)
	if err != nil {
		return nil, err
	}
	r := &RecoveryRuntime{verification: v, admission: admission}
	if err = r.Check(); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func (r *RecoveryRuntime) Check() error {
	if r == nil {
		return fmt.Errorf("Signal recovered runtime is closed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.check()
}
func (r *RecoveryRuntime) check() error {
	if r == nil || r.verification == nil || r.admission == nil {
		return fmt.Errorf("Signal recovered runtime is closed")
	}
	if err := r.admission(); err != nil {
		return err
	}
	return r.verification.Check()
}

func (r *RecoveryRuntime) File() (*os.File, error) { return r.FileFor("") }

// FileFor also binds the command's state directory to the held account.
func (r *RecoveryRuntime) FileFor(path string) (*os.File, error) {
	if r == nil {
		return nil, fmt.Errorf("Signal recovered runtime is closed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return nil, err
	}
	if path != "" && path != r.verification.path {
		return nil, fmt.Errorf("Signal command account directory changed")
	}
	return r.verification.account.File(), nil
}

func (r *RecoveryRuntime) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.verification != nil {
		r.verification.Close()
		r.verification = nil
	}
}
