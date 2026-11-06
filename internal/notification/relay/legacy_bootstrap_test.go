package relay

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestLegacySignalInboxBootstrap(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	testutil.Must(t, os.Chmod(path, 0700))
	operation := testutil.ID(t, "n")
	bootstrap, err := BootstrapLegacyInbox(ctx, path, fixtureAccount, operation)
	testutil.Must(t, err)
	again, err := BootstrapLegacyInbox(ctx, path, fixtureAccount, operation)
	testutil.Must(t, err)
	if bootstrap != again || bootstrap.HistoryComplete {
		t.Fatal("bootstrap changed or invented complete history")
	}
	source, err := OpenRecoveryInbox(path, fixtureAccount)
	testutil.Must(t, err)
	ch := baselineChallenge(t, source.Cursor())
	manifest, err := source.Capture(ctx, ch)
	testutil.Must(t, err)
	summary := interaction.SignalRecoverySummary{Challenge: ch, Manifest: manifest}
	testutil.Must(t, source.RecordSummary(ctx, summary))
	handoff := interaction.SignalRecoveryHandoff{Version: 1, Summary: summary, RequestsHash: protocol.Hash("sealed"), Before: source.Cursor(), Target: source.Cursor()}
	cursor, err := source.StageSignalCursor(ctx, handoff)
	testutil.Must(t, err)
	testutil.Must(t, source.Close())
	dest := t.TempDir()
	testutil.Must(t, os.Chmod(dest, 0700))
	installed, err := InstallSignalRuntime(ctx, path, dest, cursor)
	testutil.Must(t, err)
	if installed.LegacyBootstrap != protocol.Hash(bootstrap) {
		t.Fatal("installation lost empty-history provenance")
	}
	_, err = VerifyInstalledSignalRuntime(ctx, dest, installed.InstallationHash)
	testutil.Must(t, err)
	testutil.Must(t, os.Remove(filepath.Join(path, legacyBootstrapComplete)))
	if _, err = VerifyInstalledSignalRuntime(ctx, dest, installed.InstallationHash); err == nil {
		t.Fatal("lost bootstrap proof accepted")
	}
}
func TestLegacySignalInboxBootstrapInterrupted(t *testing.T) {
	for _, at := range []string{"legacy_inbox_intent", "legacy_inbox_identity", "legacy_inbox_transaction", "legacy_inbox_commit", "legacy_inbox_complete"} {
		t.Run(at, func(t *testing.T) {
			path := t.TempDir()
			testutil.Must(t, os.Chmod(path, 0700))
			op := testutil.ID(t, "n")
			stop := errors.New("interrupted")
			_, err := bootstrapLegacyInbox(context.Background(), path, fixtureAccount, op, func(stage string) error {
				if stage == at {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			result, err := BootstrapLegacyInbox(context.Background(), path, fixtureAccount, op)
			testutil.Must(t, err)
			testutil.Must(t, VerifyLegacyInboxBootstrap(context.Background(), path, result))
			if _, err = BootstrapLegacyInbox(context.Background(), path, fixtureAccount, testutil.ID(t, "n")); err == nil {
				t.Fatal("different operation adopted partial state")
			}
		})
	}
}
func TestLegacySignalInboxBootstrapRejectsExistingHistory(t *testing.T) {
	path := t.TempDir()
	testutil.Must(t, os.Chmod(path, 0700))
	s, err := OpenInbox(path, fixtureAccount, true)
	testutil.Must(t, err)
	testutil.Must(t, s.Close())
	if _, err = BootstrapLegacyInbox(context.Background(), path, fixtureAccount, testutil.ID(t, "n")); err == nil {
		t.Fatal("existing inbox erased")
	}
}
