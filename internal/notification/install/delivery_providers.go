package install

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

// A credential rotation retains one exact pause and leaves all producer/route
// authority unchanged. This is private lifecycle evidence, not desired YAML.
type deliveryProviderTransition struct {
	Kind           string `json:"kind,omitempty"`
	RuntimeHash    string `json:"runtime_hash"`
	ConfigHash     string `json:"config_hash"`
	Version        int    `json:"version"`
	PauseOperation string `json:"pause_operation"`
	PauseHash      string `json:"pause_hash"`
}

type DeliveryProviderReview struct {
	Before      ManagedManifest `json:"before"`
	Credentials []string        `json:"credentials"`
}

// ReviewDeliveryProviderRotation checks the candidate and both retained helper
// capabilities before a coordinator pauses delivery or fences report services.
// The returned paths only describe exclusions; they contain no secret values.
func ReviewDeliveryProviderRotation(ctx context.Context, root, expected string, runtime []byte) (DeliveryProviderReview, error) {
	return newManagedInstaller().reviewDeliveryProviderRotation(ctx, root, expected, runtime)
}
func (i managedInstaller) reviewDeliveryProviderRotation(ctx context.Context, root, expected string, raw []byte) (DeliveryProviderReview, error) {
	return i.reviewPausedRuntimeChange(ctx, root, expected, raw, "")
}
func (i managedInstaller) reviewPausedRuntimeChange(ctx context.Context, root, expected string, raw []byte, kind string) (DeliveryProviderReview, error) {
	var out DeliveryProviderReview
	if !managedPath(root) || !safefs.DigestPattern.MatchString(expected) {
		return out, fmt.Errorf("credential review requires exact active installation pins")
	}
	after, err := decodeRuntime(raw)
	if err != nil {
		return out, err
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return out, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return out, err
	}
	defer lock.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return out, err
	}
	m := j.Intent.After
	if j.Phase != "complete" || m.Mode != "delivery" || m.Root != root || protocol.Hash(m) != expected {
		return out, fmt.Errorf("credential review requires unchanged completed delivery")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return out, err
	}
	before, err := verifyManagedArtifacts(d, m)
	if err != nil {
		return out, err
	}
	if _, _, err = readDeliveryAdmission(d, m, before); err != nil {
		return out, err
	}
	paused := m
	paused.Mode, paused.DeliveryActivation = "verification", nil
	if commandPausedPolicy(kind) {
		if !disabledManagedCommandPolicy(m) {
			return out, fmt.Errorf("disable commands before changing notification policy")
		}
		paused.Commands = nil
	}
	if err = validatePausedRuntimeChange(paused, before, after, kind); err != nil {
		return out, err
	}
	if kind == "command-declaration" {
		if err = i.probeCommandDeliveryProducerPolicy(ctx, d, m); err != nil {
			return out, err
		}
	} else if kind != "declaration" {
		if err = i.probeDeliveryPause(ctx, d, m); err != nil {
			return out, err
		}
		if err = i.probePausedRuntimeChange(ctx, d, m, kind); err != nil {
			return out, err
		}
	}

	if err = refuseManagedRecoveryPreparation(d, m); err != nil {
		return out, err
	}
	units, err := managedUnitDirectory(m.UnitDirectory)
	if err != nil {
		return out, err
	}
	defer units.Close()
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return out, err
	}
	if !present || managedDigest(unit) != m.UnitHash {
		return out, fmt.Errorf("delivery unit changed before credential review")
	}
	if kind != "" {
		if err = preflightDeliveryPolicy(ctx, m, before, after, kind); err != nil {
			return out, err
		}
	}
	if err = i.probeSignalDelivery(ctx, d, m, after); err != nil {
		return out, err
	}
	paths, err := retainedDeliveryCredentials(d, j, before)
	if err != nil {
		return out, err
	}
	nextPaths, err := runtimeCredentialPathsPolicy(m, after, false)
	if err != nil {
		return out, err
	}
	paths = append(paths, nextPaths...)
	slices.Sort(paths)
	return DeliveryProviderReview{Before: m, Credentials: slices.Compact(paths)}, nil
}

// ValidateDeliveryProviderRotation permits only direct Telegram file references
// for the same bot and destination. Pending work therefore keeps its audience.
func ValidateDeliveryProviderRotation(m ManagedManifest, before, after Runtime) error {
	if m.Mode != "verification" || m.DeliveryOwnerHash == "" || m.DeliveryActivation != nil || m.RecoveryCommitHash != "" || m.Commands != nil || m.CommandHistoryHash != "" || m.SignalVerification != nil || m.SignalProfiles != nil || before.Bootstrap || after.Bootstrap || before.Telegram != nil || after.Telegram != nil || before.Signal != nil || after.Signal != nil || before.Worker.Interaction != nil || after.Worker.Interaction != nil {
		return fmt.Errorf("credential rotation requires an unrecovered paused direct-alert owner without command history")
	}
	if protocol.Hash(before.Worker) != protocol.Hash(after.Worker) || len(before.Providers) != len(after.Providers) {
		return fmt.Errorf("credential rotation must retain routes, producers and provider identities")
	}
	changed := false
	for name, old := range before.Providers {
		next, ok := after.Providers[name]
		if !ok {
			return fmt.Errorf("credential rotation cannot remove a provider")
		}
		if old.Kind == "telegram" {
			if old.Conversation != nil || next.Conversation != nil {
				return fmt.Errorf("credential rotation cannot change command audiences")
			}
			for _, p := range []provider.Profile{old, next} {
				if err := provider.CheckTelegramProfile(p); err != nil {
					return err
				}
			}
			if old.Destination != next.Destination {
				return fmt.Errorf("credential rotation must retain the same Telegram bot and destination")
			}
			changed = changed || old.TokenFile != next.TokenFile || old.ChatFile != next.ChatFile
			next.TokenFile, next.ChatFile = old.TokenFile, old.ChatFile
		}
		if old.Kind == "signal" && protocol.Hash(old) != protocol.Hash(next) {
			if m.FreshSignal == nil {
				return fmt.Errorf("Signal delivery references require retained service ownership")
			}
			if err := validateSignalDeliveryProfileChange(old, next, false); err != nil {
				return err
			}
			changed = true
			next = old
		}
		if protocol.Hash(old) != protocol.Hash(next) {
			return fmt.Errorf("credential rotation changed provider authority")
		}
	}
	want := after
	want.Providers = before.Providers
	if !changed || protocol.Hash(want) != protocol.Hash(before) {
		return fmt.Errorf("change only Telegram credential-file references")
	}
	return nil
}

func validateDeliveryProviderJournal(j managedJournal) error {
	v := j.Intent
	b := v.Before
	a := v.After
	p := v.DeliveryProvider
	if v.Version != managedVersion || !isPausedRuntimeChange(v.Operation) || !safefs.ValidID(v.ID, "n") || b == nil || b.validate() != nil || a.validate() != nil || v.Supersedes != nil || managedPhase(j.Phase) == 0 || p == nil || p.Version != 1 || !safefs.ValidID(p.PauseOperation, "n") || !safefs.DigestPattern.MatchString(p.PauseHash) {
		return fmt.Errorf("invalid paused provider transition")
	}
	if b.Mode != "verification" || b.DeliveryOwnerHash == "" || b.DeliveryActivation != nil || b.Commands != nil || (!commandPausedPolicy(p.Kind) && b.CommandHistoryHash != "") || (commandPausedPolicy(p.Kind) && !safefs.DigestPattern.MatchString(b.CommandHistoryHash)) || b.RecoveryCommitHash != "" || b.SignalVerification != nil || b.SignalProfiles != nil || a.Revision == b.Revision || a.Revision != v.ID || a.ConfigHash == b.ConfigHash || a.RuntimeHash == b.RuntimeHash {
		return fmt.Errorf("provider transition requires a new paused runtime")
	}
	if p.RuntimeHash != a.RuntimeHash || p.ConfigHash != a.ConfigHash || pausedRuntimeOperation(p.Kind) != v.Operation {
		return fmt.Errorf("paused runtime transition changed its policy kind")
	}
	want := *b
	if commandProviderKind(p.Kind) || p.Kind == "command-routes" || p.Kind == "command-producer" || p.Kind == "command-freshness" || p.Kind == "command-limits" || p.Kind == "limits" || p.Kind == "producer" || p.Kind == "routes" || p.Kind == "destination" || p.Kind == "provider-enrollment" {
		if !safefs.DigestPattern.MatchString(a.DeliveryOwnerConfigHash) || (b.DeliveryOwnerConfigHash != "" && b.DeliveryOwnerConfigHash != a.DeliveryOwnerConfigHash) {
			return fmt.Errorf("limits transition changed original owner configuration")
		}
		owner := worker.ManagedDeliveryOwner{Version: 1, Root: a.Root, Installation: a.Installation, Identity: a.Identity, ConfigHash: a.DeliveryOwnerConfigHash}
		if protocol.Hash(owner) != a.DeliveryOwnerHash {
			return fmt.Errorf("limits owner pin changed")
		}
		want.DeliveryOwnerConfigHash = a.DeliveryOwnerConfigHash
	}
	if commandProviderKind(p.Kind) || p.Kind == "" || p.Kind == "destination" || p.Kind == "provider-enrollment" {
		if a.ReportCredentialHistory == nil || a.ReportCredentialHistory.Operation != v.ID || a.ReportCredentialHistory.Hash != protocol.Hash(p) {
			return fmt.Errorf("provider transition lost credential history anchor")
		}
		want.ReportCredentialHistory = a.ReportCredentialHistory
	}
	want.Revision, want.ConfigHash, want.RuntimeHash, want.UnitHash = a.Revision, a.ConfigHash, a.RuntimeHash, a.UnitHash
	want.Paths.Executable = filepath.Join(b.Root, "releases", a.Revision, "deploy")
	want.Paths.Config = filepath.Join(b.Root, "releases", a.Revision, "runtime.json")
	if protocol.Hash(want) != protocol.Hash(a) {
		return fmt.Errorf("provider transition changed installation ownership")
	}
	old, err := renderManagedUnit(*b)
	if err != nil {
		return err
	}
	next, err := renderManagedUnit(a)
	if err != nil {
		return err
	}
	if old != v.BeforeUnit || next != v.AfterUnit || managedDigest([]byte(next)) != a.UnitHash {
		return fmt.Errorf("provider transition unit changed")
	}
	return nil
}

func deliveryProviderPause(d *safefs.Directory, j managedJournal, c Runtime) (managedJournal, error) {
	var pause managedJournal
	if err := validateDeliveryProviderJournal(j); err != nil {
		return pause, err
	}
	p := j.Intent.DeliveryProvider
	if err := d.ReadJSON("operations/"+p.PauseOperation+"/intent.json", &pause.Intent); err != nil {
		return pause, err
	}
	pause.Phase = "complete"
	if pause.Intent.ID != p.PauseOperation || pause.Intent.Operation != "pause-delivery" || protocol.Hash(pause.Intent) != p.PauseHash || pause.validate() != nil || !preservesManagedDeliveryOwner(pause) || protocol.Hash(pause.Intent.After) != protocol.Hash(*j.Intent.Before) {
		return pause, fmt.Errorf("provider rotation lost its exact delivery pause")
	}
	before, err := managedRuntime(d, *j.Intent.Before)
	if err != nil {
		return pause, err
	}
	if protocol.Hash(c) != j.Intent.After.ConfigHash {
		return pause, fmt.Errorf("provider rotation runtime changed")
	}
	return pause, validatePausedRuntimeChange(*j.Intent.Before, before, c, p.Kind)
}

func (i managedInstaller) probeDeliveryProviders(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	probe := i.deliveryProvidersProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-delivery-providers-probe")
		}
	}
	for _, b := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err := probe(ctx, d, b.name, b.hash); err != nil {
			return err
		}
	}
	return nil
}

func RotatePausedDeliveryProvider(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().rotatePausedDeliveryProvider(ctx, spec, expected)
}

func (i managedInstaller) rotatePausedDeliveryProvider(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return i.changePausedRuntime(ctx, spec, expected, "")
}
func (i managedInstaller) changePausedRuntime(ctx context.Context, spec ManagedSpec, expected, kind string) (ManagedManifest, error) {
	operation := pausedRuntimeOperation(kind)
	if operation == "" {
		return ManagedManifest{}, fmt.Errorf("unknown paused runtime change")
	}
	var zero ManagedManifest
	if err := spec.validate(); err != nil {
		return zero, err
	}
	if !safefs.DigestPattern.MatchString(expected) || spec.RecoveryCommitHash != "" || spec.SignalVerification != nil || len(spec.SignalConversations) != 0 {
		return zero, fmt.Errorf("provider rotation requires exact paused installation pins")
	}
	raw, err := readManagedInput(spec.RuntimeFile, safefs.MaxRecordBytes)
	if err != nil {
		return zero, err
	}
	afterRuntime, err := decodeRuntime(raw)
	if err != nil {
		return zero, err
	}
	binary, err := readManagedInput(spec.BinaryFile, maxManagedBinary)
	if err != nil {
		return zero, err
	}
	if managedDigest(binary) != spec.BinarySHA256 {
		return zero, fmt.Errorf("provider rotation helper changed")
	}
	if err = checkManagedELF(binary); err != nil {
		return zero, err
	}
	d, err := privateManagedDir(spec.Root, true)
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
	if err = validateManagedPointer(d, j, managedPhase(j.Phase) >= managedPhase("published")); err != nil {
		return zero, err
	}
	b := j.Intent.After
	retry := j.Intent.Operation == operation && j.Intent.Before != nil && protocol.Hash(*j.Intent.Before) == expected
	if retry {
		b = *j.Intent.Before
	}
	if protocol.Hash(b) != expected || b.Root != spec.Root || b.Paths.State != spec.State || b.UnitDirectory != spec.UnitDirectory || b.BinaryHash != spec.BinarySHA256 || protocol.Hash(b.Paths.DeniedPaths) != protocol.Hash(spec.DeniedPaths) || protocol.Hash(b.FreshSignal) != protocol.Hash(spec.FreshSignal) {
		return zero, fmt.Errorf("provider rotation changed retained installation inputs")
	}
	beforeRuntime, err := verifyManagedArtifacts(d, b)
	if err != nil {
		return zero, err
	}
	if _, err = deliveryCredentialAncestry(d, managedJournal{Intent: managedIntent{After: b}}, beforeRuntime); err != nil {
		return zero, err
	}
	if err = validatePausedRuntimeChange(b, beforeRuntime, afterRuntime, kind); err != nil {
		return zero, err
	}
	if retry {
		if j.Intent.After.RuntimeHash != managedDigest(raw) || j.Intent.After.ConfigHash != protocol.Hash(afterRuntime) {
			return zero, fmt.Errorf("provider rotation retry changed runtime")
		}
		return i.resumeLocked(ctx, d, j, lock)
	}
	if j.Phase != "complete" || j.Intent.Operation != "pause-delivery" || !preservesManagedDeliveryOwner(j) {
		return zero, fmt.Errorf("finish the exact delivery pause before credential rotation")
	}
	if err = refuseManagedRecoveryPreparation(d, b); err != nil {
		return zero, err
	}
	if err = i.probePausedRuntimeChange(ctx, d, b, kind); err != nil {
		return zero, err
	}
	if err = i.probeSignalDelivery(ctx, d, b, afterRuntime); err != nil {
		return zero, err
	}
	if err = deliveryPolicy(b, beforeRuntime); err != nil {
		return zero, err
	}
	if kind != "" {
		if err = preflightDeliveryPolicy(ctx, b, beforeRuntime, afterRuntime, kind); err != nil {
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
		return zero, fmt.Errorf("paused provider unit changed")
	}
	if _, err = inspectManagedService(ctx, i.services, b, false); err != nil {
		return zero, err
	}
	if err = d.Capacity(int64(len(binary)) + 32<<20); err != nil {
		return zero, err
	}
	if err = limitManagedHistory(d); err != nil {
		return zero, err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return zero, err
	}
	a := b
	a.Revision = id
	a.ConfigHash = protocol.Hash(afterRuntime)
	a.RuntimeHash = managedDigest(raw)
	a.Paths.Executable = filepath.Join(a.Root, "releases", id, "deploy")
	a.Paths.Config = filepath.Join(a.Root, "releases", id, "runtime.json")
	transition := &deliveryProviderTransition{Version: 1, Kind: kind, PauseOperation: j.Intent.ID, PauseHash: protocol.Hash(j.Intent), RuntimeHash: a.RuntimeHash, ConfigHash: a.ConfigHash}
	if kind != "" {
		a.DeliveryOwnerConfigHash = deliveryOwner(b, beforeRuntime).ConfigHash
	}
	if commandProviderKind(kind) || kind == "" || kind == "destination" || kind == "provider-enrollment" {
		a.ReportCredentialHistory = &ManagedReportCredentialHistory{Operation: id, Hash: protocol.Hash(transition)}
	}
	body, err := renderManagedUnit(a)
	if err != nil {
		return zero, err
	}
	a.UnitHash = managedDigest([]byte(body))
	next := managedJournal{Intent: managedIntent{Version: managedVersion, ID: id, Operation: operation, Before: &b, After: a, BeforeUnit: string(unit), AfterUnit: body, DeliveryProvider: transition}, Phase: "prepared"}
	if err = next.validate(); err != nil {
		return zero, err
	}
	if err = d.Mkdir("releases/" + id); err != nil {
		return zero, err
	}
	release, err := d.Sub("releases/" + id)
	if err != nil {
		return zero, err
	}
	err = frozenManagedFile(release, "deploy", binary, 0700)
	if err == nil {
		err = frozenManagedFile(release, "runtime.json", raw, 0600)
	}
	release.Close()
	if err != nil {
		return zero, err
	}
	if err = writeManagedIntent(d, next); err != nil {
		return zero, err
	}
	if err = i.boundary("prepared"); err != nil {
		return zero, err
	}
	return i.resumeLocked(ctx, d, next, lock)
}

// InspectPausedDeliveryProvider verifies an exact pending/completed rotation.
// It never starts a different pause or chooses replacement credentials.
func InspectPausedDeliveryProvider(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectPausedDeliveryProvider(ctx, spec, expected)
}
func (i managedInstaller) inspectPausedDeliveryProvider(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, bool, error) {
	return i.inspectPausedRuntimeChange(ctx, spec, expected, "")
}
func (i managedInstaller) inspectPausedRuntimeChange(ctx context.Context, spec ManagedSpec, expected, kind string) (ManagedManifest, bool, error) {
	var zero ManagedManifest
	if err := spec.validate(); err != nil {
		return zero, false, err
	}
	if !safefs.DigestPattern.MatchString(expected) {
		return zero, false, fmt.Errorf("provider inspection requires exact pause pins")
	}
	raw, err := readManagedInput(spec.RuntimeFile, safefs.MaxRecordBytes)
	if err != nil {
		return zero, false, err
	}
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		return zero, false, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return zero, false, err
	}
	defer lock.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return zero, false, err
	}
	if err = validateManagedPointer(d, j, managedPhase(j.Phase) >= managedPhase("published")); err != nil {
		return zero, false, err
	}
	m := j.Intent.After
	if m.Root != spec.Root || m.Paths.State != spec.State || m.UnitDirectory != spec.UnitDirectory || m.BinaryHash != spec.BinarySHA256 || protocol.Hash(m.FreshSignal) != protocol.Hash(spec.FreshSignal) || protocol.Hash(m.Paths.DeniedPaths) != protocol.Hash(spec.DeniedPaths) {
		return zero, false, fmt.Errorf("provider inspection changed target or helper")
	}
	if j.Phase == "complete" && j.Intent.Operation == "pause-delivery" && protocol.Hash(m) == expected {
		return m, false, nil
	}
	if j.Intent.Operation != pausedRuntimeOperation(kind) || j.Intent.Before == nil || protocol.Hash(*j.Intent.Before) != expected || m.RuntimeHash != managedDigest(raw) {
		return zero, false, fmt.Errorf("provider inspection belongs to another transition")
	}
	c, err := verifyManagedArtifacts(d, m)
	if err != nil {
		return zero, false, err
	}
	if _, err = deliveryProviderPause(d, j, c); err != nil {
		return zero, false, err
	}
	if j.Phase != "complete" {
		return m, false, nil
	}
	m, err = i.resumeLocked(ctx, d, j, lock)
	return m, err == nil, err
}
