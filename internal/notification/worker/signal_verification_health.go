package worker

import (
	"fmt"
	"github.com/dryaf/deploy/internal/safefs"
)

// BindSignalVerification exposes a process-local installer proof, not a durable
// provider/admission grant. Startup must independently validate it every time.
func (s *Store) BindSignalVerification(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.verificationOnly || s.installationRevision == "" || s.recoveryCommitHash == "" || !safefs.DigestPattern.MatchString(hash) || s.signalVerificationHash != "" {
		return fmt.Errorf("Signal verification health requires an unbound managed recovery runtime")
	}
	s.signalVerificationHash = hash
	return nil
}
