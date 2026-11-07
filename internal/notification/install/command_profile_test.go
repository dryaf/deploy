package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestCommandPolicyInspectionRetainsAuthorityWithoutAdmission(t *testing.T) {
	spec, i, c, m, qualification := managedCommandsFixture(t)
	ctx := context.Background()
	p := c.Worker.Producers[0].Producer
	for _, admitted := range []bool{false, true} {
		if admitted {
			var err error
			m, err = i.ensureCommandPolicy(ctx, CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Operation: "enable-commands", Mode: "read", QualificationHash: protocol.Hash(qualification)})
			if err != nil {
				t.Fatal(err)
			}
		}
		tree := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
		got, err := InspectManagedCommandPolicy(ctx, spec.Root, protocol.Hash(m), p)
		if err != nil || protocol.Hash(got) != protocol.Hash(*c.Worker.Interaction) {
			t.Fatal("changed enrolled policy", err)
		}
		if tree != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) {
			t.Fatal("policy inspection modified installation")
		}
		if _, err = InspectManagedCommandPolicy(ctx, spec.Root, protocol.Hash("stale"), p); err == nil {
			t.Fatal("accepted stale gateway")
		}
		foreign := p
		foreign.Epoch = "different"
		if _, err = InspectManagedCommandPolicy(ctx, spec.Root, protocol.Hash(m), foreign); err == nil {
			t.Fatal("accepted foreign producer")
		}
	}
	if err := os.Remove(m.Paths.Config); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectManagedCommandPolicy(ctx, spec.Root, protocol.Hash(m), p); err == nil {
		t.Fatal("accepted missing runtime")
	}
}

func TestCommandPolicyInspectionDoesNotResumePendingAdmission(t *testing.T) {
	spec, i, c, m, q := managedCommandsFixture(t)
	stop := errors.New("interrupted")
	i.fault = func(at string) error {
		if at == "manifest-published" {
			return stop
		}
		return nil
	}
	_, err := i.ensureCommandPolicy(context.Background(), CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Operation: "enable-commands", Mode: "read", QualificationHash: protocol.Hash(q)})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	tree := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
	if _, err = InspectManagedCommandPolicy(context.Background(), spec.Root, protocol.Hash(m), c.Worker.Producers[0].Producer); err == nil {
		t.Fatal("accepted pending admission")
	}
	if tree != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) {
		t.Fatal("inspection resumed pending admission")
	}
}

func TestCommandPolicyInspectionAndReviewUseEffectivePermissions(t *testing.T) {
	ctx := context.Background()
	spec, i, c, m, q := managedCommandsFixture(t)
	i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	m, err := i.ensureCommandPolicy(ctx, CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Operation: "enable-commands", Mode: "all", QualificationHash: protocol.Hash(q)})
	if err != nil {
		t.Fatal(err)
	}
	permissions := slices.Clone(c.Worker.Interaction.Permissions)
	permissions[0].Permissions = []string{"read"}
	raw, err := json.Marshal(permissions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ReviewCommandNarrowing(ctx, spec.Root, protocol.Hash(m), string(raw)); err == nil {
		t.Fatal("review accepted live intake")
	}
	m, err = i.ensureCommandPolicy(ctx, CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Operation: "disable-commands"})
	if err != nil {
		t.Fatal(err)
	}
	before := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
	request, err := ReviewCommandNarrowing(ctx, spec.Root, protocol.Hash(m), string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if before != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) {
		t.Fatal("narrowing review mutated installation")
	}
	m, err = i.ensureCommandPolicy(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := InspectManagedCommandPolicy(ctx, spec.Root, protocol.Hash(m), c.Worker.Producers[0].Producer)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(got.Permissions) != protocol.Hash(permissions) {
		t.Fatal("inspection returned superseded broader permissions")
	}
	original, err := json.Marshal(c.Worker.Interaction.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	before = activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
	if _, err = ReviewCommandNarrowing(ctx, spec.Root, protocol.Hash(m), string(original)); err == nil {
		t.Fatal("review restored removed permissions")
	}
	if before != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) {
		t.Fatal("rejected expansion changed authority")
	}
	m, err = i.ensureCommandPolicy(ctx, CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Operation: "enable-commands", Mode: "all", QualificationHash: protocol.Hash(q)})
	if err != nil {
		t.Fatal(err)
	}
	got, err = InspectManagedCommandPolicy(ctx, spec.Root, protocol.Hash(m), c.Worker.Producers[0].Producer)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(got.Permissions) != protocol.Hash(permissions) {
		t.Fatal("reenabling restored permissions")
	}
}

func TestCommandEnrollmentReviewPreservesRetirementAndRejectsRedirect(t *testing.T) {
	ctx := context.Background()
	spec, i, c, m, q := managedCommandsFixtureUsing(t, func(c *Runtime) {
		d := c.Worker.Interaction.Destinations[0]
		d.ID, d.NativeID = "retired", "-456"
		c.Worker.Interaction.Destinations = append(c.Worker.Interaction.Destinations, d)
		p := c.Worker.Interaction.Permissions[0]
		p.Conversation = d.ID
		c.Worker.Interaction.Permissions = append(c.Worker.Interaction.Permissions, p)
	})
	i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	m, err := i.ensureCommandPolicy(ctx, CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Operation: "enable-commands", Mode: "read", QualificationHash: protocol.Hash(q)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(c.Worker.Interaction)
	if err != nil {
		t.Fatal(err)
	}
	var next interaction.Config
	if err = json.Unmarshal(raw, &next); err != nil {
		t.Fatal(err)
	}
	next.Revision = "renamed"
	next.Workers[0].Alias = "archive"
	next.Destinations = next.Destinations[:1]
	next.Permissions = next.Permissions[:1]
	raw, err = json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ReviewCommandEnrollment(ctx, spec.Root, protocol.Hash(m), string(raw)); err == nil {
		t.Fatal("accepted live intake")
	}
	m, err = i.ensureCommandPolicy(ctx, CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Operation: "disable-commands"})
	if err != nil {
		t.Fatal(err)
	}
	tree := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
	request, err := ReviewCommandEnrollment(ctx, spec.Root, protocol.Hash(m), string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if tree != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) {
		t.Fatal("review changed files")
	}
	m, err = i.ensureCommandPolicy(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"redirect", "restore", "contract"} {
		var bad interaction.Config
		if err = json.Unmarshal(raw, &bad); err != nil {
			t.Fatal(err)
		}
		bad.Revision = change
		switch change {
		case "redirect":
			bad.Destinations[0].NativeID = "-999"
		case "restore":
			bad.Destinations = append(bad.Destinations, c.Worker.Interaction.Destinations[1])
		case "contract":
			bad.Workers[0].Generation = "replacement"
		}
		invalid, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		tree = activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
		if _, err = ReviewCommandEnrollment(ctx, spec.Root, protocol.Hash(m), string(invalid)); err == nil {
			t.Fatal("accepted", change)
		}
		if tree != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) {
			t.Fatal("rejected change modified state", change)
		}
	}
	// Later permission edits must keep the full enrollment overlay. An empty
	// permission set revokes all actors without restoring retired conversations.
	next.Revision, next.Permissions = "no-grants", []interaction.Permission{}
	raw, err = json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	request, err = ReviewCommandEnrollment(ctx, spec.Root, protocol.Hash(m), string(raw))
	if err != nil {
		t.Fatal(err)
	}
	m, err = i.ensureCommandPolicy(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := InspectManagedCommandPolicy(ctx, spec.Root, protocol.Hash(m), c.Worker.Producers[0].Producer)
	if err != nil || protocol.Hash(got) != protocol.Hash(next) {
		t.Fatal("lost effective policy", got, err)
	}
}
