package worker

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

type fakeTelegramDrain struct {
	Account                     string
	Events                      []provider.IntakeEvent
	Ack                         int64
	Offsets                     []int64
	Batch                       int
	File                        string
	FailPreflight, LoseResponse bool
	BeforePoll                  func(int64) error `json:"-"`
}

func (p *fakeTelegramDrain) Preflight(context.Context) error {
	if p.FailPreflight {
		return errors.New("provider conflict")
	}
	return nil
}
func (p *fakeTelegramDrain) Poll(ctx context.Context, offset int64) (page provider.IntakePage, err error) {
	if p.BeforePoll != nil {
		if err = p.BeforePoll(offset); err != nil {
			return
		}
	}
	p.Offsets = append(p.Offsets, offset)
	if offset > p.Ack {
		p.Ack = offset
	}
	if p.File != "" {
		// Model an independently durable provider ACK before returning its response.
		raw, _ := json.Marshal(struct {
			Ack     int64
			Offsets []int64
		}{p.Ack, p.Offsets})
		f, e := os.OpenFile(p.File, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if e != nil {
			return page, e
		}
		_, e = f.Write(raw)
		if e == nil {
			e = f.Sync()
		}
		f.Close()
		if e != nil {
			return page, e
		}
	}
	if p.LoseResponse {
		p.LoseResponse = false
		return page, errors.New("response lost after provider ACK")
	}
	page = provider.IntakePage{Account: p.Account, Next: offset}
	for _, e := range p.Events {
		if e.ID < offset || e.ID < p.Ack {
			continue
		}
		page.Events = append(page.Events, e)
		page.Next = e.ID + 1
		if len(page.Events) >= p.Batch {
			break
		}
	}
	return page, ctx.Err()
}
func newTelegramDrainFixture(t *testing.T) (telegramHistoryFixture, *fakeTelegramDrain) {
	t.Helper()
	f := newTelegramHistoryFixture(t)
	_, err := ReconcileTelegramHistory(context.Background(), f.Path, f.History, f.Account, f.Config)
	must(t, err)
	unknown := f.Pending
	unknown.NativeID = "update-900"
	unknown.Text = "confirm original-looking-token"
	unauthorized := unknown
	unauthorized.NativeID = "update-901"
	unauthorized.Actor = "stranger"
	unauthorized.Text = "private provider text"
	p := &fakeTelegramDrain{Account: f.Account, Batch: 2, Events: []provider.IntakeEvent{
		{ID: 51, Hash: protocol.Hash(f.Confirmation), Inbound: &f.Confirmation},
		{ID: 900, Hash: protocol.Hash(unknown), Inbound: &unknown},
		{ID: 901, Hash: protocol.Hash(unauthorized), Inbound: &unauthorized},
		{ID: 902, Hash: protocol.Hash("unsupported update"), Rejection: "unsupported_event"},
	}}
	return f, p
}
func TestTelegramDrainQuarantinesReplaysBeforeAcknowledging(t *testing.T) {
	f, p := newTelegramDrainFixture(t)
	ctx := context.Background()
	p.BeforePoll = func(offset int64) error {
		if offset == 0 {
			return nil
		}
		// The instance lease must remain held through every provider request.
		if s, err := Open(f.Path, f.Config, false); err == nil {
			s.Close()
			return errors.New("worker started during drain")
		}
		var j snapshotRecovery
		raw, err := os.ReadFile(filepath.Join(f.Path, "recovery.json"))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &j); err != nil {
			return err
		}
		var pin telegramDrainState
		raw, err = os.ReadFile(filepath.Join(f.Path, "recovery", j.Operation, telegramDrainFile(f.Account)))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &pin); err != nil {
			return err
		}
		if pin.Offset != offset {
			return fmt.Errorf("provider ACK outran durable journal")
		}
		return nil
	}
	summary, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, nil)
	must(t, err)
	if fmt.Sprint(p.Offsets) != "[0 901 903]" || p.Ack != 903 {
		t.Fatal("unexpected provider offsets", p.Offsets, p.Ack)
	}
	if summary.Mapped != 1 || summary.Unmapped != 1 || summary.Rejected != 2 || summary.ObservedEvents != 4 || summary.Pages != 3 ||
		summary.NextOffset != 903 || summary.ObservedAt.IsZero() || summary.HistoryComplete || summary.IntakeEnabled {
		t.Fatal(summary)
	}
	r, err := openRecoveryReconciler(ctx, f.Path, f.Config, nil)
	must(t, err)
	must(t, r.transaction(ctx, func(tx *sql.Tx) error {
		var offset int64
		var n int
		var body string
		if err := tx.QueryRow("SELECT offset FROM telegram_cursors WHERE account=?", f.Account).Scan(&offset); err != nil {
			return err
		}
		if offset != 6 {
			t.Fatal("operational cursor changed")
		}
		if err := tx.QueryRow("SELECT count(*) FROM commands").Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Fatal("replay created command")
		}
		if err := tx.QueryRow("SELECT count(*) FROM provider_inbox WHERE state<>'recovery_quarantined'").Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatal("replay escaped quarantine")
		}
		if err := tx.QueryRow("SELECT body FROM provider_inbox WHERE account=? AND id=901", f.Account).Scan(&body); err != nil {
			return err
		}
		if body != "" {
			t.Fatal("unauthorized text retained")
		}
		if err := tx.QueryRow("SELECT count(*) FROM reconciliation_telegram_drain_pages WHERE instr(body,'private provider text')>0").Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatal("unauthorized text in audit")
		}
		return nil
	}))
	must(t, r.Close())
	calls := len(p.Offsets)
	p.FailPreflight = true
	again, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, nil)
	must(t, err)
	if len(p.Offsets) != calls || protocol.Hash(again) != protocol.Hash(summary) {
		t.Fatal("completed observation repolled or renewed")
	}
}
func TestTelegramDrainLostResponseAndCommitResume(t *testing.T) {
	for _, stage := range []string{"response", "telegram_drain_response", "telegram_drain_transaction", "telegram_drain_commit", "telegram_drain_checkpoint"} {
		t.Run(stage, func(t *testing.T) {
			f, p := newTelegramDrainFixture(t)
			ctx := context.Background()
			if stage == "response" {
				p.LoseResponse = true
			}
			_, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, func(s string) error {
				if s == stage {
					return errors.New("interruption")
				}
				return nil
			})
			if err == nil {
				t.Fatal("interruption not reached")
			}
			if p.Ack != 0 {
				t.Fatal("uncommitted page acknowledged")
			}
			calls := len(p.Offsets)
			summary, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, nil)
			must(t, err)
			if p.Offsets[calls] != 0 || summary.IntakeEnabled || summary.HistoryComplete {
				t.Fatal("resume skipped old history")
			}
			if stage == "telegram_drain_commit" || stage == "telegram_drain_checkpoint" {
				if summary.ObservedEvents != 6 {
					t.Fatal("replayed observations not retained")
				}
			} else if summary.ObservedEvents != 4 {
				t.Fatal(summary)
			}
		})
	}
}
func TestTelegramDrainResponseLostAfterPositiveAck(t *testing.T) {
	f, p := newTelegramDrainFixture(t)
	ctx := context.Background()
	p.BeforePoll = func(offset int64) error {
		if offset > 0 {
			p.LoseResponse = true
		}
		return nil
	}
	if _, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, nil); err == nil {
		t.Fatal("lost response ignored")
	}
	if p.Ack != 901 {
		t.Fatal(p.Ack)
	}
	p.BeforePoll = nil
	before := len(p.Offsets)
	s, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, nil)
	must(t, err)
	if p.Offsets[before] != 0 || s.ObservedEvents != 4 || s.IntakeEnabled {
		t.Fatal(s, p.Offsets)
	}
}
func TestTelegramDrainRejectsDamagedProgressBeforeProviderAccess(t *testing.T) {
	for _, kind := range []string{"pin", "row", "page", "inbox", "inbox_state", "schema", "manifest", "rollback"} {
		t.Run(kind, func(t *testing.T) {
			f, p := newTelegramDrainFixture(t)
			ctx := context.Background()
			_, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, func(stage string) error {
				if stage == "telegram_drain_checkpoint" {
					return errors.New("stop")
				}
				return nil
			})
			if err == nil {
				t.Fatal("fault not reached")
			}
			r, err := openRecoveryReconciler(ctx, f.Path, f.Config, nil)
			must(t, err)
			switch kind {
			case "pin":
				must(t, os.Remove(filepath.Join(r.stage.File().Name(), telegramDrainFile(f.Account))))
			case "row":
				_, err = r.db.Exec("DELETE FROM reconciliation_telegram_drains")
			case "page":
				_, err = r.db.Exec("DELETE FROM reconciliation_telegram_drain_pages")
			case "inbox":
				_, err = r.db.Exec("DELETE FROM provider_inbox WHERE id=900")
			case "inbox_state":
				_, err = r.db.Exec("UPDATE provider_inbox SET state='pending' WHERE id=900")
			case "schema":
				_, err = r.db.Exec("DROP TABLE reconciliation_telegram_drain_pages")
			case "manifest":
				_, err = r.db.Exec("UPDATE reconciliation_telegram_drains SET body=json_set(body,'$.HistoryHash','changed')")
			case "rollback":
				must(t, r.transaction(ctx, func(tx *sql.Tx) error {
					s, err := readTelegramDrain(tx, f.Account)
					if err != nil {
						return err
					}
					if _, err = tx.Exec("DELETE FROM reconciliation_telegram_drain_pages"); err != nil {
						return err
					}
					return writeTelegramDrain(tx, initialTelegramDrain(s.Binding, s.Account, s.HistoryHash))
				}))
			}
			must(t, err)
			must(t, r.Close())
			calls := len(p.Offsets)
			if _, err = drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, nil); err == nil {
				t.Fatal("damaged recovery accepted")
			}
			if len(p.Offsets) != calls {
				t.Fatal("provider contacted before validating recovery")
			}
		})
	}
}
func TestTelegramDrainRejectsConflictsAndMalformedPages(t *testing.T) {
	for _, kind := range []string{"conflict", "input", "order", "duplicate", "cursor", "account", "preflight"} {
		t.Run(kind, func(t *testing.T) {
			f, p := newTelegramDrainFixture(t)
			ctx := context.Background()
			switch kind {
			case "conflict":
				p.Events[0].Hash = protocol.Hash("changed")
			case "input":
				in := *p.Events[0].Inbound
				in.Text = "changed"
				p.Events[0].Inbound = &in
			case "order":
				p.Events[0], p.Events[1] = p.Events[1], p.Events[0]
			case "duplicate":
				p.Events[1] = p.Events[0]
			case "account":
				p.Account = "foreign"
			case "preflight":
				p.FailPreflight = true
			case "cursor":
				r, err := openRecoveryReconciler(ctx, f.Path, f.Config, nil)
				must(t, err)
				s, err := r.openTelegramDrain(ctx, f.Account)
				must(t, err)
				_, err = r.commitTelegramDrain(ctx, s, 0, provider.IntakePage{Account: f.Account, Next: 999}, nil)
				if err == nil {
					t.Fatal("offset without evidence accepted")
				}
				must(t, r.Close())
				return
			}
			if _, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, nil); err == nil {
				t.Fatal("invalid provider result accepted")
			}
			if p.Ack != 0 {
				t.Fatal("invalid page acknowledged")
			}
		})
	}
}

func TestTelegramDrainCrashChild(t *testing.T) {
	path := os.Getenv("TELEGRAM_DRAIN_CRASH_INPUT")
	if path == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(path)
	must(t, err)
	var input struct {
		Fixture  telegramHistoryFixture
		Provider fakeTelegramDrain
	}
	must(t, json.Unmarshal(raw, &input))
	_, err = drainTelegramRecovery(context.Background(), input.Fixture.Path, input.Fixture.Config, input.Fixture.Account, &input.Provider, func(stage string) error {
		if stage == os.Getenv("TELEGRAM_DRAIN_CRASH_STAGE") && fmt.Sprint(len(input.Provider.Offsets)) == os.Getenv("TELEGRAM_DRAIN_CRASH_POLL") {
			fmt.Println("READY")
			select {}
		}
		return nil
	})
	must(t, err)
	t.Fatal("fault not reached")
}
func TestTelegramDrainSIGKILL(t *testing.T) {
	for _, tc := range []struct {
		stage        string
		poll         int
		ack          int64
		observations uint64
	}{
		{"telegram_drain_response", 1, 0, 4},
		{"telegram_drain_transaction", 1, 0, 4},
		{"telegram_drain_commit", 1, 0, 6},
		{"telegram_drain_checkpoint", 1, 0, 6},
		{"telegram_drain_response", 2, 901, 4},
		{"telegram_drain_commit", 3, 903, 4},
		{"telegram_drain_checkpoint", 3, 903, 4},
	} {
		t.Run(fmt.Sprintf("%s_poll_%d", tc.stage, tc.poll), func(t *testing.T) {
			f, p := newTelegramDrainFixture(t)
			p.File = filepath.Join(t.TempDir(), "provider.json")
			raw, err := json.Marshal(struct {
				Fixture  telegramHistoryFixture
				Provider *fakeTelegramDrain
			}{f, p})
			must(t, err)
			path := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(path, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTelegramDrainCrashChild$")
			cmd.Env = append(os.Environ(), "TELEGRAM_DRAIN_CRASH_INPUT="+path, "TELEGRAM_DRAIN_CRASH_STAGE="+tc.stage, "TELEGRAM_DRAIN_CRASH_POLL="+fmt.Sprint(tc.poll))
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
				t.Fatal("child failed", stderr.String())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			raw, err = os.ReadFile(p.File)
			must(t, err)
			var remote struct {
				Ack     int64
				Offsets []int64
			}
			must(t, json.Unmarshal(raw, &remote))
			if remote.Ack != tc.ack || len(remote.Offsets) != tc.poll {
				t.Fatal("provider ACK crossed uncommitted history")
			}
			p.Ack = remote.Ack
			p.Offsets = remote.Offsets
			summary, err := drainTelegramRecovery(context.Background(), f.Path, f.Config, f.Account, p, nil)
			must(t, err)
			if summary.IntakeEnabled || summary.HistoryComplete {
				t.Fatal("unsafe crash resume")
			}
			if tc.poll == 3 {
				if len(p.Offsets) != 3 {
					t.Fatal("completed drain repolled")
				}
			} else if p.Offsets[tc.poll] != 0 {
				t.Fatal("resume did not start from zero")
			}
			if summary.ObservedEvents != tc.observations {
				t.Fatal(summary)
			}
		})
	}
}

type drainHTTPTransport func(*http.Request) (*http.Response, error)

func (f drainHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTelegramDrainUsesPinnedProviderAdapter(t *testing.T) {
	for _, preflight := range []string{"ok", "webhook", "wrong_bot"} {
		t.Run(preflight, func(t *testing.T) {
			s, _, path, _ := commandFixture(t)
			ctx := context.Background()
			s.config.Interaction.Destinations[0].Account = "123"
			s.config.Interaction.Permissions[0].Actor = "77"
			c := s.config
			must(t, s.PersistTelegram(ctx, provider.IntakePage{Account: "123"}))
			artifact := filepath.Join(t.TempDir(), "snapshot")
			_, err := s.Snapshot(ctx, artifact)
			must(t, err)
			must(t, s.Close())
			must(t, PrepareRecoveryQuarantine(ctx, path, artifact, c))
			_, err = ReconcileTelegramHistory(ctx, path, artifact, "123", c)
			must(t, err)
			token := filepath.Join(t.TempDir(), "token")
			must(t, os.WriteFile(token, []byte("123:secret"), 0600))
			var offsets []int64
			p := provider.TelegramPoller{Profile: provider.TelegramAccount{Account: "123", TokenFile: token, BotName: "ExampleBot"}}
			p.Transport = drainHTTPTransport(func(req *http.Request) (*http.Response, error) {
				result := ""
				switch {
				case strings.HasSuffix(req.URL.Path, "/getWebhookInfo"):
					result = `{"url":""}`
					if preflight == "webhook" {
						result = `{"url":"https://existing.invalid/hook"}`
					}
				case strings.HasSuffix(req.URL.Path, "/getMe"):
					result = `{"id":123,"is_bot":true,"username":"ExampleBot"}`
					if preflight == "wrong_bot" {
						result = `{"id":456,"is_bot":true,"username":"ExampleBot"}`
					}
				case strings.HasSuffix(req.URL.Path, "/getUpdates"):
					var input struct {
						Offset  int64
						Limit   int
						Allowed []string `json:"allowed_updates"`
					}
					must(t, json.NewDecoder(req.Body).Decode(&input))
					offsets = append(offsets, input.Offset)
					if input.Limit != 16 || fmt.Sprint(input.Allowed) != "[message]" {
						t.Fatal("provider request broadened")
					}
					if input.Offset == 0 {
						result = fmt.Sprintf(`[{"update_id":7,"message":{"message_id":3,"date":%d,"from":{"id":77},"chat":{"id":-100},"text":"/worker@ExampleBot backup/storage status"}}]`, time.Now().Unix())
					} else if input.Offset == 8 {
						result = "[]"
					} else {
						t.Fatal("invalid provider offset", input.Offset)
					}
				default:
					t.Fatal("unexpected external operation", req.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":` + result + "}"))}, nil
			})
			summary, err := DrainTelegramRecovery(ctx, path, c, p)
			if preflight != "ok" {
				if err == nil || len(offsets) != 0 {
					t.Fatal("preflight failed open", err, offsets)
				}
				return
			}
			must(t, err)
			if fmt.Sprint(offsets) != "[0 8]" || summary.Unmapped != 1 || summary.IntakeEnabled || summary.HistoryComplete {
				t.Fatal(summary, offsets)
			}
		})
	}
}
func TestTelegramDrainEmptyObservationAndHistoryPrerequisite(t *testing.T) {
	f := newTelegramHistoryFixture(t)
	ctx := context.Background()
	p := &fakeTelegramDrain{Account: f.Account, Batch: 16}
	if _, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, nil); err == nil || len(p.Offsets) != 0 {
		t.Fatal("polled before history reconciliation")
	}
	_, err := ReconcileTelegramHistory(ctx, f.Path, f.History, f.Account, f.Config)
	must(t, err)
	summary, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, nil)
	must(t, err)
	if summary.Pages != 1 || summary.ObservedEvents != 0 || summary.NextOffset != 0 || summary.HistoryComplete || summary.IntakeEnabled {
		t.Fatal("empty observation claimed complete history", summary)
	}
}
func TestTelegramDrainResumeDoesNotSkipLowerNativeIDs(t *testing.T) {
	f, p := newTelegramDrainFixture(t)
	ctx := context.Background()
	_, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, func(stage string) error {
		if stage == "telegram_drain_checkpoint" {
			return errors.New("interrupted")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault not reached")
	}
	// Model an extended interruption followed by a fresh provider ID range.
	// The recovery offset is 901, but using it here would discard update 10.
	in := f.Pending
	in.NativeID = "update-10"
	p.Events = []provider.IntakeEvent{{ID: 10, Hash: protocol.Hash(in), Inbound: &in}}
	before := len(p.Offsets)
	summary, err := drainTelegramRecovery(ctx, f.Path, f.Config, f.Account, p, nil)
	must(t, err)
	if fmt.Sprint(p.Offsets[before:]) != "[0 11]" || summary.ObservedEvents != 3 || summary.Unmapped != 2 || summary.NextOffset != 11 || summary.HistoryComplete {
		t.Fatal(summary, p.Offsets)
	}
}
