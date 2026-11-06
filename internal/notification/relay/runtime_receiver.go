package relay

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// OpenManagedSignalInbox transfers the already audited destination lease into
// the receiver without an unlock gap. The original source remains leased and
// immutable. Admission belongs to the managed service; the installed inbox and
// its historical cursor never become permission to start an ordinary receiver.
func OpenManagedSignalInbox(ctx context.Context, path, pin string, endpoint SignalVerificationEndpoint, admission func(context.Context) error) (_ *Inbox, err error) {
	if admission == nil {
		return nil, fmt.Errorf("managed Signal inbox admission required")
	}
	d, e := privateInboxDirectory(path)
	if e != nil {
		return nil, e
	}
	e = absentRuntimeFile(d, signalHandoffFile)
	d.Close()
	if e != nil {
		return nil, fmt.Errorf("Signal inbox has an outgoing recovery: %w", e)
	}
	if err = admission(ctx); err != nil {
		return nil, err
	}
	v, err := OpenSignalRuntimeVerification(ctx, path, pin)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			v.Close()
		}
	}()
	got, ok, err := v.VerificationEndpoint()
	if err != nil {
		return nil, err
	}
	if !ok || got != endpoint {
		return nil, fmt.Errorf("Signal receiver endpoint binding changed")
	}
	p := v.Installation()
	dst, lease := v.dst, v.dl
	v.dst, v.dl = nil, nil
	s, err := openInboxLeased(path, p.Cursor.Handoff.Target.Account, false, &p, dst, lease)
	if err != nil {
		return nil, err
	}
	s.releaseSource = v.Close
	s.runtimeAdmission = func(ctx context.Context) error {
		if err := absentRuntimeFile(s.dir, signalHandoffFile); err != nil {
			return err
		}
		if err := admission(ctx); err != nil {
			return err
		}
		if err := checkRuntimeDirectory(p.Source, v.src); err != nil {
			return err
		}
		if err := checkRuntimeLease(v.src, v.sl); err != nil {
			return err
		}
		if err := checkRuntimeDirectory(path, s.dir); err != nil {
			return err
		}
		if err := checkRuntimeLease(s.dir, s.lease); err != nil {
			return err
		}
		var current SignalVerificationEndpoint
		if err := readRuntimeVerificationEndpoint(s.dir, &current); err != nil {
			return err
		}
		if current != endpoint {
			return fmt.Errorf("Signal receiver endpoint changed")
		}
		// The immutable image and source remain distinct from the growing live
		// inbox. Every call rechecks the pinned file identities captured at open.
		for _, item := range v.files {
			f, err := inboxArtifactFile(item.dir, item.name, 34<<20)
			if err != nil {
				return err
			}
			info, err := f.Stat()
			f.Close()
			if err != nil {
				return err
			}
			if !os.SameFile(info, item.info) {
				return fmt.Errorf("Signal receiver evidence file replaced")
			}
		}
		var intent signalRuntimeIntent
		if err := v.src.ReadJSON(runtimeSourceName(p.Version, p.Ancestor, runtimeInstallIntent), &intent); err != nil {
			return err
		}
		if protocol.Hash(intent) != protocol.Hash(p.intent()) {
			return fmt.Errorf("Signal receiver source intent changed")
		}
		complete, err := runtimeInstallationRecord(v.src, runtimeSourceName(p.Version, p.Ancestor, runtimeInstallComplete), p)
		if err != nil {
			return err
		}
		if !complete {
			return fmt.Errorf("Signal receiver source installation missing")
		}
		if err = verifyRuntimeSource(ctx, v.src, p.Cursor); err != nil {
			return err
		}
		if err := verifyIncomingRuntime(ctx, v.src, p.intent()); err != nil {
			return err
		}
		return verifyRuntimeImage(ctx, s.dir, p)
	}
	if err = s.CheckActive(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
