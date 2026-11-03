package worker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Read every predecessor and every page, not only a cached completion. No
// missing archive is interpreted as an empty history or a new provider account.
func (j *telegramRenewalJournal) load(ctx context.Context, b TelegramRenewalBinding, current bool) error {
	if err := j.s.validRenewalBinding(b); err != nil {
		return err
	}
	if j.ids[b.ID] {
		return fmt.Errorf("Telegram renewal ancestry cycle")
	}
	j.ids[b.ID] = true
	d, err := j.parent.Sub(b.ID)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = privateCheckpointDirectory(d); err != nil {
		return err
	}
	var intent TelegramRenewalBinding
	if _, err = readRenewalJSON(d, "intent.json", &intent, 16384); err != nil {
		return err
	}
	if b != intent {
		return fmt.Errorf("Telegram renewal archive intent changed")
	}
	previous := j.s.cursor
	if b.PreviousID != "" {
		pd, e := j.parent.Sub(b.PreviousID)
		if e != nil {
			return e
		}
		var pb TelegramRenewalBinding
		_, e = readRenewalJSON(pd, "intent.json", &pb, 16384)
		pd.Close()
		if e != nil {
			return e
		}
		if pb.ID != b.PreviousID || pb.Generation+1 != b.Generation {
			return fmt.Errorf("Telegram renewal ancestry skipped")
		}
		if err = j.load(ctx, pb, false); err != nil {
			return err
		}
		if j.complete == nil || j.complete.RenewalHash != b.PreviousHash {
			return fmt.Errorf("Telegram renewal predecessor completion changed")
		}
		previous = j.complete.Cursor
	}
	names, err := renewalNames(d, maxTelegramRenewalPages+18)
	if err != nil {
		return err
	}
	pages, temps := uint64(0), 0
	for _, name := range names {
		switch {
		case name == "intent.json" || name == "complete.json":
		case safefs.IsJSONTemporary(name):
			temps++
			if temps > 16 {
				return fmt.Errorf("Telegram renewal temporary budget exceeded")
			}
			if e := checkRenewalTemporary(d, name); e != nil {
				return e
			}
		case strings.HasPrefix(name, "page-") && strings.HasSuffix(name, ".json"):
			n, e := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "page-"), ".json"), 10, 64)
			if e != nil || n == 0 || n > maxTelegramRenewalPages || renewalPageName(n) != name {
				return fmt.Errorf("invalid Telegram renewal page name")
			}
			pages++
		default:
			return fmt.Errorf("unexpected Telegram renewal artifact")
		}
	}
	if j.priorPages+pages > maxTelegramRenewalPages {
		return fmt.Errorf("Telegram renewal page budget exceeded")
	}
	state := renewalSeed(b)
	prefix := state
	totals := j.initialTotals()
	var summary *TelegramDrainSummary
	for n := uint64(1); n <= pages; n++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		var page telegramRenewalPage
		size, e := readRenewalJSON(d, renewalPageName(n), &page, protocol.MaxRequest)
		if e != nil {
			return e
		}
		if j.priorBytes+state.Bytes+size > j.s.renewalByteBudget() {
			return fmt.Errorf("Telegram renewal byte budget exceeded")
		}
		if summary != nil {
			return fmt.Errorf("Telegram renewal continued after empty page")
		}
		state, totals, summary, err = j.applyPage(ctx, state, totals, previous, page, size)
		if err != nil {
			return err
		}
		if current && n == j.state.Pages {
			prefix = state
		}
	}
	if current && (pages < j.state.Pages || pages-j.state.Pages > 1 || prefix != j.state) {
		return fmt.Errorf("Telegram renewal acknowledgement pin disagrees with archive")
	}
	if j.priorBytes+state.Bytes > j.s.renewalByteBudget() {
		return fmt.Errorf("Telegram renewal byte budget exceeded")
	}
	var completed TelegramHandoffRenewal
	_, err = readRenewalJSON(d, "complete.json", &completed, 32768)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	hasCompletion := err == nil
	if !current && !hasCompletion {
		return fmt.Errorf("Telegram renewal predecessor is incomplete")
	}
	if current && hasCompletion && state != j.state {
		return fmt.Errorf("Telegram renewal completion is ahead of acknowledgement pin")
	}
	j.summary = summary
	j.totals = totals
	j.previous = previous
	if current {
		j.state = state
		if j.dir != nil {
			j.dir.Close()
		}
		j.dir, err = j.parent.Sub(b.ID)
		if err != nil {
			return err
		}
	} else {
		j.priorBytes += state.Bytes
		j.priorPages += state.Pages
	}
	j.complete = nil
	if hasCompletion {
		want := j.resultFor(state, summary)
		if summary == nil || completed != want {
			return fmt.Errorf("Telegram renewal completion changed")
		}
		j.complete = &completed
	}
	return nil
}

func (j *telegramRenewalJournal) resultFor(state telegramRenewalState, summary *TelegramDrainSummary) TelegramHandoffRenewal {
	if summary == nil {
		return TelegramHandoffRenewal{}
	}
	cursor := TelegramRecoveryCursor{Version: 1, Drain: *summary, RequestsHash: j.s.cursor.RequestsHash, BeforeOffset: j.s.cursor.BeforeOffset, NotBefore: summary.ObservedAt.Add(telegramHandoffSkew)}
	p := TelegramHandoffRenewal{Binding: state.Binding, Cursor: cursor}
	p.RenewalHash = protocol.Hash(p)
	return p
}
func (j *telegramRenewalJournal) result() TelegramHandoffRenewal {
	return j.resultFor(j.state, j.summary)
}
