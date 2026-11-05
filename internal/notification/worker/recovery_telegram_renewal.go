package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

// TelegramRenewalRequest identifies one retryable drain. PreviousHash is the
// original cursor hash for the first drain, then the last RenewalHash.
type TelegramRenewalRequest struct{ ID, PreviousHash string }

type TelegramRenewalBinding struct {
	Version                                                         int
	ID, PreviousID, PreviousHash, CommitHash, OriginalHash, Account string
	Generation                                                      uint64
}

// TelegramHandoffRenewal retains the original publication and a new observation.
// Completion is audit evidence, not activation authority, and never enables I/O.
type TelegramHandoffRenewal struct {
	Binding       TelegramRenewalBinding `json:"binding"`
	Cursor        TelegramRecoveryCursor `json:"cursor"`
	RenewalHash   string                 `json:"renewal_hash"`
	IntakeEnabled bool                   `json:"intake_enabled"`
}

const maxTelegramRenewals = 16
const maxTelegramRenewalPages = 4096

func (s *publishedTelegramSession) renewalByteBudget() int64 {
	limit := s.config.MaxDatabaseBytes
	if limit > 64<<20 {
		limit = 64 << 20
	}
	return limit
}

type telegramRenewalState struct {
	Binding TelegramRenewalBinding
	Pages   uint64
	Bytes   int64
	Offset  int64
	Head    string
}
type telegramRenewalPage struct {
	Ordinal  uint64
	Previous string
	Page     telegramDrainPage
}
type telegramRenewalClock struct {
	now  func() time.Time
	host func() (string, float64, error)
}

func renewalIndex(account string) string {
	return "telegram-renewal-" + protocol.Hash(account) + ".json"
}
func renewalSeed(b TelegramRenewalBinding) telegramRenewalState {
	return telegramRenewalState{Binding: b, Head: protocol.Hash(b)}
}

// RenewPublishedTelegramHandoff is a lifecycle building block, not a production
// CLI. The caller must stop all other account receivers first. This function
// holds the gateway lease, persists each page before acknowledging it, and never
// starts a receiver, changes the canonical cursor or grants command authority.
func RenewPublishedTelegramHandoff(ctx context.Context, path string, c Config, commit string, request TelegramRenewalRequest, p provider.TelegramPoller) (TelegramHandoffRenewal, error) {
	if c.Interaction == nil {
		return TelegramHandoffRenewal{}, fmt.Errorf("interactive recovery not configured")
	}
	p.Destinations = c.Interaction.Destinations
	return renewPublishedTelegramHandoff(ctx, path, c, commit, p.Profile.Account, request, p, telegramRenewalClock{time.Now, process.HostClock}, nil)
}

func renewPublishedTelegramHandoff(ctx context.Context, path string, c Config, commit, account string, request TelegramRenewalRequest, p telegramRecoveryPoller, clock telegramRenewalClock, fault func(string) error) (out TelegramHandoffRenewal, err error) {
	if !safefs.ValidID(request.ID, "n") || !safefs.DigestPattern.MatchString(request.PreviousHash) || p == nil || clock.now == nil || clock.host == nil {
		return out, fmt.Errorf("invalid Telegram renewal dependencies")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	s, err := openPublishedTelegramSession(ctx, path, c, commit, account)
	if err != nil {
		return out, err
	}
	defer s.close()
	j, err := s.openRenewal(ctx, &request, fault)
	if err != nil {
		return out, err
	}
	defer j.close()
	if j.complete != nil {
		if err = s.verify(ctx); err != nil {
			return out, err
		}
		if err = j.check(ctx); err != nil {
			return out, err
		}
		return *j.complete, nil
	}
	// An already persisted empty page is never polled again or re-dated.
	if j.summary == nil {
		if err = j.check(ctx); err != nil {
			return out, err
		}
		if err = p.Preflight(ctx); err != nil {
			return out, err
		}
		var offset int64 // restart at zero, even after a provider-side ACK/response loss
		for j.summary == nil {
			if j.priorPages+j.state.Pages >= maxTelegramRenewalPages {
				return out, fmt.Errorf("Telegram renewal page budget exhausted")
			}
			if err = j.check(ctx); err != nil {
				return out, err
			}
			if err = recoveryBoundary(fault, "telegram_renewal_before_poll"); err != nil {
				return out, err
			}
			if err = j.check(ctx); err != nil {
				return out, err
			}
			page, e := p.Poll(ctx, offset)
			if e != nil {
				return out, e
			}
			if err = recoveryBoundary(fault, "telegram_renewal_response"); err != nil {
				return out, err
			}
			if err = j.append(ctx, offset, page, clock, fault); err != nil {
				return out, err
			}
			offset = j.state.Offset
		}
	}
	out = j.result()
	if err = s.verify(ctx); err != nil {
		return TelegramHandoffRenewal{}, err
	}
	if err = j.check(ctx); err != nil {
		return TelegramHandoffRenewal{}, err
	}
	if err = j.dir.WriteJSON("complete.json", out, false); err != nil {
		return TelegramHandoffRenewal{}, err
	}
	if err = recoveryBoundary(fault, "telegram_renewal_complete"); err != nil {
		return TelegramHandoffRenewal{}, err
	}
	if err = j.check(ctx); err != nil {
		return TelegramHandoffRenewal{}, err
	}
	audit, e := s.openRenewal(ctx, nil, nil)
	if e != nil {
		return TelegramHandoffRenewal{}, e
	}
	defer audit.close()
	if audit.complete == nil || *audit.complete != out {
		return TelegramHandoffRenewal{}, fmt.Errorf("Telegram renewal completion failed independent verification")
	}
	return out, nil
}

// VerifyRenewedTelegramHandoff requires the exact latest completion hash. It
// performs no provider calls and cannot turn an expired completion into a grant.
func VerifyRenewedTelegramHandoff(ctx context.Context, path string, c Config, commit, account, renewalHash string) (PublishedTelegramHandoff, error) {
	return verifyRenewedTelegramHandoff(ctx, path, c, commit, account, renewalHash, telegramRenewalClock{time.Now, process.HostClock})
}

func verifyRenewedTelegramHandoff(ctx context.Context, path string, c Config, commit, account, renewalHash string, clock telegramRenewalClock) (out PublishedTelegramHandoff, err error) {
	if !safefs.DigestPattern.MatchString(renewalHash) || clock.now == nil || clock.host == nil {
		return out, fmt.Errorf("exact Telegram renewal hash and clock required")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	s, err := openPublishedTelegramSession(ctx, path, c, commit, account)
	if err != nil {
		return out, err
	}
	defer s.close()
	return s.verifyRenewedHandoff(ctx, renewalHash, clock)
}

func (s *publishedTelegramSession) verifyRenewedHandoff(ctx context.Context, renewalHash string, clock telegramRenewalClock) (out PublishedTelegramHandoff, err error) {
	j, err := s.openRenewal(ctx, nil, nil)
	if err != nil {
		return out, err
	}
	defer j.close()
	if j.complete == nil || j.complete.RenewalHash != renewalHash {
		return out, fmt.Errorf("Telegram renewal is incomplete or superseded")
	}
	if err = s.verify(ctx); err != nil {
		return out, err
	}
	if err = j.check(ctx); err != nil {
		return out, err
	}
	boot, up, err := clock.host()
	if err != nil {
		return out, err
	}
	at := clock.now().UTC()
	cursor := j.complete.Cursor
	if err = cursor.fresh(at, boot, up); err != nil {
		return out, err
	}
	if !at.After(cursor.NotBefore) {
		return out, fmt.Errorf("Telegram recovery cutoff has not passed")
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	return PublishedTelegramHandoff{Version: 1, CommitHash: s.publication.CommitHash, Cursor: cursor, CheckedAt: at, ExpiresAt: cursor.Drain.ObservedAt.Add(telegramHandoffLifetime), RequiresActivationRecheck: true, RenewalHash: renewalHash}, nil
}
