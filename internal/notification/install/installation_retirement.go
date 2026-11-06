package install

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

// ReplacementTarget is reviewed by the old owner before the terminal transition.
// The new UID receives only a copy of its completion record, never old authority.
type ReplacementTarget struct {
	Version int     `json:"version"`
	UID     int     `json:"uid"`
	Root    string  `json:"root"`
	State   string  `json:"state"`
	Runtime Runtime `json:"runtime"`
	Reason  string  `json:"reason"`
}
type InstallationRetirement struct {
	Version  int                           `json:"version"`
	Previous ManagedManifest               `json:"previous"`
	Target   ReplacementTarget             `json:"target"`
	Evidence worker.InstallationRetirement `json:"evidence"`
}

func (t ReplacementTarget) effectiveRuntime() (Runtime, error) {
	b, err := json.Marshal(t.Runtime)
	if err != nil {
		return Runtime{}, err
	}
	return decodeRuntime(b)
}
func (t ReplacementTarget) validate() error {
	if t.Version != 1 || t.UID < 0 || !managedPath(t.Root) || !managedPath(t.State) || managedWithin(t.Root, t.State) || managedWithin(t.State, t.Root) {
		return fmt.Errorf("invalid replacement UID or separate absolute root/state")
	}
	if err := worker.ValidateProducerRetirementReason("replacement", t.Reason); err != nil {
		return err
	}
	b, err := json.Marshal(t.Runtime)
	if err != nil {
		return err
	}
	_, err = decodeRuntime(b)
	return err
}
func validateReplacement(t ReplacementTarget, m ManagedManifest, c Runtime) error {
	if err := t.validate(); err != nil {
		return err
	}
	for _, old := range []string{m.Root, m.Paths.State} {
		for _, next := range []string{t.Root, t.State} {
			if managedWithin(old, next) || managedWithin(next, old) {
				return fmt.Errorf("replacement must preserve old installation and state at separate paths")
			}
		}
	}
	for _, old := range c.Worker.Producers {
		for _, next := range t.Runtime.Worker.Producers {
			if old.Producer == next.Producer {
				return fmt.Errorf("replacement requires fresh producer epochs and new specialist enrollment")
			}
		}
	}
	if c.Telegram != nil && t.Runtime.Telegram != nil && c.Telegram.Account == t.Runtime.Telegram.Account {
		return fmt.Errorf("fresh replacement intake requires a new Telegram account; use recovery to retain an account")
	}
	if c.Signal != nil && t.Runtime.Signal != nil && c.Signal.Account == t.Runtime.Signal.Account {
		return fmt.Errorf("fresh replacement intake requires a new Signal account; use recovery to retain an account")
	}
	return nil
}
func (v InstallationRetirement) validate() error {
	if v.Version != 1 || v.Previous.validate() != nil || v.Previous.Mode != "uninstalled" || v.Target.validate() != nil || v.Evidence.Version != 1 || v.Evidence.Identity != v.Previous.Identity || v.Evidence.TargetHash != protocol.Hash(v.Target) || v.Evidence.UID < 0 {
		return fmt.Errorf("invalid installation retirement certificate")
	}
	if len(v.Evidence.Producers) == 0 {
		return fmt.Errorf("missing retired producers")
	}
	for _, p := range v.Evidence.Producers {
		if p.Identity.Worker != v.Previous.Identity.Worker || !p.Producer.Valid() || !safefs.DigestPattern.MatchString(p.EvidenceHash) {
			return fmt.Errorf("invalid retained producer retirement")
		}
		for _, next := range v.Target.Runtime.Worker.Producers {
			if p.Producer == next.Producer {
				return fmt.Errorf("retired producer epoch reused")
			}
		}
	}
	return nil
}
func requireManagedNotRetired(d *safefs.Directory) error {
	f, err := d.Open("retirement-target.json", os.O_RDONLY, 0)
	if err == nil {
		f.Close()
		return fmt.Errorf("installation terminal retirement started; resume installation-retire and retain it for audit")
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}
func (i managedInstaller) retireInstallation(ctx context.Context, root, manifestHash string, target ReplacementTarget) (out InstallationRetirement, err error) {
	if err = target.validate(); err != nil {
		return
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return
	}
	defer d.Close()
	op, err := d.Lock("operation.lock", true, false)
	if err != nil {
		return
	}
	defer op.Close()
	lease, err := d.Lock("installation.lock", true, false)
	if err != nil {
		return
	}
	defer lease.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return
	}
	m := j.Intent.After
	if j.Phase != "complete" || m.Mode != "uninstalled" || protocol.Hash(m) != manifestHash || m.Root != root {
		return out, fmt.Errorf("retirement requires the exact completed uninstalled manifest")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return
	}
	c, err := verifyManagedArtifacts(d, m)
	if err != nil {
		return
	}
	if err = validateReplacement(target, m, c); err != nil {
		return
	}
	units, err := managedUnitDirectory(m.UnitDirectory)
	if err != nil {
		return
	}
	defer units.Close()
	if _, present, e := managedUnitBytes(units); e != nil {
		return out, e
	} else if present {
		return out, fmt.Errorf("retired service unit reappeared")
	}
	state, err := inspectManagedService(ctx, i.services, m, true)
	if err != nil {
		return
	}
	if state.Active != "inactive" || state.Load != "not-found" || state.UnitFile == "enabled" {
		return out, fmt.Errorf("old worker service is still present")
	}
	if m.SignalVerification != nil && m.SignalVerification.ServiceRoot != "" {
		sd, lock, e := openSignalServiceRoot(m.SignalVerification.ServiceRoot, false)
		if e != nil {
			return out, e
		}
		defer sd.Close()
		defer lock.Close()
		sm, e := readSignalService(sd, "intent.json")
		if e != nil {
			return out, e
		}
		sj, e := readSignalServiceJournal(sd, sm)
		if e != nil {
			return out, e
		}
		if sj.Phase != "removed" || sm.ManifestHash != m.SignalVerification.ServiceHash {
			return out, fmt.Errorf("uninstall the exact old Signal service before retirement")
		}
	}
	s, err := openManagedStore(m, c)
	if err != nil {
		return
	}
	defer s.Close()
	var api worker.LEDAPI
	if c.LEDSocket != "" {
		api = led.Client{Socket: c.LEDSocket, Budget: 2 * time.Second}
		if i.ledAPI != nil {
			api = i.ledAPI(c.LEDSocket)
		}
	}
	if err = s.ReviewInstallationRetirement(ctx, protocol.Hash(target), c.LEDSocket, api); err != nil {
		return
	}
	// Publish the managed fence before contacting the daemon. A retry must use
	// exactly this target and the independent worker retirement evidence.
	var prior ReplacementTarget
	if e := d.ReadJSON("retirement-target.json", &prior); e == nil {
		if protocol.Hash(prior) != protocol.Hash(target) {
			return out, fmt.Errorf("retirement target changed")
		}
	} else if os.IsNotExist(e) {
		if err = d.WriteJSON("retirement-target.json", target, false); err != nil {
			return
		}
	} else {
		return out, e
	}
	if err = i.boundary("retirement-target"); err != nil {
		return
	}
	evidence, err := s.RetireInstallation(ctx, protocol.Hash(target), c.LEDSocket, api)
	if err != nil {
		return
	}
	out = InstallationRetirement{1, m, target, evidence}
	if err = i.boundary("retirement-worker"); err != nil {
		return
	}
	if err = runtimeJSONOnceForService(d, "retirement.json", out); err != nil {
		return
	}
	err = i.boundary("retirement-published")
	return
}

// initializeReplacement runs under the target UID. The separately supplied hash
// authenticates the privately transferred certificate selected by the operator.
// It creates no service and imports no old queue, command grants or receipt IDs.
func initializeReplacement(ctx context.Context, certificate InstallationRetirement, expected string) (protocol.Identity, error) {
	return initializeReplacementWithFault(ctx, certificate, expected, nil)
}
func initializeReplacementWithFault(ctx context.Context, certificate InstallationRetirement, expected string, fault func(string) error) (protocol.Identity, error) {
	boundary := func(stage string) error {
		if fault != nil {
			return fault(stage)
		}
		return nil
	}
	var zero protocol.Identity
	if certificate.validate() != nil || protocol.Hash(certificate) != expected || certificate.Target.UID != os.Getuid() {
		return zero, fmt.Errorf("replacement certificate, digest or target UID mismatch")
	}
	t := certificate.Target
	parent, err := privateManagedDir(filepath.Dir(t.State), false)
	if err != nil {
		return zero, err
	}
	defer parent.Close()
	// The state directory may be precreated by an administrator for another UID;
	// it must remain private and contain no unrelated database.
	d, err := privateManagedDir(t.State, true)
	if err != nil {
		return zero, err
	}
	defer d.Close()
	lock, err := d.Lock("replacement.lock", true, true)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	var pinned InstallationRetirement
	err = d.ReadJSON("replacement.json", &pinned)
	if os.IsNotExist(err) {
		f, e := d.Open("state.db", os.O_RDONLY, 0)
		if e == nil {
			f.Close()
			return zero, fmt.Errorf("replacement state already contains a database")
		}
		if !os.IsNotExist(e) {
			return zero, e
		}
		names, e := d.File().Readdirnames(-1)
		if e != nil {
			return zero, e
		}
		for _, name := range names {
			if name != "replacement.lock" {
				return zero, fmt.Errorf("replacement requires an empty private state directory")
			}
		}
		if err = d.WriteJSON("replacement.json", certificate, false); err != nil {
			return zero, err
		}
	} else if err != nil {
		return zero, err
	} else if protocol.Hash(pinned) != expected {
		return zero, fmt.Errorf("replacement certificate changed")
	}
	if err = boundary("replacement-certificate"); err != nil {
		return zero, err
	}
	create := false
	f, err := d.Open("state.db", os.O_RDONLY, 0)
	if err == nil {
		f.Close()
	} else if os.IsNotExist(err) {
		create = true
	} else {
		return zero, err
	}
	effective, err := t.effectiveRuntime()
	if err != nil {
		return zero, err
	}
	s, err := worker.Open(t.State, effective.Worker, create)
	if err != nil {
		return zero, err
	}
	defer s.Close()
	if s.Identity().Worker == certificate.Previous.Identity.Worker {
		return zero, fmt.Errorf("replacement reused old worker identity")
	}

	if err = boundary("replacement-created"); err != nil {
		return zero, err
	}
	var identity protocol.Identity
	if e := d.ReadJSON("replacement-identity.json", &identity); e == nil {
		if identity != s.Identity() {
			return zero, fmt.Errorf("replacement identity changed")
		}
	} else if os.IsNotExist(e) {
		if err = s.RequireFreshReplacement(ctx); err != nil {
			return zero, err
		}
		if err = d.WriteJSON("replacement-identity.json", s.Identity(), false); err != nil {
			return zero, err
		}
	} else {
		return zero, e
	}
	if err = boundary("replacement-identity"); err != nil {
		return zero, err
	}
	return s.Identity(), nil
}

func verifyReplacementEnrollment(spec ManagedSpec, c Runtime) error {
	d, err := privateManagedDir(spec.State, true)
	if err != nil {
		return err
	}
	defer d.Close()
	var v InstallationRetirement
	err = d.ReadJSON("replacement.json", &v)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	effective, e := v.Target.effectiveRuntime()
	if e != nil || v.validate() != nil || v.Target.UID != os.Getuid() || v.Target.Root != spec.Root || v.Target.State != spec.State || protocol.Hash(effective) != protocol.Hash(c) {
		return fmt.Errorf("replacement setup differs from the retired owner's reviewed target")
	}
	var id, actual protocol.Identity
	if err = d.ReadJSON("replacement-identity.json", &id); err != nil {
		return err
	}
	if err = d.ReadJSON("identity.json", &actual); err != nil {
		return err
	}
	if id != actual || id.Worker == v.Previous.Identity.Worker {
		return fmt.Errorf("replacement identity changed")
	}
	return nil
}
