package install

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

// SignalRecoverySpec binds the account service transition to one published
// gateway and one offline inbox installation. The caller stops the gateway first;
// its exclusive lease spans the account-service shutdown and offline transition.
type SignalRecoverySpec struct {
	AccountState, GatewayState, RelaySource, RelayDestination string
	CommitHash, Operation, ServiceBinding                     string
}

func (s SignalRecoverySpec) validate() error {
	paths := []string{s.AccountState, s.GatewayState, s.RelaySource, s.RelayDestination}
	for n, p := range paths {
		if !managedPath(p) {
			return fmt.Errorf("Signal recovery requires absolute paths")
		}
		for _, q := range paths[:n] {
			if managedWithin(p, q) || managedWithin(q, p) {
				return fmt.Errorf("Signal account/gateway/inbox paths must be separate")
			}
		}
	}
	if !safefs.ValidID(s.Operation, "n") || !safefs.DigestPattern.MatchString(s.CommitHash) || !safefs.DigestPattern.MatchString(s.ServiceBinding) {
		return fmt.Errorf("Signal recovery requires operation, commit and service pins")
	}
	return nil
}

// InstallSignalRecoveryWithService is the lifecycle integration point. The
// service adapter must verify recovery-capable installed code and exact loaded
// service/account binding. It never starts a receiver or changes gateway config.
func InstallSignalRecoveryWithService(ctx context.Context, c Runtime, spec SignalRecoverySpec, svc signalaccount.RecoveryService) (relay.SignalRuntimeInstallation, signalaccount.RecoveryStatus, error) {
	return installSignalRecoveryWithService(ctx, c, spec, svc, nil)
}

func InstallNextSignalRecoveryWithService(ctx context.Context, c Runtime, spec SignalRecoverySpec, parent signalaccount.RecoveryStatus, svc signalaccount.NextRecoveryService) (relay.SignalRuntimeInstallation, signalaccount.RecoveryStatus, error) {
	return installSignalRecoveryWithService(ctx, c, spec, svc, &parent)
}

func installSignalRecoveryWithService(ctx context.Context, c Runtime, spec SignalRecoverySpec, svc signalaccount.RecoveryService, parent *signalaccount.RecoveryStatus) (result relay.SignalRuntimeInstallation, status signalaccount.RecoveryStatus, err error) {
	if err = spec.validate(); err != nil {
		return
	}
	// Detach the already resolved runtime from ReadRuntime, including nested
	// maps/pointers. Resolving it again would re-hash route revisions.
	raw, e := json.Marshal(c)
	if e != nil {
		return result, status, e
	}
	var frozen Runtime
	err = safefs.DecodeJSON(raw, &frozen, safefs.MaxRecordBytes)
	if err != nil {
		return
	}
	c = frozen
	if c.Version != 1 || c.Worker.Validate() != nil || c.Signal == nil || c.Signal.Validate() != nil {
		return result, status, fmt.Errorf("Signal recovery requires an interactive account")
	}
	err = worker.WithPublishedSignalCursorChecked(ctx, spec.GatewayState, c.Worker, spec.CommitHash, c.Signal.Account, func(cursor interaction.SignalRecoveryCursor, gatewayCheck func() error) error {
		if cursor.Handoff.Before.Identity != c.Signal.InboxIdentity {
			return fmt.Errorf("Signal source identity differs from published cursor")
		}
		target := signalRecoveryTarget(c, spec, cursor)
		request := signalaccount.RecoveryRequest{Operation: spec.Operation, Binding: spec.ServiceBinding, Target: target}
		checkBoth := func(check func() error) error {
			if e := check(); e != nil {
				return e
			}
			if e := gatewayCheck(); e != nil {
				return e
			}
			return check()
		}
		apply := func(ctx context.Context, check func() error) (string, error) {
			if e := checkBoth(check); e != nil {
				return "", e
			}
			v, e := relay.InstallSignalRuntime(ctx, spec.RelaySource, spec.RelayDestination, cursor)
			if e != nil {
				return "", e
			}
			if e = checkBoth(check); e != nil {
				return "", e
			}
			return v.InstallationHash, nil
		}
		verify := func(ctx context.Context, hash string, check func() error) error {
			if e := checkBoth(check); e != nil {
				return e
			}
			v, e := relay.VerifyInstalledSignalRuntime(ctx, spec.RelayDestination, hash)
			if e != nil {
				return e
			}
			if v.Source != spec.RelaySource || v.Destination != spec.RelayDestination || protocol.Hash(v.Cursor) != protocol.Hash(cursor) {
				return fmt.Errorf("Signal installed result differs from recovery target")
			}
			if e = checkBoth(check); e != nil {
				return e
			}
			result = v
			return nil
		}
		var e error
		if parent == nil {
			status, e = signalaccount.FenceRecovery(ctx, spec.AccountState, request, svc, apply, verify)
		} else {
			next, ok := svc.(signalaccount.NextRecoveryService)
			if !ok {
				return fmt.Errorf("Signal service lacks next recovery support")
			}
			status, e = signalaccount.FenceNextRecovery(ctx, spec.AccountState, *parent, request, next, apply, verify)
		}
		return e
	})
	if err != nil {
		result = relay.SignalRuntimeInstallation{}
	}
	return
}
