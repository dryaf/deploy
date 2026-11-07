//go:build linux

package workerctl

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func testSharedCommandHelperCLI(t *testing.T, root string, write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any, receiver backup.CommandReceiverRequest) {
	const setup = "manager/.local/state/deploy/backup-worker/setup.json"
	const receiverPath = "manager/.local/state/deploy/backup-worker/state/command-receiver.json"
	const handoff = "manager/.local/state/deploy/backup-worker/state/notification-cutover.json"
	var before backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &before))
	receiverBytes, cutoverBytes := read(t, receiverPath), read(t, handoff)
	ctx := context.Background()
	openLedger := func() (*sql.DB, *interaction.Ledger) {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+receiver.State+"/worker.db?mode=rw&_pragma=synchronous(FULL)")
		testutil.Must(t, err)
		db.SetMaxOpenConns(1)
		ledger, err := interaction.OpenDomainLedger(ctx, db, receiver.Producer, receiver.Identity, false, nil)
		testutil.Must(t, err)
		return db, ledger
	}
	db, ledger := openLedger()
	now := time.Now().UTC()
	command := interaction.Command{ID: "C-helper-complete", RequestKey: "helper-complete", InputHash: "offline-fixture", Producer: receiver.Producer, Scope: interaction.Scope{WholeWorker: true}, IssuedAt: now, Deadline: now.Add(time.Hour), Descriptor: receiver.Policy.Workers[0].Commands["status"]}
	testutil.Must(t, ledger.Import(ctx, command))
	grant := interaction.Grant{ID: "helper-grant", Identity: receiver.Identity, CommandID: command.ID, CommandHash: protocol.Hash(command), ExpiresAt: now.Add(10 * time.Second)}
	result, err := ledger.Execute(ctx, command, grant, "", now, func(*sql.Tx, interaction.Command) (interaction.Result, error) {
		return interaction.Result{State: "succeeded", Scope: command.Scope, Text: "offline retained result"}, nil
	})
	testutil.Must(t, err)
	pendingCommand := command
	pendingCommand.ID, pendingCommand.RequestKey = "C-helper-pending", "helper-pending"
	testutil.Must(t, ledger.Import(ctx, pendingCommand))
	queued, err := ledger.PendingResults(ctx)
	testutil.Must(t, err)
	testutil.Must(t, db.Close())

	candidate := append(read(t, "deploy"), []byte("offline command helper upgrade")...)
	write("upgrade-helper", string(candidate))
	testutil.Must(t, os.Chmod(root+"/upgrade-helper", 0700))
	digest := inputDigest(candidate)
	old := "      storage: {min_free_bytes: 1048576}\n      artifacts: {catalog: /tmp/wux/catalog.json}"
	next := "      storage: {min_free_bytes: 1048576}\n      artifacts: {catalog: /tmp/wux/catalog.json, helper: /tmp/wux/upgrade-helper, helper_sha256: " + digest + ", image: fixture.invalid/backup@sha256:" + strings.Repeat("c", 64) + "}"
	yaml := string(read(t, "server.yaml"))
	if !strings.Contains(yaml, old) {
		t.Fatal("command helper fixture declaration missing")
	}
	write("server.yaml", strings.Replace(yaml, old, next, 1))
	run(t, "backup-worker", "plan", true)
	write("lose-source-reply", "once")
	run(t, "backup-worker", "apply", false)
	var interrupted backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &interrupted))
	if interrupted.SharedPolicyUpdate == nil || interrupted.SharedPolicyUpdate.Version != 8 {
		t.Fatal("command helper interruption lost typed intent")
	}
	operation := interrupted.SharedPolicyUpdate.Operation
	run(t, "backup-worker", "resume", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}
	var after backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &after))
	if after.SharedPolicyHistory == nil || after.SharedPolicyHistory.Operation != operation || after.Declaration.HelperSHA256 != digest || after.WorkerID != before.WorkerID || after.ArchiveID != before.ArchiveID || !reflect.DeepEqual(after.SourceEnrollments, before.SourceEnrollments) || !reflect.DeepEqual(after.Producer, before.Producer) {
		t.Fatal("command helper upgrade replaced retained ownership")
	}
	if string(receiverBytes) != string(read(t, receiverPath)) || string(cutoverBytes) != string(read(t, handoff)) {
		t.Fatal("helper upgrade rewrote receiver or reporting authority")
	}
	if inputDigest(read(t, "manager/.local/lib/deploy/backup-worker/deploy")) != digest {
		t.Fatal("replacement command helper was not installed")
	}
	db, ledger = openLedger()
	defer db.Close()
	retained, err := ledger.PendingResults(ctx)
	testutil.Must(t, err)
	if !reflect.DeepEqual(retained, queued) {
		t.Fatal("helper upgrade lost pending command results")
	}
	for _, c := range []interaction.Command{command, pendingCommand} {
		testutil.Must(t, ledger.Import(ctx, c))
	}
	again, err := ledger.Execute(ctx, command, grant, "", time.Now(), func(*sql.Tx, interaction.Command) (interaction.Result, error) {
		t.Fatal("upgrade replayed a completed command")
		return interaction.Result{}, nil
	})
	testutil.Must(t, err)
	if protocol.Hash(again) != protocol.Hash(result) {
		t.Fatal("upgrade changed completed command result")
	}
	var count int
	testutil.Must(t, db.QueryRow("SELECT count(*) FROM interactive_inbox").Scan(&count))
	if count != 2 {
		t.Fatal("upgrade lost or duplicated commands", count)
	}
	testutil.Must(t, db.Close())
	// Later command activity must traverse the completed helper history.
	declared := string(read(t, "server.yaml"))
	for _, enabled := range []bool{false, true} {
		policy := declared
		if !enabled {
			policy = strings.Replace(policy, "commands:\n        enabled: true", "commands:\n        enabled: false", 1)
		}
		write("server.yaml", policy)
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, "notification-worker", action, true)
		}
		run(t, "backup-worker", "apply", true)
	}
	// A second helper change must inspect completed command activity using its
	// original snapshot, then connect the separately applied backup policy.
	candidate = append(candidate, []byte("second offline helper upgrade")...)
	write("upgrade-helper", string(candidate))
	testutil.Must(t, os.Chmod(root+"/upgrade-helper", 0700))
	write("server.yaml", strings.Replace(string(read(t, "server.yaml")), digest, inputDigest(candidate), 1))
	run(t, "backup-worker", "apply", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}
	if string(receiverBytes) != string(read(t, receiverPath)) {
		t.Fatal("later command activity replaced receiver")
	}
}
