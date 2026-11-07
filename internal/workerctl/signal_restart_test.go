package workerctl

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDeclaredSignalExplicitResumeKeepsSetupAndAccount(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	step, _, starts := signalSetupFixture(t)
	r, _ := bootstrapControlFixture(t)
	r.Desired.Backup = nil
	r.Desired.Notification.Providers = config.ProviderDeclarations{"signal": step.provider}
	r.Desired.Notification.Routes = map[string]config.NotificationRoute{"alerts": {Primary: "signal"}}
	r.DesiredHash, _ = r.Desired.Fingerprint()
	r.Facts = step.request.Facts
	r.HelperSHA256 = step.request.HelperSHA256
	r.SignalSHA256 = step.request.SignalSHA256
	r.Inputs = map[string][]byte{"signal_account": step.account, "signal_recipient": step.recipient}
	r.InteractiveLink = true
	r.Action = "apply"
	ctx := context.Background()
	m, _, _, err := prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, io.Discard, step.lifecycle)
	testutil.Must(t, err)
	journal, err := os.ReadFile(filepath.Join(step.paths.Control, "operation.json"))
	testutil.Must(t, err)
	inspect := step.lifecycle.inspect
	stopped, resumes := true, 0
	step.lifecycle.inspect = func(ctx context.Context, s install.FreshSignalSpec) (install.FreshSignalManifest, bool, error) {
		got, complete, err := inspect(ctx, s)
		if err == nil && complete && stopped {
			return got, false, install.FreshSignalResumeRequired{Reference: install.FreshSignalReference{Root: got.Spec.Root, ManifestHash: got.ManifestHash}}
		}
		return got, complete, err
	}
	step.lifecycle.resume = func(_ context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, error) {
		if ref.Root != m.Spec.Root || ref.ManifestHash != m.ManifestHash {
			t.Fatal("wrong resume reference")
		}
		resumes++
		stopped = false
		return m, errors.New("lost restart acknowledgement")
	}
	r.InteractiveLink = false
	if _, _, _, err = prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle); err == nil || resumes != 0 {
		t.Fatal("apply restarted service")
	}
	r.Action = "resume"
	if _, _, _, err = prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle); err == nil || resumes != 1 {
		t.Fatal("lost restart acknowledgement hidden", err)
	}
	for range 2 {
		got, _, _, err := prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle)
		testutil.Must(t, err)
		if protocol.Hash(got) != protocol.Hash(m) || resumes != 1 || *starts != 1 {
			t.Fatal("resume replayed setup or relinked")
		}
	}
	got, err := os.ReadFile(filepath.Join(step.paths.Control, "operation.json"))
	testutil.Must(t, err)
	if string(got) != string(journal) {
		t.Fatal("restart replaced setup journal")
	}
	links, err := os.ReadFile(filepath.Join(step.paths.Account, "fixture-link-count"))
	testutil.Must(t, err)
	if string(links) != "1" {
		t.Fatal("resume relinked account")
	}
	// A changed private input fails before the repair callback.
	stopped = true
	r.Inputs["signal_account"] = []byte("+19999999999")
	if _, _, _, err = prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle); err == nil || resumes != 1 {
		t.Fatal("resume accepted changed credentials")
	}
}
