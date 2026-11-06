package install

import (
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func signalDeliveryService(m ManagedManifest, c Runtime) (SignalServiceManifest, error) {
	return signalDeliveryServicePolicy(m, c, nil)
}
func signalDeliveryServicePolicy(m ManagedManifest, c Runtime, successor *SignalVerificationReference) (SignalServiceManifest, error) {
	var zero SignalServiceManifest
	if c.Signal == nil || m.SignalVerification == nil || m.SignalVerification.ServiceRoot == "" || m.RecoveryCommitHash == "" {
		return zero, fmt.Errorf("Signal delivery requires supervised recovered account admission")
	}
	if successor != nil {
		if err := validateSignalServiceDescendant(m, *successor, false); err != nil {
			return zero, err
		}
	}
	service, err := inspectSignalServiceRetained(m.SignalVerification.SignalVerificationReference, successor != nil)
	if err != nil {
		return zero, err
	}
	if service.Spec.Runtime == nil || service.Spec.UnitDirectory != m.UnitDirectory || service.Endpoint.Spec.GatewayState != m.Paths.State || service.Endpoint.Spec.CommitHash != m.RecoveryCommitHash {
		return zero, fmt.Errorf("Signal service is not admitted for this gateway")
	}
	raw, err := service.Endpoint.gateway()
	if err != nil {
		return zero, err
	}
	want, err := decodeRuntime(raw)
	if err != nil {
		return zero, err
	}
	if protocol.Hash(want.Signal) != protocol.Hash(c.Signal) {
		return zero, fmt.Errorf("Signal delivery account or inbox changed")
	}
	d, err := privateManagedDir(m.Root, true)
	if err != nil {
		return zero, err
	}
	effective, err := managedEffectiveRuntimePolicy(d, m, c, successor)
	d.Close()
	if err != nil {
		return zero, err
	}
	for _, p := range effective.Providers {
		if p.Kind != "signal" {
			continue
		}
		if m.SignalProfiles == nil || p.Conversation == nil || p.Conversation.Account != c.Signal.Account || p.Socket != c.Signal.Socket || p.RelayIdentity != c.Signal.InboxIdentity {
			return zero, fmt.Errorf("Signal delivery requires migrated conversation profiles")
		}
	}
	return service, nil
}
