package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func managedTelegramHandoffFixture(t *testing.T) (*telegramRenewalFixture, TelegramHandoffRenewal, ManagedDeliveryRecovery, string, *Store) {
	t.Helper()
	f := newTelegramRenewalFixture(t)
	renewal, err := f.run(nil)
	must(t, err)
	f.At = f.At.Add(6 * time.Second)
	f.Up += 6
	review, err := AdoptPublishedRecoveryVerification(context.Background(), f.Path, f.Config, f.Publication.Commit.CommitHash)
	must(t, err)
	revision, err := safefs.NewID("n")
	must(t, err)
	installation, err := safefs.NewID("n")
	must(t, err)
	r := ManagedDeliveryRecovery{Version: 1, Before: review.Before, After: review.After, CommitHash: review.CommitHash, Owner: ManagedDeliveryOwner{Version: 1, Root: t.TempDir(), Installation: installation, Identity: review.After, ConfigHash: protocol.Hash(f.Config)}}
	s, err := OpenManagedRecoveryVerification(f.Path, f.Config, r.After, revision, r.CommitHash)
	must(t, err)
	must(t, s.EnrollManagedDeliveryRecovery(r))
	return f, renewal, r, revision, s
}

func TestManagedTelegramHandoffSurvivesIntakeAndRestart(t *testing.T) {
	f, renewal, r, revision, s := managedTelegramHandoffFixture(t)
	ctx := context.Background()
	h, err := s.enrollManagedTelegramHandoff(ctx, r, renewal.Binding.Account, renewal.Binding.ID, renewal.RenewalHash, f.clock(), nil)
	must(t, err)
	must(t, s.Close())
	s, err = OpenManagedRecoveredDelivery(f.Path, f.Config, revision, r, protocol.Hash(r.Owner), protocol.Hash("activation"))
	must(t, err)
	defer s.Close()
	s.now = func() time.Time { return f.At }
	must(t, s.RequireManagedTelegramHandoff(ctx, h, protocol.Hash(h)))
	commandHash := protocol.Hash("command admission")
	must(t, s.CommitManagedCommandAdmission("all", commandHash, ""))
	must(t, s.ConfigureManagedCommands("all", commandHash))
	must(t, s.BindManagedCommandAuthority(func(ctx context.Context) error { return s.RequireManagedTelegramHandoff(ctx, h, protocol.Hash(h)) }))
	old := *f.Poller.Events[0].Inbound
	old.NativeID, old.SentAt = "update-3000", renewal.Cursor.NotBefore
	fresh := old
	fresh.NativeID, fresh.SentAt = "update-3001", f.At
	must(t, s.PersistTelegram(ctx, provider.IntakePage{Account: renewal.Binding.Account, Next: 3002, Events: []provider.IntakeEvent{{ID: 3000, Hash: protocol.Hash(old), Inbound: &old}, {ID: 3001, Hash: protocol.Hash(fresh), Inbound: &fresh}}}))
	for id, want := range map[int]string{3000: "rejected", 3001: "pending"} {
		var state string
		must(t, s.db.QueryRow("SELECT state FROM provider_inbox WHERE account=? AND id=?", renewal.Binding.Account, id).Scan(&state))
		if state != want {
			t.Fatal(id, state)
		}
	}
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		if err := guardTelegramRecoveryInput(tx, old); err == nil {
			return fmt.Errorf("old restored input admitted")
		}
		return guardTelegramRecoveryInput(tx, fresh)
	}))
	must(t, s.Close())
	// Audits preserve the original cutoff while accepting a legitimately
	// advancing operational offset only behind the independent handoff proof.
	got, err := AuditTelegramHandoffRenewal(ctx, f.Path, f.Config, r.CommitHash, renewal.Binding.Account, renewal.RenewalHash)
	must(t, err)
	if got != renewal {
		t.Fatal("audit changed renewal")
	}
	s, err = OpenManagedTelegramVerification(f.Path, f.Config, r.After, revision, r.CommitHash, renewal.Binding.Account, renewal.Binding.ID, renewal.RenewalHash)
	must(t, err)
	offset, err := s.TelegramOffset(ctx, renewal.Binding.Account)
	must(t, err)
	if offset != 3002 {
		t.Fatal(offset)
	}
	must(t, s.Close())
	if _, err = f.run(nil); err == nil {
		t.Fatal("new drain changed a consumed handoff")
	}
}

func TestManagedTelegramHandoffFaultsAndAnchorLoss(t *testing.T) {
	for _, at := range []string{"telegram_handoff_intent", "telegram_handoff_sql", "telegram_handoff_complete"} {
		t.Run(at, func(t *testing.T) {
			f, renewal, r, revision, s := managedTelegramHandoffFixture(t)
			stop := errors.New("interrupt handoff")
			_, err := s.enrollManagedTelegramHandoff(context.Background(), r, renewal.Binding.Account, renewal.Binding.ID, renewal.RenewalHash, f.clock(), func(p string) error {
				if p == at {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			must(t, s.Close())
			s, err = OpenManagedRecoveryVerification(f.Path, f.Config, r.After, revision, r.CommitHash)
			must(t, err)
			defer s.Close()
			h, err := s.enrollManagedTelegramHandoff(context.Background(), r, renewal.Binding.Account, renewal.Binding.ID, renewal.RenewalHash, f.clock(), nil)
			must(t, err)
			name := telegramHandoffName(renewal.Binding.Account)
			if at == "telegram_handoff_sql" {
				_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", name)
			} else {
				err = os.Remove(filepath.Join(f.Path, name+".json"))
			}
			must(t, err)
			if err := s.RequireManagedTelegramHandoff(context.Background(), h, protocol.Hash(h)); err == nil {
				t.Fatal("lost anchor admitted")
			}
			if _, err := s.enrollManagedTelegramHandoff(context.Background(), r, renewal.Binding.Account, renewal.Binding.ID, renewal.RenewalHash, f.clock(), nil); err == nil {
				t.Fatal("committed anchor reconstructed")
			}
		})
	}
}

func TestManagedTelegramHandoffRequiresFreshDrain(t *testing.T) {
	f, renewal, r, _, s := managedTelegramHandoffFixture(t)
	defer s.Close()
	f.At = f.At.Add(telegramHandoffLifetime)
	f.Up += telegramHandoffLifetime.Seconds()
	if _, err := s.enrollManagedTelegramHandoff(context.Background(), r, renewal.Binding.Account, renewal.Binding.ID, renewal.RenewalHash, f.clock(), nil); err == nil {
		t.Fatal("expired drain consumed")
	}
}

func TestManagedTelegramHandoffExpiredInterruptionCanCancel(t *testing.T) {
	for _, at := range []string{"telegram_handoff_intent", "telegram_handoff_sql"} {
		t.Run(at, func(t *testing.T) {
			f, renewal, r, revision, s := managedTelegramHandoffFixture(t)
			ctx := context.Background()
			stop := errors.New("interrupt")
			_, err := s.enrollManagedTelegramHandoff(ctx, r, renewal.Binding.Account, renewal.Binding.ID, renewal.RenewalHash, f.clock(), func(p string) error {
				if p == at {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			f.At = f.At.Add(time.Hour)
			f.Up += 3600
			must(t, s.CancelManagedTelegramHandoff(ctx, r, renewal.Binding.Account, renewal.Binding.ID, renewal.RenewalHash))
			must(t, s.Close())
			s, err = OpenManagedTelegramVerification(f.Path, f.Config, r.After, revision, r.CommitHash, renewal.Binding.Account, renewal.Binding.ID, renewal.RenewalHash)
			must(t, err)
			must(t, s.Close())
			if at == "telegram_handoff_intent" {
				// No SQL grant existed, so a retained abort permits a new drain.
				f.Request.ID, err = safefs.NewID("n")
				must(t, err)
				f.Request.PreviousHash = renewal.RenewalHash
				next, err := f.run(nil)
				must(t, err)
				f.At = f.At.Add(6 * time.Second)
				f.Up += 6
				s, err = OpenManagedRecoveryVerification(f.Path, f.Config, r.After, revision, r.CommitHash)
				must(t, err)
				_, err = s.enrollManagedTelegramHandoff(ctx, r, next.Binding.Account, next.Binding.ID, next.RenewalHash, f.clock(), nil)
				must(t, err)
				must(t, s.Close())
			} else {
				// SQL committed while fresh. Cancellation preserves this cutoff,
				// never resetting an offset that could already have been used.
				if _, err = f.run(nil); err == nil {
					t.Fatal("committed cutoff replaced")
				}
			}
		})
	}
}
