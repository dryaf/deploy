package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

type telegramRenewalFixture struct {
	Path        string
	Config      Config
	Publication RecoveryPublicationResult
	Original    TelegramRecoveryCursor
	Request     TelegramRenewalRequest
	Poller      *fakeTelegramDrain
	At          time.Time
	Up          float64
	Boot        string
}

func newTelegramRenewalFixture(t *testing.T) *telegramRenewalFixture {
	t.Helper()
	f := &telegramRenewalFixture{}
	f.Path, f.Config, f.Publication, f.Original = publishedTelegramFixture(t)
	f.Request.ID, _ = safefs.NewID("n")
	f.Request.PreviousHash = protocol.Hash(f.Original)
	f.At = f.Original.Drain.ObservedAt.Add(20 * time.Minute)
	f.Up = f.Original.Drain.Uptime + 1200
	f.Boot = f.Original.Drain.BootID
	d, err := privateDirectory(f.Path)
	must(t, err)
	defer d.Close()
	db, err := snapshotDB(d, "state.db", true)
	must(t, err)
	defer db.Close()
	var hash, raw string
	must(t, db.QueryRow("SELECT hash,body FROM provider_inbox WHERE account=? AND id=5", f.Original.Drain.Account).Scan(&hash, &raw))
	var known interaction.Inbound
	must(t, protocol.Decode([]byte(raw), &known, 8192))
	unknown := known
	unknown.NativeID = "update-2000"
	unknown.Text = "confirm old-looking-token"
	unknown.SentAt = f.At
	stranger := unknown
	stranger.NativeID = "update-2001"
	stranger.Actor = "stranger"
	stranger.Text = "discard this private text"
	f.Poller = &fakeTelegramDrain{Account: f.Original.Drain.Account, Batch: 2, Events: []provider.IntakeEvent{
		{ID: 5, Hash: hash, Inbound: &known},
		{ID: 2000, Hash: protocol.Hash(unknown), Inbound: &unknown},
		{ID: 2001, Hash: protocol.Hash(stranger), Inbound: &stranger},
		{ID: 2002, Hash: protocol.Hash("unsupported"), Rejection: "unsupported"},
	}}
	return f
}
func (f *telegramRenewalFixture) clock() telegramRenewalClock {
	return telegramRenewalClock{func() time.Time { return f.At }, func() (string, float64, error) { return f.Boot, f.Up, nil }}
}
func (f *telegramRenewalFixture) run(fault func(string) error) (TelegramHandoffRenewal, error) {
	return renewPublishedTelegramHandoff(context.Background(), f.Path, f.Config, f.Publication.Commit.CommitHash, f.Original.Drain.Account, f.Request, f.Poller, f.clock(), fault)
}
func (f *telegramRenewalFixture) directory(id string) string {
	return filepath.Join(f.Path, "recovery", f.Publication.Commit.Image.Operation, "telegram-renewals", protocol.Hash(f.Original.Drain.Account), id)
}
func (f *telegramRenewalFixture) verify(hash string) (PublishedTelegramHandoff, error) {
	return verifyRenewedTelegramHandoff(context.Background(), f.Path, f.Config, f.Publication.Commit.CommitHash, f.Original.Drain.Account, hash, f.clock())
}

func TestTelegramRenewalPreservesPublicationAndQuarantinesNewInputs(t *testing.T) {
	f := newTelegramRenewalFixture(t)
	before, err := os.ReadFile(filepath.Join(f.Path, "state.db"))
	must(t, err)
	one, err := f.run(nil)
	must(t, err)
	must(t, one.Cursor.Validate())
	if one.IntakeEnabled || one.Cursor.IntakeEnabled || one.Cursor.HistoryComplete || one.Cursor.Offset != 0 || one.Cursor.RequestsHash != f.Original.RequestsHash || one.Cursor.Drain.Pages != 3 || one.Cursor.Drain.Mapped != 1 || one.Cursor.Drain.Unmapped != 1 || one.Cursor.Drain.Rejected != 2 || !reflect.DeepEqual(f.Poller.Offsets, []int64{0, 2001, 2003}) {
		t.Fatal(one, f.Poller.Offsets)
	}
	if _, err = f.verify(one.RenewalHash); err == nil {
		t.Fatal("cutoff settling interval bypassed")
	}
	f.At = f.At.Add(6 * time.Second)
	f.Up += 6
	v, err := f.verify(one.RenewalHash)
	must(t, err)
	if v.RenewalHash != one.RenewalHash || v.IntakeEnabled || !v.RequiresActivationRecheck {
		t.Fatal(v)
	}
	_, err = verifyPublishedTelegramHandoff(context.Background(), f.Path, f.Config, f.Publication.Commit.CommitHash, f.Original.Drain.Account, func() time.Time { return f.Original.Drain.ObservedAt.Add(6 * time.Second) }, func() (string, float64, error) { return f.Original.Drain.BootID, f.Original.Drain.Uptime + 6, nil })
	if err == nil {
		t.Fatal("original handoff advertised after renewal")
	}
	// Completed audit retries must not poll, renew freshness or reset cutoffs.
	f.Poller.FailPreflight = true
	f.At = f.At.Add(20 * time.Minute)
	f.Up += 1200
	two, err := f.run(nil)
	must(t, err)
	if two != one {
		t.Fatal("completed retry renewed evidence")
	}
	if _, err = f.verify(one.RenewalHash); err == nil {
		t.Fatal("expired renewal accepted")
	}
	after, err := os.ReadFile(filepath.Join(f.Path, "state.db"))
	must(t, err)
	if string(before) != string(after) {
		t.Fatal("renewal changed canonical database")
	}
	data, err := os.ReadFile(filepath.Join(f.directory(f.Request.ID), renewalPageName(2)))
	must(t, err)
	if bytes.Contains(data, []byte("discard this private text")) {
		t.Fatal("unauthorized content retained")
	}
	im := f.Publication.Commit.Image
	_, err = VerifyRecoveryPublication(context.Background(), f.Path, f.Config, im.Operation, im.ReviewHash, im.CheckpointHash, im.ProofHash, im.ImageHash)
	must(t, err)
	if s, err := Open(f.Path, f.Config, false); err == nil {
		s.Close()
		t.Fatal("renewal activated ordinary worker")
	}
}

func TestTelegramRenewalRetainsAncestorsAndPinsLatest(t *testing.T) {
	f := newTelegramRenewalFixture(t)
	one, err := f.run(nil)
	must(t, err)
	f.Request = TelegramRenewalRequest{PreviousHash: one.RenewalHash}
	f.Request.ID, _ = safefs.NewID("n")
	f.At = f.At.Add(time.Hour)
	f.Up += 3600
	two, err := f.run(nil)
	must(t, err)
	if two.Binding.Generation != 2 || two.Binding.PreviousID != one.Binding.ID || two.Binding.PreviousHash != one.RenewalHash || !two.Cursor.NotBefore.After(one.Cursor.NotBefore) {
		t.Fatal(two)
	}
	f.At = f.At.Add(6 * time.Second)
	f.Up += 6
	_, err = f.verify(two.RenewalHash)
	must(t, err)
	if _, err = f.verify(one.RenewalHash); err == nil {
		t.Fatal("superseded renewal accepted")
	}
	f.Request = TelegramRenewalRequest{ID: one.Binding.ID, PreviousHash: one.Binding.PreviousHash}
	if _, err = f.run(nil); err == nil {
		t.Fatal("old operation reopened after successor")
	}
	must(t, os.Remove(filepath.Join(f.directory(one.Binding.ID), renewalPageName(1))))
	if _, err = f.verify(two.RenewalHash); err == nil {
		t.Fatal("missing ancestor evidence accepted")
	}
}

func TestTelegramRenewalInterruptedRetry(t *testing.T) {
	for _, at := range []string{"telegram_renewal_intent", "telegram_renewal_directory", "telegram_renewal_before_poll", "telegram_renewal_response", "telegram_renewal_page", "telegram_renewal_checkpoint", "telegram_renewal_complete"} {
		t.Run(at, func(t *testing.T) {
			f := newTelegramRenewalFixture(t)
			stop := errors.New("interrupted")
			_, err := f.run(func(point string) error {
				if point == at {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			f.Poller.Offsets = nil
			got, err := f.run(nil)
			must(t, err)
			if got.IntakeEnabled {
				t.Fatal("resumed activation")
			}
			if len(f.Poller.Offsets) > 0 && f.Poller.Offsets[0] != 0 {
				t.Fatal("retry inherited ACK offset", f.Poller.Offsets)
			}
			if at == "telegram_renewal_complete" && len(f.Poller.Offsets) != 0 {
				t.Fatal("completed replay polled")
			}
		})
	}
}

func TestTelegramRenewalRefusesDamagedProgressBeforePolling(t *testing.T) {
	for _, kind := range []string{"missing-page", "missing-index", "behind-index", "intent", "unexpected", "completion", "request", "live-cursor"} {
		t.Run(kind, func(t *testing.T) {
			f := newTelegramRenewalFixture(t)
			one, err := f.run(nil)
			must(t, err)
			switch kind {
			case "missing-page":
				must(t, os.Remove(filepath.Join(f.directory(f.Request.ID), renewalPageName(1))))
			case "missing-index":
				must(t, os.Remove(filepath.Join(f.Path, renewalIndex(f.Original.Drain.Account))))
			case "behind-index":
				d, e := privateDirectory(f.Path)
				must(t, e)
				must(t, d.WriteJSON(renewalIndex(f.Original.Drain.Account), renewalSeed(one.Binding), true))
				d.Close()
			case "intent":
				must(t, os.WriteFile(filepath.Join(f.directory(f.Request.ID), "intent.json"), []byte("{}"), 0600))
			case "unexpected":
				must(t, os.WriteFile(filepath.Join(f.directory(f.Request.ID), "stray.db"), nil, 0600))
			case "completion":
				must(t, os.WriteFile(filepath.Join(f.directory(f.Request.ID), "complete.json"), []byte("{}"), 0600))
			case "request":
				f.Request.PreviousHash = protocol.Hash("wrong")
			case "live-cursor":
				d, e := privateDirectory(f.Path)
				must(t, e)
				db, e := snapshotDB(d, "state.db", false)
				must(t, e)
				_, e = db.Exec("UPDATE telegram_cursors SET offset=9000")
				must(t, e)
				db.Close()
				d.Close()
			}
			f.Poller.Offsets = nil
			if _, err = f.run(nil); err == nil {
				t.Fatal("damaged history accepted")
			}
			if len(f.Poller.Offsets) != 0 {
				t.Fatal("damaged history acknowledged provider data")
			}
		})
	}
}

func TestTelegramRenewalChecksBeforeProviderAcknowledgement(t *testing.T) {
	f := newTelegramRenewalFixture(t)
	f.Poller.BeforePoll = func(offset int64) error {
		if offset == 0 {
			return nil
		}
		var pin telegramRenewalState
		raw, err := os.ReadFile(filepath.Join(f.Path, renewalIndex(f.Original.Drain.Account)))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &pin); err != nil {
			return err
		}
		if pin.Pages != 1 || pin.Offset != offset {
			t.Fatalf("ACK before durable checkpoint: %+v, %d", pin, offset)
		}
		data, err := os.ReadFile(filepath.Join(f.directory(f.Request.ID), renewalPageName(pin.Pages)))
		if err != nil {
			return err
		}
		var page telegramRenewalPage
		if err = json.Unmarshal(data, &page); err != nil {
			return err
		}
		if protocol.Hash(page) != pin.Head {
			t.Fatal("ACK without sealed page")
		}
		return errors.New("stop before ACK")
	}
	if _, err := f.run(nil); err == nil {
		t.Fatal("fixture should stop")
	}
	if !reflect.DeepEqual(f.Poller.Offsets, []int64{0}) {
		t.Fatal(f.Poller.Offsets)
	}
}

func TestTelegramRenewalProviderLossAndNativeIDConflict(t *testing.T) {
	for _, kind := range []string{"response-loss", "native-conflict", "clock-back", "preflight-conflict"} {
		t.Run(kind, func(t *testing.T) {
			f := newTelegramRenewalFixture(t)
			switch kind {
			case "response-loss":
				f.Poller.BeforePoll = func(offset int64) error {
					if offset > 0 {
						f.Poller.LoseResponse = true
					}
					return nil
				}
			case "native-conflict":
				f.Poller.Events[0].Hash = protocol.Hash("different provider contents")
			case "clock-back":
				f.At = f.Original.Drain.ObservedAt.Add(-time.Second)
			case "preflight-conflict":
				f.Poller.FailPreflight = true
			}
			if _, err := f.run(nil); err == nil {
				t.Fatal("invalid provider state accepted")
			}
			if kind == "response-loss" {
				if f.Poller.Ack != 2001 {
					t.Fatal("provider loss did not persist ACK", f.Poller.Ack)
				}
				f.Poller.BeforePoll = nil
				f.Poller.Offsets = nil
				_, err := f.run(nil)
				must(t, err)
				if f.Poller.Offsets[0] != 0 {
					t.Fatal("response-loss retry inherited offset")
				}
			} else if f.Poller.Ack != 0 {
				t.Fatal("unverified provider page acknowledged")
			}
		})
	}
}

func TestTelegramRenewalAuthorityLossStopsNextACK(t *testing.T) {
	for _, kind := range []string{"lease", "database", "recovery", "index", "completion"} {
		t.Run(kind, func(t *testing.T) {
			f := newTelegramRenewalFixture(t)
			_, err := f.run(func(at string) error {
				if at != "telegram_renewal_checkpoint" {
					return nil
				}
				switch kind {
				case "lease":
					if e := os.Rename(filepath.Join(f.Path, "worker.lock"), filepath.Join(f.Path, "old.lock")); e != nil {
						return e
					}
					return os.WriteFile(filepath.Join(f.Path, "worker.lock"), nil, 0600)
				case "database":
					file := filepath.Join(f.Path, "state.db")
					data, e := os.ReadFile(file)
					if e != nil {
						return e
					}
					if e = os.Rename(file, file+".saved"); e != nil {
						return e
					}
					return os.WriteFile(file, data, 0600)
				case "recovery":
					return os.WriteFile(filepath.Join(f.Path, "recovery.json"), []byte("{}"), 0600)
				case "index":
					return os.Remove(filepath.Join(f.Path, renewalIndex(f.Original.Drain.Account)))
				default:
					return os.Remove(filepath.Join(f.Path, publicationCommitComplete))
				}
			})
			if err == nil {
				t.Fatal("authority loss accepted")
			}
			if !reflect.DeepEqual(f.Poller.Offsets, []int64{0}) {
				t.Fatal("next provider ACK escaped fence", f.Poller.Offsets)
			}
		})
	}
}

func TestTelegramRenewalFreshnessAndIncompleteSuccessor(t *testing.T) {
	f := newTelegramRenewalFixture(t)
	completed, err := f.run(nil)
	must(t, err)
	for _, kind := range []string{"expired", "reboot", "wall-jump", "monotonic-back"} {
		t.Run(kind, func(t *testing.T) {
			at, up, boot := f.At, f.Up, f.Boot
			defer func() { f.At, f.Up, f.Boot = at, up, boot }()
			switch kind {
			case "expired":
				f.At = f.At.Add(10 * time.Minute)
				f.Up += 600
			case "reboot":
				f.Boot = "other-boot"
			case "wall-jump":
				f.At = f.At.Add(time.Minute)
				f.Up += 6
			case "monotonic-back":
				f.Up--
			}
			if got, err := f.verify(completed.RenewalHash); err == nil || got != (PublishedTelegramHandoff{}) {
				t.Fatal("invalid renewal freshness accepted", got, err)
			}
		})
	}
	f.Request = TelegramRenewalRequest{PreviousHash: completed.RenewalHash}
	f.Request.ID, _ = safefs.NewID("n")
	_, err = f.run(func(at string) error {
		if at == "telegram_renewal_intent" {
			return errors.New("stop")
		}
		return nil
	})
	if err == nil {
		t.Fatal("missing interruption")
	}
	if _, err = f.verify(completed.RenewalHash); err == nil {
		t.Fatal("old completion accepted during successor")
	}
	_, err = f.run(nil)
	must(t, err)
}

func TestTelegramRenewalPrivateTemporaryAndBudgets(t *testing.T) {
	f := newTelegramRenewalFixture(t)
	completed, err := f.run(nil)
	must(t, err)
	id, err := safefs.NewID("n")
	must(t, err)
	tmp := filepath.Join(f.directory(f.Request.ID), "."+id+".json")
	f.Poller.FailPreflight = true
	for _, body := range [][]byte{nil, []byte(`{"partial":`)} {
		must(t, os.WriteFile(tmp, body, 0600))
		again, e := f.run(nil)
		must(t, e)
		if again != completed {
			t.Fatal("unpublished temporary changed evidence")
		}
	}
	must(t, os.Chmod(tmp, 0644))
	if _, err = f.run(nil); err == nil {
		t.Fatal("unsafe temporary accepted")
	}
	must(t, os.Chmod(tmp, 0600))
	s, err := openPublishedTelegramSession(context.Background(), f.Path, f.Config, f.Publication.Commit.CommitHash, f.Original.Drain.Account)
	must(t, err)
	defer s.close()
	// Inject a small reader budget after opening the otherwise valid publication.
	// The archive must reject before expanding pages into the retained-event map.
	s.config.MaxDatabaseBytes = 1
	if j, e := s.openRenewal(context.Background(), nil, nil); e == nil {
		j.close()
		t.Fatal("over-budget history accepted")
	} else if !strings.Contains(e.Error(), "byte budget") {
		t.Fatal(e)
	}
}
