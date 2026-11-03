package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

// An empty provider response is a dated observation, not proof of complete
// history or authorization to activate intake. Neither operational cursor moves.
type TelegramDrainSummary struct {
	Operation, Account, HistoryHash, EvidenceDigest   string
	Identity                                          protocol.Identity
	Pages, ObservedEvents, Mapped, Unmapped, Rejected uint64
	NextOffset                                        int64
	ObservedAt                                        time.Time
	BootID                                            string
	Uptime                                            float64
	HistoryComplete, IntakeEnabled                    bool
}
type telegramDrainState struct {
	Binding                    recoveryBinding
	Account, HistoryHash, Head string
	Pages                      uint64
	Offset                     int64
	Summary                    *TelegramDrainSummary
}
type telegramDrainEvent struct {
	Event                  provider.IntakeEvent
	CommandID, Disposition string
}
type telegramDrainPage struct {
	Offset, Next int64
	ReceivedHash string
	Events       []telegramDrainEvent
	ObservedAt   time.Time
	BootID       string
	Uptime       float64
}
type telegramRecoveryPoller interface {
	Preflight(context.Context) error
	Poll(context.Context, int64) (provider.IntakePage, error)
}

// DrainTelegramRecovery is an internal recovery building block. It makes real
// provider requests when invoked, so managed lifecycle code must stop all other
// receivers first. The exclusive worker lease fences this installation's normal
// receiver. No production CLI or automatic recovery activation invokes it.
func DrainTelegramRecovery(ctx context.Context, path string, c Config, p provider.TelegramPoller) (TelegramDrainSummary, error) {
	// Normalize against the recovery-bound configuration, never caller routing.
	if c.Interaction == nil {
		return TelegramDrainSummary{}, fmt.Errorf("interactive recovery not configured")
	}
	p.Destinations = c.Interaction.Destinations
	return drainTelegramRecovery(ctx, path, c, p.Profile.Account, p, nil)
}
func drainTelegramRecovery(ctx context.Context, path string, c Config, account string, p telegramRecoveryPoller, fault func(string) error) (summary TelegramDrainSummary, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if p == nil {
		return summary, fmt.Errorf("Telegram recovery poller missing")
	}
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return summary, err
	}
	defer r.Close()
	s, err := r.openTelegramDrain(ctx, account)
	if err != nil {
		return summary, err
	}
	if s.Summary != nil {
		return *s.Summary, nil
	}
	if err = p.Preflight(ctx); err != nil {
		return summary, err
	}
	// Every invocation starts conservatively at zero, including after a long
	// interruption. A retained high offset cannot acknowledge unseen provider
	// history, and idle-period native-ID resets cannot be skipped on resume.
	var offset int64
	for s.Pages < 4096 {
		if err = ctx.Err(); err != nil {
			return summary, err
		}
		if err = recoveryBoundary(fault, "telegram_drain_before_poll"); err != nil {
			return summary, err
		}
		page, e := p.Poll(ctx, offset)
		if e != nil {
			return summary, e
		}
		if err = recoveryBoundary(fault, "telegram_drain_response"); err != nil {
			return summary, err
		}
		s, err = r.commitTelegramDrain(ctx, s, offset, page, fault)
		if err != nil {
			return summary, err
		}
		if err = r.stage.WriteJSON(telegramDrainFile(account), s, true); err != nil {
			return summary, err
		}
		if err = recoveryBoundary(fault, "telegram_drain_checkpoint"); err != nil {
			return summary, err
		}
		if s.Summary != nil {
			return *s.Summary, nil
		}
		offset = s.Offset
	}
	return summary, fmt.Errorf("Telegram recovery page budget exhausted; intake remains fenced")
}
func readTelegramDrain(tx *sql.Tx, account string) (s telegramDrainState, err error) {
	var body string
	if err = tx.QueryRow("SELECT body FROM reconciliation_telegram_drains WHERE account=?", account).Scan(&body); err != nil {
		return
	}
	err = protocol.Decode([]byte(body), &s, 16384)
	return
}
func writeTelegramDrain(tx *sql.Tx, s telegramDrainState) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO reconciliation_telegram_drains VALUES(?,?) ON CONFLICT(account) DO UPDATE SET body=excluded.body", s.Account, string(body))
	return err
}
func telegramDrainHead(previous string, p telegramDrainPage) string {
	return protocol.Hash(struct {
		Previous string
		Page     telegramDrainPage
	}{previous, p})
}
func advanceTelegramDrain(s telegramDrainState, p telegramDrainPage) telegramDrainState {
	s.Pages++
	s.Offset = p.Next
	s.Head = telegramDrainHead(s.Head, p)
	return s
}
func (r *recoveryReconciler) commitTelegramDrain(ctx context.Context, prior telegramDrainState, offset int64, page provider.IntakePage, fault func(string) error) (next telegramDrainState, err error) {
	raw, e := json.Marshal(page)
	if e != nil || len(raw)+4096 > protocol.MaxRequest || len(page.Events) > 16 || page.Account != prior.Account ||
		offset < 0 || (offset != 0 && offset != prior.Offset) || page.Next < offset {
		return next, fmt.Errorf("invalid Telegram recovery poll response")
	}
	expected := offset
	for _, event := range page.Events {
		if event.ID < expected || event.ID >= 1<<63-1 || (event.Inbound == nil) == (event.Rejection == "") {
			return next, fmt.Errorf("invalid Telegram recovery update order")
		}
		expected = event.ID + 1
	}
	if page.Next != expected {
		return next, fmt.Errorf("Telegram recovery cursor skips evidence")
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := readTelegramDrain(tx, prior.Account)
		if err != nil {
			return err
		}
		if protocol.Hash(s) != protocol.Hash(prior) || s.Summary != nil || s.Pages >= 4096 {
			return fmt.Errorf("Telegram recovery poll session changed")
		}
		if err = r.capacity(tx, len(raw)+16384); err != nil {
			return err
		}
		boot, up, err := r.recoveryClock()
		if err != nil {
			return err
		}
		observed := telegramDrainPage{Offset: offset, Next: page.Next, ReceivedHash: protocol.Hash(page), ObservedAt: r.now().UTC(), BootID: boot, Uptime: up}
		for _, event := range page.Events {
			v, err := r.quarantineTelegramReplay(tx, prior.Account, page.Next, event)
			if err != nil {
				return err
			}
			observed.Events = append(observed.Events, v)
		}
		next = advanceTelegramDrain(s, observed)
		body, _ := json.Marshal(observed)
		if _, err = tx.Exec("INSERT INTO reconciliation_telegram_drain_pages VALUES(?,?,?,?,?)", s.Account, next.Pages, s.Head, next.Head, string(body)); err != nil {
			return err
		}
		if len(page.Events) == 0 {
			// Recompute completion from all retained pages, including their inbox
			// evidence, rather than relying on mutable cached counters.
			verified, _, err := r.verifyTelegramDrain(tx, next, next.Pages)
			if err != nil {
				return err
			}
			next = verified
		}
		if err = writeTelegramDrain(tx, next); err != nil {
			return err
		}
		return recoveryBoundary(fault, "telegram_drain_transaction")
	})
	if err == nil {
		err = recoveryBoundary(fault, "telegram_drain_commit")
	}
	return
}
