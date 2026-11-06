package install

import (
	"context"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestManagedSignalMigrationProfile(t *testing.T) {
	spec, i, _, before, service, record := managedSignalDeliveryFixtureEnrolled(t, nil, true)
	ctx := context.Background()
	m, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
	testutil.Must(t, err)
	c, effective := effectiveRuntimeForTest(t, m)
	bootstrap := service.Endpoint.Installation.LegacyBootstrap
	if bootstrap == "" {
		t.Fatal("fixture did not retain legacy bootstrap")
	}
	request := ManagedProducerRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Producer: c.Worker.Producers[0].Producer, Generation: "legacy-adoption", Route: "ops"}
	testutil.Must(t, i.withManagedProducer(ctx, request, func(session ManagedProducerSession) error {
		migration := session.(ManagedSignalMigrationSession)
		p, err := migration.SignalMigrationProfile(ctx, service.Endpoint.Spec.AccountState, bootstrap, protocol.Hash(c.Signal.Account), "signal", "")
		if err != nil {
			return err
		}
		if protocol.Hash(p) != protocol.Hash(effective.Providers["signal"]) {
			t.Fatal("profile differs from admitted runtime")
		}
		for _, input := range [][4]string{
			{service.Endpoint.Spec.AccountState + "/foreign", bootstrap, "signal", ""},
			{service.Endpoint.Spec.AccountState, protocol.Hash("foreign"), "signal", ""},
			{service.Endpoint.Spec.AccountState, bootstrap, "telegram", "signal"},
		} {
			if _, err := migration.SignalMigrationProfile(ctx, input[0], input[1], protocol.Hash(c.Signal.Account), input[2], input[3]); err == nil {
				t.Fatal("foreign migration authority accepted")
			}
		}
		if _, err := migration.SignalMigrationProfile(ctx, service.Endpoint.Spec.AccountState, bootstrap, protocol.Hash("another-selector"), "signal", ""); err == nil {
			t.Fatal("changed CLI account selector accepted")
		}
		status, err := session.Status()
		if err != nil {
			return err
		}
		if status.State != "unknown" || status.Active {
			t.Fatal("profile review granted delivery authority")
		}
		return nil
	}))
}
