package workerctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/ledhost"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/transport"
)

type LEDHostRemoval struct {
	Plan        ledhost.RemovalPlan `json:"plan"`
	RequestFile string              `json:"request_file"`
	ReviewFile  string              `json:"review_file"`
	Command     string              `json:"command"`
}
type ledRemovalPins struct {
	Version      int          `json:"version"`
	UID          int          `json:"uid"`
	Home         string       `json:"home"`
	DesiredHash  string       `json:"desired_hash"`
	HelperSHA256 string       `json:"helper_sha256"`
	Removal      string       `json:"removal"`
	Spec         ledhost.Spec `json:"spec"`
}
type ledRemovalLifecycle struct {
	installed func(int) (ledhost.Receipt, error)
	removed   func(int) (ledhost.RemovalReceipt, error)
}

func finishNotificationHostRemoval(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, result ControlResult, lifecycle ledRemovalLifecycle) (ControlResult, error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	d, err := safefs.Open(paths.Control)
	if err != nil {
		return result, err
	}
	defer d.Close()
	lease, err := d.Lock("operation.lock", true, false)
	if err != nil {
		return result, err
	}
	defer lease.Close()
	var j OperationJournal
	if err = d.ReadJSON("operation.json", &j); err != nil {
		return result, err
	}
	if validateOperationJournal(j) != nil || !j.Complete || j.Pins.Action != "uninstall" || j.Pins.DesiredHash != r.DesiredHash || j.Pins.UID != r.Facts.UID || j.Pins.Home != r.Facts.Home || j.Pins.Artifacts["helper"] != r.HelperSHA256 || j.Pins.Installations["notification-worker"] != paths.Installation || j.Pins.Installations["state"] != paths.State || j.Pins.Installations["runtime"] != paths.Runtime {
		return result, fmt.Errorf("LED host cleanup requires completed gateway removal authority")
	}
	receipt, valid := canonicalNotificationReceipt(j)
	if !valid {
		return result, fmt.Errorf("gateway removal receipt unavailable")
	}
	var pins ledRemovalPins
	err = d.ReadJSON("led-host-removal.json", &pins)
	starting := errors.Is(err, os.ErrNotExist)
	if !starting && err != nil {
		return result, err
	}
	if starting {
		// Use retained execution input, never a changed YAML daemon mode, to
		// identify the host service actually attached to this installation.
		var inputPins notificationInputPins
		if err = d.ReadJSON("notification-inputs.json", &inputPins); err != nil {
			return result, err
		}
		info, e := os.Lstat(paths.Runtime)
		if e != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxControlFrame {
			return result, fmt.Errorf("retained LED runtime unavailable")
		}
		raw, e := readPrivateGeneratedInput(paths.Runtime, int(info.Size()))
		if e != nil || inputDigest(raw) != inputPins.Runtime {
			return result, fmt.Errorf("retained LED runtime changed")
		}
		var input install.RuntimeInput
		if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
			return result, err
		}
		if input.LEDSocket != ledhost.Socket(r.Facts.UID) {
			return result, nil
		}
		var prior OperationJournal
		if err = d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior); err != nil {
			return result, err
		}
		if validateOperationJournal(prior) != nil || !prior.Complete || prior.ID != j.Pins.PreviousOperation || prior.Digest() != j.Pins.Previous {
			return result, fmt.Errorf("LED daemon ownership predecessor changed")
		}
		// An existing-mode client may use the same socket. Only the explicit
		// managed artifact pin grants ownership of the host prerequisite.
		artifact, owned := prior.Pins.Artifacts["led-daemon"]
		if !owned {
			return result, nil
		}
		if !validDigest(artifact) {
			return result, fmt.Errorf("managed LED ownership pin changed")
		}
		host, e := lifecycle.installed(r.Facts.UID)
		if e != nil {
			return result, fmt.Errorf("gateway removed; managed LED host receipt requires review: %w", e)
		}
		p, e := ledhost.Build(host.Spec)
		if e != nil || !host.Complete || host.Version != 1 || host.Spec.UID != r.Facts.UID || host.Hash != p.Hash || host.Spec.ArtifactSHA256 != artifact {
			return result, fmt.Errorf("managed LED host receipt differs")
		}
		pins = ledRemovalPins{Version: 1, UID: r.Facts.UID, Home: r.Facts.Home, DesiredHash: r.DesiredHash, HelperSHA256: r.HelperSHA256, Removal: receipt, Spec: host.Spec}
		if err = d.WriteJSON("led-host-removal.json", pins, false); err != nil {
			return result, err
		}
	}
	if pins.Version != 1 || pins.UID != r.Facts.UID || pins.Home != r.Facts.Home || pins.DesiredHash != r.DesiredHash || pins.HelperSHA256 != r.HelperSHA256 || pins.Removal != receipt || pins.Spec.UID != r.Facts.UID {
		return result, fmt.Errorf("LED removal staging differs from retained intent")
	}
	p, err := ledhost.BuildRemoval(pins.Spec)
	if err != nil {
		return result, err
	}
	removed, e := lifecycle.removed(r.Facts.UID)
	if e == nil {
		if removed.Version != 1 || !removed.Complete || removed.Spec != p.Spec || removed.Hash != p.Hash || removed.Installation != p.Installation {
			return result, fmt.Errorf("LED host removal receipt differs")
		}
		result.Next += "; managed LED daemon removed, registry and hardware lock history retained"
		return result, nil
	}
	if !errors.Is(e, os.ErrNotExist) {
		return result, fmt.Errorf("LED host removal outcome requires review: %w", e)
	}
	base := filepath.Join(paths.Data, "led-removal", p.Hash)
	dir, err := ensureConfigDirectory(base)
	if err != nil {
		return result, err
	}
	defer dir.Close()
	if err = publishSignalExecutable(filepath.Join(base, "deploy"), helper, r.HelperSHA256, r.Facts.Arch, true); err != nil {
		return result, err
	}
	for name, value := range map[string]any{"request.json": ledhost.Request{Spec: p.Spec}, "review.json": p} {
		raw, e := json.Marshal(value)
		if e != nil {
			return result, e
		}
		if err = publishPrivateInput(filepath.Join(base, name), raw); err != nil {
			return result, err
		}
	}
	result.LEDHostRemoval = &LEDHostRemoval{Plan: p, RequestFile: filepath.Join(base, "request.json"), ReviewFile: filepath.Join(base, "review.json"), Command: "sudo -- " + transport.ShellQuote(filepath.Join(base, "deploy")) + " led-host remove " + transport.ShellQuote(filepath.Join(base, "request.json")) + " " + p.Hash}
	result.State, result.Code = "cleanup_pending", "led_host_removal_required"
	result.Next = "gateway removed; review the staged administrator LED removal on " + r.Server + ", run its displayed command, then repeat deploy notification-worker uninstall " + r.Server
	return result, nil
}

func inspectNotificationHostRemoval(r ControlRequest, d *safefs.Directory, j OperationJournal, result ControlResult, read func(int) (ledhost.RemovalReceipt, error)) (ControlResult, error) {
	var pins ledRemovalPins
	if err := d.ReadJSON("led-host-removal.json", &pins); errors.Is(err, os.ErrNotExist) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	receipt, valid := canonicalNotificationReceipt(j)
	if !valid || !j.Complete || j.Pins.Action != "uninstall" || pins.Version != 1 || pins.UID != r.Facts.UID || pins.Home != r.Facts.Home || pins.DesiredHash != j.Pins.DesiredHash || pins.Removal != receipt || pins.Spec.UID != r.Facts.UID {
		return result, fmt.Errorf("LED removal authority differs")
	}
	p, err := ledhost.BuildRemoval(pins.Spec)
	if err != nil {
		return result, err
	}
	removed, err := read(r.Facts.UID)
	if errors.Is(err, os.ErrNotExist) {
		result.State, result.Code = "cleanup_pending", "led_host_removal_required"
		result.Next = "gateway removed; run deploy notification-worker uninstall " + r.Server + " to inspect/resume the retained administrator LED removal"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if removed.Version != 1 || !removed.Complete || removed.Spec != p.Spec || removed.Hash != p.Hash || removed.Installation != p.Installation {
		return result, fmt.Errorf("LED removal completion differs")
	}
	result.Next += "; managed LED daemon removed, persistent registry and host history retained"
	return result, nil
}
