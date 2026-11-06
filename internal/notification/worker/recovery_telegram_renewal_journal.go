package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

func (j *telegramRenewalJournal) check(ctx context.Context) error {
	if err := j.s.check(ctx); err != nil {
		return err
	}
	stage, err := retainedDirectory(j.s.root, "recovery", j.s.proof.Operation)
	if err != nil {
		return err
	}
	err = sameRetainedDirectory(stage, j.s.stage)
	stage.Close()
	if err != nil {
		return err
	}
	p, err := j.s.renewalParent(false)
	if err != nil {
		return err
	}
	defer p.Close()
	if err = sameRetainedDirectory(p, j.parent); err != nil {
		return err
	}
	d, err := p.Sub(j.state.Binding.ID)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = privateCheckpointDirectory(d); err != nil {
		return err
	}
	if err = sameRetainedDirectory(d, j.dir); err != nil {
		return err
	}
	var pin telegramRenewalState
	if _, err = readRenewalJSON(j.s.root, j.s.renewalIndex(), &pin, 16384); err != nil {
		return err
	}
	if pin != j.state {
		return fmt.Errorf("Telegram renewal acknowledgement authority changed")
	}
	var binding TelegramRenewalBinding
	if _, err = readRenewalJSON(d, "intent.json", &binding, 16384); err != nil {
		return err
	}
	if binding != j.state.Binding {
		return fmt.Errorf("Telegram renewal intent changed")
	}
	if j.state.Pages > 0 {
		var last telegramRenewalPage
		if _, err = readRenewalJSON(d, renewalPageName(j.state.Pages), &last, protocol.MaxRequest); err != nil {
			return err
		}
		if protocol.Hash(last) != j.state.Head {
			return fmt.Errorf("Telegram renewal last page changed")
		}
	}
	var completed TelegramHandoffRenewal
	_, err = readRenewalJSON(d, "complete.json", &completed, 32768)
	if err == nil {
		if j.summary == nil || completed != j.result() {
			return fmt.Errorf("Telegram renewal completion changed")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if j.complete != nil {
		return fmt.Errorf("Telegram renewal completion disappeared")
	}
	return nil
}

func (j *telegramRenewalJournal) append(ctx context.Context, offset int64, page provider.IntakePage, clock telegramRenewalClock, fault func(string) error) error {
	if err := j.check(ctx); err != nil {
		return err
	}
	raw, err := json.Marshal(page)
	if err != nil || len(raw)+4096 > protocol.MaxRequest || page.Account != j.state.Binding.Account || len(page.Events) > 16 {
		return fmt.Errorf("invalid Telegram renewal response")
	}
	boot, up, err := clock.host()
	if err != nil {
		return err
	}
	observed := telegramDrainPage{Offset: offset, Next: page.Next, ReceivedHash: protocol.Hash(page), ObservedAt: clock.now().UTC(), BootID: boot, Uptime: up}
	for _, event := range page.Events {
		v, e := j.s.renewalEvent(ctx, event, page.Next)
		if e != nil {
			return e
		}
		observed.Events = append(observed.Events, v)
	}
	record := telegramRenewalPage{j.state.Pages + 1, j.state.Head, observed}
	raw, err = json.Marshal(record)
	if err != nil || len(raw) > protocol.MaxRequest {
		return fmt.Errorf("Telegram renewal page exceeds bound")
	}
	next, totals, summary, err := j.applyPage(ctx, j.state, j.totals, j.previous, record, int64(len(raw)))
	if err != nil {
		return err
	}
	if j.priorBytes+next.Bytes > j.s.renewalByteBudget() {
		return fmt.Errorf("Telegram renewal byte budget exhausted")
	}
	if err = j.dir.Capacity(j.s.config.MinFreeBytes + int64(len(raw))*3 + (1 << 20)); err != nil {
		return err
	}
	if err = j.check(ctx); err != nil {
		return err
	}
	if err = j.dir.WriteJSON(renewalPageName(next.Pages), record, false); err != nil {
		return err
	}
	if err = recoveryBoundary(fault, "telegram_renewal_page"); err != nil {
		return err
	}
	if err = j.check(ctx); err != nil {
		return err
	}
	if err = j.s.root.WriteJSON(j.s.renewalIndex(), next, true); err != nil {
		return err
	}
	j.state = next
	j.totals = totals
	j.summary = summary
	return recoveryBoundary(fault, "telegram_renewal_checkpoint")
}
