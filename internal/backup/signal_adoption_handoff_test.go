package backup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/testutil"
)

func legacyAdoptionGateway(t *testing.T, a backupSignalAdoption) (backupSignalHandoffSpec, install.Runtime) {
	t.Helper()
	ctx := context.Background()
	p := notify.Producer{ID: "backup", Epoch: "one"}
	socket := filepath.Join(a.Spec.Home, "legacy-socket", "signal.sock")
	dest := interaction.Destination{ID: "private", Generation: "one", Provider: "signal", Account: a.Spec.Account, NativeID: "22222222-2222-4222-8222-222222222222"}
	c := install.Runtime{Version: 1, Worker: worker.Config{Producers: []worker.Enrollment{{Producer: p, Required: true, Routes: []string{"ops"}, MaxMessages: 100, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400}}, Routes: map[string]worker.Route{"ops": {Revision: "one", Primary: "signal"}}, MaxMessages: 100, MaxPayloadBytes: 1 << 20, MaxDatabaseBytes: 16 << 20, Interaction: &interaction.Config{Revision: "one", Destinations: []interaction.Destination{dest}, Workers: []interaction.Worker{{Alias: "backup/local", Producer: p, Generation: "one", Commands: map[string]interaction.Descriptor{"status": {ID: "fixture.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "fixture"}}}}}}, Providers: map[string]provider.Profile{"signal": {Kind: "signal", Socket: socket, Destination: notify.Hash("legacy-recipient")}}, Signal: &provider.SignalAccount{Account: a.Spec.Account, Socket: socket, InboxIdentity: a.Inbox.Identity}}
	config := filepath.Join(t.TempDir(), "runtime.json")
	raw, err := json.Marshal(c)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(config, raw, 0600))
	c, err = install.ReadRuntime(config)
	testutil.Must(t, err)
	state := t.TempDir()
	testutil.Must(t, os.Chmod(state, 0700))
	s, err := worker.Open(state, c.Worker, true)
	testutil.Must(t, err)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	snapshot, err := s.Snapshot(ctx, artifact)
	testutil.Must(t, err)
	testutil.Must(t, s.Close())
	q, err := worker.QuarantineRecovery(ctx, state, artifact, c.Worker, snapshot.ID, snapshot.DatabaseHash)
	testutil.Must(t, err)
	source, err := relay.OpenRecoveryInbox(a.Spec.Inbox, a.Spec.Account)
	testutil.Must(t, err)
	remote, err := worker.OpenSignalRecoverySession(ctx, state, c.Worker)
	testutil.Must(t, err)
	summary, err := interaction.ReconcileSignal(ctx, source, remote)
	testutil.Must(t, err)
	if summary.Manifest.Highest != 0 || summary.Manifest.EventCount() != 0 {
		t.Fatal("legacy adoption invented old Signal input")
	}
	testutil.Must(t, remote.Close())
	testutil.Must(t, source.Close())
	requests, err := worker.OpenRequestRecoverySession(ctx, state, c.Worker)
	testutil.Must(t, err)
	_, err = requests.SealRecoveryRequests(ctx)
	testutil.Must(t, err)
	testutil.Must(t, requests.Close())
	source, err = relay.OpenRecoveryInbox(a.Spec.Inbox, a.Spec.Account)
	testutil.Must(t, err)
	cursor, err := worker.OpenSignalCursorSession(ctx, state, c.Worker)
	testutil.Must(t, err)
	_, err = interaction.PrepareSignalRecoveryHandoff(ctx, source, cursor)
	testutil.Must(t, err)
	_, err = interaction.InstallSignalRecoveryCursor(ctx, source, cursor)
	testutil.Must(t, err)
	testutil.Must(t, cursor.Close())
	testutil.Must(t, source.Close())
	_, err = worker.FenceRecoveryOwnership(ctx, state, c.Worker, p)
	testutil.Must(t, err)
	review, err := worker.ReviewRecovery(ctx, state, c.Worker, q.Operation)
	testutil.Must(t, err)
	cp, err := worker.CheckpointRecovery(ctx, state, c.Worker, q.Operation, review.ReviewHash)
	testutil.Must(t, err)
	proof, err := worker.VerifyRecoveryCheckpoint(ctx, state, c.Worker, q.Operation, review.ReviewHash, cp.CheckpointHash)
	testutil.Must(t, err)
	image, err := worker.PrepareRecoveryPublication(ctx, state, c.Worker, q.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash)
	testutil.Must(t, err)
	publication, err := worker.PublishRecovery(ctx, state, c.Worker, q.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash, image.ImageHash)
	testutil.Must(t, err)
	destination := t.TempDir()
	testutil.Must(t, os.Chmod(destination, 0700))
	return backupSignalHandoffSpec{Version: 1, Operation: testutil.ID(t, "n"), Adoption: a, GatewayConfig: config, GatewaySHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), GatewayState: state, CommitHash: publication.Commit.CommitHash, RelayDestination: destination}, c
}

func TestLegacyBackupSignalPublishedHandoff(t *testing.T) {
	ctx := context.Background()
	spec, svc, _ := legacySignalAdoptionFixture(t)
	adoption, err := adoptBackupSignal(ctx, spec, svc)
	testutil.Must(t, err)
	handoff, c := legacyAdoptionGateway(t, adoption)
	_, err = handoff.prepare()
	testutil.Must(t, err)
	installed, status, err := handoffBackupSignal(ctx, handoff, c, svc)
	testutil.Must(t, err)
	if installed.LegacyBootstrap != notify.Hash(adoption.Inbox) || installed.IntakeEnabled || status.RuntimeAllowed {
		t.Fatal("handoff lost history or enabled ordinary runtime")
	}
	again, againStatus, err := handoffBackupSignal(ctx, handoff, c, svc)
	testutil.Must(t, err)
	if notify.Hash(again) != notify.Hash(installed) || status != againStatus {
		t.Fatal("handoff retry changed authority")
	}
	testutil.Must(t, signalaccount.VerifyRecoveryAncestor(spec.accountState(), status, adoption.Recovery))
	if l, err := signalaccount.AcquireRuntime(spec.accountState()); err == nil {
		l.Close()
		t.Fatal("legacy owner bypassed migration")
	}
	endpointSpec := install.SignalVerificationSpec{Version: 1, Operation: testutil.ID(t, "n"), Root: filepath.Join(t.TempDir(), "endpoint"), GatewayConfig: handoff.GatewayConfig, GatewaySHA256: handoff.GatewaySHA256, GatewayState: handoff.GatewayState, CommitHash: handoff.CommitHash, AccountState: spec.accountState(), InboxDestination: handoff.RelayDestination, Recovery: status}
	testutil.Must(t, os.Mkdir(endpointSpec.Root, 0700))
	endpoint, err := install.PrepareSignalVerification(ctx, endpointSpec)
	testutil.Must(t, err)
	if endpoint.Installation.LegacyBootstrap != installed.LegacyBootstrap {
		t.Fatal("managed endpoint lost legacy lineage")
	}
	testutil.Must(t, os.Remove(filepath.Join(spec.Inbox, "legacy-bootstrap-intent.json")))
	if _, err = relay.VerifyInstalledSignalRuntime(ctx, handoff.RelayDestination, installed.InstallationHash); err == nil {
		t.Fatal("managed installation accepted lost legacy ancestry")
	}
}
