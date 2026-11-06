package install

import (
	"encoding/json"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
)

func validateNextSignalEndpoint(before ManagedManifest, next SignalVerificationManifest) error {
	if before.Mode != "verification" || before.SignalVerification == nil || before.SignalVerification.ServiceRoot == "" || before.RecoveryCommitHash == "" {
		return fmt.Errorf("next Signal endpoint requires a recovered supervised predecessor")
	}
	old, err := removedSignalRecoveryService(before.SignalVerification.SignalVerificationReference)
	if err != nil {
		return err
	}
	p := old.Endpoint
	if next.Spec.Root == p.Spec.Root || next.Spec.GatewayState != before.Paths.State || next.Spec.GatewaySHA256 != before.RuntimeHash || next.Spec.CommitHash == before.RecoveryCommitHash || next.Spec.AccountState != p.Spec.AccountState || next.Installation.Source != p.Installation.Destination || next.Installation.Version != 2 || next.Installation.Ancestor != p.Installation.InstallationHash || next.Spec.Recovery.Binding != old.ManifestHash {
		return fmt.Errorf("next Signal endpoint changed predecessor or gateway pins")
	}
	return signalaccount.VerifyRetainedRecoveryAncestor(next.Spec.AccountState, next.Spec.Recovery, p.Spec.Recovery)
}

func validateManagedNextSignalChange(before ManagedManifest, after Runtime, raw []byte, ref SignalVerificationReference, commit string) (p SignalVerificationManifest, err error) {
	if ref.ServiceRoot == "" {
		return p, fmt.Errorf("next Signal adoption requires a managed successor service")
	}
	if err = validateManagedSignalService(before, ref); err != nil {
		return
	}
	p, err = inspectSignalVerificationPublication(ref)
	if err != nil {
		return
	}
	if err = validateNextSignalEndpoint(before, p); err != nil {
		return
	}
	if p.Spec.CommitHash != commit || p.NextGatewaySHA256 != managedDigest(raw) {
		return p, fmt.Errorf("next Signal endpoint publication/configuration changed")
	}
	if err = validateManagedSignalPaths(before, p); err != nil {
		return
	}
	original, err := decodeRuntime(p.OriginalConfig)
	if err != nil {
		return p, err
	}
	next, err := p.gateway()
	if err != nil {
		return p, err
	}
	expected, err := decodeRuntime(next)
	if err != nil {
		return p, err
	}
	if protocol.Hash(original) != before.ConfigHash || protocol.Hash(expected) != protocol.Hash(after) || protocol.Hash(original.Worker) != protocol.Hash(after.Worker) {
		return p, fmt.Errorf("next Signal adoption changed unrelated configuration")
	}
	return p, nil
}

// Rebase the independently retained effective profiles onto the next endpoint.
// Canonical worker configuration and conversation enrollment remain pinned;
// affected routes receive new revisions so queued work cannot change transport.
func prepareNextSignalProfiles(d *safefs.Directory, before, after ManagedManifest, canonical []byte, endpoint SignalVerificationManifest, operation string) (signalProfilesManifest, []byte, error) {
	if before.SignalProfiles == nil || after.SignalVerification == nil {
		return signalProfilesManifest{}, nil, fmt.Errorf("next Signal profiles require prior enrollment")
	}
	original, err := managedRuntime(d, before)
	if err != nil {
		return signalProfilesManifest{}, nil, err
	}
	ref := after.SignalVerification.SignalVerificationReference
	effective, err := managedEffectiveRuntimeRetained(d, before, original, &ref, true)
	if err != nil {
		return signalProfilesManifest{}, nil, err
	}
	var priorIntent managedIntent
	if err = d.ReadJSON("operations/"+before.SignalProfiles.Operation+"/intent.json", &priorIntent); err != nil {
		return signalProfilesManifest{}, nil, err
	}
	base := "releases/" + priorIntent.After.Revision + "/"
	priorRaw, err := ownedManagedFile(d, base+"signal-runtime.json", safefs.MaxRecordBytes)
	if err != nil {
		return signalProfilesManifest{}, nil, err
	}
	var retained signalProfilesManifest
	if err = d.ReadJSON(base+"signal-profiles.json", &retained); err != nil {
		return signalProfilesManifest{}, nil, err
	}
	var next Runtime
	if err = safefs.DecodeJSON(priorRaw, &next, safefs.MaxRecordBytes); err != nil {
		return signalProfilesManifest{}, nil, err
	}
	current, err := decodeRuntime(canonical)
	if err != nil {
		return signalProfilesManifest{}, nil, err
	}
	if original.Signal == nil || current.Signal == nil || protocol.Hash(effective) != retained.EffectiveConfig || protocol.Hash(retained) != before.SignalProfiles.Hash || protocol.Hash(original.Worker) != protocol.Hash(current.Worker) {
		return signalProfilesManifest{}, nil, fmt.Errorf("Signal ancestor profiles changed")
	}
	next.Signal = current.Signal
	for name, p := range next.Providers {
		if p.Kind != "signal" {
			continue
		}
		if p.Conversation == nil || p.Socket != original.Signal.Socket || p.RelayIdentity != original.Signal.InboxIdentity || p.Conversation.Account != current.Signal.Account {
			return signalProfilesManifest{}, nil, fmt.Errorf("Signal ancestor transport changed")
		}
		p.Socket, p.RelayIdentity = current.Signal.Socket, current.Signal.InboxIdentity
		next.Providers[name] = p
	}
	for name, route := range next.Worker.Routes {
		if next.Providers[route.Primary].Kind == "signal" || next.Providers[route.Fallback].Kind == "signal" {
			route.Revision = protocol.Hash(struct{ Label, Endpoint, Ancestor string }{route.Revision, endpoint.ManifestHash, before.SignalProfiles.Hash})
			next.Worker.Routes[name] = route
		}
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return signalProfilesManifest{}, nil, err
	}
	raw = append(raw, '\n')
	resolved, err := decodeRuntime(raw)
	if err != nil {
		return signalProfilesManifest{}, nil, err
	}
	p := signalProfilesManifest{Version: 2, Ancestor: before.SignalProfiles.Hash, Operation: operation, Installation: after.Installation, Identity: after.Identity, EndpointHash: endpoint.ManifestHash, CanonicalRuntime: after.RuntimeHash, CanonicalConfig: after.ConfigHash, EffectiveRuntime: managedDigest(raw), EffectiveConfig: protocol.Hash(resolved), Conversations: retained.Conversations}
	return p, raw, nil
}
