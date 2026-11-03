package install

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func PrepareSignalVerification(ctx context.Context, s SignalVerificationSpec) (SignalVerificationManifest, error) {
	return prepareSignalVerification(ctx, s, nil)
}
func prepareSignalVerification(ctx context.Context, s SignalVerificationSpec, fault func(string) error) (out SignalVerificationManifest, err error) {
	c, raw, err := s.prepare()
	if err != nil {
		return out, err
	}
	boundary := func(at string) error {
		if fault != nil {
			return fault(at)
		}
		return nil
	}
	err = worker.WithPublishedSignalCursorChecked(ctx, s.GatewayState, c.Worker, s.CommitHash, c.Signal.Account, func(cursor interaction.SignalRecoveryCursor, gatewayCheck func() error) error {
		account, e := signalaccount.OpenRecoveryVerification(s.AccountState, s.Recovery)
		if e != nil {
			return e
		}
		defer account.Close()
		inbox, e := relay.OpenSignalRuntimeVerification(ctx, s.InboxDestination, s.Recovery.ResultHash)
		if e != nil {
			return e
		}
		defer inbox.Close()
		m := SignalVerificationManifest{Spec: s, OriginalConfig: raw, Installation: inbox.Installation()}
		if protocol.Hash(cursor) != protocol.Hash(m.Installation.Cursor) {
			return fmt.Errorf("verification cursor differs from gateway publication")
		}
		next, e := m.gateway()
		if e != nil {
			return e
		}
		// Validate all account/source/configuration relationships before writing
		// even the endpoint operation lock into the caller-supplied directory.
		r, e := openSignalVerificationRoot(s.Root, true)
		if e != nil {
			return e
		}
		defer r.close()
		if e = r.pin(&m); e != nil {
			return e
		}
		m.NextGatewaySHA256 = managedDigest(next)
		m.ManifestHash = protocol.Hash(m)
		if e = m.validate(); e != nil {
			return e
		}
		check := func() error {
			if e := ctx.Err(); e != nil {
				return e
			}
			if e := r.check(m); e != nil {
				return e
			}
			if e := account.Check(); e != nil {
				return e
			}
			if e := gatewayCheck(); e != nil {
				return e
			}
			return inbox.Check(ctx)
		}
		checkReceipt := func() error {
			saved, ok, e := inbox.VerificationEndpoint()
			if e != nil {
				return e
			}
			if !ok || saved != m.receipt() {
				return fmt.Errorf("verification installation endpoint receipt missing or changed")
			}
			return nil
		}
		if e = check(); e != nil {
			return e
		}
		completed, e := readSignalVerificationManifest(r.dir, signalVerificationComplete)
		if e == nil {
			if protocol.Hash(completed) != protocol.Hash(m) {
				return fmt.Errorf("verification endpoint already completed with different pins")
			}
			if e = r.audit(m, true); e != nil {
				return e
			}
			if e = checkReceipt(); e != nil {
				return e
			}
			if e = check(); e != nil {
				return e
			}
			out = m
			return nil
		}
		if !os.IsNotExist(e) {
			return e
		}
		intent, e := readSignalVerificationManifest(r.dir, signalVerificationIntent)
		if os.IsNotExist(e) {
			// Only a first enrollment may create intent. A retained destination binding
			// forbids rebuilding a lost endpoint publication, even at a different root.
			_, exists, e := inbox.VerificationEndpoint()
			if e != nil {
				return e
			}
			if exists {
				return fmt.Errorf("Signal installation already has an endpoint publication")
			}
			names, e := signalVerificationNames(r.dir)
			if e != nil {
				return e
			}
			for _, name := range names {
				if name == signalVerificationLock {
					continue
				}
				id := strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json")
				if strings.HasPrefix(name, ".") && safefs.ValidID(id, "n") {
					continue
				}
				return fmt.Errorf("verification root contains foreign or orphaned files")
			}
			if e = r.dir.WriteJSON(signalVerificationIntent, m, false); e != nil {
				return e
			}
		} else if e != nil {
			return e
		} else if protocol.Hash(intent) != protocol.Hash(m) {
			return fmt.Errorf("verification intent pins changed")
		}
		if e = boundary("endpoint_intent"); e != nil {
			return e
		}
		if e = inbox.BindVerificationEndpoint(ctx, m.receipt()); e != nil {
			return e
		}
		if e = boundary("endpoint_destination_bound"); e != nil {
			return e
		}
		if e = frozenManagedFile(r.dir, signalVerificationGateway, next, 0600); e != nil {
			return e
		}
		if e = boundary("endpoint_gateway_staged"); e != nil {
			return e
		}
		if e = r.audit(m, false); e != nil {
			return e
		}
		if e = checkReceipt(); e != nil {
			return e
		}
		if e = check(); e != nil {
			return e
		}
		if e = r.dir.WriteJSON(signalVerificationComplete, m, false); e != nil {
			return e
		}
		if e = boundary("endpoint_complete"); e != nil {
			return e
		}
		if e = r.audit(m, true); e != nil {
			return e
		}
		if e = checkReceipt(); e != nil {
			return e
		}
		if e = check(); e != nil {
			return e
		}
		out = m
		return nil
	})
	return
}
