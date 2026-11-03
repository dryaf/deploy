package relay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const runtimeInstallIntent = "signal-runtime-install.json"
const runtimeInstallSeal = "signal-runtime-install-sealed.json"
const runtimeInstallComplete = "signal-runtime-install-complete.json"
const runtimeInstallImage = "signal-runtime-image.db"

// SignalRuntimeInstallation publishes a baseline-aware inbox into a separate
// private directory. The source stays fenced and retained. This is not authority
// to start a Signal receiver or to activate gateway intake.
type SignalRuntimeInstallation struct {
	Version           int                              `json:"version"`
	Source            string                           `json:"source"`
	Destination       string                           `json:"destination"`
	DestinationDevice uint64                           `json:"destination_device"`
	DestinationInode  uint64                           `json:"destination_inode"`
	Cursor            interaction.SignalRecoveryCursor `json:"cursor"`
	DatabaseHash      string                           `json:"database_hash"`
	DatabaseBytes     int64                            `json:"database_bytes"`
	InstallationHash  string                           `json:"installation_hash"`
	IntakeEnabled     bool                             `json:"intake_enabled"`
}

type signalRuntimeIntent struct {
	Version                             int
	Source, Destination                 string
	DestinationDevice, DestinationInode uint64
	Cursor                              interaction.SignalRecoveryCursor
}

func (v SignalRuntimeInstallation) intent() signalRuntimeIntent {
	return signalRuntimeIntent{v.Version, v.Source, v.Destination, v.DestinationDevice, v.DestinationInode, v.Cursor}
}
func (v SignalRuntimeInstallation) validate() error {
	if err := v.Cursor.Validate(); err != nil {
		return err
	}
	if v.Version != 1 || v.IntakeEnabled || !runtimeInstallationPaths(v.Source, v.Destination) || v.DestinationInode == 0 || v.DatabaseBytes <= 0 || v.DatabaseBytes > 34<<20 || !safefs.DigestPattern.MatchString(v.DatabaseHash) {
		return fmt.Errorf("invalid Signal runtime installation")
	}
	hash := v.InstallationHash
	v.InstallationHash = ""
	if !safefs.DigestPattern.MatchString(hash) || protocol.Hash(v) != hash {
		return fmt.Errorf("Signal installation digest changed")
	}
	return nil
}
func runtimeInstallationPaths(source, destination string) bool {
	return filepath.IsAbs(source) && filepath.Clean(source) == source && source != "/" && filepath.IsAbs(destination) && filepath.Clean(destination) == destination && destination != "/" && source != destination && !strings.HasPrefix(source, destination+"/") && !strings.HasPrefix(destination, source+"/")
}

// InstallSignalRuntime requires a previously completed source cursor. Destination
// must already be private and empty on the first call. The managed caller stops
// all account consumers before calling this offline operation. Retries use the
// exact source, destination and cursor; no method here starts a service.
func InstallSignalRuntime(ctx context.Context, source, destination string, p interaction.SignalRecoveryCursor) (SignalRuntimeInstallation, error) {
	return installSignalRuntime(ctx, source, destination, p, nil)
}

func installSignalRuntime(ctx context.Context, source, destination string, p interaction.SignalRecoveryCursor, fault func(string) error) (out SignalRuntimeInstallation, err error) {
	if !runtimeInstallationPaths(source, destination) {
		return out, fmt.Errorf("Signal source and destination must be separate absolute directories")
	}
	if err = p.Validate(); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	src, err := privateInboxDirectory(source)
	if err != nil {
		return out, err
	}
	defer src.Close()
	sourceLease, err := src.Lock("inbox.lock", true, false)
	if err != nil {
		return out, err
	}
	defer sourceLease.Close()
	dst, err := privateInboxDirectory(destination)
	if err != nil {
		return out, err
	}
	defer dst.Close()
	info, err := dst.File().Stat()
	if err != nil {
		return out, err
	}
	st := info.Sys().(*syscall.Stat_t)
	want := signalRuntimeIntent{1, source, destination, uint64(st.Dev), st.Ino, p}
	var saved signalRuntimeIntent
	e := src.ReadJSON(runtimeInstallIntent, &saved)
	if os.IsNotExist(e) {
		if e = emptyRuntimeDestination(dst); e != nil {
			return out, e
		}
		if e = absentRuntimeFile(src, runtimeInstallComplete); e != nil {
			return out, e
		}
		if e = verifyRuntimeSource(ctx, src, p); e != nil {
			return out, e
		}
		if err = src.WriteJSON(runtimeInstallIntent, want, false); err != nil {
			return
		}
	} else if e != nil {
		return out, e
	} else if protocol.Hash(saved) != protocol.Hash(want) {
		return out, fmt.Errorf("Signal source is bound to another runtime installation")
	}
	if err = inboxRestoreBoundary(fault, "runtime_source_fenced"); err != nil {
		return
	}
	// Only the frozen source intent owns creation of the destination lease. Once
	// any other destination artifact exists, a missing lease is never recreated.
	lease, err := dst.Lock("inbox.lock", true, false)
	if os.IsNotExist(err) {
		if err = emptyRuntimeDestination(dst); err != nil {
			return out, err
		}
		lease, err = dst.Lock("inbox.lock", true, true)
	}
	if err != nil {
		return out, err
	}
	defer lease.Close()
	check := func() error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := checkRuntimeLease(src, sourceLease); e != nil {
			return e
		}
		if e := checkRuntimeLease(dst, lease); e != nil {
			return e
		}
		if e := checkRuntimeDirectory(source, src); e != nil {
			return e
		}
		if e := checkRuntimeDirectory(destination, dst); e != nil {
			return e
		}
		var current signalRuntimeIntent
		if e := src.ReadJSON(runtimeInstallIntent, &current); e != nil {
			return e
		}
		if protocol.Hash(current) != protocol.Hash(want) {
			return fmt.Errorf("Signal source installation intent changed")
		}
		return verifyRuntimeSource(ctx, src, p)
	}
	if err = check(); err != nil {
		return
	}
	saved = signalRuntimeIntent{}
	e = dst.ReadJSON(runtimeInstallIntent, &saved)
	if os.IsNotExist(e) {
		if err = onlyRuntimeLease(dst); err != nil {
			return
		}
		if err = dst.WriteJSON(runtimeInstallIntent, want, false); err != nil {
			return
		}
	} else if e != nil {
		return out, e
	} else if protocol.Hash(saved) != protocol.Hash(want) {
		return out, fmt.Errorf("Signal destination intent changed")
	}
	if err = inboxRestoreBoundary(fault, "runtime_destination_intent"); err != nil {
		return
	}
	e = dst.ReadJSON(runtimeInstallSeal, &out)
	if os.IsNotExist(e) {
		if err = absentRuntimeFile(dst, runtimeInstallComplete); err != nil {
			return
		}
		if err = absentRuntimeFile(src, runtimeInstallComplete); err != nil {
			return
		}
		for _, name := range []string{"inbox.db", "identity.json", signalRuntimeFile} {
			if err = absentRuntimeFile(dst, name); err != nil {
				return
			}
		}
		if err = resetRuntimeCopy(dst, runtimeInstallImage); err != nil {
			return
		}
		if err = dst.Capacity(80 << 20); err != nil {
			return
		}
		candidate, closeCandidate, e := runtimeSourceDirectory(src, p)
		if e != nil {
			return out, e
		}
		err = copyInboxArtifact(candidate, signalCursorDatabase, dst, runtimeInstallImage, InboxSnapshotManifest{DatabaseBytes: p.DatabaseBytes, DatabaseHash: p.DatabaseHash})
		closeCandidate()
		if err != nil {
			return
		}
		if err = inboxRestoreBoundary(fault, "runtime_image_copy"); err != nil {
			return
		}
		if err = promoteRuntimeImage(ctx, dst, p, fault); err != nil {
			return
		}
		if err = inboxRestoreBoundary(fault, "runtime_image_promoted"); err != nil {
			return
		}
		out = SignalRuntimeInstallation{Version: 1, Source: source, Destination: destination, DestinationDevice: want.DestinationDevice, DestinationInode: want.DestinationInode, Cursor: p}
		out.DatabaseHash, out.DatabaseBytes, err = runtimeImageDigest(dst)
		if err != nil {
			return
		}
		out.InstallationHash = protocol.Hash(out)
		if err = verifyRuntimeImage(ctx, dst, out); err != nil {
			return
		}
		if err = check(); err != nil {
			return
		}
		if err = dst.WriteJSON(runtimeInstallSeal, out, false); err != nil {
			return
		}
	} else if e != nil {
		return out, e
	}
	if err = out.validate(); err != nil {
		return
	}
	if protocol.Hash(out.intent()) != protocol.Hash(want) {
		return out, fmt.Errorf("Signal runtime seal changed")
	}
	if err = verifyRuntimeImage(ctx, dst, out); err != nil {
		return
	}
	if err = inboxRestoreBoundary(fault, "runtime_image_sealed"); err != nil {
		return
	}
	if err = verifyRuntimeSeal(ctx, dst, out); err != nil {
		return
	}
	complete, e := runtimeInstallationRecord(dst, runtimeInstallComplete, out)
	if e != nil {
		return out, e
	}
	sourceComplete, e := runtimeInstallationRecord(src, runtimeInstallComplete, out)
	if e != nil {
		return out, e
	}
	if sourceComplete && !complete {
		return out, fmt.Errorf("Signal source completion lost destination completion")
	}
	if !complete {
		// A copy interrupted before completion belongs to this intent. No runtime
		// can open it before both independent completion records exist.
		if err = resetRuntimeCopy(dst, "inbox.db"); err != nil {
			return
		}
		if err = copyInboxArtifact(dst, runtimeInstallImage, dst, "inbox.db", InboxSnapshotManifest{DatabaseBytes: out.DatabaseBytes, DatabaseHash: out.DatabaseHash}); err != nil {
			return
		}
		if err = inboxRestoreBoundary(fault, "runtime_database_installed"); err != nil {
			return
		}
		pin := runtimePin(p)
		if err = runtimeJSONOnce(dst, "identity.json", pin); err != nil {
			return
		}
		if err = runtimeJSONOnce(dst, signalRuntimeFile, p); err != nil {
			return
		}
		if err = inboxRestoreBoundary(fault, "runtime_identity_installed"); err != nil {
			return
		}
		if err = check(); err != nil {
			return
		}
		if err = verifyStandaloneInboxFile(dst, "inbox.db", out.DatabaseBytes, out.DatabaseHash); err != nil {
			return
		}
		if err = verifyRuntimeSeal(ctx, dst, out); err != nil {
			return
		}
		if err = dst.WriteJSON(runtimeInstallComplete, out, false); err != nil {
			return
		}
	}
	if err = inboxRestoreBoundary(fault, "runtime_destination_complete"); err != nil {
		return
	}
	if !sourceComplete {
		if err = verifyStandaloneInboxFile(dst, "inbox.db", out.DatabaseBytes, out.DatabaseHash); err != nil {
			return
		}
		if err = check(); err != nil {
			return
		}
		if err = verifyRuntimeSeal(ctx, dst, out); err != nil {
			return
		}
		if ok, e := runtimeInstallationRecord(dst, runtimeInstallComplete, out); e != nil {
			return out, e
		} else if !ok {
			return out, fmt.Errorf("Signal destination completion disappeared")
		}
		if err = src.WriteJSON(runtimeInstallComplete, out, false); err != nil {
			return
		}
	}
	if err = inboxRestoreBoundary(fault, "runtime_source_complete"); err != nil {
		return
	}
	if err = check(); err != nil {
		return
	}
	err = verifyRuntimeInstallation(ctx, src, dst, out)
	return
}
