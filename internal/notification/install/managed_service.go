package install

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
)

type managedServiceState struct {
	Load, Active, Fragment, DropIns, UnitFile, Names string
}

type managedServices interface {
	Inspect(context.Context) (managedServiceState, error)
	Stop(context.Context) error
	Reload(context.Context) error
	EnableStart(context.Context) error
	Disable(context.Context) error
	Verify(context.Context, ManagedManifest) error
}

type userManagedServices struct{ deliveryInspector *userDeliveryInspector }

func (userManagedServices) command(ctx context.Context, args ...string) error {
	_, err := process.LimitedOutput(exec.CommandContext(ctx, "systemctl", append([]string{"--user", "--no-pager"}, args...)...), 45*time.Second, 16384)
	return err
}
func (u userManagedServices) Stop(ctx context.Context) error {
	return u.command(ctx, "stop", managedUnit)
}
func (u userManagedServices) Reload(ctx context.Context) error {
	return u.command(ctx, "daemon-reload")
}
func (u userManagedServices) EnableStart(ctx context.Context) error {
	return u.command(ctx, "enable", "--now", managedUnit)
}
func (u userManagedServices) Disable(ctx context.Context) error {
	return u.command(ctx, "disable", managedUnit)
}

func (userManagedServices) Inspect(ctx context.Context) (s managedServiceState, err error) {
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "--no-pager", "show", managedUnit, "--property=LoadState,ActiveState,FragmentPath,DropInPaths,UnitFileState,Names")
	var out, stderr process.Buffer
	out.Limit = 16384
	stderr.Limit = 4096
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = process.Run(cmd, 15*time.Second, 2*time.Second)
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out.Bytes())), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || seen[key] {
			return s, fmt.Errorf("invalid systemd inspection")
		}
		seen[key] = true
		switch key {
		case "LoadState":
			s.Load = value
		case "ActiveState":
			s.Active = value
		case "FragmentPath":
			s.Fragment = value
		case "DropInPaths":
			s.DropIns = value
		case "UnitFileState":
			s.UnitFile = value
		case "Names":
			s.Names = value
		default:
			return s, fmt.Errorf("unexpected systemd property")
		}
	}
	if len(seen) != 6 || s.Load == "" || s.Active == "" {
		return s, fmt.Errorf("incomplete systemd inspection: %w", err)
	}
	if err != nil && !(s.Load == "not-found" && s.Active == "inactive" && s.Fragment == "" && s.DropIns == "") {
		return s, err
	}
	return s, nil
}

func (u userManagedServices) Verify(ctx context.Context, m ManagedManifest) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	api := client.Client{Socket: m.Paths.Socket, Identity: m.Identity}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var h worker.Health
		call, cancelCall := context.WithTimeout(ctx, time.Second)
		var err error
		if m.Mode == "delivery" {
			inspector := userDeliveryInspector{}
			if u.deliveryInspector != nil {
				inspector = *u.deliveryInspector
			}
			api.PeerPID, err = inspector.inspect(call, m, false)
			if err == nil && api.PeerPID == 0 {
				err = fmt.Errorf("delivery process is not running")
			}
		}
		if err == nil {
			_, err = api.Call(call, protocol.Request{Operation: "system.status"}, &h)
		}
		cancelCall()
		if err == nil {
			profileHash := ""
			if m.SignalProfiles != nil {
				profileHash = m.SignalProfiles.Hash
			}
			if h.SignalProfilesHash != profileHash {
				return fmt.Errorf("managed worker has a different effective Signal profile binding")
			}
			activationHash := ""
			if m.DeliveryActivation != nil {
				activationHash = m.DeliveryActivation.Hash
			}
			if h.VerificationOnly != (m.Mode != "delivery") || h.DeliveryActivationHash != activationHash || h.Identity != m.Identity || h.InstallationRevision != m.Revision || h.RecoveryCommitHash != m.RecoveryCommitHash || h.TelegramRenewalID != m.TelegramRenewalID {
				return fmt.Errorf("managed worker health does not identify the expected verification revision")
			}
			if m.Mode == "delivery" {
				loop, ok := h.Loops["delivery-admission"]
				if !ok || loop.Condition != "ok" || !loop.ValidUntil.After(time.Now()) {
					return fmt.Errorf("delivery runtime has no current admission observation")
				}
			}
			if m.SignalVerification != nil {
				loop, ok := h.Loops["signal-verification"]
				if h.SignalVerificationHash != m.SignalVerification.ManifestHash || !ok || loop.Condition != "ok" || !loop.ValidUntil.After(time.Now()) {
					return fmt.Errorf("managed worker has no current Signal verification binding")
				}
				return checkManagedSignalHealth(ctx, m)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("managed worker did not become ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func inspectManagedService(ctx context.Context, svc managedServices, m ManagedManifest, allowMissing bool) (managedServiceState, error) {
	s, err := svc.Inspect(ctx)
	if err != nil {
		return s, err
	}
	if s.DropIns != "" {
		return s, fmt.Errorf("managed unit has foreign drop-ins")
	}
	if s.Names != "" && s.Names != managedUnit {
		return s, fmt.Errorf("managed unit has unexpected aliases")
	}
	if s.Load == "not-found" && s.Fragment == "" && s.Active == "inactive" && allowMissing {
		return s, nil
	}
	if s.Load != "loaded" || s.Fragment != filepath.Join(m.UnitDirectory, managedUnit) {
		return s, fmt.Errorf("systemd unit is not the pinned managed unit")
	}
	switch s.UnitFile {
	case "enabled", "disabled":
	default:
		return s, fmt.Errorf("unsupported unit enablement state")
	}
	return s, nil
}
