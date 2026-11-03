package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
)

func signalRuntimeGatewayFixture(t *testing.T) (string, Config, string, RecoveryPublicationResult, interaction.SignalRecoveryCursor) {
	t.Helper()
	f, p := signalCursorFixture(t)
	ctx := context.Background()
	must(t, f.r.InstallSignalCursor(ctx, p))
	_, err := f.r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	must(t, f.source.Close())
	path, c, image := sealPublicationFixture(t, f.r)
	result, err := publishImage(ctx, path, c, image, nil)
	must(t, err)
	return path, c, f.relayPath, result, p
}
func TestPublishedSignalRuntimeInstallation(t *testing.T) {
	path, c, source, result, p := signalRuntimeGatewayFixture(t)
	ctx := context.Background()
	destination := t.TempDir()
	must(t, os.Chmod(destination, 0700))
	var installed relay.SignalRuntimeInstallation
	for attempt := 0; attempt < 2; attempt++ {
		err := WithPublishedSignalCursor(ctx, path, c, result.Commit.CommitHash, p.Handoff.Target.Account, func(cursor interaction.SignalRecoveryCursor) error {
			if protocol.Hash(cursor) != protocol.Hash(p) {
				t.Fatal("published cursor changed")
			}
			// The callback cannot open a second gateway: its offline lease spans handoff.
			if s, e := Open(path, c, false); e == nil {
				s.Close()
				t.Fatal("gateway lease released during installation")
			}
			var e error
			installed, e = relay.InstallSignalRuntime(ctx, source, destination, cursor)
			return e
		})
		must(t, err)
	}
	inbox, err := relay.OpenInstalledInboxVerification(ctx, destination, installed.InstallationHash)
	must(t, err)
	if inbox.Cursor() != p.Handoff.Target {
		t.Fatal(inbox.Cursor())
	}
	must(t, inbox.Close())
	if s, e := Open(path, c, false); e == nil {
		s.Close()
		t.Fatal("Signal installation activated gateway")
	}
	im := result.Commit.Image
	_, err = VerifyRecoveryPublication(ctx, path, c, im.Operation, im.ReviewHash, im.CheckpointHash, im.ProofHash, im.ImageHash)
	must(t, err)
}
func TestPublishedSignalRuntimeRefusesChangedGateway(t *testing.T) {
	for _, kind := range []string{"commit", "account", "cursor", "after-action"} {
		t.Run(kind, func(t *testing.T) {
			path, c, _, result, p := signalRuntimeGatewayFixture(t)
			ctx := context.Background()
			account := p.Handoff.Target.Account
			commit := result.Commit.CommitHash
			change := func() {
				d, e := privateDirectory(path)
				must(t, e)
				defer d.Close()
				db, e := snapshotDB(d, "state.db", false)
				must(t, e)
				_, e = db.Exec("UPDATE signal_imports SET cursor='{}'")
				must(t, e)
				must(t, db.Close())
			}
			switch kind {
			case "commit":
				commit = protocol.Hash("wrong")
			case "account":
				account = "wrong-account"
			case "cursor":
				change()
			}
			called := false
			err := WithPublishedSignalCursor(ctx, path, c, commit, account, func(interaction.SignalRecoveryCursor) error {
				called = true
				if kind == "after-action" {
					change()
				}
				return nil
			})
			if err == nil {
				t.Fatal("changed gateway accepted")
			}
			if called != (kind == "after-action") {
				t.Fatal("callback ran with unverified gateway")
			}
		})
	}
}
func TestPublishedSignalRuntimeRefusesIncompletePublication(t *testing.T) {
	path, c, _, result, p := signalRuntimeGatewayFixture(t)
	must(t, os.Remove(filepath.Join(path, publicationCommitComplete)))
	err := WithPublishedSignalCursor(context.Background(), path, c, result.Commit.CommitHash, p.Handoff.Target.Account, func(interaction.SignalRecoveryCursor) error {
		t.Fatal("incomplete publication installed relay")
		return nil
	})
	if err == nil {
		t.Fatal("incomplete publication accepted")
	}
}
