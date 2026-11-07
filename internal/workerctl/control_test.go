package workerctl

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func bootstrapControlFixture(t *testing.T) (ControlRequest, ResolvedPaths) {
	t.Helper()
	dir, err := os.MkdirTemp("", "gateway-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	data := strings.Replace(cliInventory, "providers: [telegram]", `providers:
        telegram:
          token_input: ./token
          chat_id_input: ./chat`, 1)
	inv, err := config.ParseInventory(filepath.Join(dir, "server.yaml"), []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	desired := *inv.Servers["manager"].Desired
	hash, _ := desired.Fingerprint()
	facts := testFacts()
	facts.Home = dir
	facts.UID = os.Getuid()
	paths, err := ResolvePaths("notification-worker", desired.Notification.Paths, facts)
	if err != nil {
		t.Fatal(err)
	}
	r := ControlRequest{Version: 1, Worker: "notification-worker", Server: "manager", Action: "apply", Facts: facts, Desired: desired, DesiredHash: hash, HelperSHA256: strings.Repeat("a", 64), Inputs: map[string][]byte{"token": []byte("123:fixture-secret"), "chat_id": []byte("-456")}}
	return r, paths
}

func TestControlBootstrapLostReplyAndCredentialDrift(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	r, paths := bootstrapControlFixture(t)
	var saved *install.ManagedManifest
	calls := 0
	lost := true
	lifecycle := notificationLifecycle{
		ensure: func(ctx context.Context, spec install.ManagedSpec) (install.ManagedManifest, error) {
			calls++
			if saved == nil {
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
				saved = &install.ManagedManifest{Identity: b.Identity, Mode: "verification", Root: spec.Root}
			}
			if lost {
				lost = false
				return *saved, errors.New("lost acknowledgement")
			}
			return *saved, nil
		},
		inspect: func(ctx context.Context, spec install.ManagedSpec) (install.ManagedManifest, bool, error) {
			if saved == nil {
				return install.ManagedManifest{}, false, nil
			}
			return *saved, true, nil
		},
	}
	if _, err := applyNotificationBootstrap(context.Background(), r, paths, "/helper", lifecycle); err == nil || !strings.Contains(err.Error(), "lost acknowledgement") {
		t.Fatal("did not surface lost reply", err)
	}
	result, err := applyNotificationBootstrap(context.Background(), r, paths, "/helper", lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.State != "waiting_for_producers" || result.Identity == nil || *result.Identity != saved.Identity {
		t.Fatal("replayed or changed installation", calls, result)
	}
	observation := func(context.Context, string) (install.InstallationObservation, error) {
		return install.InstallationObservation{Manifest: *saved, Phase: "complete", Unbound: true}, nil
	}
	status, err := inspectNotificationControl(context.Background(), r, paths, observation, nil)
	if err != nil || status.State != "waiting_for_producers" {
		t.Fatal("canonical status unavailable", status, err)
	}
	changed := r
	changed.DesiredHash = strings.Repeat("b", 64)
	status, err = inspectNotificationControl(context.Background(), changed, paths, observation, nil)
	if err != nil || status.Code != "policy_changed" {
		t.Fatal("status hid YAML drift", status, err)
	}
	// A credential rotation with the same bot ID leaves the destination fingerprint
	// unchanged. It must still be detected before claiming a no-op.
	r.Inputs["token"] = []byte("123:rotated-secret")
	if _, err = applyNotificationBootstrap(context.Background(), r, paths, "/helper", lifecycle); err == nil {
		t.Fatal("ignored changed credential input")
	}
	raw, _ := os.ReadFile(filepath.Join(paths.Credentials, "notification-worker/telegram-token"))
	if string(raw) != "123:fixture-secret" {
		t.Fatal("rotated an active credential")
	}
	r.Inputs = nil
	r.Action = "uninstall"
	removes := 0
	remove := func(ctx context.Context, root, expected string) (install.ManagedManifest, error) {
		if expected != protocol.Hash(*saved) {
			t.Fatal("uninstall lost original receipt")
		}
		removes++
		saved.Mode = "uninstalled"
		return *saved, errors.New("lost removal response")
	}
	if _, err = uninstallNotificationControl(context.Background(), r, paths, observation, remove); err == nil {
		t.Fatal("lost removal response hidden")
	}
	removed, err := uninstallNotificationControl(context.Background(), r, paths, observation, remove)
	if err != nil || removed.State != "uninstalled" || removes != 1 {
		t.Fatal("removal replay or identity change", removed, removes, err)
	}
	if _, err = os.Stat(filepath.Join(paths.State, "identity.json")); err != nil {
		t.Fatal("removal erased gateway state", err)
	}
	status, err = inspectNotificationControl(context.Background(), r, paths, observation, nil)
	if err != nil || status.State != "uninstalled" {
		t.Fatal("retained state falsely reported installed", status, err)
	}
}

func TestUnboundCompilerStableAndRejectsDroppedPolicy(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless paths")
	}
	r, paths := bootstrapControlFixture(t)
	a, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	x, err := a.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	y, err := a.Resolve()
	if err != nil || protocol.Hash(x) != protocol.Hash(y) {
		t.Fatal("unstable resolution", err)
	}
	if a.Worker.Routes["alerts"].Revision != "declaration-v3" || x.Worker.Routes["alerts"].Revision == a.Worker.Routes["alerts"].Revision {
		t.Fatal("raw and resolved routes confused")
	}
	r.Desired.Notification.StatusLEDs.Enabled = true
	withLED, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs)
	if err != nil || withLED.LEDSocket == "" || !withLED.Bootstrap {
		t.Fatal("managed aggregate LEDs were dropped", withLED, err)
	}
	r.Desired.Notification.StatusLEDs.Bindings = map[string]config.LEDBindingDeclaration{"custom": {Producer: "unreserved-specialist", Subjects: []string{"backup"}, LED: "green"}}
	if _, err = CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs); err == nil {
		t.Fatal("dropped requested custom LED mapping")
	}
}

func TestControlFramesAreBoundedStrictAndRedacted(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`{"version":1,"version":1}`), []byte(`{"version":1,"unexpected":"private-secret"}`)} {
		var b bytes.Buffer
		binary.Write(&b, binary.BigEndian, uint32(len(raw)))
		b.Write(raw)
		var out bytes.Buffer
		if err := ControlMain(context.Background(), &b, &out); err != nil {
			t.Fatal(err)
		}
		var result ControlResult
		if err := readControlFrame(&out, &result); err != nil {
			t.Fatal(err)
		}
		if result.Code != "invalid_request" || strings.Contains(result.Next, "private-secret") {
			t.Fatal(result)
		}
	}
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(maxControlFrame+1))
	var r ControlRequest
	if readControlFrame(&b, &r) == nil {
		t.Fatal("accepted oversized frame")
	}
}
