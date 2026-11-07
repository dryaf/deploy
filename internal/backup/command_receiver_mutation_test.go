//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func TestSharedMutationReceiverPreservesReadVersionAndRejectsRewriting(t *testing.T) {
	r := sharedCommandReceiverFixture(t)
	ctx := context.Background()
	_, err := StageSharedCommandReceiver(ctx, r)
	testutil.Must(t, err)
	r.Policy.Workers[0].Commands = BackupIncidentCommandDescriptors()
	r.Policy.Permissions[0].Permissions = []string{"read", "incident.manage"}
	if err = r.validate(); err == nil {
		t.Fatal("version 1 acquired mutation rights")
	}
	r.Version = 2
	testutil.Must(t, r.validate())
	if _, err = StageSharedCommandReceiver(ctx, r); err == nil {
		t.Fatal("rewrote an installed receiver without migration")
	}
	r.Policy.Permissions[0].Permissions = []string{"job.request"}
	if err = r.validate(); err == nil {
		t.Fatal("accepted unimplemented job authority")
	}
}

func TestSharedMutationReceiverRunsIncidentDecisionsOnce(t *testing.T) {
	r, session := sharedCommandReceiverModeFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := StageSharedCommandReceiver(ctx, r)
	testutil.Must(t, err)
	state, err := openBackupState(r.State, false)
	testutil.Must(t, err)
	defer state.Close()
	policy := configfixture.WorkerPolicy(t)
	for _, subject := range []string{"a", "b"} {
		testutil.Must(t, state.observeIncident("capture", subject, "capture failed", time.Now().UTC().Add(-time.Minute), policy.Notifications))
	}
	var incident string
	testutil.Must(t, state.db.QueryRow("SELECT id FROM incidents WHERE instance='a' AND resolved IS NULL").Scan(&incident))
	done := make(chan error, 1)
	go func() { done <- session.store.Serve(ctx, r.Socket) }()
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
		conn, err := net.Dial("unix", r.Socket)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway unavailable")
		}
		time.Sleep(5 * time.Millisecond)
	}
	destination := r.Policy.Destinations[0]
	for i, decision := range []struct {
		name, revision, extra, outcome string
	}{
		{"ack", "1", "", "succeeded"},
		{"snooze", "1", " duration=30m", "failed"}, // Stale revision cannot renew suppression.
		{"snooze", "2", " duration=30m", "succeeded"},
		{"unsnooze", "3", "", "succeeded"},
	} {
		inbound := interaction.Inbound{Provider: destination.Provider, Account: destination.Account, NativeID: fmt.Sprint(i), Actor: "789", Conversation: destination.ID, DestinationFingerprint: destination.Fingerprint(), SentAt: time.Now().UTC(), Text: fmt.Sprintf("backup/manager %s subject=a target=%s evidence_revision=1 decision_revision=%s%s", decision.name, incident, decision.revision, decision.extra)}
		view, err := session.store.AdmitCommand(ctx, inbound)
		testutil.Must(t, err)
		for range 2 {
			testutil.Must(t, runScheduledSharedCommands(ctx, state, StepSharedCommandReceiver))
		}
		var body string
		var acked int
		testutil.Must(t, state.db.QueryRow("SELECT body,acked FROM interactive_results WHERE id=?", view.Command.ID).Scan(&body, &acked))
		var result interaction.Result
		testutil.Must(t, json.Unmarshal([]byte(body), &result))
		if result.State != decision.outcome || acked != 1 {
			t.Fatalf("%s: %+v, acked=%d", decision.name, result, acked)
		}
		again, err := session.store.AdmitCommand(ctx, inbound)
		testutil.Must(t, err)
		if notify.Hash(again.Command) != notify.Hash(view.Command) {
			t.Fatal("replayed provider input created another command")
		}
		testutil.Must(t, runScheduledSharedCommands(ctx, state, StepSharedCommandReceiver))
	}
	var count, revision int
	testutil.Must(t, state.db.QueryRow("SELECT count(*) FROM backup_command_audit").Scan(&count))
	testutil.Must(t, state.db.QueryRow("SELECT revision FROM backup_command_suppressions WHERE incident=?", incident).Scan(&revision))
	if count != 3 || revision != 4 {
		t.Fatal("replayed or stale decisions changed the audit", count, revision)
	}
	testutil.Must(t, state.db.QueryRow("SELECT count(*) FROM incidents WHERE resolved IS NULL AND instance IN ('a','b')").Scan(&count))
	if count != 2 {
		t.Fatal("reminder decisions resolved health incidents")
	}
}
