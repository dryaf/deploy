package install

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// The canonical recovery configuration is retained byte-for-byte. This binding
// selects a separately frozen effective runtime whose only differences are the
// outgoing Signal socket/inbox pins and their derived route revisions.
type ManagedSignalProfiles struct {
	Operation string `json:"operation"`
	Hash      string `json:"hash"`
}

type signalProfilesManifest struct {
	Version          int               `json:"version"`
	Operation        string            `json:"operation"`
	Installation     string            `json:"installation"`
	Identity         protocol.Identity `json:"identity"`
	EndpointHash     string            `json:"endpoint_hash"`
	CanonicalRuntime string            `json:"canonical_runtime_hash"`
	CanonicalConfig  string            `json:"canonical_config_hash"`
	EffectiveRuntime string            `json:"effective_runtime_hash"`
	EffectiveConfig  string            `json:"effective_config_hash"`
}

func (p *ManagedSignalProfiles) validate() error {
	if p == nil || !safefs.ValidID(p.Operation, "n") || !safefs.DigestPattern.MatchString(p.Hash) {
		return fmt.Errorf("invalid managed Signal profile binding")
	}
	return nil
}

func signalProfilesRuntime(before ManagedManifest, canonical []byte, endpoint SignalVerificationManifest) ([]byte, error) {
	if before.Mode != "verification" || before.SignalProfiles != nil || before.SignalVerification == nil || before.SignalVerification.ServiceRoot == "" || before.RecoveryCommitHash == "" || before.RuntimeHash != managedDigest(canonical) || before.RuntimeHash != endpoint.NextGatewaySHA256 || before.SignalVerification.ManifestHash != endpoint.ManifestHash {
		return nil, fmt.Errorf("Signal profile migration requires the exact supervised recovery verification installation")
	}
	c, err := decodeRuntime(canonical)
	if err != nil {
		return nil, err
	}
	original, err := decodeRuntime(endpoint.OriginalConfig)
	if err != nil {
		return nil, err
	}
	if c.Signal == nil || original.Signal == nil || protocol.Hash(c) != before.ConfigHash {
		return nil, fmt.Errorf("Signal profile migration configuration changed")
	}
	// Decode unresolved bytes so human route labels are resolved exactly once.
	var next Runtime
	if err = safefs.DecodeJSON(canonical, &next, safefs.MaxRecordBytes); err != nil {
		return nil, err
	}
	changed := 0
	for name, p := range next.Providers {
		if p.Kind != "signal" {
			continue
		}
		if p.Conversation == nil {
			return nil, fmt.Errorf("legacy fixed-recipient Signal profiles require explicit conversation enrollment first")
		}
		if p.Conversation.Account != c.Signal.Account || p.Socket != original.Signal.Socket || p.RelayIdentity != original.Signal.InboxIdentity {
			return nil, fmt.Errorf("Signal profile does not match the retained original account/socket/inbox")
		}
		p.Socket, p.RelayIdentity = c.Signal.Socket, c.Signal.InboxIdentity
		next.Providers[name] = p
		changed++
	}
	if changed == 0 {
		return nil, fmt.Errorf("no outgoing Signal conversation profiles to migrate")
	}
	for name, route := range next.Worker.Routes {
		if next.Providers[route.Primary].Kind == "signal" || next.Providers[route.Fallback].Kind == "signal" {
			// A surviving inbox can retain its identity while moving sockets.
			// Bind affected routes to this proven transition even in that case;
			// old queued revisions must never silently acquire the new transport.
			route.Revision = protocol.Hash(struct{ Label, Endpoint, Canonical string }{route.Revision, endpoint.ManifestHash, before.RuntimeHash})
			next.Worker.Routes[name] = route
		}
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	effective, err := decodeRuntime(raw)
	if err != nil {
		return nil, err
	}
	if protocol.Hash(effective) == before.ConfigHash {
		return nil, fmt.Errorf("Signal profile migration must change the effective configuration")
	}
	return raw, nil
}

func prepareSignalProfiles(before ManagedManifest, canonical []byte, endpoint SignalVerificationManifest, operation string) (signalProfilesManifest, []byte, error) {
	raw, err := signalProfilesRuntime(before, canonical, endpoint)
	if err != nil {
		return signalProfilesManifest{}, nil, err
	}
	c, err := decodeRuntime(raw)
	if err != nil {
		return signalProfilesManifest{}, nil, err
	}
	p := signalProfilesManifest{Version: 1, Operation: operation, Installation: before.Installation, Identity: before.Identity, EndpointHash: endpoint.ManifestHash, CanonicalRuntime: before.RuntimeHash, CanonicalConfig: before.ConfigHash, EffectiveRuntime: managedDigest(raw), EffectiveConfig: protocol.Hash(c)}
	return p, raw, nil
}

// Every runtime/lifecycle retry requires the original migration intent and the
// independent frozen effective bytes. Later updates never regenerate this proof.
func managedEffectiveRuntime(d *safefs.Directory, m ManagedManifest, canonical Runtime) (Runtime, error) {
	if m.SignalProfiles == nil {
		return canonical, nil
	}
	if err := m.SignalProfiles.validate(); err != nil {
		return Runtime{}, err
	}
	intentBase := "operations/" + m.SignalProfiles.Operation + "/"
	var intent managedIntent
	if err := d.ReadJSON(intentBase+"intent.json", &intent); err != nil {
		return Runtime{}, err
	}
	if err := (managedJournal{Intent: intent, Phase: "complete"}).validate(); err != nil {
		return Runtime{}, err
	}
	a := intent.After
	base := "releases/" + a.Revision + "/"
	if intent.Operation != "migrate-signal-profiles" || intent.ID != m.SignalProfiles.Operation || intent.Before == nil || protocol.Hash(a.SignalProfiles) != protocol.Hash(m.SignalProfiles) || a.Installation != m.Installation || a.Identity != m.Identity || a.Root != m.Root || a.Paths.State != m.Paths.State || a.UnitDirectory != m.UnitDirectory || a.ConfigHash != m.ConfigHash || a.RuntimeHash != m.RuntimeHash || a.RecoveryCommitHash != m.RecoveryCommitHash || a.Schema != m.Schema || protocol.Hash(a.SignalVerification) != protocol.Hash(m.SignalVerification) {
		return Runtime{}, fmt.Errorf("managed Signal profile migration authority changed")
	}
	original, err := managedRuntime(d, *intent.Before)
	if err != nil {
		return Runtime{}, err
	}
	if protocol.Hash(original) != protocol.Hash(canonical) || protocol.Hash(canonical) != m.ConfigHash {
		return Runtime{}, fmt.Errorf("canonical Signal profile configuration changed")
	}
	endpoint, err := readManagedSignalBinding(d, m)
	if err != nil {
		return Runtime{}, err
	}
	canonicalBytes, err := ownedManagedFile(d, "releases/"+intent.Before.Revision+"/runtime.json", safefs.MaxRecordBytes)
	if err != nil {
		return Runtime{}, err
	}
	want, expected, err := prepareSignalProfiles(*intent.Before, canonicalBytes, endpoint, intent.ID)
	if err != nil {
		return Runtime{}, err
	}
	var retained signalProfilesManifest
	if err = d.ReadJSON(base+"signal-profiles.json", &retained); err != nil {
		return Runtime{}, err
	}
	if retained != want || protocol.Hash(retained) != m.SignalProfiles.Hash {
		return Runtime{}, fmt.Errorf("retained Signal profile proof changed")
	}
	raw, err := ownedManagedFile(d, base+"signal-runtime.json", safefs.MaxRecordBytes)
	if err != nil {
		return Runtime{}, err
	}
	if managedDigest(raw) != retained.EffectiveRuntime || managedDigest(raw) != managedDigest(expected) {
		return Runtime{}, fmt.Errorf("effective Signal profile bytes changed")
	}
	effective, err := decodeRuntime(raw)
	if err != nil {
		return Runtime{}, err
	}
	if protocol.Hash(effective) != retained.EffectiveConfig {
		return Runtime{}, fmt.Errorf("effective Signal profile configuration changed")
	}
	return effective, nil
}

func (i managedInstaller) probeSignalProfiles(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	if m.SignalProfiles == nil {
		return nil
	}
	probe := i.signalProfilesProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-signal-profiles-probe")
		}
	}
	if err := probe(ctx, d, "releases/"+m.Revision+"/deploy", m.BinaryHash); err != nil {
		return err
	}
	return probe(ctx, d, managedHelperName(m), m.HelperHash)
}

func reviewSignalProfilesInput(spec ManagedSpec, raw []byte) error {
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		return err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	m, _, err := qualificationInstallation(d, spec.Root)
	if err != nil {
		return err
	}
	if m.Paths.State != spec.State || m.UnitDirectory != spec.UnitDirectory || protocol.Hash(m.Paths.DeniedPaths) != protocol.Hash(spec.DeniedPaths) || m.SignalVerification == nil {
		return fmt.Errorf("Signal profile request changed installation authority")
	}
	endpoint, err := readManagedSignalBinding(d, m)
	if err != nil {
		return err
	}
	_, err = signalProfilesRuntime(m, raw, endpoint)
	return err
}

func checkManagedSignalProfilesRuntime(d *safefs.Directory, m ManagedManifest, installation *safefs.Lock) error {
	if m.SignalProfiles == nil {
		return nil
	}
	if err := checkManagedDirectoryPath(d, m.Root, true); err != nil {
		return err
	}
	if err := managedLeaseMatches(d, "installation.lock", installation); err != nil {
		return err
	}
	canonical, err := managedRuntime(d, m)
	if err != nil {
		return err
	}
	_, err = managedEffectiveRuntime(d, m, canonical)
	return err
}
