package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func validateManagedBootstrapChange(m ManagedManifest, before, after Runtime) error {
	if m.Mode != "verification" || m.RecoveryCommitHash != "" || m.DeliveryOwnerHash != "" || m.Commands != nil || !before.Bootstrap || after.Bootstrap || len(after.Worker.Producers) == 0 {
		return fmt.Errorf("enrollment requires a pristine unbound verification gateway")
	}
	old, next := before, after
	next.Bootstrap = true
	next.Worker.Producers = nil
	if after.Worker.Interaction != nil {
		// Initial Telegram intake uses an already owned fixed-alert credential.
		// Signal intake needs a separate native inbox ownership handoff.
		if before.Worker.Interaction != nil || before.Telegram != nil || before.Signal != nil || after.Telegram == nil || after.Signal != nil {
			return fmt.Errorf("initial command enrollment requires new Telegram intake on the existing credential")
		}
		credential := false
		for _, p := range before.Providers {
			if p.Kind == "telegram" && p.Conversation == nil && p.TokenFile == after.Telegram.TokenFile {
				credential = true
			}
		}
		if !credential {
			return fmt.Errorf("initial command intake must retain the installed Telegram credential")
		}
		for _, destination := range after.Worker.Interaction.Destinations {
			if destination.Provider != "telegram" || destination.Account != after.Telegram.Account {
				return fmt.Errorf("initial command enrollment requires the declared Telegram account")
			}
		}
		next.Worker.Interaction, next.Telegram = nil, nil
	}
	if protocol.Hash(old) != protocol.Hash(next) {
		return fmt.Errorf("initial enrollment may only bind consumers and command intake; alert providers, routes and budgets must remain unchanged")
	}
	return after.Worker.Validate()
}
func managedBootstrapPair(d *safefs.Directory, j managedJournal, after Runtime) (Runtime, error) {
	if j.Intent.Before == nil {
		return Runtime{}, fmt.Errorf("bootstrap enrollment lacks prior manifest")
	}
	before, err := managedRuntime(d, *j.Intent.Before)
	if err != nil {
		return before, err
	}
	return before, validateManagedBootstrapChange(*j.Intent.Before, before, after)
}

// EnrollVerificationConsumers retains the gateway installation/worker identities
// and records a forward-only migration. expected is the reviewed current manifest
// hash. A lost response is resumed through the existing installation journal.
func EnrollVerificationConsumers(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().enrollVerificationConsumers(ctx, spec, expected)
}
func (i managedInstaller) enrollVerificationConsumers(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	if !safefs.DigestPattern.MatchString(expected) {
		return ManagedManifest{}, fmt.Errorf("exact prior installation receipt required")
	}
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		return ManagedManifest{}, err
	}
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		d.Close()
		return ManagedManifest{}, err
	}
	j, err := readManagedJournal(d)
	lock.Close()
	d.Close()
	if err != nil {
		return ManagedManifest{}, err
	}
	if j.Intent.Operation == "enroll-bootstrap" && j.Intent.Before != nil && protocol.Hash(*j.Intent.Before) == expected {
		return i.ensureVerification(ctx, spec)
	}
	return i.prepareExpected(ctx, "enroll-bootstrap", spec, expected)
}

func (i managedInstaller) requireBootstrapEnrollmentLEDCapability(ctx context.Context, d *safefs.Directory, m ManagedManifest, required bool) error {
	present, err := worker.BootstrapEnrollmentLEDRequired(m.Paths.State)
	if err != nil {
		return err
	}
	if !required && !present {
		return nil
	}
	probe := i.bootstrapEnrollmentLEDProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-bootstrap-enrollment-led-probe")
		}
	}
	for _, binary := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err = probe(ctx, d, binary.name, binary.hash); err != nil {
			return err
		}
	}
	return nil
}
