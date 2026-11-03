package relay

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"syscall"
)

// VerifyInstalledSignalRuntime checks both offline leases, retained source,
// immutable runtime image, completion records and the installed runtime schema.
func VerifyInstalledSignalRuntime(ctx context.Context, path, pin string) (out SignalRuntimeInstallation, err error) {
	if !safefs.DigestPattern.MatchString(pin) {
		return out, fmt.Errorf("exact Signal installation digest required")
	}
	dst, err := privateInboxDirectory(path)
	if err != nil {
		return out, err
	}
	defer dst.Close()
	if err = dst.ReadJSON(runtimeInstallComplete, &out); err != nil {
		return
	}
	if err = out.validate(); err != nil {
		return
	}
	if out.Destination != path || out.InstallationHash != pin {
		return out, fmt.Errorf("Signal installation reference changed")
	}
	src, err := privateInboxDirectory(out.Source)
	if err != nil {
		return out, err
	}
	defer src.Close()
	sl, err := src.Lock("inbox.lock", true, false)
	if err != nil {
		return out, err
	}
	defer sl.Close()
	dl, err := dst.Lock("inbox.lock", true, false)
	if err != nil {
		return out, err
	}
	defer dl.Close()
	if err = checkRuntimeDirectory(out.Source, src); err != nil {
		return
	}
	if err = checkRuntimeDirectory(path, dst); err != nil {
		return
	}
	if err = verifyRuntimeSource(ctx, src, out.Cursor); err != nil {
		return
	}
	err = verifyRuntimeInstallation(ctx, src, dst, out)
	return
}

func verifyRuntimeInstallation(ctx context.Context, src, dst *safefs.Directory, p SignalRuntimeInstallation) error {
	if err := p.validate(); err != nil {
		return err
	}
	info, err := dst.File().Stat()
	if err != nil {
		return err
	}
	st := info.Sys().(*syscall.Stat_t)
	if uint64(st.Dev) != p.DestinationDevice || st.Ino != p.DestinationInode {
		return fmt.Errorf("Signal installation destination replaced")
	}
	for _, d := range []*safefs.Directory{src, dst} {
		var intent signalRuntimeIntent
		if err = d.ReadJSON(runtimeInstallIntent, &intent); err != nil {
			return err
		}
		if protocol.Hash(intent) != protocol.Hash(p.intent()) {
			return fmt.Errorf("Signal installation intent changed")
		}
		exists, e := runtimeInstallationRecord(d, runtimeInstallComplete, p)
		if e != nil {
			return e
		}
		if !exists {
			return fmt.Errorf("Signal installation incomplete")
		}
	}
	exists, err := runtimeInstallationRecord(dst, runtimeInstallSeal, p)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("Signal installation seal missing")
	}
	if err = verifyRuntimeImage(ctx, dst, p); err != nil {
		return err
	}
	return inspectInstalledRuntime(ctx, dst, p)
}

// Only verification is exposed until the managed account/gateway activation
// handoff exists. These instances cannot receive, list or ACK provider messages.
func OpenInstalledInboxVerification(ctx context.Context, path, pin string) (*Inbox, error) {
	p, err := VerifyInstalledSignalRuntime(ctx, path, pin)
	if err != nil {
		return nil, err
	}
	return openInbox(path, p.Cursor.Handoff.Target.Account, false, &p)
}

func (s *Inbox) checkRuntimeFence() error {
	if s.installation == nil {
		return checkInboxRestoreFence(s.dir)
	}
	p := *s.installation
	info, err := s.dir.File().Stat()
	if err != nil {
		return err
	}
	st := info.Sys().(*syscall.Stat_t)
	if uint64(st.Dev) != p.DestinationDevice || st.Ino != p.DestinationInode {
		return fmt.Errorf("Signal verification destination replaced")
	}
	if err := p.validate(); err != nil {
		return err
	}
	if p.Destination != s.dir.File().Name() || p.Cursor.Handoff.Target.Account != s.account {
		return fmt.Errorf("Signal verification installation path/account changed")
	}
	if err := absentRuntimeFile(s.dir, "restore.json"); err != nil {
		return err
	}
	var intent signalRuntimeIntent
	if err := s.dir.ReadJSON(runtimeInstallIntent, &intent); err != nil {
		return err
	}
	if protocol.Hash(intent) != protocol.Hash(p.intent()) {
		return fmt.Errorf("Signal verification intent changed")
	}
	for _, name := range []string{runtimeInstallSeal, runtimeInstallComplete} {
		ok, err := runtimeInstallationRecord(s.dir, name, p)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("Signal verification installation incomplete")
		}
	}
	return nil
}

func verifyRuntimeSeal(ctx context.Context, d *safefs.Directory, p SignalRuntimeInstallation) error {
	var intent signalRuntimeIntent
	if err := d.ReadJSON(runtimeInstallIntent, &intent); err != nil {
		return err
	}
	if protocol.Hash(intent) != protocol.Hash(p.intent()) {
		return fmt.Errorf("Signal destination intent changed")
	}
	ok, err := runtimeInstallationRecord(d, runtimeInstallSeal, p)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Signal runtime seal disappeared")
	}
	return verifyRuntimeImage(ctx, d, p)
}
