package workerctl

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func notificationReleaseFixture(t *testing.T) (ControlRequest, ResolvedPaths, *install.InstallationObservation, *safefs.Directory) {
	t.Helper()
	r, paths, o := notificationOutputFixture(t)
	o.Manifest.BinaryHash = r.HelperSHA256
	d, err := safefs.Open(paths.Control)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	var j OperationJournal
	if err = d.ReadJSON("operation.json", &j); err != nil {
		t.Fatal(err)
	}
	j.Children[0].Receipt = protocol.Hash(o.Manifest)
	if err = d.WriteJSON("operation.json", j, true); err != nil {
		t.Fatal(err)
	}
	return r, paths, o, d
}

func TestNotificationReleaseLostReplyScopeConversionAndNoop(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	r, paths, o, d := notificationReleaseFixture(t)
	ctx := context.Background()
	identity := o.Manifest.Identity
	starts, calls := 0, 0
	l := notificationReleaseLifecycle{inspect: func(context.Context, string) (install.InstallationObservation, error) { return *o, nil }, update: func(_ context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
		calls++
		if protocol.Hash(o.Manifest) != expected {
			t.Fatal("release lost predecessor")
		}
		if spec.BinarySHA256 != o.Manifest.BinaryHash {
			starts++
			o.Manifest.BinaryHash = spec.BinarySHA256
			o.Manifest.Revision, _ = safefs.NewID("n")
			return o.Manifest, errors.New("lost update response")
		}
		return o.Manifest, nil
	}}
	r.DesiredScope = 1
	r.DesiredHash, _ = r.Desired.NotificationFingerprint()
	for range 2 {
		got, handled, err := reconcileNotificationRelease(ctx, r, paths, "/helper", l)
		if err != nil || !handled || got.State != "waiting_for_producers" || starts != 0 || calls != 1 || *got.Identity != identity {
			t.Fatal("scope conversion restarted or changed authority", got, handled, err, starts, calls)
		}
	}
	r.HelperSHA256 = strings.Repeat("b", 64)
	if _, handled, err := reconcileNotificationRelease(ctx, r, paths, "/helper", l); !handled || err == nil {
		t.Fatal("lost update result hidden")
	}
	for range 2 {
		got, handled, err := reconcileNotificationRelease(ctx, r, paths, "/helper", l)
		if err != nil || !handled || starts != 1 || calls != 2 || *got.Identity != identity {
			t.Fatal("release retry repeated effects", got, err, starts, calls)
		}
	}
	status, err := inspectNotificationControl(ctx, r, paths, l.inspect, nil)
	if err != nil || status.State != "waiting_for_producers" {
		t.Fatal("release status lost authority", status, err)
	}
	plan, err := planNotificationControl(ctx, r, paths, l.inspect)
	if err != nil || plan.State != "up_to_date" {
		t.Fatal("release plan lost desired policy", plan, err)
	}
	_, err = testNotificationControl(ctx, r, paths, func(_ context.Context, root, receipt string) (install.DiagnosticResult, error) {
		if root != paths.Installation || receipt != protocol.Hash(o.Manifest) {
			t.Fatal("diagnostic lost updated gateway receipt")
		}
		return install.DiagnosticResult{Status: "provider_accepted"}, nil
	})
	if err != nil {
		t.Fatal("updated gateway cannot perform explicit diagnostic", err)
	}
	// Equivalent operator artifact-reference edits retain the same executable.
	r.Desired.Notification.Artifacts.Catalog = "/new/local/catalog.json"
	r.DesiredHash, _ = r.Desired.NotificationFingerprint()
	if _, handled, err := reconcileNotificationRelease(ctx, r, paths, "/helper", l); err != nil || !handled || starts != 1 {
		t.Fatal("equivalent release reference restarted", handled, err)
	}
	var j OperationJournal
	if err = d.ReadJSON("operation.json", &j); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(paths.Control + "/artifact-" + j.Children[0].Operation + ".json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = reconcileNotificationRelease(ctx, r, paths, "/helper", l); err == nil {
		t.Fatal("missing completed artifact receipt recreated")
	}
	if _, err = inspectNotificationControl(ctx, r, paths, l.inspect, nil); err == nil {
		t.Fatal("missing artifact evidence reported healthy")
	}
}

func TestNotificationReleaseRefusesPolicyCredentialAndAuthorityChanges(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, kind := range []string{"bound", "policy", "credential", "snapshot", "pending", "led-artifact"} {
		t.Run(kind, func(t *testing.T) {
			r, paths, o, d := notificationReleaseFixture(t)
			switch kind {
			case "led-artifact":
				r.LEDSHA256 = strings.Repeat("d", 64)
			case "bound":
				o.Unbound = false
			case "policy":
				r.Desired.Notification.Limits.MaxMessages++
				r.DesiredHash, _ = r.Desired.Fingerprint()
			case "credential":
				r.Inputs["token"] = []byte("123:changed")
			case "snapshot":
				if err := os.Remove(paths.Control + "/declarations/" + r.DesiredHash + ".json"); err != nil {
					t.Fatal(err)
				}
			case "pending":
				var j OperationJournal
				if err := d.ReadJSON("operation.json", &j); err != nil {
					t.Fatal(err)
				}
				j.Complete = false
				if err := d.WriteJSON("operation.json", j, true); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(paths.Control + "/operation.json")
			if err != nil {
				t.Fatal(err)
			}
			r.HelperSHA256 = strings.Repeat("b", 64)
			l := notificationReleaseLifecycle{inspect: func(context.Context, string) (install.InstallationObservation, error) { return *o, nil }, update: func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error) {
				t.Fatal("unsafe release reached domain")
				return install.ManagedManifest{}, nil
			}}
			if _, handled, err := reconcileNotificationRelease(context.Background(), r, paths, "/helper", l); !handled || err == nil {
				t.Fatal("unsafe release accepted", handled, err)
			}
			after, _ := os.ReadFile(paths.Control + "/operation.json")
			if string(before) != string(after) {
				t.Fatal("rejected update changed operation")
			}
		})
	}
}
