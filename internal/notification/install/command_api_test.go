package install

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestCommandPolicyTypedRetryPinsAdmission(t *testing.T) {
	for _, boundary := range []string{"prepared", "stopped", "manifest-published", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			spec, i, _, active, qualification := managedCommandsFixture(t)
			ctx := context.Background()
			r, ready, err := ReviewCommandAdmission(ctx, spec.Root, protocol.Hash(active), "read")
			if err != nil || !ready || r.QualificationHash != protocol.Hash(qualification) {
				t.Fatal(r, ready, err)
			}
			if _, done, err := i.inspectCommandPolicy(ctx, r); err != nil || done {
				t.Fatal(done, err)
			}
			stop := errors.New("command acknowledgement lost")
			i.fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			if _, err = i.ensureCommandPolicy(ctx, r); !errors.Is(err, stop) {
				t.Fatal("boundary missed", boundary, err)
			}
			for _, change := range []string{"qualification", "mode", "operation"} {
				changed := r
				switch change {
				case "qualification":
					changed.QualificationHash = protocol.Hash("other")
				case "mode":
					changed.Mode = "all"
				case "operation":
					changed.Operation = "disable-commands"
					changed.Mode, changed.QualificationHash = "", ""
				}
				if _, _, err = i.inspectCommandPolicy(ctx, changed); err == nil {
					t.Fatal("retargeted command inspection", change)
				}
				if _, err = i.ensureCommandPolicy(ctx, changed); err == nil {
					t.Fatal("retargeted command apply", change)
				}
			}
			i.fault = nil
			enabled, err := i.ensureCommandPolicy(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			if enabled.Commands == nil || enabled.Commands.Mode != "read" || enabled.DeliveryActivation.Hash != active.DeliveryActivation.Hash {
				t.Fatal("command request changed delivery")
			}
			for range 2 {
				got, done, err := i.inspectCommandPolicy(ctx, r)
				if err != nil || !done || protocol.Hash(got) != protocol.Hash(enabled) {
					t.Fatal(done, err)
				}
				got, err = i.ensureCommandPolicy(ctx, r)
				if err != nil || protocol.Hash(got) != protocol.Hash(enabled) {
					t.Fatal("completed retry", err)
				}
			}
			disable := CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(enabled), Operation: "disable-commands"}
			drained, err := i.ensureCommandPolicy(ctx, disable)
			if err != nil {
				t.Fatal(err)
			}
			if drained.Commands.Mode != "drain" || drained.DeliveryActivation.Hash != active.DeliveryActivation.Hash {
				t.Fatal("disable changed delivery")
			}
			if _, done, err := i.inspectCommandPolicy(ctx, disable); err != nil || !done {
				t.Fatal(done, err)
			}
		})
	}
}

func TestCommandPolicyTypedMigrationRetainsDisabledAdmission(t *testing.T) {
	ctx := context.Background()
	spec, i, c, active, qualification := managedCommandsFixture(t)
	i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	enabled, err := i.ensureCommandPolicy(ctx, CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(active), Operation: "enable-commands", QualificationHash: protocol.Hash(qualification), Mode: "read"})
	if err != nil {
		t.Fatal(err)
	}
	drained, err := i.ensureCommandPolicy(ctx, CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(enabled), Operation: "disable-commands"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"narrow-commands", "migrate-commands"} {
		c.Worker.Interaction.Permissions[0].Permissions = []string{"read"}
		var raw []byte
		if kind == "narrow-commands" {
			raw, err = json.Marshal(c.Worker.Interaction.Permissions)
		} else {
			c.Worker.Interaction.Revision = "next"
			c.Worker.Interaction.Workers[0].Alias = "worker/renamed"
			raw, err = json.Marshal(c.Worker.Interaction)
		}
		if err != nil {
			t.Fatal(err)
		}
		r := CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(drained), Operation: kind, Policy: string(raw)}
		stop := errors.New("policy publication lost")
		i.fault = func(at string) error {
			if at == "manifest-published" {
				return stop
			}
			return nil
		}
		if _, err = i.ensureCommandPolicy(ctx, r); !errors.Is(err, stop) {
			t.Fatal(err)
		}
		changed := r
		changed.Policy = "[]"
		if kind == "narrow-commands" {
			changed.Policy = "[{}]"
		}
		if _, err = i.ensureCommandPolicy(ctx, changed); err == nil {
			t.Fatal("changed pending policy admitted")
		}
		i.fault = nil
		drained, err = i.ensureCommandPolicy(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if drained.Commands.Mode != "drain" || drained.DeliveryActivation.Hash != active.DeliveryActivation.Hash {
			t.Fatal("migration enabled new rights")
		}
		if _, done, err := i.inspectCommandPolicy(ctx, r); err != nil || !done {
			t.Fatal(done, err)
		}
	}
}

func TestCommandAdmissionReviewIsReadOnlyAndRequiresDelivery(t *testing.T) {
	ctx := context.Background()
	spec, _, _, active, _ := managedCommandsFixture(t)
	before := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
	request, ready, err := ReviewCommandAdmission(ctx, spec.Root, protocol.Hash(active), "read")
	if err != nil || !ready || request.Mode != "read" || before != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) {
		t.Fatal("review changed authority", ready, err)
	}
	if _, _, err = ReviewCommandAdmission(ctx, spec.Root, protocol.Hash("other"), "read"); err == nil {
		t.Fatal("review changed predecessor")
	}
	if _, _, err = ReviewCommandAdmission(ctx, spec.Root, protocol.Hash(active), "mutation"); err == nil {
		t.Fatal("unknown domain mode accepted")
	}
	unqualified, _, _, report := qualificationFixture(t)
	tree := activationTreeHash(t, unqualified.Root, unqualified.State, unqualified.UnitDirectory)
	if _, ready, err = ReviewCommandAdmission(ctx, unqualified.Root, protocol.Hash(report.Binding.Manifest), "all"); err == nil || ready {
		t.Fatal("verification granted command rights", ready, err)
	}
	if tree != activationTreeHash(t, unqualified.Root, unqualified.State, unqualified.UnitDirectory) {
		t.Fatal("review fabricated admission")
	}
}
