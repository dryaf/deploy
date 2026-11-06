package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type telegramRenewalJournal struct {
	s           *publishedTelegramSession
	parent, dir *safefs.Directory
	state       telegramRenewalState
	previous    TelegramRecoveryCursor
	priorBytes  int64
	priorPages  uint64
	seen        map[int64]string
	ids         map[string]bool
	totals      TelegramDrainSummary
	summary     *TelegramDrainSummary
	complete    *TelegramHandoffRenewal
}

func (j *telegramRenewalJournal) close() {
	if j.dir != nil {
		j.dir.Close()
	}
	if j.parent != nil {
		j.parent.Close()
	}
}

func (s *publishedTelegramSession) renewalParent(create bool) (*safefs.Directory, error) {
	if create {
		if err := s.stage.Mkdir("telegram-renewals"); err != nil {
			return nil, err
		}
	}
	p, err := s.stage.Sub("telegram-renewals")
	if err != nil {
		return nil, err
	}
	defer p.Close()
	if err = privateCheckpointDirectory(p); err != nil {
		return nil, err
	}
	account := protocol.Hash(s.cursor.Drain.Account)
	if create {
		if err = p.Mkdir(account); err != nil {
			return nil, err
		}
	}
	d, err := p.Sub(account)
	if err != nil {
		return nil, err
	}
	if err = privateCheckpointDirectory(d); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func renewalNames(d *safefs.Directory, limit int) ([]string, error) {
	scan, err := d.Sub(".")
	if err != nil {
		return nil, err
	}
	defer scan.Close()
	names, err := scan.File().Readdirnames(limit + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > limit {
		return nil, fmt.Errorf("Telegram renewal directory budget exceeded")
	}
	return names, nil
}

// WriteJSON may leave a zero-length or partial private temporary after SIGKILL.
// It is not a published page and never counts as acknowledgement evidence.
func checkRenewalTemporary(d *safefs.Directory, name string) error {
	f, err := d.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || i.Mode().Perm() != 0600 || i.Size() > protocol.MaxRequest {
		return fmt.Errorf("unsafe Telegram renewal temporary")
	}
	return nil
}

func readRenewalJSON[T any](d *safefs.Directory, name string, out *T, limit int64) (int64, error) {
	f, err := snapshotFile(d, name, limit)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return 0, err
	}
	var v T
	if err = protocol.Decode(raw, &v, int(limit)); err != nil {
		return 0, err
	}
	*out = v
	return int64(len(raw)), nil
}

func (s *publishedTelegramSession) validRenewalBinding(b TelegramRenewalBinding) error {
	if b.Version != 1 || !safefs.ValidID(b.ID, "n") || !safefs.DigestPattern.MatchString(b.PreviousHash) || b.CommitHash != s.publication.CommitHash || b.OriginalHash != protocol.Hash(s.cursor) || b.Account != s.cursor.Drain.Account || b.Generation == 0 || b.Generation > maxTelegramRenewals {
		return fmt.Errorf("Telegram renewal binding changed")
	}
	if b.Generation == 1 {
		if b.PreviousID != "" || b.PreviousHash != b.OriginalHash {
			return fmt.Errorf("Telegram renewal root changed")
		}
	} else if !safefs.ValidID(b.PreviousID, "n") || b.PreviousID == b.ID {
		return fmt.Errorf("Telegram renewal ancestry invalid")
	}
	return nil
}

func (s *publishedTelegramSession) openRenewal(ctx context.Context, request *TelegramRenewalRequest, fault func(string) error) (j *telegramRenewalJournal, err error) {
	j = &telegramRenewalJournal{s: s, seen: map[int64]string{}, ids: map[string]bool{}}
	defer func() {
		if err != nil {
			j.close()
			j = nil
		}
	}()
	_, err = readRenewalJSON(s.root, s.renewalIndex(), &j.state, 16384)
	if os.IsNotExist(err) && request != nil {
		if request.PreviousHash != protocol.Hash(s.cursor) {
			return j, fmt.Errorf("Telegram renewal predecessor mismatch")
		}
		p, e := s.renewalParent(false)
		if e == nil {
			names, e2 := renewalNames(p, 1)
			p.Close()
			if e2 != nil {
				return j, e2
			}
			if len(names) != 0 {
				return j, fmt.Errorf("Telegram renewal index missing but history exists")
			}
		} else if !os.IsNotExist(e) {
			return j, e
		}
		j.state = renewalSeed(TelegramRenewalBinding{Version: 1, ID: request.ID, PreviousHash: request.PreviousHash, CommitHash: s.publication.CommitHash, OriginalHash: protocol.Hash(s.cursor), Account: s.cursor.Drain.Account, Generation: 1})
		if err = s.check(ctx); err != nil {
			return
		}
		if err = s.root.WriteJSON(s.renewalIndex(), j.state, false); err != nil {
			return
		}
		if err = recoveryBoundary(fault, "telegram_renewal_intent"); err != nil {
			return
		}
	} else if err != nil {
		return
	}
	if err = s.validRenewalBinding(j.state.Binding); err != nil {
		return
	}
	if request != nil && request.ID == j.state.Binding.ID && request.PreviousHash != j.state.Binding.PreviousHash {
		return j, fmt.Errorf("Telegram renewal retry changed predecessor")
	}
	resume := request != nil && request.ID == j.state.Binding.ID
	if j.parent, err = s.renewalParent(resume && j.state.Pages == 0); err != nil {
		return
	}
	if resume && j.state.Pages == 0 {
		if err = j.initialize(fault); err != nil {
			return
		}
	}
	if err = j.load(ctx, j.state.Binding, true); err != nil {
		return
	}
	names, err := renewalNames(j.parent, maxTelegramRenewals)
	if err != nil {
		return
	}
	if len(names) != len(j.ids) {
		return j, fmt.Errorf("Telegram renewal history directory count changed")
	}
	for _, name := range names {
		if !j.ids[name] {
			return j, fmt.Errorf("unexpected Telegram renewal generation")
		}
	}
	if request == nil {
		return j, nil
	}
	if resume {
		// A durable page may be one step ahead of the root acknowledgement pin.
		var pin telegramRenewalState
		if _, err = readRenewalJSON(s.root, s.renewalIndex(), &pin, 16384); err != nil {
			return
		}
		if pin != j.state {
			if err = s.check(ctx); err != nil {
				return
			}
			if err = s.root.WriteJSON(s.renewalIndex(), j.state, true); err != nil {
				return
			}
		}
		return j, nil
	}
	if j.complete == nil || request.PreviousHash != j.complete.RenewalHash || j.ids[request.ID] {
		return j, fmt.Errorf("Telegram renewal requires exact completed predecessor")
	}
	if j.state.Binding.Generation >= maxTelegramRenewals || j.priorPages+j.state.Pages >= maxTelegramRenewalPages {
		return j, fmt.Errorf("Telegram renewal history budget exhausted")
	}
	if err = j.check(ctx); err != nil {
		return
	}
	j.previous = j.complete.Cursor
	j.priorBytes += j.state.Bytes
	j.priorPages += j.state.Pages
	j.state = renewalSeed(TelegramRenewalBinding{Version: 1, ID: request.ID, PreviousID: j.complete.Binding.ID, PreviousHash: request.PreviousHash, CommitHash: s.publication.CommitHash, OriginalHash: protocol.Hash(s.cursor), Account: s.cursor.Drain.Account, Generation: j.complete.Binding.Generation + 1})
	j.dir.Close()
	j.dir = nil
	j.summary = nil
	j.complete = nil
	j.totals = j.initialTotals()
	if err = s.root.WriteJSON(s.renewalIndex(), j.state, true); err != nil {
		return
	}
	if err = recoveryBoundary(fault, "telegram_renewal_intent"); err != nil {
		return
	}
	err = j.initialize(fault)
	return
}

func (j *telegramRenewalJournal) initialize(fault func(string) error) error {
	if j.state != renewalSeed(j.state.Binding) {
		return fmt.Errorf("Telegram renewal initialization lost progress")
	}
	d, err := j.parent.Sub(j.state.Binding.ID)
	if os.IsNotExist(err) {
		if err = j.parent.Mkdir(j.state.Binding.ID); err != nil {
			return err
		}
		d, err = j.parent.Sub(j.state.Binding.ID)
	}
	if err != nil {
		return err
	}
	defer d.Close()
	if err = privateCheckpointDirectory(d); err != nil {
		return err
	}
	var b TelegramRenewalBinding
	_, err = readRenewalJSON(d, "intent.json", &b, 16384)
	if os.IsNotExist(err) {
		names, e := renewalNames(d, 16)
		if e != nil {
			return e
		}
		for _, n := range names {
			if !safefs.IsJSONTemporary(n) {
				return fmt.Errorf("unowned Telegram renewal directory")
			}
			if e = checkRenewalTemporary(d, n); e != nil {
				return e
			}
		}
		err = d.WriteJSON("intent.json", j.state.Binding, false)
	} else if err == nil && b != j.state.Binding {
		return fmt.Errorf("Telegram renewal intent changed")
	}
	if err != nil {
		return err
	}
	if j.dir == nil {
		j.dir, err = j.parent.Sub(j.state.Binding.ID)
		if err != nil {
			return err
		}
	}
	return recoveryBoundary(fault, "telegram_renewal_directory")
}

func (j *telegramRenewalJournal) initialTotals() TelegramDrainSummary {
	return TelegramDrainSummary{Operation: j.s.proof.Operation, Identity: j.s.proof.After, Account: j.s.cursor.Drain.Account, HistoryHash: j.s.cursor.Drain.HistoryHash}
}

func renewalPageName(n uint64) string { return fmt.Sprintf("page-%04d.json", n) }
