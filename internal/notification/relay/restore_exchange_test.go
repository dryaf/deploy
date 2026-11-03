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
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

func restoredExchangeFixture(t *testing.T, n int) (*RestoredRecoveryInbox, string, interaction.SignalRecoveryChallenge) {
	t.Helper()
	ctx := context.Background()
	old, path, ch := relayRecoveryFixture(t, n)
	if _, err := old.Capture(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "snapshot")
	if _, err := SnapshotInbox(ctx, path, fixtureAccount, artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareInboxRestore(ctx, path, fixtureAccount, artifact); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRestoredRecoveryInbox(ctx, path, fixtureAccount)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, path, ch
}

func TestRestoredRelayExchangeBoundsAndAuthority(t *testing.T) {
	r, path, ch := restoredExchangeFixture(t, 150)
	ctx := context.Background()
	if other, err := OpenRestoredRecoveryInbox(ctx, path, fixtureAccount); err == nil {
		other.Close()
		t.Fatal("concurrent source opened")
	}
	m, err := r.Capture(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	if m.Highest != 150 || m.Acknowledged != 150 || m.Listed != 150 ||
		m.Restore.After != r.journal.After || m.Restore.SnapshotHash != r.journal.Snapshot.DatabaseHash {
		t.Fatal(m)
	}
	var after uint64
	var hashes []string
	pages := 0
	for {
		p, err := r.Page(ctx, ch, after)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(p)
		if len(raw) > protocol.MaxRequest || len(p.Events) > 128 {
			t.Fatal("oversized page")
		}
		for _, e := range p.Events {
			hashes = append(hashes, protocol.Hash(e))
		}
		pages++
		if p.Done {
			break
		}
		if p.Next <= after {
			t.Fatal("page did not advance")
		}
		after = p.Next
	}
	if pages < 2 || interaction.RecoveryEvidenceDigest(hashes) != m.Digest {
		t.Fatal("history mismatch", pages)
	}
	bad := ch
	bad.Operation, err = safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Capture(ctx, bad); err == nil {
		t.Fatal("gateway operation changed")
	}
	bad = ch
	bad.Nonce, err = safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	bad.IssuedAt = time.Now().UTC()
	bad.ExpiresAt = bad.IssuedAt.Add(10 * time.Minute)
	bad.Uptime++
	renewed, err := r.Capture(ctx, bad)
	if err != nil || !renewed.Equal(m) {
		t.Fatal("renewal changed evidence", err)
	}
	if _, err = r.Page(ctx, ch, 0); err == nil {
		t.Fatal("old nonce accepted")
	}
	if _, err = r.Page(ctx, bad, m.Highest+1); err == nil {
		t.Fatal("invalid page cursor accepted")
	}
	summary := interaction.SignalRecoverySummary{Challenge: bad, Manifest: m, Unmapped: 150, GatewayHighest: 152, MissingRelayFrom: 151, MissingRelayThrough: 152}
	wrong := summary
	wrong.MissingRelayThrough++
	if err = r.RecordSummary(ctx, wrong); err == nil {
		t.Fatal("invalid missing range accepted")
	}
	wrong = summary
	wrong.HistoryComplete = true
	if err = r.RecordSummary(ctx, wrong); err == nil {
		t.Fatal("invented completeness accepted")
	}
	wrong = summary
	wrong.IntakeEnabled = true
	if err = r.RecordSummary(ctx, wrong); err == nil {
		t.Fatal("intake enabled")
	}
	if err = r.RecordSummary(ctx, summary); err != nil {
		t.Fatal(err)
	}
	wrong = summary
	wrong.Mapped++
	wrong.Unmapped--
	if err = r.RecordSummary(ctx, wrong); err == nil {
		t.Fatal("completed disposition changed")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = OpenRestoredRecoveryInbox(ctx, path, fixtureAccount)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	done, err := r.Completed(ctx, bad)
	if err != nil || !done {
		t.Fatal("completion lost", err)
	}
	if err = r.RecordSummary(ctx, summary); err != nil {
		t.Fatal(err)
	}
}

func TestRestoredRelayExchangeDetectsLostAuthority(t *testing.T) {
	for _, mode := range []string{"export_missing", "export_corrupt", "source", "candidate", "sidecar", "pin", "journal", "lock", "lock_fifo"} {
		t.Run(mode, func(t *testing.T) {
			r, path, ch := restoredExchangeFixture(t, 3)
			ctx := context.Background()
			if _, err := r.Capture(ctx, ch); err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(path, "restore", r.journal.Operation)
			var err error
			switch mode {
			case "export_missing":
				err = os.Remove(filepath.Join(stage, "gateway-export.json"))
			case "export_corrupt":
				err = os.WriteFile(filepath.Join(stage, "gateway-export.json"), []byte("{"), 0600)
			case "source":
				err = os.WriteFile(filepath.Join(stage, "source.db"), []byte("damaged"), 0600)
			case "candidate":
				err = os.WriteFile(filepath.Join(stage, "candidate.db"), []byte("damaged"), 0600)
			case "sidecar":
				err = os.WriteFile(filepath.Join(stage, "source.db-wal"), []byte("unbound"), 0600)
			case "pin":
				err = os.Remove(filepath.Join(path, "identity.json"))
			case "journal":
				err = os.Remove(filepath.Join(path, "restore.json"))
			case "lock", "lock_fifo":
				err = os.Remove(filepath.Join(path, "inbox.lock"))
				if err == nil {
					if mode == "lock_fifo" {
						err = unix.Mkfifo(filepath.Join(path, "inbox.lock"), 0600)
					} else {
						err = os.WriteFile(filepath.Join(path, "inbox.lock"), nil, 0600)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.Page(ctx, ch, 0); err == nil {
				t.Fatal("changed evidence read")
			}
			if _, err = r.Capture(ctx, ch); err == nil {
				t.Fatal("missing evidence recreated")
			}
			if _, err = r.Completed(ctx, ch); err == nil {
				t.Fatal("missing evidence treated as incomplete")
			}
			if mode == "lock_fifo" {
				r.Close()
				if reopened, err := OpenRestoredRecoveryInbox(ctx, path, fixtureAccount); err == nil {
					reopened.Close()
					t.Fatal("FIFO lease accepted")
				}
			}
		})
	}
}

type restoredExchangeCrashInput struct {
	Path      string
	Challenge interaction.SignalRecoveryChallenge
}

func TestRestoredRelayExchangeCompletionRollback(t *testing.T) {
	r, path, ch := restoredExchangeFixture(t, 3)
	ctx := context.Background()
	m, err := r.Capture(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	exportPath := filepath.Join(path, "restore", r.journal.Operation, "gateway-export.json")
	before, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	s := interaction.SignalRecoverySummary{Challenge: ch, Manifest: m, Unmapped: m.Highest}
	if err = r.RecordSummary(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(exportPath, before, 0600); err != nil {
		t.Fatal(err)
	}
	r, err = OpenRestoredRecoveryInbox(ctx, path, fixtureAccount)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = r.Completed(ctx, ch); err == nil {
		t.Fatal("rolled-back completion treated as unfinished")
	}
	if _, err = r.Capture(ctx, ch); err == nil {
		t.Fatal("rolled-back completion recaptured")
	}
	if err = r.RecordSummary(ctx, s); err == nil {
		t.Fatal("lost source completion silently recreated")
	}
}

func TestRestoredRelayExchangeCrashChild(t *testing.T) {
	path := os.Getenv("RESTORED_RELAY_EXCHANGE_INPUT")
	if path == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input restoredExchangeCrashInput
	if err = json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := OpenRestoredRecoveryInbox(ctx, input.Path, fixtureAccount)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.fault = func(stage string) error {
		if stage == os.Getenv("RESTORED_RELAY_EXCHANGE_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	m, err := r.Capture(ctx, input.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.RecordSummary(ctx, interaction.SignalRecoverySummary{Challenge: input.Challenge, Manifest: m, Unmapped: m.Highest}); err != nil {
		t.Fatal(err)
	}
	t.Fatal("fault not reached")
}
func TestRestoredRelayExchangeSIGKILL(t *testing.T) {
	for _, stage := range []string{"relay_restore_export_file", "relay_restore_export_pin", "relay_restore_summary_before", "relay_restore_summary_file", "relay_restore_summary_after"} {
		t.Run(stage, func(t *testing.T) {
			r, path, ch := restoredExchangeFixture(t, 3)
			before := r.journal
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(restoredExchangeCrashInput{path, ch})
			if err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(t.TempDir(), "input.json")
			if err = os.WriteFile(input, raw, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRestoredRelayExchangeCrashChild$")
			cmd.Env = append(os.Environ(), "RESTORED_RELAY_EXCHANGE_INPUT="+input, "RESTORED_RELAY_EXCHANGE_STAGE="+stage)
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
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
				t.Fatal("fault missing", stderr.String())
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			r, err = OpenRestoredRecoveryInbox(ctx, path, fixtureAccount)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			done, err := r.Completed(ctx, ch)
			if err != nil || done != (stage == "relay_restore_summary_file" || stage == "relay_restore_summary_after") {
				t.Fatal(done, err)
			}
			m, err := r.Capture(ctx, ch)
			if err != nil {
				t.Fatal(err)
			}
			s := interaction.SignalRecoverySummary{Challenge: ch, Manifest: m, Unmapped: m.Highest}
			if err = r.RecordSummary(ctx, s); err != nil {
				t.Fatal(err)
			}
			if r.journal.After != before.After || r.journal.CandidateHash != before.CandidateHash || r.journal.GatewayExport != ch.Operation || r.journal.GatewaySummary != protocol.Hash(s) {
				t.Fatal("restore identity or candidate changed")
			}
		})
	}
}
