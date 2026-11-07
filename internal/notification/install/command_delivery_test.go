package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestCommandDeliveryContinuationPreservesOriginalAdmission(t *testing.T) {
	ctx := context.Background()
	spec, i, _, original, qualification := managedCommandsFixture(t)
	current := original
	for _, mode := range []string{"read", "drain", "all", "drain"} {
		r := CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(current), Operation: "enable-commands", Mode: mode, QualificationHash: protocol.Hash(qualification)}
		if mode == "drain" {
			r.Operation, r.Mode, r.QualificationHash = "disable-commands", "", ""
		}
		var err error
		current, err = i.ensureCommandPolicy(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		tree := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
		got, err := InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(current), protocol.Hash(original))
		if err != nil || protocol.Hash(got) != protocol.Hash(current) {
			t.Fatal("lost original delivery", err)
		}
		if tree != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) {
			t.Fatal("continuation inspection mutated installation")
		}
		if _, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(current), protocol.Hash("foreign")); err == nil {
			t.Fatal("accepted unrelated predecessor")
		}
	}
	if _, err := InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(original), protocol.Hash(original)); err == nil {
		t.Fatal("accepted stale current receipt")
	}
	var intent managedIntent
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.ReadJSON("operations/"+current.Commands.Operation+"/intent.json", &intent); err != nil {
		t.Fatal(err)
	}
	// Damage an intermediate transition while retaining the current head.
	if err = os.Remove(filepath.Join(spec.Root, "operations", intent.Before.Commands.Operation, "command-admission.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(current), protocol.Hash(original)); err == nil {
		t.Fatal("accepted missing intermediate authority")
	}
}

func TestCommandDeliveryContinuationDoesNotCompletePendingAdmission(t *testing.T) {
	ctx := context.Background()
	spec, i, _, original, qualification := managedCommandsFixture(t)
	stop := errors.New("interrupted command publication")
	i.fault = func(at string) error {
		if at == "manifest-published" {
			return stop
		}
		return nil
	}
	_, err := i.ensureCommandPolicy(ctx, CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(original), Operation: "enable-commands", Mode: "read", QualificationHash: protocol.Hash(qualification)})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		t.Fatal(err)
	}
	tree := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
	if _, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(j.Intent.After), protocol.Hash(original)); err == nil {
		t.Fatal("accepted pending admission")
	}
	if tree != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) {
		t.Fatal("inspection resumed pending operation")
	}
}
