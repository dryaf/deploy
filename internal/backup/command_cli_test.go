//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func TestBackupCommandCLIReadsThroughActualGateway(t *testing.T) {
	_, state, _, _, _ := archiveFixture(t)
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	policy := configfixture.WorkerPolicy(t)
	testutil.Must(t, state.observeIncident("capture", "a", "offline", time.Now().UTC(), policy.Notifications))
	testutil.Must(t, state.observeIncident("capture", "b", "private", time.Now().UTC(), policy.Notifications))
	p := notify.Producer{ID: control.WriterID, Epoch: "command-install-one"}
	d := interaction.Destination{ID: "ops", Generation: "one", Provider: "telegram", Account: "123", NativeID: "-100"}
	manifest := interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "backup/storage", Producer: p, Generation: "one", Commands: backupCommandManifest(false)}}, Destinations: []interaction.Destination{d}, Permissions: []interaction.Permission{{Actor: "actor", Conversation: d.ID, Worker: p.ID, Permissions: []string{"read"}, Scope: interaction.Scope{Subjects: []string{"a"}}}}}
	root := t.TempDir()
	testutil.Must(t, os.Chmod(root, 0700))
	cfg := worker.Config{Producers: []worker.Enrollment{{Producer: p, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, MaxMessages: 100}}, Routes: map[string]worker.Route{}, MaxMessages: 100, MaxPayloadBytes: 1 << 20, MaxDatabaseBytes: 16 << 20, Interaction: &manifest}
	testutil.Must(t, os.Mkdir(filepath.Join(root, "gateway"), 0700))
	gateway, err := worker.Open(filepath.Join(root, "gateway"), cfg, true)
	testutil.Must(t, err)
	defer gateway.Close()
	socket := filepath.Join(root, "gateway.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- gateway.Serve(ctx, socket) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("gateway failed to stop")
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, e := net.Dial("unix", socket)
		if e == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway failed to start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	runtime := backupCommandRuntime{Version: 1, Producer: p, Identity: gateway.Identity(), Socket: socket, Policy: manifest}
	file := filepath.Join(root, "commands.json")
	raw, err := json.Marshal(runtime)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(file, raw, 0600))
	directory := state.directory
	testutil.Must(t, state.Close())
	testutil.Must(t, RunCLI([]string{"commands-init", "--local", "--config", file, "--state-dir", directory}))
	in := interaction.Inbound{Provider: d.Provider, Account: d.Account, NativeID: "request", Actor: "actor", Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Now().UTC(), Text: "backup/storage status"}
	v, err := gateway.AdmitCommand(ctx, in)
	testutil.Must(t, err)
	testutil.Must(t, RunCLI([]string{"commands-step", "--local", "--config", file, "--state-dir", directory}))
	reopened, err := openBackupState(directory, false)
	testutil.Must(t, err)
	defer reopened.Close()
	var body string
	testutil.Must(t, reopened.db.QueryRow("SELECT body FROM interactive_results WHERE id=?", v.Command.ID).Scan(&body))
	var result interaction.Result
	testutil.Must(t, notify.Decode([]byte(body), &result, notify.MaxResponse))
	if result.State != "succeeded" || result.Text != "Open incidents in authorized scope: 1. Incident state does not establish current backup freshness or application recovery." {
		t.Fatal(result)
	}
	var acked int
	testutil.Must(t, reopened.db.QueryRow("SELECT acked FROM interactive_results WHERE id=?", v.Command.ID).Scan(&acked))
	if acked != 1 {
		t.Fatal("result not durably acknowledged")
	}
	// This local staging path must not turn a manifest edit into mutation authority.
	runtime.Policy.Workers[0].Commands = backupCommandManifest(true)
	raw, err = json.Marshal(runtime)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(file, raw, 0600))
	if _, err = readBackupCommandRuntime(file); err == nil {
		t.Fatal("staged CLI accepted mutation capabilities")
	}
}
