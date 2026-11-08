package install

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/safefs"
)

func checkFreshSignalDelivery(m FreshSignalManifest, p provider.Profile, committed bool) error {
	if p.SignalDelivery == nil {
		return nil
	}
	v := *p.SignalDelivery
	a, r, err := v.Values()
	if err != nil {
		return err
	}
	selector, err := provider.ReadSecret(m.Spec.Root + "/account")
	if err != nil {
		return err
	}
	if p.Kind != "signal" || p.Conversation != nil || p.Socket != m.Spec.socket() || v.ServiceHash != m.ManifestHash || a != selector || p.Destination != relay.Destination(a, r) {
		return fmt.Errorf("Signal recipient change must retain the owned account, service and inbox")
	}
	if !committed {
		return nil
	}
	d, err := privateManagedDir(m.Spec.Root, true)
	if err != nil {
		return err
	}
	defer d.Close()
	var saved provider.SignalDelivery
	if err = d.ReadJSON("delivery/"+v.Hash()+".json", &saved); err != nil {
		return err
	}
	if saved != v {
		return fmt.Errorf("Signal delivery receipt changed")
	}
	return nil
}

// InspectFreshSignalDeliveryReferences proves a later declaration against its
// immutable recipient approval while retaining the original native setup.
func InspectFreshSignalDeliveryReferences(ctx context.Context, ref FreshSignalReference, v provider.SignalDelivery, destination string) error {
	m, err := InspectFreshSignal(ctx, ref, false)
	if err != nil {
		return err
	}
	return checkFreshSignalDelivery(m, provider.Profile{Kind: "signal", Socket: m.Spec.socket(), Destination: destination, SignalDelivery: &v}, true)
}
func stageFreshSignalDelivery(m ManagedManifest, c Runtime) error {
	p, ok := c.Providers["signal"]
	if !ok || p.SignalDelivery == nil {
		return nil
	}
	if m.FreshSignal == nil {
		return fmt.Errorf("Signal recipient approval requires an owned native service")
	}
	service, err := InspectFreshSignal(context.Background(), *m.FreshSignal, false)
	if err != nil {
		return err
	}
	if err = checkFreshSignalDelivery(service, p, false); err != nil {
		return err
	}
	d, err := privateManagedDir(service.Spec.Root, true)
	if err != nil {
		return err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", true, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = checkFreshSignalFiles(d, service, true); err != nil {
		return err
	}
	if err = d.Mkdir("delivery"); err != nil {
		return err
	}
	raw, err := json.Marshal(*p.SignalDelivery)
	if err != nil {
		return err
	}
	approval, err := d.Sub("delivery")
	if err != nil {
		return err
	}
	defer approval.Close()
	return frozenManagedFile(approval, p.SignalDelivery.Hash()+".json", raw, 0600)
}
func freshSignalRecipient(m FreshSignalManifest, hash, destination string) (string, error) {
	if !safefs.DigestPattern.MatchString(hash) {
		return "", fmt.Errorf("Signal delivery approval required")
	}
	d, err := privateManagedDir(m.Spec.Root, true)
	if err != nil {
		return "", err
	}
	defer d.Close()
	var v provider.SignalDelivery
	if err = d.ReadJSON("delivery/"+hash+".json", &v); err != nil {
		return "", err
	}
	if v.Hash() != hash {
		return "", fmt.Errorf("Signal delivery receipt changed")
	}
	p := provider.Profile{Kind: "signal", Socket: m.Spec.socket(), Destination: destination, SignalDelivery: &v}
	if err = checkFreshSignalDelivery(m, p, false); err != nil {
		return "", err
	}
	_, recipient, err := v.Values()
	return recipient, err
}

// ReviewFreshSignalDelivery resolves references without publishing an approval.
// The original enrollment needs no overlay and remains byte-for-byte unchanged.
func ReviewFreshSignalDelivery(ctx context.Context, root, accountFile, recipientFile, accountHash, recipientHash string) (*provider.SignalDelivery, error) {
	d, err := privateManagedDir(root, true)
	if err != nil {
		return nil, err
	}
	m, err := readFreshSignal(d, "ready.json")
	d.Close()
	if err != nil {
		return nil, err
	}
	if _, err = InspectFreshSignal(ctx, m.reference(), false); err != nil {
		return nil, err
	}
	if accountFile == m.Spec.AccountFile && recipientFile == m.Spec.RecipientFile && accountHash == m.Spec.AccountSHA256 && recipientHash == m.Spec.RecipientSHA256 {
		return nil, nil
	}
	v := &provider.SignalDelivery{ServiceHash: m.ManifestHash, AccountFile: accountFile, AccountHash: accountHash, RecipientFile: recipientFile, RecipientHash: recipientHash}
	a, r, err := v.Values()
	if err != nil {
		return nil, err
	}
	p := provider.Profile{Kind: "signal", Socket: m.Spec.socket(), Destination: relay.Destination(a, r), SignalDelivery: v}
	return v, checkFreshSignalDelivery(m, p, false)
}
func validateSignalDeliveryProfileChange(old, next provider.Profile, destination bool) error {
	if old.Kind != "signal" || next.Kind != "signal" || old.Conversation != nil || next.Conversation != nil || old.Socket != next.Socket || next.SignalDelivery == nil {
		return fmt.Errorf("Signal delivery changes require retained direct native service")
	}
	a, r, err := next.SignalDelivery.Values()
	if err != nil {
		return err
	}
	if next.Destination != relay.Destination(a, r) || !destination && next.Destination != old.Destination {
		return fmt.Errorf("Signal destination differs from selected private inputs")
	}
	if old.SignalDelivery != nil && old.SignalDelivery.ServiceHash != next.SignalDelivery.ServiceHash {
		return fmt.Errorf("retain native Signal service ownership")
	}
	copy := next
	copy.SignalDelivery, copy.Destination = old.SignalDelivery, old.Destination
	if protocol.Hash(copy) != protocol.Hash(old) {
		return fmt.Errorf("retain native Signal provider policy")
	}
	return nil
}

func (i managedInstaller) probeSignalDelivery(ctx context.Context, d *safefs.Directory, m ManagedManifest, c Runtime) error {
	p := c.Providers["signal"]
	if p.SignalDelivery == nil {
		return nil
	}
	if m.FreshSignal == nil {
		return fmt.Errorf("Signal delivery approval needs the original native service")
	}
	native, err := InspectFreshSignal(ctx, *m.FreshSignal, false)
	if err != nil {
		return err
	}
	if err = checkFreshSignalDelivery(native, p, false); err != nil {
		return err
	}
	probe := i.signalDeliveryProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-signal-delivery-probe")
		}
	}
	for _, b := range []struct{ n, h string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err = probe(ctx, d, b.n, b.h); err != nil {
			return err
		}
	}
	service, err := privateManagedDir(native.Spec.Root, true)
	if err != nil {
		return err
	}
	defer service.Close()
	return probe(ctx, service, "deploy", native.Spec.BinarySHA256)
}

func (i managedInstaller) stageSignalDeliveryIntent(ctx context.Context, d *safefs.Directory, j managedJournal) error {
	if j.Intent.Before == nil {
		return nil
	}
	allowed := isPausedRuntimeChange(j.Intent.Operation) || j.Intent.Operation == "bootstrap-providers" || j.Intent.Operation == "enrolled-providers"
	if !allowed {
		return nil
	}
	c, err := managedRuntime(d, j.Intent.After)
	if err != nil {
		return err
	}
	p := c.Providers["signal"]
	if p.SignalDelivery == nil {
		return nil
	}
	before, err := verifyManagedArtifacts(d, *j.Intent.Before)
	if err != nil {
		return err
	}
	if protocol.Hash(before.Providers["signal"]) == protocol.Hash(p) {
		return nil
	}
	if !isPausedRuntimeChange(j.Intent.Operation) && !validStagedProviderCredentialHistory(j.Intent) {
		return fmt.Errorf("native provider transition lost staged credential history")
	}
	if isPausedRuntimeChange(j.Intent.Operation) {
		err = validatePausedRuntimeChange(*j.Intent.Before, before, c, j.Intent.DeliveryProvider.Kind)
	} else if j.Intent.Operation == "enrolled-providers" {
		err = ValidateManagedEnrolledProviders(*j.Intent.Before, before, c)
	} else {
		err = ValidateBootstrapProvidersChange(before, c)
	}
	if err != nil {
		return err
	}
	if j.Phase != "prepared" {
		native, err := InspectFreshSignal(ctx, *j.Intent.After.FreshSignal, false)
		if err != nil {
			return err
		}
		return checkFreshSignalDelivery(native, p, true)
	}
	if err = i.probeSignalDelivery(ctx, d, j.Intent.After, c); err != nil {
		return err
	}
	return stageFreshSignalDelivery(j.Intent.After, c)
}

// InspectRetainedFreshSignalDeliverySelection checks an existing approval before
// repairing a stopped service. It grants no restart, account or link authority.
func InspectRetainedFreshSignalDeliverySelection(root, accountFile, recipientFile, accountHash, recipientHash string) error {
	d, err := privateManagedDir(root, true)
	if err != nil {
		return err
	}
	m, err := readFreshSignal(d, "ready.json")
	d.Close()
	if err != nil {
		return err
	}
	if _, _, err = inspectRetainedFreshSignal(m.reference()); err != nil {
		return err
	}
	v := &provider.SignalDelivery{ServiceHash: m.ManifestHash, AccountFile: accountFile, AccountHash: accountHash, RecipientFile: recipientFile, RecipientHash: recipientHash}
	a, r, err := v.Values()
	if err != nil {
		return err
	}
	return checkFreshSignalDelivery(m, provider.Profile{Kind: "signal", Socket: m.Spec.socket(), Destination: relay.Destination(a, r), SignalDelivery: v}, true)
}
