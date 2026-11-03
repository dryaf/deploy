package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func relayRecoveryFixture(t *testing.T, n int) (*RecoveryInbox, string, interaction.SignalRecoveryChallenge) {
	t.Helper()
	s, path := inboxFixture(t)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		in := interaction.Inbound{Provider: "signal", Account: fixtureAccount, NativeID: fmt.Sprintf("native-%d", i), Actor: fixtureActor, Conversation: "private", DestinationFingerprint: protocol.Hash("destination"), SentAt: time.Now().UTC(), Text: strings.Repeat("x", 4096)}
		if err := s.Enqueue(ctx, interaction.SpoolEvent{Key: in.NativeID, Hash: protocol.Hash(in), Inbound: &in}); err != nil {
			t.Fatal(err)
		}
	}
	c := s.Cursor()
	for {
		page, err := s.List(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) == 0 {
			break
		}
		if err = s.Ack(ctx, page.Cursor); err != nil {
			t.Fatal(err)
		}
		c = page.Cursor
	}
	if r, err := OpenRecoveryInbox(path, fixtureAccount); err == nil {
		r.Close()
		t.Fatal("live relay lease ignored")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRecoveryInbox(path, fixtureAccount)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	op, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ch := interaction.SignalRecoveryChallenge{Operation: op, Gateway: protocol.Identity{Worker: "gateway", Recovery: "recovery"}, Relay: r.Cursor(), Nonce: nonce, IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute), BootID: "gateway-on-another-host", Uptime: 100}
	return r, path, ch
}
func TestSignalRecoveryExportBoundsAndWatermarks(t *testing.T) {
	r, path, ch := relayRecoveryFixture(t, 150)
	ctx := context.Background()
	m, err := r.Capture(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	if m.Highest != 150 || m.Acknowledged != 150 || m.Listed != 150 {
		t.Fatal(m)
	}
	var after uint64
	var hashes []string
	pages := 0
	for {
		page, err := r.Page(ctx, ch, after)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(page)
		if len(raw) > protocol.MaxRequest || len(page.Events) > 128 {
			t.Fatal("unbounded page")
		}
		for _, e := range page.Events {
			if e.Position != uint64(len(hashes))+1 {
				t.Fatal("missing event")
			}
			hashes = append(hashes, protocol.Hash(e))
		}
		pages++
		if page.Done {
			break
		}
		if page.Next <= after {
			t.Fatal("no page progress")
		}
		after = page.Next
	}
	if pages < 2 || interaction.RecoveryEvidenceDigest(hashes) != m.Digest {
		t.Fatal("bad export digest")
	}
	summary := interaction.SignalRecoverySummary{Challenge: ch, Manifest: m, Unmapped: 150}
	if err = r.RecordSummary(ctx, summary); err != nil {
		t.Fatal(err)
	}
	if done, err := r.Completed(ctx, ch); err != nil || !done {
		t.Fatal(done, err)
	}
	summary.Mapped = 1
	if r.RecordSummary(ctx, summary) == nil {
		t.Fatal("changed summary accepted")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenInbox(path, fixtureAccount, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var ack, listed uint64
	if err = s.db.QueryRow("SELECT acked,listed FROM identity").Scan(&ack, &listed); err != nil {
		t.Fatal(err)
	}
	if ack != m.Acknowledged || listed != m.Listed {
		t.Fatal("export moved operational watermarks")
	}
}
func TestSignalRecoveryEmptyExport(t *testing.T) {
	r, _, ch := relayRecoveryFixture(t, 0)
	ctx := context.Background()
	m, err := r.Capture(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	if m.Highest != 0 || m.Digest != interaction.RecoveryEvidenceDigest(nil) {
		t.Fatal(m)
	}
	p, err := r.Page(ctx, ch, 0)
	if err != nil || !p.Done || p.Next != 0 || len(p.Events) != 0 {
		t.Fatal(p, err)
	}
	if err = r.RecordSummary(ctx, interaction.SignalRecoverySummary{Challenge: ch, Manifest: m}); err != nil {
		t.Fatal(err)
	}
}
func TestSignalRecoveryExportDetectsDamage(t *testing.T) {
	for _, kind := range []string{"gap", "deleted_tail", "changed_body", "deleted_reference", "expired", "wrong_relay", "missing_schema"} {
		t.Run(kind, func(t *testing.T) {
			r, path, ch := relayRecoveryFixture(t, 3)
			ctx := context.Background()
			switch kind {
			case "gap", "deleted_tail":
				pos := 2
				if kind == "deleted_tail" {
					pos = 3
				}
				if _, err := r.inbox.db.Exec("DELETE FROM events WHERE position=?", pos); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Capture(ctx, ch); err == nil {
					t.Fatal("missing retained event accepted")
				}
				return
			case "missing_schema":
				if _, err := r.inbox.db.Exec("DROP TABLE recovery_exports;DROP TABLE recovery_records;"); err != nil {
					t.Fatal(err)
				}
				r.Close()
				if opened, err := OpenRecoveryInbox(path, fixtureAccount); err == nil {
					opened.Close()
					t.Fatal("lost export schema recreated")
				}
				return
			}
			if _, err := r.Capture(ctx, ch); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "changed_body":
				if _, err := r.inbox.db.Exec("UPDATE events SET body=json_set(body,'$.inbound.text','changed') WHERE position=2"); err != nil {
					t.Fatal(err)
				}
			case "deleted_reference":
				if _, err := r.inbox.db.Exec("DELETE FROM recovery_records WHERE position=2"); err != nil {
					t.Fatal(err)
				}
			case "expired":
				ch.IssuedAt = ch.IssuedAt.Add(-time.Hour)
				ch.ExpiresAt = ch.ExpiresAt.Add(-time.Hour)
			case "wrong_relay":
				ch.Relay.Identity.Recovery = "foreign"
			}
			if _, err := r.Page(ctx, ch, 0); err == nil {
				t.Fatal("damaged/invalid export accepted")
			}
		})
	}
}
func TestSignalInboxMissingLockNeverRecreated(t *testing.T) {
	s, path := inboxFixture(t)
	if err := os.Remove(filepath.Join(path, "inbox.lock")); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		if opened, err := OpenInbox(path, fixtureAccount, create); err == nil {
			opened.Close()
			t.Fatal("missing stable lock accepted")
		}
		if _, err := os.Stat(filepath.Join(path, "inbox.lock")); !os.IsNotExist(err) {
			t.Fatal("replacement lock created", err)
		}
	}
	if err := s.Enqueue(context.Background(), interaction.SpoolEvent{Key: "one", Hash: protocol.Hash("one")}); err == nil {
		t.Fatal("old handle survived missing lock")
	}
}

func TestSignalRecoveryCaptureCrashChild(t *testing.T) {
	path := os.Getenv("SIGNAL_CAPTURE_CRASH_PATH")
	if path == "" {
		t.Skip("subprocess")
	}
	var ch interaction.SignalRecoveryChallenge
	if err := json.Unmarshal([]byte(os.Getenv("SIGNAL_CAPTURE_CRASH_CHALLENGE")), &ch); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRecoveryInbox(path, fixtureAccount)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.fault = func(stage string) error {
		if stage == os.Getenv("SIGNAL_CAPTURE_CRASH_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	if _, err = r.Capture(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	t.Fatal("fault not reached")
}
func TestSignalRecoveryCaptureSIGKILL(t *testing.T) {
	for _, stage := range []string{"relay_capture_transaction", "relay_capture_commit"} {
		t.Run(stage, func(t *testing.T) {
			r, path, ch := relayRecoveryFixture(t, 3)
			r.Close()
			raw, _ := json.Marshal(ch)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalRecoveryCaptureCrashChild$")
			cmd.Env = append(os.Environ(), "SIGNAL_CAPTURE_CRASH_PATH="+path, "SIGNAL_CAPTURE_CRASH_CHALLENGE="+string(raw), "SIGNAL_CAPTURE_CRASH_STAGE="+stage)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			scan := bufio.NewScanner(stdout)
			if !scan.Scan() || scan.Text() != "READY" {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("crash hook missing", stderr.String())
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			r, err = OpenRecoveryInbox(path, fixtureAccount)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			var exports, records int
			if err = r.inbox.db.QueryRow("SELECT count(*) FROM recovery_exports").Scan(&exports); err != nil {
				t.Fatal(err)
			}
			if err = r.inbox.db.QueryRow("SELECT count(*) FROM recovery_records").Scan(&records); err != nil {
				t.Fatal(err)
			}
			if stage == "relay_capture_transaction" && (exports != 0 || records != 0) {
				t.Fatal("partial capture committed", exports, records)
			}
			if stage == "relay_capture_commit" && (exports != 1 || records != 3) {
				t.Fatal("committed capture lost", exports, records)
			}
			m, err := r.Capture(ctx, ch)
			if err != nil {
				t.Fatal(err)
			}
			page, err := r.Page(ctx, ch, 0)
			if err != nil || !page.Done || len(page.Events) != 3 || m.Highest != 3 {
				t.Fatal(page, m, err)
			}
		})
	}
}
