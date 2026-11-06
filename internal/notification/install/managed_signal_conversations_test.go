package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func legacySignalProfile(c *Runtime) {
	signalProfilesConfiguration(c)
	p := c.Providers["signal"]
	p.Conversation = nil
	p.Destination = protocol.Hash("legacy fixed destination")
	c.Providers["signal"] = p
}

func TestManagedSignalConversationEnrollment(t *testing.T) {
	spec, i, _, _, before := managedSignalProfilesFixtureUsing(t, false, legacySignalProfile)
	canonical := mustRuntime(t, before.Paths.Config)
	dest := canonical.Worker.Interaction.Destinations[0]
	spec.SignalConversations = map[string]SignalConversationEnrollment{"signal": {
		PreviousDestination: canonical.Providers["signal"].Destination, Conversation: dest.ID,
	}}
	state := activationTreeHash(t, spec.State)
	interrupted := errors.New("published enrollment interrupted")
	i.fault = func(at string) error {
		if at == "manifest-published" {
			return interrupted
		}
		return nil
	}
	if _, err := i.prepare(context.Background(), "migrate-signal-profiles", spec); !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	// Resume consumes retained proof, even after the input mapping and files vanish.
	spec.SignalConversations = nil
	if err := os.Remove(spec.RuntimeFile); err != nil {
		t.Fatal(err)
	}
	i.fault = nil
	after, err := i.resume(context.Background(), spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	retained, effective := effectiveRuntimeForTest(t, after)
	p := effective.Providers["signal"]
	if retained.Providers["signal"].Conversation != nil || protocol.Hash(retained) != protocol.Hash(canonical) ||
		p.Conversation == nil || protocol.Hash(*p.Conversation) != protocol.Hash(dest) || p.Destination != dest.Fingerprint() ||
		p.Socket != effective.Signal.Socket || p.RelayIdentity != effective.Signal.InboxIdentity ||
		effective.Worker.Routes["ops"].Revision == canonical.Worker.Routes["ops"].Revision || state != activationTreeHash(t, spec.State) {
		t.Fatal("enrollment changed original authority or lost the explicit destination")
	}
	// The immutable mapping remains necessary; it cannot be inferred from the new profile.
	proof := filepath.Join(spec.Root, "releases", after.Revision, "signal-profiles.json")
	if err = os.Remove(proof); err != nil {
		t.Fatal(err)
	}
	if _, err = i.resume(context.Background(), spec.Root); err == nil {
		t.Fatal("lost enrollment proof accepted")
	}
}

func TestSignalConversationEnrollmentRejectsUnprovenMapping(t *testing.T) {
	spec, i, f, _, before := managedSignalProfilesFixtureUsing(t, false, legacySignalProfile)
	c := mustRuntime(t, before.Paths.Config)
	valid := SignalConversationEnrollment{PreviousDestination: c.Providers["signal"].Destination, Conversation: c.Worker.Interaction.Destinations[0].ID}
	for _, kind := range []string{"missing", "old-destination", "unknown-conversation", "unknown-profile", "other-operation"} {
		t.Run(kind, func(t *testing.T) {
			s := spec
			s.SignalConversations = map[string]SignalConversationEnrollment{"signal": valid}
			operation := "migrate-signal-profiles"
			switch kind {
			case "missing":
				s.SignalConversations = nil
			case "old-destination":
				b := valid
				b.PreviousDestination = protocol.Hash("different destination")
				s.SignalConversations["signal"] = b
			case "unknown-conversation":
				b := valid
				b.Conversation = "unreviewed-conversation"
				s.SignalConversations["signal"] = b
			case "unknown-profile":
				s.SignalConversations["unknown"] = valid
			case "other-operation":
				operation = "update"
			}
			stops := f.stops
			state := activationTreeHash(t, spec.State)
			if _, err := i.prepare(context.Background(), operation, s); err == nil || f.stops != stops || state != activationTreeHash(t, spec.State) {
				t.Fatal("unproven mapping admitted or gateway changed", err)
			}
		})
	}
}
