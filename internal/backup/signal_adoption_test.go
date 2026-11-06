package backup

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/testutil"
)

func legacySignalAdoptionFixture(t *testing.T) (backupSignalAdoptionSpec, *signalService, *bool) {
	return legacySignalAdoptionFixtureLease(t, nil)
}
func legacySignalAdoptionFixtureLease(t *testing.T, child func(*signalaccount.Lease)) (backupSignalAdoptionSpec, *signalService, *bool) {
	t.Helper()
	svc, paths, _ := signalServiceFixture(t)
	testutil.Must(t, os.WriteFile(paths["account"], []byte("11111111-1111-4111-8111-111111111111"), 0600))
	testutil.Must(t, os.WriteFile(paths["recipient"], []byte("22222222-2222-4222-8222-222222222222"), 0600))
	view, err := svc.Inspect(context.Background())
	testutil.Must(t, err)
	svc.pinned = view.Binding
	spec := backupSignalAdoptionSpec{Version: 1, Operation: testutil.ID(t, "n"), Home: svc.home, ConfigHash: notify.Hash(svc.installation), ServiceBinding: svc.pinned, Account: "11111111-1111-4111-8111-111111111111", Inbox: t.TempDir()}
	testutil.Must(t, os.Chmod(spec.Inbox, 0700))
	testutil.Must(t, os.MkdirAll(spec.accountState(), 0700))
	lease, err := signalaccount.AcquireRuntime(spec.accountState())
	testutil.Must(t, err)
	t.Cleanup(func() { lease.Close() })
	if child != nil {
		child(lease)
	}
	active := true
	svc.run = func(ctx context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "show":
			raw := signalServiceProperties(paths["unit"], paths["fragment"])
			if !active {
				raw = strings.NewReplacer("ActiveState=active", "ActiveState=inactive", "SubState=running", "SubState=dead", "MainPID=42", "MainPID=0").Replace(raw)
			}
			return []byte(raw), nil
		case "stop":
			active = false
			lease.Close()
			return nil, nil
		default:
			t.Fatalf("unexpected legacy account manager effect: %v", args)
			return nil, nil
		}
	}
	svc.adoptionProbe = func(context.Context, string) error { return nil }
	return spec, svc, &active
}
func TestLegacyBackupSignalAdoption(t *testing.T) {
	ctx := context.Background()
	spec, svc, active := legacySignalAdoptionFixture(t)
	result, err := adoptBackupSignal(ctx, spec, svc)
	testutil.Must(t, err)
	if *active || result.Inbox.HistoryComplete || result.Recovery.RuntimeAllowed {
		t.Fatal("adoption left old owner or invented history")
	}
	again, err := adoptBackupSignal(ctx, spec, svc)
	testutil.Must(t, err)
	if notify.Hash(result) != notify.Hash(again) {
		t.Fatal("adoption retry changed identity")
	}
	if l, err := signalaccount.AcquireRuntime(spec.accountState()); err == nil {
		l.Close()
		t.Fatal("legacy account owner restarted")
	}
	inbox, err := relay.OpenRecoveryInbox(spec.Inbox, spec.Account)
	testutil.Must(t, err)
	if inbox.Cursor().Identity != result.Inbox.Identity || inbox.Cursor().Position != 0 {
		t.Fatal("bootstrap cursor changed")
	}
	testutil.Must(t, inbox.Close())
	testutil.Must(t, os.Remove(filepath.Join(spec.Inbox, "legacy-bootstrap-complete.json")))
	if _, err = adoptBackupSignal(ctx, spec, svc); err == nil {
		t.Fatal("lost completed bootstrap regenerated")
	}
}
func TestLegacyBackupSignalAdoptionPreflight(t *testing.T) {
	for _, kind := range []string{"old-image", "changed-binding", "occupied-inbox"} {
		t.Run(kind, func(t *testing.T) {
			spec, svc, active := legacySignalAdoptionFixture(t)
			switch kind {
			case "old-image":
				svc.adoptionProbe = func(context.Context, string) error { return errors.New("old image") }
			case "changed-binding":
				spec.ServiceBinding = notify.Hash("foreign")
			case "occupied-inbox":
				testutil.Must(t, os.WriteFile(filepath.Join(spec.Inbox, "foreign"), []byte("untouched"), 0600))
			}
			if _, err := adoptBackupSignal(context.Background(), spec, svc); err == nil {
				t.Fatal("invalid adoption accepted")
			}
			if kind != "occupied-inbox" && !*active {
				t.Fatal("preflight stopped service")
			}
			if kind == "occupied-inbox" {
				raw, err := os.ReadFile(filepath.Join(spec.Inbox, "foreign"))
				testutil.Must(t, err)
				if string(raw) != "untouched" {
					t.Fatal("foreign state changed")
				}
			}
		})
	}
}

func TestLegacyBackupSignalHeldChild(t *testing.T) {
	if os.Getenv("DEPLOY_LEGACY_SIGNAL_HELD_CHILD") == "" {
		t.Skip("child helper")
	}
	os.Stdout.WriteString("READY\n")
	select {}
}
func TestLegacyBackupSignalAdoptionExcludesChildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	spec, svc, _ := legacySignalAdoptionFixtureLease(t, func(lease *signalaccount.Lease) {
		cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLegacyBackupSignalHeldChild$")
		cmd.Env = append(os.Environ(), "DEPLOY_LEGACY_SIGNAL_HELD_CHILD=1")
		cmd.ExtraFiles = []*os.File{lease.File()}
		pipe, err := cmd.StdoutPipe()
		testutil.Must(t, err)
		testutil.Must(t, cmd.Start())
		scan := bufio.NewScanner(pipe)
		if !scan.Scan() || scan.Text() != "READY" {
			t.Fatal("account child did not start")
		}
	})
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	if _, err := adoptBackupSignal(ctx, spec, svc); err == nil {
		t.Fatal("adoption bypassed inherited account lease")
	}
	if _, err := os.Stat(filepath.Join(spec.Inbox, "legacy-bootstrap-intent.json")); !os.IsNotExist(err) {
		t.Fatal("bootstrap began while old CLI child remained", err)
	}
	testutil.Must(t, cmd.Process.Kill())
	if cmd.Wait() == nil {
		t.Fatal("child survived")
	}
	_, err := adoptBackupSignal(ctx, spec, svc)
	testutil.Must(t, err)
}
