package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/dryaf/deploy/internal/safefs"
)

// EnsureVerification is a typed first-install/resume facade. It never admits
// production or treats an arbitrary edited runtime as an authorized migration.
// Existing unchanged installations are verified without restarting their service.
func EnsureVerification(ctx context.Context, spec ManagedSpec) (ManagedManifest, error) {
	return newManagedInstaller().ensureVerification(ctx, spec)
}
func (i managedInstaller) ensureVerification(ctx context.Context, spec ManagedSpec) (ManagedManifest, error) {
	var zero ManagedManifest
	if err := spec.validate(); err != nil {
		return zero, err
	}
	raw, err := readManagedInput(spec.RuntimeFile, safefs.MaxRecordBytes)
	if err != nil {
		return zero, err
	}
	if _, err = decodeRuntime(raw); err != nil {
		return zero, err
	}
	d, err := privateManagedDir(spec.Root, true)
	if errors.Is(err, os.ErrNotExist) {
		if _, err = i.prepare(ctx, "setup", spec); err != nil {
			return zero, err
		}
		return i.resume(ctx, spec.Root)
	}
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
	m := j.Intent.After
	if m.Mode != "verification" || m.Root != spec.Root || m.Paths.State != spec.State || m.UnitDirectory != spec.UnitDirectory || m.RuntimeHash != managedDigest(raw) || m.BinaryHash != spec.BinarySHA256 || !reflect.DeepEqual(m.Paths.DeniedPaths, spec.DeniedPaths) || !reflect.DeepEqual(m.FreshSignal, spec.FreshSignal) {
		return zero, fmt.Errorf("existing installation differs; an explicit domain migration is required")
	}
	return i.resumeLocked(ctx, d, j, lock)
}

// InspectVerification checks canonical completion and artifacts without resuming
// a pending install. It is suitable for an outer journal's child receipt check.
func InspectVerification(ctx context.Context, spec ManagedSpec) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectVerification(ctx, spec)
}
func (i managedInstaller) inspectVerification(ctx context.Context, spec ManagedSpec) (ManagedManifest, bool, error) {
	var zero ManagedManifest
	if err := spec.validate(); err != nil {
		return zero, false, err
	}
	d, err := privateManagedDir(spec.Root, true)
	if errors.Is(err, os.ErrNotExist) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", true, false)
	if err != nil {
		return zero, false, err
	}
	defer lock.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return zero, false, err
	}
	if j.Phase != "complete" {
		return zero, false, nil
	}
	raw, err := readManagedInput(spec.RuntimeFile, safefs.MaxRecordBytes)
	if err != nil {
		return zero, false, err
	}
	m := j.Intent.After
	if m.Mode != "verification" || m.Root != spec.Root || m.Paths.State != spec.State || m.UnitDirectory != spec.UnitDirectory || m.RuntimeHash != managedDigest(raw) || m.BinaryHash != spec.BinarySHA256 || !reflect.DeepEqual(m.Paths.DeniedPaths, spec.DeniedPaths) || !reflect.DeepEqual(m.FreshSignal, spec.FreshSignal) {
		return zero, false, fmt.Errorf("canonical installation differs from desired verification input")
	}
	m, err = i.resumeLocked(ctx, d, j, lock)
	return m, err == nil, err
}

// InstallationObservation contains no provider configuration or credential
// values. A completed service is checked against its canonical retained pins.
type InstallationObservation struct {
	Manifest ManagedManifest
	Phase    string
	Unbound  bool
	LEDs     bool
}

func InspectInstallation(ctx context.Context, root string) (InstallationObservation, error) {
	return newManagedInstaller().inspectInstallation(ctx, root)
}

// InspectRetainedInstallation verifies retained authority without requiring a
// healthy running service; removal must remain possible after service failure.
func InspectRetainedInstallation(ctx context.Context, root string) (InstallationObservation, error) {
	return newManagedInstaller().observeInstallation(ctx, root, false)
}
func (i managedInstaller) inspectInstallation(ctx context.Context, root string) (InstallationObservation, error) {
	return i.observeInstallation(ctx, root, true)
}
func (i managedInstaller) observeInstallation(ctx context.Context, root string, verifyService bool) (InstallationObservation, error) {
	var result InstallationObservation
	d, err := privateManagedDir(root, true)
	if err != nil {
		return result, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return result, err
	}
	if j.Intent.After.Root != root {
		return result, fmt.Errorf("installation root changed")
	}
	if err = validateManagedPointer(d, j, managedPhase(j.Phase) >= managedPhase("published")); err != nil {
		return result, err
	}
	c, err := verifyManagedArtifacts(d, j.Intent.After)
	if err != nil {
		return result, err
	}
	result = InstallationObservation{Manifest: j.Intent.After, Phase: j.Phase, Unbound: c.Bootstrap, LEDs: c.LEDSocket != ""}
	if j.Phase != "complete" || !verifyService && j.Intent.After.Mode != "uninstalled" {
		return result, nil
	}
	// The complete branch verifies artifact, manager and runtime receipts without
	// performing a stop/start or completing any pending operation.
	_, err = i.resumeLocked(ctx, d, j, lock)
	return result, err
}

// UninstallVerification retains database, identities and audit history. The
// expected manifest binds the explicit removal decision and survives retries.
func UninstallVerification(ctx context.Context, root, expected string) (ManagedManifest, error) {
	if !safefs.DigestPattern.MatchString(expected) {
		return ManagedManifest{}, fmt.Errorf("exact installation receipt required for uninstall")
	}
	return newManagedInstaller().uninstallExpected(ctx, root, expected)
}
