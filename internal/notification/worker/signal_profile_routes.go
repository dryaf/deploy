package worker

import (
	"fmt"
	"maps"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// BindSignalProfileRoutes installs only process-local effective route revisions
// after the installer has verified its immutable Signal profile transition.
// Recovery is opened against its original configuration first. Existing queued
// route revisions, destinations, receipts and recovery anchors are never rewritten.
// Binding never changes canonical recovery/ownership configuration or grants
// delivery/intake. An already admitted delivery instance may use these routes.
func (s *Store) BindSignalProfileRoutes(c Config, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if (!s.verificationOnly && s.deliveryActivationHash == "") || s.installationRevision == "" || s.recoveryCommitHash == "" || s.signalProfilesHash != "" || s.signalVerificationHash != "" || len(s.runtimeLoops) != 0 || !safefs.DigestPattern.MatchString(hash) {
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
	s.signalProfileRoutes = maps.Clone(c.Routes)
	s.signalProfilesHash = hash
	return nil
}

// Accessed under the store mutex, or during single-threaded runtime preparation.
func (s *Store) effectiveRoutes() map[string]Route {
	if s.signalProfileRoutes != nil {
		return s.signalProfileRoutes
	}
	return s.config.Routes
}
