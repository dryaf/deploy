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
	if (!s.verificationOnly && s.deliveryActivationHash == "") || s.installationRevision == "" || s.recoveryCommitHash == "" || !safefs.DigestPattern.MatchString(hash) || s.signalVerificationHash != "" {
		return fmt.Errorf("Signal verification health requires an unbound managed recovery runtime")
	}
	s.signalVerificationHash = hash
	return nil
}

// BindFreshSignalService exposes supervised first-install health independently
// of recovery verification. It grants no delivery or command authority.
func (s *Store) BindFreshSignalService(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if (!s.verificationOnly && s.deliveryActivationHash == "") || s.installationRevision == "" || s.recoveryCommitHash != "" || !safefs.DigestPattern.MatchString(hash) || s.signalVerificationHash != "" || s.freshSignalServiceHash != "" {
		return fmt.Errorf("fresh Signal health requires its managed first-install runtime")
	}
	s.freshSignalServiceHash = hash
	return nil
}
