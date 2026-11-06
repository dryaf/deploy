package install

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

// Restore a gateway from before the domain commit, preserving the specialist's
// independent database. Exercise publication and explicit managed reactivation,
// rather than borrowing the recovery candidate as an ordinary runtime.
func TestManagedCommandRecoveryCannotRepeatCommittedEffect(t *testing.T) {
	testManagedCommandRecoveryEffect(t, false)
}
func TestManagedCommandRecoveryBeforeConversionAndAliasMigration(t *testing.T) {
	testManagedCommandRecoveryEffect(t, true)
}
func testManagedCommandRecoveryEffect(t *testing.T, migrate bool) {
	ctx := context.Background()
	spec, i, c, active, record := managedCommandsFixture(t)
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.recoveryProbe, i.deliveryRecoveryProbe, i.telegramHandoffProbe, i.telegramProbe, i.commandRecoveryProbe = nop, nop, nop, nop, nop
	i.telegramTransport = telegramCheckTransport{}
	i.commandPolicyProbe = nop
	enabled, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "all", false)
	check(err)
	s, err := openManagedStore(enabled, c)
	check(err)
	check(s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
	check(s.PersistTelegram(ctx, provider.IntakePage{Account: c.Telegram.Account}))
	p := c.Worker.Producers[0].Producer
	d := c.Worker.Interaction.Destinations[0]
	in := interaction.Inbound{Provider: "telegram", Account: "123", NativeID: "committed-effect", Actor: "789", Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Now().UTC(), Text: "worker/local retry subject=a target=incident-one"}
	snapshot := filepath.Join(t.TempDir(), "before-effect")
	var manifest worker.SnapshotManifest
	if migrate {
		in.NativeID = "update-1"
		check(s.PersistTelegram(ctx, provider.IntakePage{Account: in.Account, Next: 2, Events: []provider.IntakeEvent{{ID: 1, Hash: protocol.Hash(in), Inbound: &in}}}))
		manifest, err = s.Snapshot(ctx, snapshot)
		check(err)
		check(s.ConvertIntake(ctx))
	}
	first, err := s.AdmitCommand(ctx, in)
	check(err)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "domain.db")+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	check(err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	ledger, err := interaction.OpenDomainLedger(ctx, db, p, enabled.Identity, true, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE domain_calls(value INTEGER); INSERT INTO domain_calls VALUES(0)")
		return err
	})
	check(err)
	check(ledger.Import(ctx, first.Command))
	check(s.ImportCommand(ctx, p, first.Command.ID, protocol.Hash(first.Command)))
	grant, err := s.GrantCommand(ctx, p, first.Command.ID)
	check(err)
	if grant.ID == "" {
		t.Fatal("missing mutation grant")
	}
	if !migrate {
		manifest, err = s.Snapshot(ctx, snapshot)
		check(err)
	}
	result, err := ledger.Execute(ctx, first.Command, grant, "", time.Now().UTC(), func(tx *sql.Tx, command interaction.Command) (interaction.Result, error) {
		_, err := tx.Exec("UPDATE domain_calls SET value=value+1")
		return interaction.Result{State: "succeeded", Scope: command.Scope, Text: "Applied once"}, err
	})
	check(err)
	receipt, err := s.SubmitCommandResult(ctx, p, result)
	check(err)
	check(ledger.ImportReceipt(ctx, receipt))
	check(s.AckCommandResult(ctx, p, receipt))
	check(ledger.Acknowledged(ctx, receipt))
	pendingInput := in
	pendingInput.NativeID, pendingInput.Text = "post-snapshot-pending", "worker/local retry subject=a target=incident-two"
	pending, err := s.AdmitCommand(ctx, pendingInput)
	check(err)
	check(ledger.Import(ctx, pending.Command))
	check(s.Close())
	drained, err := i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(enabled), "", "", false)
	check(err)
	if migrate {
		var next interaction.Config
		raw, e := json.Marshal(c.Worker.Interaction)
		check(e)
		check(json.Unmarshal(raw, &next))
		next.Revision = "renamed-after-snapshot"
		next.Workers[0].Alias = "worker/renamed"
		raw, e = json.Marshal(next)
		check(e)
		drained, err = i.prepareCommands(ctx, "migrate-commands", spec.Root, protocol.Hash(drained), "", "", false, string(raw))
		check(err)
	}
	_, err = i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(drained), "", false)
	check(err)
	check(i.services.Stop(ctx))
	publication := publishManagedRecoveryFromUsing(t, spec, snapshot, &manifest, func(ctx context.Context, path string, cfg worker.Config) {
		r, err := worker.OpenRecoveryGenerationSession(ctx, path, cfg)
		check(err)
		session := r.(protocol.RecoverySession)
		challenge, err := session.Challenge(ctx, p)
		check(err)
		check(session.Begin(ctx, challenge, protocol.RecoveryManifest{ImportedReceipts: protocol.Cursor{Identity: enabled.Identity, Producer: p}}))
		_, err = session.Finish(ctx, challenge, protocol.Snapshot{Producer: p, Sequence: 1, BasisRevision: 1, ConfigRevision: "recovered", EvaluatedAt: challenge.IssuedAt, Subjects: []protocol.Subject{{Key: "a", Condition: "ok", Reason: "synthetic recovery", ValidUntil: challenge.IssuedAt.Add(time.Minute)}}})
		check(err)
		_, err = r.(worker.RecoveryOwnershipSession).PrepareRecoveryOwnership(ctx, p)
		check(err)
		_, err = r.PrepareRecoveryGeneration(ctx, p)
		check(err)
		check(r.Close())
		commands, err := worker.OpenCommandRecoveryInstallSession(ctx, path, cfg)
		check(err)
		defer commands.Close()
		summary, err := interaction.ReconcileCommands(ctx, ledger, commands)
		check(err)
		if summary.Terminal != 1 || summary.Uncertain != 1 || summary.ExecutionEnabled {
			t.Fatal("wrong command recovery summary", summary)
		}
		_, err = interaction.CommitCommandRecovery(ctx, ledger, commands)
		check(err)
		_, err = interaction.InstallCommandRecovery(ctx, ledger, commands)
		check(err)
	})
	spec.RecoveryCommitHash = publication.Commit.CommitHash
	after, err := i.prepare(ctx, "adopt-recovery", spec)
	check(err)
	if after.Commands != nil || after.Mode != "verification" {
		t.Fatal("adoption enabled commands", after)
	}
	after, err = i.prepareTelegramRenewal(ctx, spec.Root)
	check(err)
	renewal := managedTelegramReceiptForTest(t, after)
	if delay := time.Until(renewal.Renewal.Cursor.NotBefore.Add(time.Millisecond)); delay > 0 {
		time.Sleep(delay)
	}
	record = freshDeliveryQualification(t, spec.Root)
	active, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(after), protocol.Hash(record), false)
	check(err)
	enabled, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "all", false)
	check(err)
	for restart := 0; restart < 2; restart++ {
		s, err = openManagedStore(enabled, c)
		check(err)
		check(s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
		// The recovery cursor rejects old transport events; direct retained request
		// lookup must also never produce a new executable identity.
		for _, input := range []interaction.Inbound{in, pendingInput} {
			want := first.Command
			if input.RequestKey() == pendingInput.RequestKey() {
				want = pending.Command
			}
			v, e := s.AdmitCommand(ctx, input)
			if e == nil && (protocol.Hash(v.Command) != protocol.Hash(want) || v.Grant != nil || v.Prepared != nil || (v.State != "succeeded" && v.State != "outcome_unknown")) {
				t.Fatal("old request revived", v)
			}
		}
		for n, command := range []interaction.Command{first.Command, pending.Command} {
			raw, err := json.Marshal(map[string]string{"id": command.ID})
			check(err)
			reply := s.Handle(ctx, protocol.Request{Version: protocol.Version, Operation: "command.get", Expected: s.Identity(), Producer: p, Interaction: raw})
			if !reply.OK {
				t.Fatal("retained outcome unavailable after managed activation", reply.Error)
			}
			var retained interaction.View
			check(protocol.Decode(reply.Result, &retained, protocol.MaxRequest))
			if protocol.Hash(retained.Command) != protocol.Hash(command) || retained.State != []string{"succeeded", "outcome_unknown"}[n] || retained.Grant != nil || retained.Prepared != nil {
				t.Fatal("retained request/outcome changed", retained)
			}
			if _, err = s.GrantCommand(ctx, p, command.ID); err == nil {
				t.Fatal("recovered history granted execution")
			}
			if err = s.ImportCommand(ctx, p, command.ID, protocol.Hash(command)); err == nil {
				t.Fatal("historical command admitted anew")
			}
		}
		if _, err = s.SubmitCommandResult(ctx, p, result); err == nil {
			t.Fatal("old result minted a new receipt")
		}
		queued, e := s.Commands(ctx, p)
		check(e)
		if len(queued) != 0 {
			t.Fatal("historical commands queued", queued)
		}
		if migrate {
			fresh := in
			fresh.NativeID = "after-recovery-alias"
			fresh.Text = "worker/renamed status"
			fresh.SentAt = time.Now().UTC()
			if restart == 1 {
				v, e := s.AdmitCommand(ctx, fresh)
				check(e)
				if v.Command.Producer != p {
					t.Fatal("renamed alias redirected producer")
				}
			}
		}
		check(s.Close())
	}
	if _, err = ledger.Execute(ctx, first.Command, grant, "", time.Now().UTC(), func(*sql.Tx, interaction.Command) (interaction.Result, error) {
		t.Fatal("domain effect repeated")
		return interaction.Result{}, nil
	}); err == nil {
		t.Fatal("old specialist handle bypassed recovery fence")
	}
	var calls int
	check(db.QueryRow("SELECT value FROM domain_calls").Scan(&calls))
	if calls != 1 {
		t.Fatal("wrong domain effect count", calls)
	}
	var retained, acknowledged int
	check(db.QueryRow("SELECT count(*),sum(acked) FROM interactive_results WHERE id=?", first.Command.ID).Scan(&retained, &acknowledged))
	if retained != 1 || acknowledged != 1 {
		t.Fatal("original result provenance lost", retained, acknowledged)
	}
}
