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

// ManagedLEDOutput pins output permission separately from bindings and fault
// history. A disabled binding remains enrolled; its incidents are not resolved.
type ManagedLEDOutput struct {
	Operation string `json:"operation"`
	Disabled  bool   `json:"disabled"`
}

func validateLEDOutputJournal(j managedJournal) error {
	i := j.Intent
	if i.Version != managedVersion || i.Operation != "set-led-output" || !safefs.ValidID(i.ID, "n") || i.Before == nil || i.Supersedes != nil || i.Before.validate() != nil || i.After.validate() != nil || managedPhase(j.Phase) == 0 {
		return fmt.Errorf("invalid LED output lifecycle intent")
	}
	b := *i.Before
	if (b.Mode != "verification" && b.Mode != "delivery") || b.RecoveryCommitHash != "" || (b.Mode == "verification" && b.DeliveryOwnerHash != "") || (b.Mode == "delivery" && b.DeliveryActivation == nil) || i.After.LEDOutput == nil || i.After.LEDOutput.Operation != i.ID {
		return fmt.Errorf("LED output change requires unrecovered verification or retained delivery authority")
	}
	if b.LEDOutput != nil && b.LEDOutput.Operation == i.ID || !i.After.LEDOutput.Disabled && (b.LEDOutput == nil || !b.LEDOutput.Disabled) {
		return fmt.Errorf("invalid LED output predecessor")
	}
	b.LEDOutput = i.After.LEDOutput
	if protocol.Hash(b) != protocol.Hash(i.After) || i.BeforeUnit != i.AfterUnit || managedDigest([]byte(i.AfterUnit)) != i.After.UnitHash {
		return fmt.Errorf("LED output change modified pinned runtime or service")
	}
	want, err := renderManagedUnit(i.After)
	if err != nil {
		return err
	}
	if want != i.AfterUnit {
		return fmt.Errorf("LED output service bytes changed")
	}
	return nil
}

func requireManagedLEDOutput(ctx context.Context, s *worker.Store, m ManagedManifest) error {
	if m.LEDOutput == nil {
		return s.RequireLEDOutput(ctx, "", false)
	}
	return s.RequireLEDOutput(ctx, m.LEDOutput.Operation, m.LEDOutput.Disabled)
}

func (i managedInstaller) probeLEDOutput(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	if m.LEDOutput == nil {
		return nil
	}
	probe := i.ledOutputProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			capability := "__managed-led-output-probe"
			if m.Mode == "delivery" {
				capability = "__managed-delivery-led-output-probe"
			}
			return probeManagedCapability(ctx, d, name, hash, capability)
		}
	}
	if err := probe(ctx, d, "releases/"+m.Revision+"/deploy", m.BinaryHash); err != nil {
		return err
	}
	return probe(ctx, d, managedHelperName(m), m.HelperHash)
}

// SetLEDOutput changes only an existing gateway's output permission.
// expected is the complete manifest fingerprint reviewed by the coordinator.
// The managed resume operation continues an interruption using the same ID.
func SetLEDOutput(ctx context.Context, root, expected string, disabled bool) (ManagedManifest, error) {
	return newManagedInstaller().ensureLEDOutput(ctx, root, expected, disabled)
}

func (i managedInstaller) ensureLEDOutput(ctx context.Context, root, expected string, disabled bool) (ManagedManifest, error) {
	d, err := privateManagedDir(root, true)
	if err != nil {
		return ManagedManifest{}, err
	}
	lock, err := d.Lock("operation.lock", true, false)
	if err != nil {
		d.Close()
		return ManagedManifest{}, err
	}
	j, err := readManagedJournal(d)
	if err == nil && j.Intent.Operation == "set-led-output" && j.Intent.Before != nil && protocol.Hash(*j.Intent.Before) == expected && j.Intent.After.LEDOutput != nil && j.Intent.After.LEDOutput.Disabled == disabled {
		m, e := i.resumeLocked(ctx, d, j, lock)
		lock.Close()
		d.Close()
		return m, e
	}
	lock.Close()
	d.Close()
	if err != nil {
		return ManagedManifest{}, err
	}
	return i.prepareLEDOutput(ctx, root, expected, disabled)
}

func (i managedInstaller) prepareLEDOutput(ctx context.Context, root, expected string, disabled bool) (ManagedManifest, error) {
	var zero ManagedManifest
	if !safefs.DigestPattern.MatchString(expected) {
		return zero, fmt.Errorf("LED output change requires reviewed installation fingerprint")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return zero, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", true, false)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return zero, err
	}
	if j.Phase != "complete" {
		return zero, fmt.Errorf("unfinished managed operation; use resume")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return zero, err
	}
	b := j.Intent.After
	if b.Root != root || protocol.Hash(b) != expected || (b.Mode != "verification" && b.Mode != "delivery") || (b.Mode == "verification" && b.DeliveryOwnerHash != "") || b.RecoveryCommitHash != "" {
		return zero, fmt.Errorf("LED output change requires exact unrecovered installation")
	}
	c, err := verifyManagedArtifacts(d, b)
	if err != nil {
		return zero, err
	}
	if c.LEDSocket == "" {
		return zero, fmt.Errorf("installation has no declared LED daemon")
	}
	if b.Mode == "delivery" {
		if err = reviewDeliveryLEDOutput(d, b, c, disabled, time.Now().UTC()); err != nil {
			return zero, err
		}
	}
	units, err := managedUnitDirectory(b.UnitDirectory)
	if err != nil {
		return zero, err
	}
	defer units.Close()
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return zero, err
	}
	if !present || managedDigest(unit) != b.UnitHash {
		return zero, fmt.Errorf("managed unit changed")
	}
	if _, err = inspectManagedService(ctx, i.services, b, false); err != nil {
		return zero, err
	}
	if b.LEDOutput == nil && !disabled || b.LEDOutput != nil && b.LEDOutput.Disabled == disabled {
		return b, i.services.Verify(ctx, b)
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return zero, err
	}
	after := b
	after.LEDOutput = &ManagedLEDOutput{Operation: id, Disabled: disabled}
	if err = i.probeLEDOutput(ctx, d, after); err != nil {
		return zero, err
	}
	if err = limitManagedHistory(d); err != nil {
		return zero, err
	}
	j = managedJournal{Intent: managedIntent{Version: managedVersion, ID: id, Operation: "set-led-output", Before: &b, After: after, BeforeUnit: string(unit), AfterUnit: string(unit)}, Phase: "prepared"}
	if err = writeManagedIntent(d, j); err != nil {
		return zero, err
	}
	if err = i.boundary("prepared"); err != nil {
		return zero, err
	}
	return i.resumeLocked(ctx, d, j, lock)
}

func (i managedInstaller) configureLEDOutput(ctx context.Context, d *safefs.Directory, j managedJournal, c Runtime) error {
	m := j.Intent.After
	if err := validateLEDOutputJournal(j); err != nil {
		return err
	}
	if c.LEDSocket == "" {
		return fmt.Errorf("LED output has no daemon socket")
	}
	if err := i.probeLEDOutput(ctx, d, m); err != nil {
		return err
	}
	s, err := openCanonicalManagedStore(m, c)
	if err != nil {
		return err
	}
	status, err := s.LEDOutputStatus(ctx)
	if err == nil {
		if managedPhase(j.Phase) >= managedPhase("stopped") {
			err = requireManagedLEDOutput(ctx, s, m)
		} else if status.Operation != j.Intent.ID {
			err = requireManagedLEDOutput(ctx, s, *j.Intent.Before)
		}
	}
	s.Close()
	if err != nil || managedPhase(j.Phase) >= managedPhase("stopped") {
		return err
	}
	var api worker.LEDAPI = led.Client{Socket: c.LEDSocket, Budget: 2 * time.Second, Endpoint: c.LEDEndpoint}
	if i.ledAPI != nil {
		api = i.ledAPI(c.LEDSocket)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_, err = worker.ConfigureLEDOutput(ctx, m.Paths.State, c.Worker, worker.LEDOutputRequest{Identity: m.Identity, Operation: j.Intent.ID, Socket: c.LEDSocket, Disabled: m.LEDOutput.Disabled}, api)
	return err
}
