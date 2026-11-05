package worker

import (
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// BindSignalProfileRoutes installs only process-local effective route revisions
// after the installer has verified its immutable Signal profile transition.
// Recovery is opened against its original configuration first. Existing queued
// route revisions, destinations, receipts and recovery anchors are never rewritten.
// The instance remains verification-only; this cannot authorize dispatch/intake.
func (s *Store) BindSignalProfileRoutes(c Config, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.verificationOnly || s.installationRevision == "" || s.recoveryCommitHash == "" || s.signalProfilesHash != "" || s.signalVerificationHash != "" || len(s.runtimeLoops) != 0 || !safefs.DigestPattern.MatchString(hash) {
		return fmt.Errorf("Signal profile routes require a managed recovered verification instance")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	base := c
	base.Routes = s.config.Routes
	if protocol.Hash(base) != protocol.Hash(s.config) || len(c.Routes) != len(s.config.Routes) {
		return fmt.Errorf("Signal profile routes changed non-route configuration")
	}
	for name, old := range s.config.Routes {
		next, ok := c.Routes[name]
		if !ok || old.Primary != next.Primary || old.Fallback != next.Fallback {
			return fmt.Errorf("Signal profile routes changed provider selection")
		}
	}
	s.config = c
	s.signalProfilesHash = hash
	return nil
}
