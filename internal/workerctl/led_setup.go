package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/ledartifact"
	"github.com/dryaf/deploy/internal/notification/ledhost"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/transport"
	"golang.org/x/sys/unix"
)

type LEDPrerequisite struct {
	Plan        ledhost.Plan `json:"plan"`
	RequestFile string       `json:"request_file"`
	ReviewFile  string       `json:"review_file"`
	Command     string       `json:"command"`
}
type ledSetupLifecycle struct {
	discover func(string) (string, error)
	receipt  func(int) (ledhost.Receipt, error)
}
type ledSetupPins struct {
	Version      int          `json:"version"`
	DesiredHash  string       `json:"desired_hash"`
	HelperSHA256 string       `json:"helper_sha256"`
	Spec         ledhost.Spec `json:"spec"`
}

// prepareManagedLED stages a reviewable administrator request. It never invokes
// sudo, changes hardware permissions, writes system units or starts a root daemon.
func prepareManagedLED(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper, source string, lifecycle ledSetupLifecycle) (*LEDPrerequisite, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d := r.Desired.Notification.StatusLEDs
	if !d.Enabled || d.Daemon.Mode != "managed" {
		return nil, nil
	}
	if _, err := declaredLEDSocket(d, r.Facts.Home, r.Facts.UID); err != nil {
		return nil, err
	}
	if !validDigest(r.LEDSHA256) || d.Daemon.SHA256 != "" && d.Daemon.SHA256 != r.LEDSHA256 {
		return nil, fmt.Errorf("managed LED artifact selection changed")
	}
	spec, err := ledhost.ResolveSpec(r.Facts.UID, r.Facts.LEDs, d.Daemon.Device, d.PowerLED, r.LEDSHA256, lifecycle.discover)
	if err != nil {
		return nil, err
	}
	p, err := ledhost.Build(spec)
	if err != nil {
		return nil, err
	}
	receipt, err := lifecycle.receipt(r.Facts.UID)
	if err == nil {
		if receipt.Version != 1 || !receipt.Complete || receipt.Hash != p.Hash || receipt.Spec != spec {
			return nil, fmt.Errorf("managed LED host pins changed; explicit daemon update is required")
		}
		return nil, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("managed LED host receipt cannot be verified: %w", err)
	}
	// Once a worker exists, a lost prerequisite receipt cannot be recreated as
	// fresh setup, even if the host administrator removed its entire directory.
	if _, e := os.Lstat(paths.Installation); !errors.Is(e, os.ErrNotExist) {
		return nil, fmt.Errorf("LED prerequisite authority missing from an existing worker; inspect host state")
	}
	if _, err = ledartifact.Verify(source, r.LEDSHA256); err != nil {
		return nil, err
	}
	base := filepath.Join(paths.Data, "led-prerequisites", p.Hash)
	dir, err := ensureConfigDirectory(base)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	lock, err := dir.Lock("operation.lock", true, true)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	pins := ledSetupPins{1, r.DesiredHash, r.HelperSHA256, spec}
	var previous ledSetupPins
	if e := dir.ReadJSON("inputs.json", &previous); errors.Is(e, os.ErrNotExist) {
		if e = dir.WriteJSON("inputs.json", pins, false); e != nil {
			return nil, e
		}
	} else if e != nil || previous != pins {
		return nil, fmt.Errorf("LED prerequisite inputs changed; finish the retained plan with its original declaration and helper")
	}
	artifactFile := filepath.Join(base, "daemon.pyz")
	if err = publishLEDArtifact(dir, source, r.LEDSHA256); err != nil {
		return nil, err
	}
	// Keep the exact helper available after the temporary SSH bootstrap exits.
	helperFile := filepath.Join(base, "deploy")
	if err = publishSignalExecutable(helperFile, helper, r.HelperSHA256, r.Facts.Arch, true); err != nil {
		return nil, err
	}
	request := ledhost.Request{Spec: spec, Artifact: artifactFile}
	for name, value := range map[string]any{"request.json": request, "review.json": p} {
		want, e := json.Marshal(value)
		if e != nil {
			return nil, e
		}
		got, e := safefs.ReadFile(dir, name, 64<<10)
		if errors.Is(e, os.ErrNotExist) {
			if e = dir.WriteJSON(name, value, false); e != nil {
				return nil, e
			}
		} else if e != nil || !bytes.Equal(got, want) {
			return nil, fmt.Errorf("retained LED prerequisite request changed")
		}
	}
	requestFile := filepath.Join(base, "request.json")
	return &LEDPrerequisite{Plan: p, RequestFile: requestFile, ReviewFile: filepath.Join(base, "review.json"), Command: "sudo -- " + transport.ShellQuote(helperFile) + " led-host apply " + transport.ShellQuote(requestFile) + " " + p.Hash}, nil
}

func inspectDeclaredLEDProgress(ctx context.Context, r ControlRequest, paths ResolvedPaths, readReceipt func(int) (ledhost.Receipt, error)) (ControlResult, bool, error) {
	result := ControlResult{Version: 1, State: "resumable", Code: "led_host_prerequisite", Next: "run deploy notification-worker resume " + r.Server + " with the same declaration"}
	if _, err := os.Lstat(filepath.Join(paths.Control, "operation.json")); err == nil {
		return result, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, false, err
	}
	d, err := safefs.Open(filepath.Join(paths.Data, "led-prerequisites"))
	if errors.Is(err, os.ErrNotExist) {
		return result, false, nil
	}
	if err != nil {
		return result, true, err
	}
	defer d.Close()
	entries, err := d.File().ReadDir(65)
	if err != nil && err != io.EOF {
		return result, true, err
	}
	if len(entries) != 1 || !entries[0].IsDir() || !validDigest(entries[0].Name()) {
		return result, true, fmt.Errorf("LED prerequisite staging is ambiguous; inspect retained requests")
	}
	sub, err := d.Sub(entries[0].Name())
	if err != nil {
		return result, true, err
	}
	defer sub.Close()
	lock, err := sub.Lock("operation.lock", false, false)
	if err != nil {
		return result, true, err
	}
	defer lock.Close()
	var pins ledSetupPins
	if err = sub.ReadJSON("inputs.json", &pins); err != nil {
		return result, true, err
	}
	p, err := ledhost.Build(pins.Spec)
	if err != nil {
		return result, true, err
	}
	if pins.Version != 1 || pins.HelperSHA256 != r.HelperSHA256 || pins.Spec.UID != r.Facts.UID || p.Hash != entries[0].Name() {
		return result, true, fmt.Errorf("LED prerequisite target or helper changed")
	}
	if pins.DesiredHash != r.DesiredHash {
		return ControlResult{Version: 1, State: "needs_action", Code: "policy_changed", Next: "finish the retained LED host prerequisite with its original declaration"}, true, nil
	}
	base := filepath.Join(paths.Data, "led-prerequisites", p.Hash)
	var request ledhost.Request
	if err = sub.ReadJSON("request.json", &request); errors.Is(err, os.ErrNotExist) {
		return result, true, nil
	} else if err != nil {
		return result, true, err
	}
	if request.Spec != pins.Spec || request.Artifact != filepath.Join(base, "daemon.pyz") {
		return result, true, fmt.Errorf("LED prerequisite request changed")
	}
	if _, err = ledartifact.Verify(request.Artifact, pins.Spec.ArtifactSHA256); err != nil {
		return result, true, err
	}
	helper := filepath.Join(base, "deploy")
	if err = VerifyArtifact(Artifact{helper, pins.HelperSHA256}, r.Facts.Arch, true); err != nil {
		return result, true, err
	}
	receipt, err := readReceipt(r.Facts.UID)
	if err == nil {
		if receipt.Spec != pins.Spec || receipt.Hash != p.Hash || !receipt.Complete {
			return result, true, fmt.Errorf("LED host receipt differs from staging")
		}
		// Other setup children may now be pending; let their canonical status run.
		if _, e := os.Lstat(filepath.Join(paths.Control, "signal")); e == nil {
			return result, false, nil
		} else if !errors.Is(e, os.ErrNotExist) {
			return result, true, e
		}
		result.State, result.Code = "configured", "led_host_ready"
		result.Next = "managed LED host prerequisite complete; run deploy notification-worker resume " + r.Server + " to continue worker setup"
		return result, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return result, true, err
	}
	result.State = "needs_action"
	result.LEDPrerequisite = &LEDPrerequisite{Plan: p, RequestFile: filepath.Join(base, "request.json"), ReviewFile: filepath.Join(base, "review.json"), Command: "sudo -- " + transport.ShellQuote(helper) + " led-host apply " + transport.ShellQuote(filepath.Join(base, "request.json")) + " " + p.Hash}
	result.Next = "review and apply the retained administrator prerequisite on " + r.Server + ", then run deploy notification-worker resume " + r.Server
	return result, true, ctx.Err()
}

func publishLEDArtifact(dir *safefs.Directory, source, digest string) error {
	if raw, err := safefs.ReadFile(dir, "daemon.pyz", ledartifact.MaxBytes); err == nil {
		if inputDigest(raw) != digest {
			return fmt.Errorf("staged LED artifact changed")
		}
		_, err = ledartifact.Inspect(raw)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(f, ledartifact.MaxBytes+1))
	f.Close()
	if err != nil || len(raw) > ledartifact.MaxBytes || inputDigest(raw) != digest {
		return fmt.Errorf("LED artifact changed during staging")
	}
	if _, err = ledartifact.Inspect(raw); err != nil {
		return err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return err
	}
	tmp := "." + id
	out, err := dir.Open(tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(dir.File().Fd()), tmp, 0)
	_, err = out.Write(raw)
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return dir.Rename(tmp, dir, "daemon.pyz")
}

// RunLEDHostCLI is deliberately separate from rootless worker apply. Review is
// read-only; apply requires root and the exact displayed plan hash.
func RunLEDHostCLI(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 3 && args[0] == "verify-artifact" {
		metadata, err := ledartifact.Verify(args[1], args[2])
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(metadata)
	}
	if len(args) < 2 || len(args) > 3 || (args[0] != "review" && args[0] != "apply" && args[0] != "review-remove" && args[0] != "remove") || (args[0] == "review" || args[0] == "review-remove") && len(args) != 2 || (args[0] == "apply" || args[0] == "remove") && len(args) != 3 {
		return fmt.Errorf("usage: deploy led-host review|review-remove REQUEST | deploy led-host apply|remove REQUEST PLAN_HASH")
	}
	if !filepath.IsAbs(args[1]) || filepath.Clean(args[1]) != args[1] {
		return fmt.Errorf("LED prerequisite request path must be absolute")
	}
	dir, err := safefs.Open(filepath.Dir(args[1]))
	if err != nil {
		return err
	}
	defer dir.Close()
	var r ledhost.Request
	if err = dir.ReadJSON(filepath.Base(args[1]), &r); err != nil {
		return err
	}
	if args[0] == "review-remove" || args[0] == "remove" {
		p, err := ledhost.BuildRemoval(r.Spec)
		if err != nil {
			return err
		}
		if args[0] == "review-remove" {
			return json.NewEncoder(out).Encode(p)
		}
		receipt, err := ledhost.Remove(ctx, r.Spec, args[2])
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(receipt)
	}
	p, err := ledhost.Build(r.Spec)
	if err != nil {
		return err
	}
	if args[0] == "review" {
		return json.NewEncoder(out).Encode(p)
	}
	receipt, err := ledhost.Apply(ctx, r, args[2])
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(receipt)
}
