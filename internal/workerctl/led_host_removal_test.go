package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/ledhost"
	"github.com/dryaf/deploy/internal/safefs"
)

func removalStagingFixture(t *testing.T) (ControlRequest, ResolvedPaths, Artifact, ledhost.Spec, *safefs.Directory, OperationJournal) {
	t.Helper()
	r, paths, helper, artifact, _ := managedLEDSetupFixture(t)
	r.Action = "uninstall"
	d, err := ensureConfigDirectory(paths.Control)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	op, _ := safefs.NewID("n")
	child, _ := safefs.NewID("n")
	j := OperationJournal{Version: 1, ID: op, Complete: true, Pins: OperationPins{Worker: r.Worker, Action: r.Action, DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Artifacts: map[string]string{"helper": r.HelperSHA256}}, Children: []ChildReference{{Name: "notification-uninstall", Operation: child, Complete: true, Receipt: strings.Repeat("b", 64)}}}
	lease, err := d.Lock("operation.lock", true, true)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if err = d.WriteJSON("operation.json", j, false); err != nil {
		t.Fatal(err)
	}
	prior := j
	prior.ID, _ = safefs.NewID("n")
	prior.Pins.Artifacts = map[string]string{"helper": helper.SHA256, "led-daemon": artifact.SHA256}
	prior.Pins.Action = "apply"
	if err = d.Mkdir("history"); err != nil {
		t.Fatal(err)
	}
	if err = d.WriteJSON("history/"+prior.ID+".json", prior, false); err != nil {
		t.Fatal(err)
	}
	j.Pins.PreviousOperation, j.Pins.Previous = prior.ID, prior.Digest()
	j.Pins.Installations = map[string]string{"notification-worker": paths.Installation, "state": paths.State, "runtime": paths.Runtime}
	if err = d.WriteJSON("operation.json", j, true); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(install.RuntimeInput{LEDSocket: ledhost.Socket(r.Facts.UID)})
	if err != nil {
		t.Fatal(err)
	}
	if err = publishPrivateInput(paths.Runtime, raw); err != nil {
		t.Fatal(err)
	}
	if err = d.WriteJSON("notification-inputs.json", notificationInputPins{Version: 1, Operation: child, Runtime: inputDigest(raw)}, false); err != nil {
		t.Fatal(err)
	}
	s := ledhost.Spec{Version: 1, UID: r.Facts.UID, Device: "ACT", DevicePath: "/sys/devices/platform/leds/leds/ACT", ArtifactSHA256: artifact.SHA256}
	return r, paths, helper, s, d, j
}

func TestLEDHostRemovalStagingReviewStatusAndCompletion(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless staging")
	}
	r, paths, helper, s, d, j := removalStagingFixture(t)
	p, _ := ledhost.Build(s)
	finished := false
	l := ledRemovalLifecycle{installed: func(int) (ledhost.Receipt, error) {
		return ledhost.Receipt{Version: 1, Spec: s, Hash: p.Hash, Complete: true}, nil
	}, removed: func(int) (ledhost.RemovalReceipt, error) {
		if !finished {
			return ledhost.RemovalReceipt{}, os.ErrNotExist
		}
		remove, _ := ledhost.BuildRemoval(s)
		return ledhost.RemovalReceipt{Version: 1, Spec: s, Installation: p.Hash, Hash: remove.Hash, Complete: true}, nil
	}}
	result := ControlResult{Version: 1, State: "uninstalled", Code: "ok", Next: "gateway removed"}
	got, err := finishNotificationHostRemoval(context.Background(), r, paths, helper.File, result, l)
	if err != nil || got.State != "cleanup_pending" || got.LEDHostRemoval == nil {
		t.Fatal(got, err)
	}
	if !strings.Contains(got.LEDHostRemoval.Command, " led-host remove ") || !strings.HasSuffix(got.LEDHostRemoval.Command, got.LEDHostRemoval.Plan.Hash) {
		t.Fatal("removal command lost review pins")
	}
	var out bytes.Buffer
	if err = printControlResult(&out, r.Server, r.Worker, got); err != nil || !strings.Contains(out.String(), got.LEDHostRemoval.Command) || !strings.Contains(out.String(), "every client") {
		t.Fatal("removal effects hidden", out.String(), err)
	}
	before, err := os.ReadFile(got.LEDHostRemoval.RequestFile)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = RunLEDHostCLI(context.Background(), []string{"review-remove", got.LEDHostRemoval.RequestFile}, &out); err != nil {
		t.Fatal(err)
	}
	var reviewed ledhost.RemovalPlan
	if err = json.Unmarshal(out.Bytes(), &reviewed); err != nil || !reflect.DeepEqual(reviewed, got.LEDHostRemoval.Plan) {
		t.Fatal("removal review changed", err)
	}
	again, err := finishNotificationHostRemoval(context.Background(), r, paths, helper.File, result, l)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatal("staging retry changed plan", err)
	}
	after, _ := os.ReadFile(got.LEDHostRemoval.RequestFile)
	if !bytes.Equal(before, after) {
		t.Fatal("staging changed request")
	}
	status, err := inspectNotificationHostRemoval(r, d, j, result, l.removed)
	if err != nil || status.State != "cleanup_pending" {
		t.Fatal("pending host removal invisible", status, err)
	}
	finished = true
	complete, err := finishNotificationHostRemoval(context.Background(), r, paths, helper.File, result, l)
	if err != nil || complete.State != "uninstalled" || complete.LEDHostRemoval != nil || !strings.Contains(complete.Next, "daemon removed") {
		t.Fatal("host removal completion lost", complete, err)
	}
	status, err = inspectNotificationHostRemoval(r, d, j, result, l.removed)
	if err != nil || status.State != "uninstalled" || !strings.Contains(status.Next, "daemon removed") {
		t.Fatal(status, err)
	}
	r.HelperSHA256 = strings.Repeat("c", 64)
	if _, err = finishNotificationHostRemoval(context.Background(), r, paths, helper.File, result, l); err == nil {
		t.Fatal("changed helper silently adopted")
	}
}

func TestLEDHostRemovalRefusesUnconfirmedGatewayAndChangedRuntime(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless staging")
	}
	for _, kind := range []string{"pending", "runtime", "host"} {
		t.Run(kind, func(t *testing.T) {
			r, paths, helper, _, d, j := removalStagingFixture(t)
			if kind == "pending" {
				j.Complete = false
				if err := d.WriteJSON("operation.json", j, true); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "runtime" {
				if err := os.WriteFile(paths.Runtime, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			l := ledRemovalLifecycle{installed: func(int) (ledhost.Receipt, error) { return ledhost.Receipt{}, errors.New("host authority unavailable") }}
			if _, err := finishNotificationHostRemoval(context.Background(), r, paths, helper.File, ControlResult{}, l); err == nil {
				t.Fatal("unconfirmed removal staged")
			}
			if _, err := os.Stat(filepath.Join(paths.Control, "led-host-removal.json")); !os.IsNotExist(err) {
				t.Fatal("rejected operation retained host intent")
			}
		})
	}
}
