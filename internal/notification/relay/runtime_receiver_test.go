package relay

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func managedInboxFixture(t *testing.T, restored bool) (string, string, SignalRuntimeInstallation, SignalVerificationEndpoint) {
	t.Helper()
	ctx := context.Background()
	src, dst, cursor := runtimeInstallFixture(t, restored)
	p, err := InstallSignalRuntime(ctx, src, dst, cursor)
	if err != nil {
		t.Fatal(err)
	}
	v, err := OpenSignalRuntimeVerification(ctx, dst, p.InstallationHash)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := SignalVerificationEndpoint{Version: 1, Root: t.TempDir(), Device: 1, Inode: 2, ManifestHash: protocol.Hash("fixture-managed-endpoint")}
	if err = v.BindVerificationEndpoint(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	v.Close()
	return src, dst, p, endpoint
}

func TestManagedSignalInboxContinuesBaselineAndRetainsLeases(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "restored"}[restored], func(t *testing.T) {
			src, dst, p, endpoint := managedInboxFixture(t, restored)
			ctx := context.Background()
			admitted := true
			admission := func(context.Context) error {
				if !admitted {
					return errors.New("revoked")
				}
				return nil
			}
			s, err := OpenManagedSignalInbox(ctx, dst, p.InstallationHash, endpoint, admission)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for _, path := range []string{src, dst} {
				d, err := safefs.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				l, err := d.Lock("inbox.lock", true, false)
				d.Close()
				if err == nil {
					l.Close()
					t.Fatal("source/destination lease released", path)
				}
			}
			initial := s.Cursor()
			if initial != p.Cursor.Handoff.Target {
				t.Fatal("baseline changed", initial)
			}
			event := interaction.SpoolEvent{Key: "new-after-managed-admission", Hash: protocol.Hash("new-event")}
			if err = s.Enqueue(ctx, event); err != nil {
				t.Fatal(err)
			}
			page, err := s.List(ctx, initial)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Events) != 1 || page.Events[0].Key != event.Key || page.Cursor.Position != initial.Position+1 {
				t.Fatal(page)
			}
			if err = s.Ack(ctx, page.Cursor); err != nil {
				t.Fatal(err)
			}
			admitted = false
			if s.CanReceive(ctx) == nil || s.Enqueue(ctx, event) == nil || s.Ack(ctx, page.Cursor) == nil {
				t.Fatal("revoked callback admitted work")
			}
			if _, err = s.List(ctx, page.Cursor); err == nil {
				t.Fatal("revoked callback exposed inbox")
			}
			s.Close()
			if old, err := OpenInbox(dst, p.Cursor.Handoff.Target.Account, false); err == nil {
				old.Close()
				t.Fatal("ordinary receiver bypassed managed admission")
			}
			if _, err = OpenManagedSignalInbox(ctx, dst, p.InstallationHash, endpoint, admission); err == nil {
				t.Fatal("revoked receiver restarted")
			}
			admitted = true
			s, err = OpenManagedSignalInbox(ctx, dst, p.InstallationHash, endpoint, admission)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err = s.List(ctx, initial); err == nil {
				t.Fatal("acknowledgement regressed after restart")
			}
			if err = s.Enqueue(ctx, event); err != nil {
				t.Fatal(err)
			}
			again, err := s.List(ctx, page.Cursor)
			if err != nil || len(again.Events) != 0 {
				t.Fatal("event repeated", again, err)
			}
		})
	}
}

func TestManagedSignalInboxMissingEvidence(t *testing.T) {
	for _, name := range []string{verificationEndpointFile, runtimeInstallImage, runtimeInstallComplete, "inbox.lock", "source-intent"} {
		t.Run(name, func(t *testing.T) {
			src, dst, p, endpoint := managedInboxFixture(t, false)
			s, err := OpenManagedSignalInbox(context.Background(), dst, p.InstallationHash, endpoint, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			path := filepath.Join(dst, name)
			if name == "source-intent" {
				path = filepath.Join(src, runtimeInstallIntent)
			}
			if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err = s.Enqueue(context.Background(), interaction.SpoolEvent{Key: "new", Hash: protocol.Hash("new")}); err == nil {
				t.Fatal("lost evidence admitted receive")
			}
			s.Close()
			if next, err := OpenManagedSignalInbox(context.Background(), dst, p.InstallationHash, endpoint, func(context.Context) error { return nil }); err == nil {
				next.Close()
				t.Fatal("lost evidence recreated")
			}
		})
	}
}
