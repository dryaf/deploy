package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
)

type signalRecoveryFixture struct {
	r               *recoveryReconciler
	source          *relay.RecoveryInbox
	path, relayPath string
	first, unknown  interaction.Inbound
	relayArtifact   string
}

func newSignalRecoveryFixture(t *testing.T) signalRecoveryFixture {
	return newSignalRecoveryFixtureWithRelaySnapshot(t, -1)
}
func newSignalRecoveryFixtureWithRelaySnapshot(t *testing.T, snapshotAt int) signalRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	s, now, path, in := commandFixture(t)
	*now = time.Now().UTC()
	in.SentAt = *now
	account := "11111111-1111-4111-8111-111111111111"
	actor := "22222222-2222-4222-8222-222222222222"
	dest := interaction.Destination{ID: "private", Generation: "one", Provider: "signal", Account: account, NativeID: actor}
	s.config.Interaction.Destinations = []interaction.Destination{dest}
	s.config.Interaction.Permissions[0].Actor = actor
	in.Provider = "signal"
	in.Account = account
	in.Actor = actor
	in.DestinationFingerprint = dest.Fingerprint()
	in.NativeID = "signal-one"
	relayPath := t.TempDir()
	must(t, os.Chmod(relayPath, 0700))
	inbox, err := relay.OpenInbox(relayPath, account, true)
	must(t, err)
	t.Cleanup(func() { inbox.Close() })
	relayArtifact := ""
	snapshotRelay := func() {
		must(t, inbox.Close())
		relayArtifact = filepath.Join(t.TempDir(), "relay-snapshot")
		_, err = relay.SnapshotInbox(ctx, relayPath, account, relayArtifact)
		must(t, err)
		inbox, err = relay.OpenInbox(relayPath, account, false)
		must(t, err)
	}
	if snapshotAt == 0 {
		snapshotRelay()
	}
	p := provider.SignalAccount{Account: account, Socket: "/private/relay.sock", InboxIdentity: inbox.Cursor().Identity}
	cursor := inbox.Cursor()
	add := func(event interaction.SpoolEvent, redact bool) {
		must(t, inbox.Enqueue(ctx, event))
		page, err := inbox.List(ctx, cursor)
		must(t, err)
		must(t, s.PersistSignal(ctx, p, page))
		permissions := s.config.Interaction.Permissions
		if redact {
			s.config.Interaction.Permissions = nil
		}
		must(t, s.ConvertIntake(ctx))
		s.config.Interaction.Permissions = permissions
		must(t, inbox.Ack(ctx, page.Cursor))
		cursor = page.Cursor
		if snapshotAt > 0 && cursor.Position == uint64(snapshotAt) {
			snapshotRelay()
		}
	}
	first := in
	add(interaction.SpoolEvent{Key: in.NativeID, Hash: protocol.Hash(in), Inbound: &in}, false)
	in.NativeID = "signal-help"
	in.Text = "help"
	add(interaction.SpoolEvent{Key: in.NativeID, Hash: protocol.Hash(in), Inbound: &in}, false)
	in.NativeID = "signal-rejected"
	in.Text = "sensitive rejected text"
	add(interaction.SpoolEvent{Key: in.NativeID, Hash: protocol.Hash(in), Inbound: &in}, true)
	add(interaction.SpoolEvent{Key: "unsupported", Hash: protocol.Hash("unsupported envelope")}, false)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(ctx, artifact)
	must(t, err)
	in.NativeID = "signal-after-snapshot"
	in.Text = "backup/storage retry subject=a target=incident-one"
	unknown := in
	add(interaction.SpoolEvent{Key: in.NativeID, Hash: protocol.Hash(in), Inbound: &in}, false)
	// A still-recent replay might already have executed after the saved snapshot.
	v := getCommand(t, s, in.CommandID())
	must(t, s.ImportCommand(ctx, producer, v.Command.ID, protocol.Hash(v.Command)))
	g, err := s.GrantCommand(ctx, producer, v.Command.ID)
	must(t, err)
	if g.ID == "" {
		t.Fatal("fixture missing grant")
	}
	_, err = s.SubmitCommandResult(ctx, producer, interaction.Result{CommandID: v.Command.ID, CommandHash: protocol.Hash(v.Command), Revision: 1, Phase: "execute", State: "succeeded", Scope: v.Command.Scope, Text: "Completed after snapshot"})
	must(t, err)
	in.NativeID = "signal-confirm"
	in.Text = "confirm old-token"
	add(interaction.SpoolEvent{Key: in.NativeID, Hash: protocol.Hash(in), Inbound: &in}, false)
	if _, err = inbox.List(ctx, interaction.SpoolCursor{Identity: cursor.Identity, Account: account, Position: 4}); err == nil {
		t.Fatal("ordinary relay list crossed ACK boundary")
	}
	c := s.config
	must(t, s.Close())
	must(t, inbox.Close())
	must(t, PrepareRecoveryQuarantine(ctx, path, artifact, c))
	session, err := OpenSignalRecoverySession(ctx, path, c)
	must(t, err)
	r := session.(*recoveryReconciler)
	t.Cleanup(func() { r.Close() })
	source, err := relay.OpenRecoveryInbox(relayPath, account)
	must(t, err)
	t.Cleanup(func() { source.Close() })
	return signalRecoveryFixture{r: r, source: source, path: path, relayPath: relayPath, first: first, unknown: unknown, relayArtifact: relayArtifact}
}
func TestSignalRecoveryReadsPastAckWithoutReauthorizingReplay(t *testing.T) {
	f := newSignalRecoveryFixture(t)
	ctx := context.Background()
	summary, err := interaction.ReconcileSignal(ctx, f.source, f.r)
	must(t, err)
	if summary.Manifest.Highest != 6 || summary.Manifest.Acknowledged != 6 || summary.Manifest.Listed != 6 ||
		summary.Mapped != 2 || summary.Rejected != 2 || summary.Unmapped != 2 || summary.IntakeEnabled {
		t.Fatal(summary)
	}
	var commands, pending, events int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM commands").Scan(&commands))
	must(t, f.r.db.QueryRow("SELECT count(*) FROM provider_inbox WHERE state='pending'").Scan(&pending))
	must(t, f.r.db.QueryRow("SELECT count(*) FROM provider_inbox WHERE state='recovery_quarantined'").Scan(&events))
	if commands != 1 || pending != 0 || events != 6 {
		t.Fatal("replayed input became authority", commands, pending, events)
	}
	var id string
	must(t, f.r.db.QueryRow("SELECT command_id FROM command_inbox WHERE request_key=?", f.first.RequestKey()).Scan(&id))
	if id != f.first.CommandID() {
		t.Fatal("original request identity changed")
	}
	var count int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM command_inbox WHERE request_key=?", f.unknown.RequestKey()).Scan(&count))
	if count != 0 {
		t.Fatal("unknown prior command reconstructed by fresh routing")
	}
	var raw string
	var old interaction.SpoolCursor
	must(t, f.r.db.QueryRow("SELECT cursor FROM signal_imports WHERE account=?", f.source.Cursor().Account).Scan(&raw))
	must(t, protocol.Decode([]byte(raw), &old, 4096))
	if old.Position != 4 {
		t.Fatal("operational cursor advanced during recovery")
	}
	if s, err := Open(f.path, f.r.config, false); err == nil {
		s.Close()
		t.Fatal("ordinary worker started")
	}
	prior := protocol.Hash(summary)
	must(t, f.r.Close())
	must(t, f.source.Close())
	session, err := OpenSignalRecoverySession(ctx, f.path, f.r.config)
	must(t, err)
	defer session.Close()
	source, err := relay.OpenRecoveryInbox(f.relayPath, summary.Manifest.Relay.Account)
	must(t, err)
	defer source.Close()
	summary, err = interaction.ReconcileSignal(ctx, source, session)
	must(t, err)
	if protocol.Hash(summary) != prior {
		t.Fatal("completion changed on reopen")
	}
}

type lostSignalRecovery struct {
	interaction.SignalRecoverySession
	stage string
	lost  bool
}

type brokenSignalSource struct {
	interaction.SignalRecoverySource
	mode string
}

func (s brokenSignalSource) Page(ctx context.Context, ch interaction.SignalRecoveryChallenge, after uint64) (interaction.SignalRecoveryPage, error) {
	p, err := s.SignalRecoverySource.Page(ctx, ch, after)
	if err != nil {
		return p, err
	}
	switch s.mode {
	case "empty":
		p.Events = nil
		p.Next = after
		p.Done = false
	case "cursor":
		p.Next++
	case "end":
		p.Done = false
	}
	return p, nil
}
func TestSignalRecoveryRejectsBrokenSourceProgress(t *testing.T) {
	for _, mode := range []string{"empty", "cursor", "end"} {
		t.Run(mode, func(t *testing.T) {
			f := newSignalRecoveryFixture(t)
			if _, err := interaction.ReconcileSignal(context.Background(), brokenSignalSource{f.source, mode}, f.r); err == nil {
				t.Fatal("broken page accepted")
			}
			_, err := interaction.ReconcileSignal(context.Background(), f.source, f.r)
			must(t, err)
		})
	}
}

func (s *lostSignalRecovery) ImportSignal(ctx context.Context, ch interaction.SignalRecoveryChallenge, after uint64, events []interaction.SpoolEvent) error {
	err := s.SignalRecoverySession.ImportSignal(ctx, ch, after, events)
	if err == nil && !s.lost && s.stage == "import" {
		s.lost = true
		return errors.New("lost Signal import response")
	}
	return err
}
func (s *lostSignalRecovery) FinishSignal(ctx context.Context, ch interaction.SignalRecoveryChallenge) (interaction.SignalRecoverySummary, error) {
	v, err := s.SignalRecoverySession.FinishSignal(ctx, ch)
	if err == nil && !s.lost && s.stage == "finish" {
		s.lost = true
		return v, errors.New("lost Signal finish response")
	}
	return v, err
}
func TestSignalRecoveryLostResponses(t *testing.T) {
	for _, stage := range []string{"import", "finish"} {
		t.Run(stage, func(t *testing.T) {
			f := newSignalRecoveryFixture(t)
			ctx := context.Background()
			broken := &lostSignalRecovery{SignalRecoverySession: f.r, stage: stage}
			if _, err := interaction.ReconcileSignal(ctx, f.source, broken); err == nil {
				t.Fatal("response loss not surfaced")
			}
			v, err := interaction.ReconcileSignal(ctx, f.source, f.r)
			must(t, err)
			if v.Unmapped != 2 || v.IntakeEnabled {
				t.Fatal(v)
			}
		})
	}
}
func TestSignalRecoveryRejectsChangedIdentityPagesAndGaps(t *testing.T) {
	f := newSignalRecoveryFixture(t)
	ctx := context.Background()
	wrong := f.source.Cursor()
	wrong.Identity.Recovery = "foreign"
	if _, err := f.r.SignalChallenge(ctx, wrong); err == nil {
		t.Fatal("foreign relay accepted")
	}
	ch, err := f.r.SignalChallenge(ctx, f.source.Cursor())
	must(t, err)
	m, err := f.source.Capture(ctx, ch)
	must(t, err)
	bad := m
	bad.Highest = 3
	bad.Acknowledged = 3
	bad.Listed = 3
	if err = f.r.BeginSignal(ctx, ch, bad); err == nil {
		t.Fatal("relay rollback accepted")
	}
	must(t, f.r.BeginSignal(ctx, ch, m))
	page, err := f.source.Page(ctx, ch, 0)
	must(t, err)
	if _, err = f.r.FinishSignal(ctx, ch); err == nil {
		t.Fatal("missing history completed")
	}
	badCh := ch
	badCh.Nonce = "changed"
	if err = f.r.ImportSignal(ctx, badCh, 0, page.Events); err == nil {
		t.Fatal("changed challenge accepted")
	}
	if err = f.r.ImportSignal(ctx, ch, 1, page.Events); err == nil {
		t.Fatal("position mismatch accepted")
	}
	page.Events[len(page.Events)-1].Position = 4
	if err = f.r.ImportSignal(ctx, ch, 0, page.Events); err == nil {
		t.Fatal("non-atomic invalid page accepted")
	}
	var n int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_signal_events").Scan(&n))
	if n != 0 {
		t.Fatal("partial page committed")
	}
	page, err = f.source.Page(ctx, ch, 0)
	must(t, err)
	must(t, f.r.ImportSignal(ctx, ch, 0, page.Events))
	page.Events[0].Inbound.Text = "changed command"
	if err = f.r.ImportSignal(ctx, ch, 0, page.Events); err == nil {
		t.Fatal("changed replay accepted")
	}
	_, err = f.r.FinishSignal(ctx, ch)
	must(t, err)
}

func TestSignalRecoveryLeaseRenewalAndRollbackFence(t *testing.T) {
	f := newSignalRecoveryFixture(t)
	ctx := context.Background()
	ch, err := f.r.SignalChallenge(ctx, f.source.Cursor())
	must(t, err)
	m, err := f.source.Capture(ctx, ch)
	must(t, err)
	must(t, f.r.BeginSignal(ctx, ch, m))
	page, err := f.source.Page(ctx, ch, 0)
	must(t, err)
	f.r.clock = func() (string, float64, error) { return ch.BootID, ch.Uptime + 601, nil }
	if err = f.r.ImportSignal(ctx, ch, 0, page.Events); err == nil {
		t.Fatal("monotonic expiry ignored")
	}
	renewed, err := f.r.SignalChallenge(ctx, f.source.Cursor())
	must(t, err)
	if renewed.Nonce == ch.Nonce {
		t.Fatal("expired nonce renewed in place")
	}
	if err = f.r.ImportSignal(ctx, ch, 0, page.Events); err == nil {
		t.Fatal("old nonce accepted")
	}
	must(t, f.r.BeginSignal(ctx, renewed, m))
	must(t, f.r.ImportSignal(ctx, renewed, 0, page.Events))
	f.r.clock = func() (string, float64, error) { return "different-boot", 1, nil }
	if _, err = f.r.FinishSignal(ctx, renewed); err == nil {
		t.Fatal("pre-reboot lease accepted")
	}
	renewed, err = f.r.SignalChallenge(ctx, f.source.Cursor())
	must(t, err)
	must(t, f.r.BeginSignal(ctx, renewed, m))
	_, err = f.r.FinishSignal(ctx, renewed)
	must(t, err)
}
func TestSignalRecoveryLostCompletionCannotRestart(t *testing.T) {
	f := newSignalRecoveryFixture(t)
	ctx := context.Background()
	_, err := interaction.ReconcileSignal(ctx, f.source, f.r)
	must(t, err)
	_, err = f.r.db.Exec("UPDATE reconciliation_signal_sessions SET body=json_remove(body,'$.Summary')")
	must(t, err)
	if _, err = interaction.ReconcileSignal(ctx, f.source, f.r); err == nil {
		t.Fatal("gateway completion loss silently restarted")
	}
}
func TestSignalRecoveryRejectsDigestAndMissingSchema(t *testing.T) {
	f := newSignalRecoveryFixture(t)
	ctx := context.Background()
	ch, err := f.r.SignalChallenge(ctx, f.source.Cursor())
	must(t, err)
	m, err := f.source.Capture(ctx, ch)
	must(t, err)
	m.Digest = protocol.Hash("wrong manifest")
	must(t, f.r.BeginSignal(ctx, ch, m))
	page, err := f.source.Page(ctx, ch, 0)
	must(t, err)
	must(t, f.r.ImportSignal(ctx, ch, 0, page.Events))
	if _, err = f.r.FinishSignal(ctx, ch); err == nil {
		t.Fatal("digest mismatch completed")
	}
	_, err = f.r.db.Exec("DROP TABLE reconciliation_signal_events")
	must(t, err)
	must(t, f.r.Close())
	if s, err := OpenSignalRecoverySession(ctx, f.path, f.r.config); err == nil {
		s.Close()
		t.Fatal("lost evidence recreated")
	}
}

type signalRecoveryCrashInput struct {
	WorkerPath, RelayPath, Account string
	Config                         Config
	Restored                       bool
}

func openSignalCrashSource(ctx context.Context, input signalRecoveryCrashInput) (interaction.SignalRecoverySource, error) {
	if input.Restored {
		return relay.OpenRestoredRecoveryInbox(ctx, input.RelayPath, input.Account)
	}
	return relay.OpenRecoveryInbox(input.RelayPath, input.Account)
}

type crashingSignalSession struct{ *recoveryReconciler }

func (s crashingSignalSession) ImportSignal(ctx context.Context, ch interaction.SignalRecoveryChallenge, after uint64, events []interaction.SpoolEvent) error {
	s.fault = func(stage string) error {
		if stage == os.Getenv("SIGNAL_IMPORT_CRASH_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	return s.recoveryReconciler.ImportSignal(ctx, ch, after, events)
}
func TestSignalRecoveryImportCrashChild(t *testing.T) {
	path := os.Getenv("SIGNAL_IMPORT_CRASH_INPUT")
	if path == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(path)
	must(t, err)
	var input signalRecoveryCrashInput
	must(t, json.Unmarshal(raw, &input))
	session, err := OpenSignalRecoverySession(context.Background(), input.WorkerPath, input.Config)
	must(t, err)
	defer session.Close()
	source, err := openSignalCrashSource(context.Background(), input)
	must(t, err)
	defer source.Close()
	_, err = interaction.ReconcileSignal(context.Background(), source, crashingSignalSession{session.(*recoveryReconciler)})
	must(t, err)
	t.Fatal("fault not reached")
}
func TestSignalRecoveryImportSIGKILL(t *testing.T) {
	testSignalRecoveryImportSIGKILL(t, false)
}
func TestRestoredSignalRecoveryImportSIGKILL(t *testing.T) {
	testSignalRecoveryImportSIGKILL(t, true)
}
func testSignalRecoveryImportSIGKILL(t *testing.T, restored bool) {
	for _, stage := range []string{"reconciliation_transaction", "reconciliation_commit"} {
		t.Run(stage, func(t *testing.T) {
			snapshotAt, expected := -1, 6
			if restored {
				snapshotAt, expected = 2, 2
			}
			f := newSignalRecoveryFixtureWithRelaySnapshot(t, snapshotAt)
			if restored {
				source, _ := prepareRestoredSignalSource(t, f)
				must(t, source.Close())
			}
			input := signalRecoveryCrashInput{WorkerPath: f.path, RelayPath: f.relayPath, Account: f.source.Cursor().Account, Config: f.r.config, Restored: restored}
			must(t, f.source.Close())
			must(t, f.r.Close())
			raw, err := json.Marshal(input)
			must(t, err)
			path := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(path, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalRecoveryImportCrashChild$")
			cmd.Env = append(os.Environ(), "SIGNAL_IMPORT_CRASH_INPUT="+path, "SIGNAL_IMPORT_CRASH_STAGE="+stage)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			must(t, cmd.Start())
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			})
			scan := bufio.NewScanner(pipe)
			if !scan.Scan() || scan.Text() != "READY" {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("crash hook missing", stderr.String())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			session, err := OpenSignalRecoverySession(ctx, input.WorkerPath, input.Config)
			must(t, err)
			defer session.Close()
			source, err := openSignalCrashSource(ctx, input)
			must(t, err)
			defer source.Close()
			r := session.(*recoveryReconciler)
			var count int
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_signal_events").Scan(&count))
			if stage == "reconciliation_transaction" && count != 0 {
				t.Fatal("partial import committed", count)
			}
			if stage == "reconciliation_commit" && count != expected {
				t.Fatal("committed import lost", count)
			}
			summary, err := interaction.ReconcileSignal(ctx, source, session)
			must(t, err)
			if summary.Mapped != 2 || summary.IntakeEnabled || summary.HistoryComplete {
				t.Fatal(summary)
			}
			if restored {
				if summary.Unmapped != 0 || summary.Rejected != 0 || summary.MissingRelayFrom != 3 || summary.MissingRelayThrough != 4 {
					t.Fatal(summary)
				}
			} else if summary.Unmapped != 2 || summary.Rejected != 2 {
				t.Fatal(summary)
			}
		})
	}
}
