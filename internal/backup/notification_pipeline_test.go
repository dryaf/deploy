//go:build linux

package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

type pipelineFixture struct {
	state         *backupState
	gateway       *worker.Store
	client        client.Client
	binding       backupNotificationBinding
	now           time.Time
	gatewayPath   string
	gatewayConfig worker.Config
	stopGateway   func()
}

func newPipelineFixture(t *testing.T) *pipelineFixture {
	t.Helper()
	_, state, _, _, _ := archiveFixture(t)
	return newPipelineFixtureFromState(t, state)
}

func newPipelineFixtureFromState(t *testing.T, state *backupState, configure ...func(*worker.Config)) *pipelineFixture {
	t.Helper()
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	p := notify.Producer{ID: control.WriterID, Epoch: "one"}
	dest := interaction.Destination{ID: "ops", Generation: "one", Provider: "telegram", Account: "123", NativeID: "-100"}
	commands := interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "backup/storage", Producer: p, Generation: "one", Commands: backupCommandManifest(false)}}, Destinations: []interaction.Destination{dest}, Permissions: []interaction.Permission{{Actor: "actor", Conversation: "ops", Worker: p.ID, Permissions: []string{"read"}, Scope: interaction.Scope{Subjects: []string{"a"}}}}}
	cfg := worker.Config{Producers: []worker.Enrollment{{Producer: p, Routes: []string{"ops"}, Required: true, MaxMessages: 1000, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, InteractiveContext: true, ContextKinds: []string{"incident"}}}, Routes: map[string]worker.Route{"ops": {Revision: "one", Primary: "telegram"}}, MaxMessages: 1000, MaxPayloadBytes: 1 << 20, MaxDatabaseBytes: 32 << 20, Interaction: &commands}
	for _, apply := range configure {
		apply(&cfg)
	}
	root := t.TempDir()
	testutil.Must(t, os.Chmod(root, 0700))
	g, err := worker.Open(root, cfg, true)
	testutil.Must(t, err)
	testutil.Must(t, g.Stage(p, "one"))
	testutil.Must(t, g.Activate(p, "one"))
	sockets, err := os.MkdirTemp("", "backup-pipeline-")
	testutil.Must(t, err)
	socket := filepath.Join(sockets, "api.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Serve(ctx, socket) }()
	var stopped sync.Once
	stopGateway := func() {
		stopped.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("gateway did not stop")
			}
			g.Close()
			os.RemoveAll(sockets)
		})
	}
	t.Cleanup(stopGateway)
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, e := net.Dial("unix", socket)
		if e == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	b := backupNotificationBinding{Identity: g.Identity(), Producer: p, Archive: control.ArchiveID, Epoch: control.Epoch, Generation: "one", Route: "ops"}
	ledgerTx(t, state, func(tx *sql.Tx) error {
		if err := initializeBackupCommands(tx); err != nil {
			return err
		}
		return installBackupNotificationPipeline(tx, b)
	})
	return &pipelineFixture{state: state, gateway: g, client: client.Client{Socket: socket, Identity: g.Identity()}, binding: b, now: time.Now().UTC(), gatewayPath: root, gatewayConfig: cfg, stopGateway: stopGateway}
}
func (f *pipelineFixture) observe(t *testing.T, subject, message string, at time.Time) {
	t.Helper()
	testutil.Must(t, f.state.observeIncident("capture", subject, message, at, configfixture.WorkerPolicy(t).Notifications))
}
func (f *pipelineFixture) freeze(t *testing.T, seq, basis uint64) []notify.Message {
	t.Helper()
	var out []notify.Message
	ledgerTx(t, f.state, func(tx *sql.Tx) error {
		snapshot := notify.Snapshot{Producer: f.binding.Producer, Sequence: seq, BasisRevision: basis, ConfigRevision: "one", EvaluatedAt: f.now, Subjects: []notify.Subject{{Key: "backup", Condition: "critical", Reason: "Fixture current failure evidence", ValidUntil: f.now.Add(time.Hour)}, {Key: "a", Condition: "critical", Reason: "Fixture current failure evidence", ValidUntil: f.now.Add(time.Hour)}}}
		var err error
		out, err = freezeBackupNotificationPass(tx, backupNotificationEvaluation{Snapshot: snapshot, ReportText: "Current fixture backup failures remain actionable."})
		return err
	})
	return out
}
func (f *pipelineFixture) transfer(t *testing.T, api backupNotificationAPI) {
	t.Helper()
	testutil.Must(t, transferBackupNotifications(context.Background(), f.state, api))
}
func (f *pipelineFixture) accept(t *testing.T, m notify.Message) {
	t.Helper()
	job, ok, err := f.gateway.Grant(context.Background(), m.Producer, m.ID)
	testutil.Must(t, err)
	if !ok {
		t.Fatal("message not granted", m.Kind)
	}
	testutil.Must(t, f.gateway.Finish(context.Background(), job, worker.Result{Accepted: true, Binding: &interaction.DeliveryBinding{Destination: interaction.Destination{ID: "ops", Generation: "one", Provider: "telegram", Account: "123", NativeID: "-100"}, Native: interaction.NativeReply{ID: fmt.Sprint(m.Sequence), Sender: "123"}}}))
}

type losePipelineResponse struct {
	api       backupNotificationAPI
	operation string
	lost      bool
	skip      int
}

func (l *losePipelineResponse) Call(ctx context.Context, r notify.Request, out any) (notify.Identity, error) {
	id, err := l.api.Call(ctx, r, out)
	if err == nil && r.Operation == l.operation && !l.lost {
		if l.skip > 0 {
			l.skip--
			return id, err
		}
		l.lost = true
		return id, fmt.Errorf("injected lost response")
	}
	return id, err
}

func TestBackupPipelineFrozenContextAndLostEnqueueResponse(t *testing.T) {
	f := newPipelineFixture(t)
	f.observe(t, "a", "original evidence", f.now)
	var incident string
	testutil.Must(t, f.state.db.QueryRow("SELECT id FROM incidents WHERE instance='a'").Scan(&incident))
	// Change evidence before export: source context must still name revision one.
	f.observe(t, "a", "changed evidence", f.now)
	messages := f.freeze(t, 1, 1)
	if len(messages) != 1 || messages[0].Context == nil || messages[0].Context.Targets[0].EvidenceRevision != 1 {
		t.Fatal("context was reconstructed from newer evidence", messages)
	}
	lost := &losePipelineResponse{api: f.client, operation: "message.enqueue"}
	if err := transferBackupNotifications(context.Background(), f.state, lost); err == nil || !lost.lost {
		t.Fatal("lost response not injected", err)
	}
	testutil.Must(t, f.state.Close())
	reopened, err := openBackupState(f.state.directory, false)
	testutil.Must(t, err)
	f.state = reopened
	t.Cleanup(func() { reopened.Close() })
	f.transfer(t, f.client)
	f.transfer(t, f.client)
	if again := f.freeze(t, 1, 1); len(again) != 0 {
		t.Fatal("pending export duplicated")
	}
	f.accept(t, messages[0])
	f.transfer(t, f.client)
	var accepted string
	testutil.Must(t, f.state.db.QueryRow("SELECT accepted FROM outbox WHERE id=?", incident+"/failure").Scan(&accepted))
	var count int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_exports").Scan(&count))
	if count != 1 {
		t.Fatal("frozen event duplicated")
	}
	d := interaction.Destination{ID: "ops", Generation: "one", Provider: "telegram", Account: "123", NativeID: "-100"}
	in := interaction.Inbound{Provider: d.Provider, Account: d.Account, NativeID: "reply", Actor: "actor", Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Now().UTC(), Text: "details", Reply: &interaction.NativeReply{ID: "1", Sender: d.Account}}
	cmd, err := f.gateway.AdmitCommand(context.Background(), in)
	testutil.Must(t, err)
	if cmd.Command.Arguments["target"] != incident || cmd.Command.Context.Context.Targets[0].EvidenceRevision != 1 {
		t.Fatal("real source context lost in gateway")
	}
}

func TestBackupPipelineCancellationPrecedesSubmissionAndPreservesOtherMembers(t *testing.T) {
	f := newPipelineFixture(t)
	past := f.now.Add(-7 * time.Hour)
	f.observe(t, "a", "failure a", past)
	f.observe(t, "a", "failure a", f.now)
	for i := 0; i < 8; i++ {
		f.observe(t, fmt.Sprintf("other-%d", i), "unrelated", f.now)
	}
	testutil.Must(t, coalesceNotificationBacklog(f.state, f.now))
	messages := f.freeze(t, 1, 1)
	if len(messages) != 1 || messages[0].Context == nil || messages[0].Context.Kind != "summary" {
		t.Fatal(messages)
	}
	var incident string
	testutil.Must(t, f.state.db.QueryRow("SELECT id FROM incidents WHERE instance='a'").Scan(&incident))
	ledgerTx(t, f.state, func(tx *sql.Tx) error { return cancelIncidentReminders(tx, incident, "C-ack") })
	f.transfer(t, f.client)
	job, err := f.gateway.Get(context.Background(), f.binding.Producer, messages[0].ID)
	testutil.Must(t, err)
	if job.State != "cancelled" || job.Tries != 0 {
		t.Fatal("cancelled unsent batch dispatched", job)
	}
	var open, waived int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='waived'").Scan(&waived))
	if open != 9 || waived != 1 {
		t.Fatal("unrelated obligations lost", open, waived)
	}
	replacements := f.freeze(t, 2, 1)
	if len(replacements) != 9 {
		t.Fatal("unrelated members not re-exported", len(replacements))
	}
	f.transfer(t, f.client)
	for _, m := range replacements {
		f.accept(t, m)
	}
	f.transfer(t, f.client)
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
	if open != 0 {
		t.Fatal("unrelated obligations remain pending", open)
	}
}

func TestBackupPipelineCancellationAfterGrantPreservesIneligibleAcceptance(t *testing.T) {
	f := newPipelineFixture(t)
	past := f.now.Add(-7 * time.Hour)
	f.observe(t, "a", "offline", past)
	f.observe(t, "a", "offline", f.now)
	messages := f.freeze(t, 1, 1)
	f.transfer(t, f.client)
	var reminder notify.Message
	for _, m := range messages {
		if m.Kind == "reminder" {
			reminder = m
		}
	}
	granted, ok, err := f.gateway.Grant(context.Background(), f.binding.Producer, reminder.ID)
	testutil.Must(t, err)
	if !ok {
		t.Fatal("reminder not granted")
	}
	ledgerTx(t, f.state, func(tx *sql.Tx) error { return cancelIncidentReminders(tx, reminder.Incidents[0].ID, "C-ack") })
	f.transfer(t, f.client)
	testutil.Must(t, f.gateway.Finish(context.Background(), granted, worker.Result{Accepted: true}))
	f.transfer(t, f.client)
	var accepted, eligible int
	testutil.Must(t, f.state.db.QueryRow("SELECT accepted,eligible FROM notification_exports WHERE id=?", reminder.ID).Scan(&accepted, &eligible))
	if accepted != 1 || eligible != 0 {
		t.Fatal("late cancelled send fabricated coverage", accepted, eligible)
	}
	var rawAccepted sql.NullString
	testutil.Must(t, f.state.db.QueryRow("SELECT accepted FROM outbox WHERE kind='reminder'").Scan(&rawAccepted))
	if rawAccepted.Valid {
		t.Fatal("cancelled reminder marked delivered for coverage")
	}
}

func TestBackupPipelineReportReplacementAndLostReceiptAck(t *testing.T) {
	f := newPipelineFixture(t)
	c := WorkerInstallation{WorkerID: f.binding.Producer.ID, Policy: configfixture.WorkerPolicy(t)}
	c.Policy.Notifications.DailyAt = "00:00"
	testutil.Must(t, f.state.enqueueDaily(c, f.now, "OLD REPORT TEXT"))
	first := f.freeze(t, 1, 1)
	if len(first) != 1 || strings.Contains(first[0].Text, "OLD REPORT TEXT") {
		t.Fatal("old report was reused", first)
	}
	f.transfer(t, f.client)
	// A changed basis supersedes the queued report without satisfying its period.
	if got := f.freeze(t, 2, 2); len(got) != 0 {
		t.Fatal("live report replaced before settlement")
	}
	f.transfer(t, f.client)
	second := f.freeze(t, 2, 2)
	if len(second) != 1 || second[0].ReportKey != first[0].ReportKey || second[0].ReportRevision != 2 {
		t.Fatal("logical report period changed", second)
	}
	f.transfer(t, f.client)
	f.accept(t, second[0])
	lost := &losePipelineResponse{api: f.client, operation: "receipt.list"}
	if err := transferBackupNotifications(context.Background(), f.state, lost); err == nil {
		t.Fatal("lost receipt page not injected")
	}
	// Lose the acknowledgement after importing the newly received page, rather
	// than the initial acknowledgement of the previous durable cursor.
	lost = &losePipelineResponse{api: f.client, operation: "receipt.ack", skip: 1}
	if err := transferBackupNotifications(context.Background(), f.state, lost); err == nil || !lost.lost {
		t.Fatal("post-commit ack loss not injected", err)
	}
	var disposition, covered string
	testutil.Must(t, f.state.db.QueryRow("SELECT disposition,covered_by FROM notification_obligations WHERE id=?", first[0].ReportKey).Scan(&disposition, &covered))
	if disposition != "satisfied" || covered != second[0].ID {
		t.Fatal(disposition, covered)
	}
	f.transfer(t, f.client)
}

func TestBackupPipelineAuthorityAndLegacyContextAreNotAdopted(t *testing.T) {
	f := newPipelineFixture(t)
	// A pre-extension row has no stored context even though its incident is known.
	f.observe(t, "a", "legacy", f.now)
	_, err := f.state.db.Exec("DELETE FROM backup_command_event_context")
	testutil.Must(t, err)
	messages := f.freeze(t, 1, 1)
	if len(messages) != 1 || messages[0].Context != nil {
		t.Fatal("legacy prose acquired mutation context")
	}
	var control ArchiveControl
	testutil.Must(t, f.state.get("control", &control))
	control.Epoch++
	testutil.Must(t, f.state.put("control", control))
	if err = transferBackupNotifications(context.Background(), f.state, f.client); err == nil {
		t.Fatal("archive authority change ignored")
	}
	var count int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM backup_notification_transfers WHERE submitted=1").Scan(&count))
	if count != 0 {
		t.Fatal("export submitted after authority change")
	}
}

func TestBackupContextAndCancellationRollbackWithDomainTransaction(t *testing.T) {
	f := newPipelineFixture(t)
	f.observe(t, "a", "offline", f.now.Add(-7*time.Hour))
	f.observe(t, "a", "offline", f.now)
	messages := f.freeze(t, 1, 1)
	var incident string
	for _, m := range messages {
		if m.Kind == "reminder" {
			incident = m.Incidents[0].ID
		}
	}
	tx, err := f.state.db.Begin()
	testutil.Must(t, err)
	testutil.Must(t, cancelIncidentReminders(tx, incident, "rollback"))
	testutil.Must(t, tx.Rollback())
	var count int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM backup_notification_transfers WHERE cancel_reason<>''").Scan(&count))
	if count != 0 {
		t.Fatal("cancellation survived rollback")
	}
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='waived'").Scan(&count))
	if count != 0 {
		t.Fatal("waiver survived rollback")
	}
	// Source context is independently frozen before any export attempt.
	var body string
	testutil.Must(t, f.state.db.QueryRow("SELECT body FROM backup_command_event_context LIMIT 1").Scan(&body))
	var context notify.InteractionContext
	testutil.Must(t, json.Unmarshal([]byte(body), &context))
	if context.Targets[0].Occurrence != incident {
		t.Fatal(context)
	}
}

func TestBackupPipelineDailyHistoryNeedsAcceptedCatchup(t *testing.T) {
	f := newPipelineFixture(t)
	c := WorkerInstallation{WorkerID: f.binding.Producer.ID, Policy: configfixture.WorkerPolicy(t)}
	c.Policy.Notifications.DailyAt = "00:00"
	testutil.Must(t, f.state.enqueueDaily(c, f.now.Add(-24*time.Hour), "yesterday"))
	old := f.freeze(t, 1, 1)
	if len(old) != 1 {
		t.Fatal(old)
	}
	f.transfer(t, f.client)
	testutil.Must(t, f.state.enqueueDaily(c, f.now, "today"))
	ledgerTx(t, f.state, func(tx *sql.Tx) error {
		return enqueueNotification(tx, notification.NotificationEvent{ID: "catchup/outage-a", Kind: "delivery-catchup", Message: "old catchup text"}, f.now.Add(-time.Hour))
	})
	current := f.freeze(t, 2, 1)
	if len(current) != 1 || current[0].ReportType != "daily" {
		t.Fatal("catchup overtook outstanding daily report", current)
	}
	f.transfer(t, f.client)
	job, err := f.gateway.Get(context.Background(), f.binding.Producer, old[0].ID)
	testutil.Must(t, err)
	if job.State != "cancelled" {
		t.Fatal("coalesced daily export remained live", job.State)
	}
	f.accept(t, current[0])
	f.transfer(t, f.client)
	var disposition string
	testutil.Must(t, f.state.db.QueryRow("SELECT disposition FROM notification_obligations WHERE id=?", old[0].ReportKey).Scan(&disposition))
	if disposition != "open" {
		t.Fatal("today's daily report covered yesterday", disposition)
	}
	catchup := f.freeze(t, 3, 1)
	if len(catchup) != 1 || catchup[0].Kind != "catchup" || catchup[0].OutageID != "outage-a" {
		t.Fatal(catchup)
	}
	f.transfer(t, f.client)
	if got := f.freeze(t, 3, 1); len(got) != 0 {
		t.Fatal("live catchup replaced", got)
	}
	f.transfer(t, f.client)
	queued, err := f.gateway.Get(context.Background(), f.binding.Producer, catchup[0].ID)
	testutil.Must(t, err)
	if queued.State != "queued" {
		t.Fatal("historical daily membership cancelled live catchup", queued.State)
	}
	f.freeze(t, 4, 2)
	f.transfer(t, f.client)
	replacement := f.freeze(t, 4, 2)
	if len(replacement) != 1 || replacement[0].ReportRevision != 2 || replacement[0].OutageID != catchup[0].OutageID {
		t.Fatal("catchup replacement lost outage", replacement)
	}
	f.transfer(t, f.client)
	f.accept(t, replacement[0])
	f.transfer(t, f.client)
	var covered string
	testutil.Must(t, f.state.db.QueryRow("SELECT disposition,covered_by FROM notification_obligations WHERE id=?", old[0].ReportKey).Scan(&disposition, &covered))
	if disposition != "satisfied" || covered != replacement[0].ID {
		t.Fatal("catchup did not settle older period", disposition, covered)
	}
	var rawAccepted sql.NullString
	testutil.Must(t, f.state.db.QueryRow("SELECT accepted FROM outbox WHERE id=?", old[0].ReportKey).Scan(&rawAccepted))
	if rawAccepted.Valid {
		t.Fatal("historical unsent row falsely marked sent")
	}
}

func TestBackupPipelinePendingWindowDoesNotStarveNewAlerts(t *testing.T) {
	f := newPipelineFixture(t)
	for i := 0; i < 40; i++ {
		f.observe(t, fmt.Sprintf("instance-%02d", i), "still failing", f.now.Add(time.Duration(i-40)*time.Second))
	}
	first := f.freeze(t, 1, 1)
	second := f.freeze(t, 2, 1)
	if len(first) != 32 || len(second) != 8 {
		t.Fatal("live exports starved new source rows", len(first), len(second))
	}
	f.transfer(t, f.client)
	f.transfer(t, f.client)
	job, err := f.gateway.Get(context.Background(), f.binding.Producer, second[7].ID)
	testutil.Must(t, err)
	if job.State != "queued" {
		t.Fatal(job.State)
	}
}

func TestBackupPipelineRetirementCancelsOnlyRetiredMembers(t *testing.T) {
	f := newPipelineFixture(t)
	f.observe(t, "a", "retire this instance", f.now)
	for i := 0; i < 8; i++ {
		f.observe(t, fmt.Sprintf("unrelated-%d", i), "still protected", f.now)
	}
	testutil.Must(t, coalesceNotificationBacklog(f.state, f.now))
	batch := f.freeze(t, 1, 1)
	if len(batch) != 1 {
		t.Fatal(batch)
	}
	f.transfer(t, f.client)
	var control ArchiveControl
	testutil.Must(t, f.state.get("control", &control))
	control.RetiredSources["a"] = "operator decommissioned fixture"
	testutil.Must(t, f.state.put("control", control))
	testutil.Must(t, f.state.reconcileRetiredIncidents(f.now))
	f.transfer(t, f.client)
	job, err := f.gateway.Get(context.Background(), f.binding.Producer, batch[0].ID)
	testutil.Must(t, err)
	if job.State != "cancelled" {
		t.Fatal("obsolete retirement batch stayed live", job.State)
	}
	next := f.freeze(t, 2, 2)
	var failures, retirements int
	for _, m := range next {
		if m.Kind == "failure" {
			failures++
		}
		if m.ReportType == "retirement" {
			retirements++
		}
		if m.Kind == "recovery" {
			t.Fatal("retirement claimed recovery")
		}
	}
	if failures != 8 || retirements != 1 {
		t.Fatal("retirement lost unrelated obligations", failures, retirements)
	}
	f.transfer(t, f.client)
	for _, m := range next {
		f.accept(t, m)
	}
	f.transfer(t, f.client)
	var open, waived int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='waived'").Scan(&waived))
	if open != 0 || waived != 1 {
		t.Fatal(open, waived)
	}
}

func TestBackupPipelineEvaluationRollbackAndIdentity(t *testing.T) {
	f := newPipelineFixture(t)
	tx, err := f.state.db.Begin()
	testutil.Must(t, err)
	testutil.Must(t, enqueueNotification(tx, notification.NotificationEvent{ID: "test/rollback", Kind: "test", Message: "transactional fixture"}, f.now))
	evaluation := backupNotificationEvaluation{Snapshot: notify.Snapshot{Producer: f.binding.Producer, Sequence: 1, BasisRevision: 1, ConfigRevision: "one", EvaluatedAt: f.now, Subjects: []notify.Subject{{Key: "backup", Condition: "unknown", Reason: "Fixture evaluation", ValidUntil: f.now.Add(time.Hour)}}}, ReportText: "Uncertain current state"}
	messages, err := freezeBackupNotificationPass(tx, evaluation)
	testutil.Must(t, err)
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	testutil.Must(t, tx.Rollback())
	for _, table := range []string{"outbox", "backup_notification_snapshots", "notification_exports", "notification_obligations", "backup_notification_transfers"} {
		var count int
		testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM "+table).Scan(&count))
		if count != 0 {
			t.Fatal("partial transaction survived", table, count)
		}
	}
	ledgerTx(t, f.state, func(tx *sql.Tx) error { _, err := freezeBackupNotificationPass(tx, evaluation); return err })
	evaluation.ReportText = "Changed report without a new evaluation"
	tx, err = f.state.db.Begin()
	testutil.Must(t, err)
	defer tx.Rollback()
	if _, err = freezeBackupNotificationPass(tx, evaluation); err == nil {
		t.Fatal("evaluation identity allowed report substitution")
	}
}

func TestBackupPipelineBatchSurvivesMemberAcceptedBeforeExport(t *testing.T) {
	f := newPipelineFixture(t)
	for i := 0; i < 9; i++ {
		f.observe(t, fmt.Sprintf("member-%d", i), "failure", f.now)
	}
	individuals := f.freeze(t, 1, 1)
	f.transfer(t, f.client)
	testutil.Must(t, coalesceNotificationBacklog(f.state, f.now))
	if got := f.freeze(t, 2, 1); len(got) != 0 {
		t.Fatal("batch duplicated live individual exports")
	}
	f.accept(t, individuals[0])
	for _, m := range individuals[1:] {
		_, err := f.client.Call(context.Background(), notify.Request{Operation: "message.cancel", Producer: m.Producer, EventID: m.ID, Sequence: m.Sequence, PayloadHash: notify.Hash(m)}, nil)
		testutil.Must(t, err)
	}
	f.transfer(t, f.client)
	batches := f.freeze(t, 3, 1)
	if len(batches) != 1 || batches[0].ReportType != "backlog" {
		t.Fatal("partial acceptance prevented fresh batch", batches)
	}
	var members int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_members WHERE event=?", batches[0].ID).Scan(&members))
	if members != 8 {
		t.Fatal("accepted member incorrectly re-exported", members)
	}
	f.transfer(t, f.client)
	f.accept(t, batches[0])
	f.transfer(t, f.client)
	if got := f.freeze(t, 4, 1); len(got) != 0 {
		t.Fatal("settled batch exported again", got)
	}
	var open int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
	if open != 0 {
		t.Fatal("batch lost open members", open)
	}
}

func TestBackupPipelineResolutionInvalidatesFailureOnlyBatch(t *testing.T) {
	f := newPipelineFixture(t)
	f.observe(t, "a", "will recover", f.now)
	for i := 0; i < 8; i++ {
		f.observe(t, fmt.Sprintf("unrelated-%d", i), "still failed", f.now)
	}
	testutil.Must(t, coalesceNotificationBacklog(f.state, f.now))
	batch := f.freeze(t, 1, 1)
	if len(batch) != 1 {
		t.Fatal(batch)
	}
	f.transfer(t, f.client)
	attempt, ok, err := f.gateway.Grant(context.Background(), f.binding.Producer, batch[0].ID)
	testutil.Must(t, err)
	if !ok {
		t.Fatal("batch not granted")
	}
	testutil.Must(t, f.state.resolveIncident("capture", "a", f.now, configfixture.WorkerPolicy(t).Notifications))
	f.transfer(t, f.client)
	next := f.freeze(t, 2, 2)
	var recovery notify.Message
	for _, m := range next {
		if m.Kind == "recovery" {
			recovery = m
		}
	}
	if len(next) != 10 || recovery.ID == "" {
		t.Fatal("resolution lost failure obligations", len(next), recovery)
	}
	f.transfer(t, f.client)
	_, ok, err = f.gateway.Grant(context.Background(), f.binding.Producer, recovery.ID)
	testutil.Must(t, err)
	if ok {
		t.Fatal("recovery overtook in-flight batch")
	}
	testutil.Must(t, f.gateway.Finish(context.Background(), attempt, worker.Result{Accepted: true}))
	f.transfer(t, f.client)
	var eligible int
	testutil.Must(t, f.state.db.QueryRow("SELECT eligible FROM notification_exports WHERE id=?", batch[0].ID).Scan(&eligible))
	if eligible != 0 {
		t.Fatal("obsolete batch satisfied obligations")
	}
	for _, m := range next {
		if m.Kind == "failure" {
			f.accept(t, m)
		}
	}
	f.accept(t, recovery)
	f.transfer(t, f.client)
}
