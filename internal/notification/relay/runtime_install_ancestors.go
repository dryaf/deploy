package relay

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func runtimeSourceName(version int, ancestor, name string) string {
	if version == 2 {
		return "next-" + ancestor + "-" + name
	}
	return name
}

type runtimeAncestorDepth struct{}

// RequireLegacyRuntimeAncestor follows retained incoming installations to the
// bootstrap that adopted an old fixed-recipient relay. It does not acquire a
// second runtime account lease or grant permission to start another receiver.
func RequireLegacyRuntimeAncestor(ctx context.Context, current SignalRuntimeInstallation, bootstrap string) error {
	if !safefs.DigestPattern.MatchString(bootstrap) {
		return fmt.Errorf("legacy bootstrap pin required")
	}
	for depth := 0; depth < 32; depth++ {
		if err := current.validate(); err != nil {
			return err
		}
		d, err := privateInboxDirectory(current.Source)
		if err != nil {
			return err
		}
		err = verifyIncomingRuntime(ctx, d, current.intent())
		if err != nil {
			d.Close()
			return err
		}
		if current.Version == 1 {
			d.Close()
			if current.LegacyBootstrap != bootstrap {
				return fmt.Errorf("Signal runtime has a different legacy bootstrap")
			}
			return nil
		}
		var prior SignalRuntimeInstallation
		err = d.ReadJSON(runtimeInstallComplete, &prior)
		d.Close()
		if err != nil {
			return err
		}
		if prior.InstallationHash != current.Ancestor || prior.Destination != current.Source {
			return fmt.Errorf("Signal migration ancestor changed")
		}
		current = prior
	}
	return fmt.Errorf("Signal migration ancestor limit reached")
}

// Incoming and outgoing publications have separate slots. Each later source
// retains its incoming installation, original source and immutable image.
func verifyIncomingRuntime(ctx context.Context, src *safefs.Directory, want signalRuntimeIntent) error {
	if want.LegacyBootstrap != "" {
		var bootstrap LegacyInboxBootstrap
		if err := src.ReadJSON(legacyBootstrapIntent, &bootstrap); err != nil {
			return err
		}
		if protocol.Hash(bootstrap) != want.LegacyBootstrap || bootstrap.Account != want.Cursor.Handoff.Before.Account || bootstrap.Identity != want.Cursor.Handoff.Before.Identity {
			return fmt.Errorf("legacy Signal bootstrap source changed")
		}
		if err := verifyLegacyBootstrap(ctx, src, bootstrap); err != nil {
			return err
		}
	}

	if want.Version == 1 {
		return nil
	}
	depth, _ := ctx.Value(runtimeAncestorDepth{}).(int)
	if depth >= 32 {
		return fmt.Errorf("Signal installation ancestry limit reached")
	}
	ctx = context.WithValue(ctx, runtimeAncestorDepth{}, depth+1)
	var incoming SignalRuntimeInstallation
	if err := src.ReadJSON(runtimeInstallComplete, &incoming); err != nil {
		return err
	}
	if err := incoming.validate(); err != nil {
		return err
	}
	if incoming.Destination != want.Source || incoming.InstallationHash != want.Ancestor || protocol.Hash(incoming.Cursor) != want.Cursor.Handoff.Summary.Manifest.BaselineHash {
		return fmt.Errorf("Signal incoming installation ancestor changed")
	}
	prior, err := privateInboxDirectory(incoming.Source)
	if err != nil {
		return err
	}
	defer prior.Close()
	if err = verifyRuntimeSource(ctx, prior, incoming.Cursor); err != nil {
		return err
	}
	return verifyRuntimeInstallation(ctx, prior, src, incoming)
}
