package worker

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

// TelegramRenewalHead is offline audit state, not freshness or intake authority.
type TelegramRenewalHead struct {
	ID, Hash string
	Complete bool
}

// ReviewTelegramRenewalHead lets a stopped lifecycle coordinator freeze the
// exact predecessor before provider I/O. An absent renewal refers to the
// immutable published cursor; retained history can never be silently discarded.
func ReviewTelegramRenewalHead(ctx context.Context, path string, c Config, commit, account string) (out TelegramRenewalHead, err error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	s, err := openPublishedTelegramSession(ctx, path, c, commit, account)
	if err != nil {
		return out, err
	}
	defer s.close()
	var state telegramRenewalState
	_, err = readRenewalJSON(s.root, s.renewalIndex(), &state, 16384)
	if os.IsNotExist(err) {
		if err = s.requireOriginalTelegramHandoff(); err != nil {
			return out, err
		}
		return TelegramRenewalHead{Hash: protocol.Hash(s.cursor), Complete: true}, s.verify(ctx)
	}
	if err != nil {
		return out, err
	}
	j, err := s.openRenewal(ctx, nil, nil)
	if err != nil {
		return out, err
	}
	defer j.close()
	if err = s.verify(ctx); err != nil {
		return out, err
	}
	out.ID = j.state.Binding.ID
	if j.complete != nil {
		out.Hash = j.complete.RenewalHash
		out.Complete = true
	}
	return out, nil
}

// AuditTelegramHandoffRenewal verifies complete retained evidence without
// refreshing its timestamps. Verification services may restart after expiry;
// production activation must separately call VerifyRenewedTelegramHandoff.
func AuditTelegramHandoffRenewal(ctx context.Context, path string, c Config, commit, account, hash string) (out TelegramHandoffRenewal, err error) {
	if !safefs.DigestPattern.MatchString(hash) {
		return out, fmt.Errorf("exact renewal hash required")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	s, err := openPublishedTelegramSession(ctx, path, c, commit, account)
	if err != nil {
		return out, err
	}
	defer s.close()
	return auditTelegramRenewal(ctx, s, hash)
}

func auditTelegramRenewal(ctx context.Context, s *publishedTelegramSession, hash string) (out TelegramHandoffRenewal, err error) {
	j, err := s.openRenewal(ctx, nil, nil)
	if err != nil {
		return out, err
	}
	defer j.close()
	if j.complete == nil || j.complete.RenewalHash != hash {
		return out, fmt.Errorf("Telegram renewal is incomplete or superseded")
	}
	if err = s.verify(ctx); err != nil {
		return out, err
	}
	if err = j.check(ctx); err != nil {
		return out, err
	}
	return *j.complete, nil
}

// OpenManagedTelegramVerification audits the exact renewal under the same lease
// retained for the runtime's lifetime. It never installs a production cutoff.
func OpenManagedTelegramVerification(path string, c Config, expected protocol.Identity, revision, commit, account, renewalID, hash string) (*Store, error) {
	if !safefs.ValidID(renewalID, "n") || !safefs.DigestPattern.MatchString(hash) {
		return nil, fmt.Errorf("managed Telegram verification requires exact renewal pins")
	}
	s, err := OpenManagedRecoveryVerification(path, c, expected, revision, commit)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	session, err := openPublishedTelegramSessionWithLease(ctx, path, c, commit, account, s.lease)
	if err != nil {
		s.Close()
		return nil, err
	}
	proof, err := auditTelegramRenewal(ctx, session, hash)
	session.close()
	if err == nil && proof.Binding.ID != renewalID {
		err = fmt.Errorf("managed Telegram renewal ID changed")
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	s.telegramRenewalID = renewalID
	return s, nil
}

type lifecycleTelegramPoller struct {
	p     provider.TelegramPoller
	check func(context.Context) error
}

func (p lifecycleTelegramPoller) Preflight(ctx context.Context) error {
	if err := p.check(ctx); err != nil {
		return err
	}
	if err := p.p.Preflight(ctx); err != nil {
		return err
	}
	return p.check(ctx)
}
func (p lifecycleTelegramPoller) Poll(ctx context.Context, offset int64) (out provider.IntakePage, err error) {
	if err = p.check(ctx); err != nil {
		return out, err
	}
	out, err = p.p.Poll(ctx, offset)
	if err != nil {
		return out, err
	}
	if err = p.check(ctx); err != nil {
		return provider.IntakePage{}, err
	}
	return out, nil
}

// RenewManagedTelegramHandoff keeps external service/installation authority
// checks around each preflight and poll while the renewal owns the worker lease.
func RenewManagedTelegramHandoff(ctx context.Context, path string, c Config, commit string, request TelegramRenewalRequest, p provider.TelegramPoller, check func(context.Context) error) (TelegramHandoffRenewal, error) {
	if c.Interaction == nil || check == nil {
		return TelegramHandoffRenewal{}, fmt.Errorf("managed Telegram renewal dependencies missing")
	}
	p.Destinations = c.Interaction.Destinations
	if err := check(ctx); err != nil {
		return TelegramHandoffRenewal{}, err
	}
	out, err := renewPublishedTelegramHandoff(ctx, path, c, commit, p.Profile.Account, request, lifecycleTelegramPoller{p, check}, telegramRenewalClock{time.Now, process.HostClock}, nil)
	if err != nil {
		return TelegramHandoffRenewal{}, err
	}
	if err = check(ctx); err != nil {
		return TelegramHandoffRenewal{}, err
	}
	return out, nil
}
