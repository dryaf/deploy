package worker

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

// Classify against retained canonical identities without changing provider_inbox,
// command mappings, request seals or any execution/confirmation authority.
func (s *publishedTelegramSession) renewalEvent(ctx context.Context, e provider.IntakeEvent, next int64) (v telegramDrainEvent, err error) {
	if (e.Inbound == nil) == (e.Rejection == "") {
		return v, fmt.Errorf("invalid Telegram renewal event disposition")
	}
	check := telegramRecoveryRecord{ID: e.ID, Hash: e.Hash, State: "pending", Inbound: e.Inbound}
	if e.Inbound == nil {
		check.State = "rejected"
	}
	if err = check.validate(s.cursor.Drain.Account, next); err != nil {
		return
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return
	}
	defer tx.Rollback()
	var hash, raw string
	err = tx.QueryRowContext(ctx, "SELECT hash,body FROM provider_inbox WHERE account=? AND id=?", s.cursor.Drain.Account, e.ID).Scan(&hash, &raw)
	if err == nil {
		if hash != e.Hash {
			return v, fmt.Errorf("Telegram renewal conflicts with retained native identity")
		}
		if raw != "" {
			var before interaction.Inbound
			if err = protocol.Decode([]byte(raw), &before, 8192); err != nil {
				return
			}
			if e.Inbound == nil || protocol.Hash(before) != protocol.Hash(e.Inbound) {
				return v, fmt.Errorf("Telegram renewal changed retained input")
			}
		} else {
			e.Inbound = nil
			e.Rejection = "retained_rejection"
		}
	} else if err == sql.ErrNoRows {
		if e.Inbound != nil {
			allowed, destination := false, false
			for _, p := range s.config.Interaction.Permissions {
				if p.Actor == e.Inbound.Actor && p.Conversation == e.Inbound.Conversation {
					allowed = true
				}
			}
			for _, d := range s.config.Interaction.Destinations {
				if d.Provider == "telegram" && d.Account == s.cursor.Drain.Account && d.ID == e.Inbound.Conversation && d.Fingerprint() == e.Inbound.DestinationFingerprint {
					destination = true
				}
			}
			if !allowed || !destination {
				e.Inbound = nil
				e.Rejection = "recovery_not_enrolled"
			}
		}
	} else {
		return
	}
	v = telegramDrainEvent{Event: e, Disposition: "rejected"}
	if e.Inbound != nil {
		v.Disposition = "unmapped"
		m, e2 := readRetainedRequest(tx, *e.Inbound)
		if e2 != nil {
			return v, e2
		}
		if m != nil {
			v.CommandID = m.ID
			v.Disposition = "mapped"
			if m.Query != nil && m.Query.Kind == "rejected" {
				v.Disposition = "rejected"
			}
		}
	}
	return v, nil
}

func (j *telegramRenewalJournal) applyPage(ctx context.Context, state telegramRenewalState, totals TelegramDrainSummary, previous TelegramRecoveryCursor, record telegramRenewalPage, size int64) (next telegramRenewalState, sum TelegramDrainSummary, done *TelegramDrainSummary, err error) {
	next = state
	sum = totals
	p := record.Page
	if record.Ordinal != state.Pages+1 || record.Previous != state.Head || len(p.Events) > 16 || p.Offset < 0 || (p.Offset != 0 && p.Offset != state.Offset) || p.Next < p.Offset || p.ObservedAt.IsZero() || p.ObservedAt.Before(previous.Drain.ObservedAt) || !protocol.ValidToken(p.BootID) || p.Uptime < 0 || math.IsNaN(p.Uptime) || math.IsInf(p.Uptime, 0) || !safefs.DigestPattern.MatchString(p.ReceivedHash) {
		return next, sum, nil, fmt.Errorf("Telegram renewal page chain invalid")
	}
	expected := p.Offset
	for _, v := range p.Events {
		if v.Event.ID < expected || v.Event.ID >= math.MaxInt64 {
			return next, sum, nil, fmt.Errorf("Telegram renewal update order invalid")
		}
		expected = v.Event.ID + 1
		actual, e := j.s.renewalEvent(ctx, v.Event, p.Next)
		if e != nil {
			return next, sum, nil, e
		}
		if protocol.Hash(actual) != protocol.Hash(v) {
			return next, sum, nil, fmt.Errorf("Telegram renewal retained mapping changed")
		}
		if old, exists := j.seen[v.Event.ID]; exists && old != protocol.Hash(v) {
			return next, sum, nil, fmt.Errorf("Telegram renewal native identity reused with different evidence")
		}
		j.seen[v.Event.ID] = protocol.Hash(v)
		sum.ObservedEvents++
		switch v.Disposition {
		case "mapped":
			sum.Mapped++
		case "unmapped":
			sum.Unmapped++
		case "rejected":
			sum.Rejected++
		default:
			return next, sum, nil, fmt.Errorf("Telegram renewal disposition invalid")
		}
	}
	if p.Next != expected {
		return next, sum, nil, fmt.Errorf("Telegram renewal offset skips evidence")
	}
	next.Pages++
	next.Bytes += size
	next.Offset = p.Next
	next.Head = protocol.Hash(record)
	if len(p.Events) == 0 {
		sum.Pages = next.Pages
		sum.NextOffset = next.Offset
		sum.EvidenceDigest = next.Head
		sum.ObservedAt = p.ObservedAt
		sum.BootID = p.BootID
		sum.Uptime = p.Uptime
		done = &sum
		candidate := j.resultFor(next, done)
		if err = candidate.Cursor.Validate(); err != nil {
			return next, sum, nil, err
		}
	}
	return
}
