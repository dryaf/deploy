package relay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func nextInstalledCursor(t *testing.T, path string, prior SignalRuntimeInstallation) interaction.SignalRecoveryCursor {
	t.Helper()
	ctx := context.Background()
	r, err := OpenInstalledRecoveryInbox(ctx, path, prior.InstallationHash)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	challenge := baselineChallenge(t, r.Cursor())
	m, err := r.Capture(ctx, challenge)
	if err != nil {
		t.Fatal(err)
	}
	summary := interaction.SignalRecoverySummary{Challenge: challenge, Manifest: m, Unmapped: m.EventCount()}
	if err = r.RecordSummary(ctx, summary); err != nil {
		t.Fatal(err)
	}
	handoff := interaction.SignalRecoveryHandoff{Version: 1, Summary: summary, RequestsHash: protocol.Hash(challenge), Before: r.Cursor(), Target: r.Cursor()}
	handoff.Target.Position = m.Highest
	p, err := r.StageSignalCursor(ctx, handoff)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstalledSignalRepeatedRecovery(t *testing.T) {
	ctx := context.Background()
	_, path, prior, endpoint := managedInboxFixture(t, false)
	first := path
	admission := func(context.Context) error { return nil }
	for epoch := 2; epoch <= 3; epoch++ {
		s, err := OpenManagedSignalInbox(ctx, path, prior.InstallationHash, endpoint, admission)
		if err != nil {
			t.Fatal(epoch, err)
		}
		key := fmt.Sprintf("epoch-%d", epoch)
		if err = s.Enqueue(ctx, interaction.SpoolEvent{Key: key, Hash: protocol.Hash(key)}); err != nil {
			t.Fatal(err)
		}
		page, err := s.List(ctx, s.Cursor())
		if err != nil || len(page.Events) != 1 {
			t.Fatal(page, err)
		}
		if err = s.Ack(ctx, page.Cursor); err != nil {
			t.Fatal(err)
		}
		s.Close()
		cursor := nextInstalledCursor(t, path, prior)
		if stale, err := OpenManagedSignalInbox(ctx, path, prior.InstallationHash, endpoint, admission); err == nil {
			stale.Close()
			t.Fatal("outgoing handoff allowed old receiver")
		}
		dst := t.TempDir()
		if err = os.Chmod(dst, 0700); err != nil {
			t.Fatal(err)
		}
		next, err := InstallSignalRuntime(ctx, path, dst, cursor)
		if err != nil {
			t.Fatal(epoch, err)
		}
		if next.Version != 2 || next.Ancestor != prior.InstallationHash || next.Cursor.Handoff.Target.Position != page.Cursor.Position {
			t.Fatal(next)
		}
		again, err := InstallSignalRuntime(ctx, path, dst, cursor)
		if err != nil || protocol.Hash(again) != protocol.Hash(next) {
			t.Fatal(again, err)
		}
		v, err := OpenSignalRuntimeVerification(ctx, dst, next.InstallationHash)
		if err != nil {
			t.Fatal(err)
		}
		endpoint = SignalVerificationEndpoint{Version: 1, Root: t.TempDir(), Device: 1, Inode: uint64(epoch), ManifestHash: protocol.Hash(key)}
		if err = v.BindVerificationEndpoint(ctx, endpoint); err != nil {
			t.Fatal(err)
		}
		v.Close()
		path, prior = dst, next
	}
	if err := os.Remove(filepath.Join(first, runtimeInstallImage)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyInstalledSignalRuntime(ctx, path, prior.InstallationHash); err == nil {
		t.Fatal("missing grandparent image accepted")
	}
}
