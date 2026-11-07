package workerctl

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func notificationOutputFixture(t *testing.T) (ControlRequest, ResolvedPaths, *install.InstallationObservation) {
	t.Helper()
	r, paths, endpoint, _ := declaredLEDFixture(t)
	r.Desired.Backup = nil
	r.DesiredHash, _ = r.Desired.Fingerprint()
	r.ledEndpoint = &endpoint
	var observed *install.InstallationObservation
	lifecycle := notificationLifecycle{
		inspect: func(context.Context, install.ManagedSpec) (install.ManagedManifest, bool, error) {
			if observed == nil {
				return install.ManagedManifest{}, false, nil
			}
			return observed.Manifest, true, nil
		},
		ensure: func(ctx context.Context, spec install.ManagedSpec) (install.ManagedManifest, error) {
			d, err := safefs.Open(spec.State)
			if err != nil {
				t.Fatal(err)
			}
			var b worker.GatewayBootstrap
			err = d.ReadJSON("bootstrap.json", &b)
			d.Close()
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Mkdir(spec.Root, 0700); err != nil {
				t.Fatal(err)
			}
			m := install.ManagedManifest{Root: spec.Root, Identity: b.Identity, Mode: "verification", Paths: install.ServicePaths{State: spec.State}, UnitDirectory: spec.UnitDirectory}
			observed = &install.InstallationObservation{Manifest: m, Phase: "complete", Unbound: true}
			return m, nil
		},
	}
	if _, err := applyNotificationBootstrap(context.Background(), r, paths, "/helper", lifecycle); err != nil {
		t.Fatal(err)
	}
	return r, paths, observed
}

func TestDeclaredLEDOutputSuccessorsRetainIdentityAndLostReply(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	r, paths, observed := notificationOutputFixture(t)
	identity := observed.Manifest.Identity
	calls := 0
	lifecycle := notificationLEDLifecycle{
		inspect: func(context.Context, string) (install.InstallationObservation, error) { return *observed, nil },
		set: func(_ context.Context, root, expected string, disabled bool) (install.ManagedManifest, error) {
			if root != paths.Installation || expected != protocol.Hash(observed.Manifest) {
				t.Fatal("lost exact predecessor")
			}
			calls++
			op, err := safefs.NewID("n")
			if err != nil {
				t.Fatal(err)
			}
			observed.Manifest.LEDOutput = &install.ManagedLEDOutput{Operation: op, Disabled: disabled}
			return observed.Manifest, errors.New("lost response")
		},
	}
	for round, enabled := range []bool{false, true, false} {
		r.Desired.Notification.StatusLEDs.Enabled = enabled
		r.DesiredHash, _ = r.Desired.Fingerprint()
		if _, handled, err := reconcileDeclaredLEDOutput(context.Background(), r, paths, lifecycle); !handled || err == nil {
			t.Fatal("lost response not reported", handled, err)
		}
		for range 2 {
			result, handled, err := reconcileDeclaredLEDOutput(context.Background(), r, paths, lifecycle)
			if err != nil || !handled || result.Code != "ok" || result.LEDOutput == nil || result.LEDOutput.Disabled == enabled || result.Identity == nil || *result.Identity != identity || calls != round+1 {
				t.Fatal(result, handled, calls, err)
			}
		}
		status, err := inspectNotificationControl(context.Background(), r, paths, lifecycle.inspect, nil)
		if err != nil || status.LEDOutput == nil || status.LEDOutput.Disabled == enabled {
			t.Fatal("status lost output permission", status, err)
		}
	}
	changed := *r.Desired.Notification
	r.Desired.Notification = &changed
	r.Desired.Notification.Limits.MaxMessages++
	r.DesiredHash, _ = r.Desired.Fingerprint()
	if _, _, err := reconcileDeclaredLEDOutput(context.Background(), r, paths, lifecycle); err == nil {
		t.Fatal("unrelated policy mutation admitted")
	}
	if calls != 3 {
		t.Fatal("unrelated change reached service")
	}
}

func TestDeclaredLEDOutputRefusesLostPolicyAndChangedCredentials(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, kind := range []string{"declaration", "credential", "runtime", "bound"} {
		t.Run(kind, func(t *testing.T) {
			r, paths, observed := notificationOutputFixture(t)
			d, err := safefs.Open(paths.Control)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			switch kind {
			case "declaration":
				if err = os.Remove(paths.Control + "/declarations/" + r.DesiredHash + ".json"); err != nil {
					t.Fatal(err)
				}
			case "credential":
				r.Inputs["token"] = []byte("123:changed-token")
			case "runtime":
				if err = os.WriteFile(paths.Runtime, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "bound":
				observed.Unbound = false
			}
			r.Desired.Notification.StatusLEDs.Enabled = false
			r.DesiredHash, _ = r.Desired.Fingerprint()
			lifecycle := notificationLEDLifecycle{inspect: func(context.Context, string) (install.InstallationObservation, error) { return *observed, nil }, set: func(context.Context, string, string, bool) (install.ManagedManifest, error) {
				t.Fatal("unsafe update reached service")
				return install.ManagedManifest{}, nil
			}}
			if _, handled, err := reconcileDeclaredLEDOutput(context.Background(), r, paths, lifecycle); !handled || err == nil {
				t.Fatal("unsafe update accepted", handled, err)
			}
		})
	}
}

func TestDeclarationHistoryDetectsChangedSnapshot(t *testing.T) {
	r, paths := bootstrapControlFixture(t)
	d, err := ensureConfigDirectory(paths.Control)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = retainDeclaration(d, r.Desired, r.DesiredHash); err != nil {
		t.Fatal(err)
	}
	if err = d.WriteJSON("declarations/"+r.DesiredHash+".json", config.DesiredWorkers{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err = readDeclaration(d, r.DesiredHash); err == nil {
		t.Fatal("changed policy snapshot accepted")
	}
	if err = retainDeclaration(d, r.Desired, r.DesiredHash); err == nil {
		t.Fatal("changed policy snapshot overwritten")
	}
}
