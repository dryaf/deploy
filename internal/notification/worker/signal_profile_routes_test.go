package worker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestManagedSignalProfileRoutesPreserveRecoveryAndQueue(t *testing.T) {
	path, c, publication, review := verificationFixture(t)
	ctx := context.Background()
	_, err := AdoptPublishedRecoveryVerification(ctx, path, c, publication.Commit.CommitHash)
	must(t, err)
	revision, err := safefs.NewID("n")
	must(t, err)
	s, err := OpenManagedRecoveryVerification(path, c, review.After, revision, publication.Commit.CommitHash)
	must(t, err)
	defer s.Close()
	var next Config
	raw, err := json.Marshal(c)
	must(t, err)
	must(t, json.Unmarshal(raw, &next))
	route := next.Routes["operations"]
	route.Revision = protocol.Hash("effective-signal-route")
	next.Routes["operations"] = route
	before := retainedTreeHash(t, path)
	must(t, s.BindSignalProfileRoutes(next, protocol.Hash("immutable-profile-proof")))
	if retainedTreeHash(t, path) != before || s.config.Routes["operations"].Revision != route.Revision || !s.VerificationOnly() {
		t.Fatal("binding rewrote queue/recovery or did not install effective routes")
	}
	health, err := s.Health(ctx)
	must(t, err)
	if health.SignalProfilesHash != protocol.Hash("immutable-profile-proof") || !health.VerificationOnly || health.RecoveryCommitHash != publication.Commit.CommitHash {
		t.Fatal(health)
	}
	if err = s.PullSignal(ctx, provider.SignalInboxClient{}); err == nil {
		t.Fatal("profile routes enabled provider intake")
	}
	if err = s.BindSignalProfileRoutes(next, protocol.Hash("second-binding")); err == nil {
		t.Fatal("runtime binding replaced")
	}
	must(t, s.Close())
	// Reopening still proves the original canonical recovery configuration. The
	// effective route revision never becomes a replacement historical proof.
	if invalid, err := OpenManagedRecoveryVerification(path, next, review.After, revision, publication.Commit.CommitHash); err == nil {
		invalid.Close()
		t.Fatal("effective configuration replaced original recovery authority")
	}
	again, err := OpenManagedRecoveryVerification(path, c, review.After, revision, publication.Commit.CommitHash)
	must(t, err)
	defer again.Close()
	if again.signalProfilesHash != "" || protocol.Hash(again.config) != protocol.Hash(c) {
		t.Fatal("unproved process-local binding survived restart")
	}
}

func TestManagedSignalProfileRoutesRejectAuthorityChanges(t *testing.T) {
	path, c, publication, review := verificationFixture(t)
	ctx := context.Background()
	_, err := AdoptPublishedRecoveryVerification(ctx, path, c, publication.Commit.CommitHash)
	must(t, err)
	revision, err := safefs.NewID("n")
	must(t, err)
	s, err := OpenManagedRecoveryVerification(path, c, review.After, revision, publication.Commit.CommitHash)
	must(t, err)
	defer s.Close()
	for _, kind := range []string{"primary", "fallback", "missing", "budget", "invalid-hash"} {
		t.Run(kind, func(t *testing.T) {
			var next Config
			raw, err := json.Marshal(c)
			must(t, err)
			must(t, json.Unmarshal(raw, &next))
			route := next.Routes["operations"]
			hash := protocol.Hash("proof")
			switch kind {
			case "primary":
				route.Primary = "foreign"
			case "fallback":
				route.Fallback = "foreign"
			case "budget":
				next.MaxMessages++
			case "invalid-hash":
				hash = "unbound"
			}
			next.Routes["operations"] = route
			if kind == "missing" {
				delete(next.Routes, "operations")
			}
			if s.BindSignalProfileRoutes(next, hash) == nil || s.signalProfilesHash != "" || protocol.Hash(s.config) != protocol.Hash(c) {
				t.Fatal("changed authority accepted", kind)
			}
		})
	}
	_, err = s.MonitorRuntimeLoop("already-serving")
	must(t, err)
	if s.BindSignalProfileRoutes(c, protocol.Hash("late-proof")) == nil {
		t.Fatal("binding changed after runtime loop registration")
	}
}
