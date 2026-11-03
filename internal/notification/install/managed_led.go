package install

import (
	"context"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func validateManagedLEDChange(before, after Runtime) error {
	old, next := before, after
	old.Worker.LEDBindings, next.Worker.LEDBindings = nil, nil
	if before.LEDSocket == "" || protocol.Hash(old) != protocol.Hash(next) {
		return fmt.Errorf("managed LED migration requires changed bindings and unchanged socket, providers, enrollment and limits")
	}
	return worker.ValidateLEDBindingChange(before.Worker, after.Worker)
}

func (i managedInstaller) probeLEDMapping(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	probe := i.ledProbe
	if probe == nil {
		probe = probeManagedLEDMapping
	}
	if err := probe(ctx, d, "releases/"+m.Revision+"/deploy", m.BinaryHash); err != nil {
		return err
	}
	return probe(ctx, d, managedHelperName(m), m.HelperHash)
}

func managedLEDPair(d *safefs.Directory, j managedJournal, after Runtime) (Runtime, error) {
	if j.Intent.Before == nil {
		return Runtime{}, fmt.Errorf("LED migration lacks prior installation")
	}
	before, err := managedRuntime(d, *j.Intent.Before)
	if err != nil {
		return before, err
	}
	return before, validateManagedLEDChange(before, after)
}

func (i managedInstaller) migrateLEDMapping(ctx context.Context, d *safefs.Directory, j managedJournal, after Runtime) error {
	before, err := managedLEDPair(d, j, after)
	if err != nil {
		return err
	}
	if err = i.probeLEDMapping(ctx, d, j.Intent.After); err != nil {
		return err
	}
	var api worker.LEDAPI = led.Client{Socket: before.LEDSocket, Budget: 2 * time.Second}
	if i.ledAPI != nil {
		api = i.ledAPI(before.LEDSocket)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	m := j.Intent.After
	_, err = worker.MigrateManagedLEDBindings(ctx, m.Paths.State, before.Worker, after.Worker, before.LEDSocket, api, m.Identity, j.Intent.ID)
	return err
}

func verifyManagedLEDCompletion(ctx context.Context, d *safefs.Directory, j managedJournal, after Runtime) error {
	before, err := managedLEDPair(d, j, after)
	if err != nil {
		return err
	}
	m := j.Intent.After
	return worker.VerifyManagedLEDMapping(ctx, m.Paths.State, before.Worker, after.Worker, before.LEDSocket, m.Identity, j.Intent.ID)
}
